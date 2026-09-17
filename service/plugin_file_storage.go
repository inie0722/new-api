package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
)

// Object names carry immutable expiry and 128 random bits; never use client filenames.
var pluginFileObjectPattern = regexp.MustCompile(`^([0-9]{10})-[0-9a-f]{32}$`)
var pluginFileCleanupOnce sync.Once

func pluginFileExpiry(name string) (time.Time, bool) {
	match := pluginFileObjectPattern.FindStringSubmatch(name)
	if match == nil {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(match[1], 10, 64)
	return time.Unix(seconds, 0), err == nil
}

func pluginFileSignature(object, signingKey string) string {
	mac := hmac.New(sha256.New, []byte(signingKey))
	_, _ = mac.Write([]byte("plugin-input-file:v1\x00" + object))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func VerifyPluginFileAccess(object, signature string, now time.Time) bool {
	expires, valid := pluginFileExpiry(object)
	signingKey := system_setting.GetPluginFileStorage().SigningKey
	if !valid || !now.Before(expires) || len(signingKey) != 64 || len(signature) != 43 {
		return false
	}
	return hmac.Equal([]byte(signature), []byte(pluginFileSignature(object, signingKey)))
}

func PluginFilePublicAddress() string {
	address := strings.TrimSpace(system_setting.TaskPublicAddress)
	if address == "" {
		address = strings.TrimSpace(system_setting.ServerAddress)
	}
	return address
}

func pluginFileS3Client(config system_setting.PluginFileStorageConfig) *s3.Client {
	return s3.New(s3.Options{
		Region: config.Region, BaseEndpoint: aws.String(config.Endpoint), UsePathStyle: config.PathStyle,
		Credentials: credentials.NewStaticCredentialsProvider(config.AccessKey, config.SecretKey, ""),
		HTTPClient:  &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
}

// A fixed sub-prefix prevents cleanup from deleting unrelated bucket objects.
func pluginFileS3Prefix(config system_setting.PluginFileStorageConfig) string {
	return path.Join(config.Prefix, "plugin-input-files-v1") + "/"
}

// StorePluginFile only receives host-owned request bytes. It validates actual size
// while streaming to disk, and publishes a URL only after storage has succeeded.
func StorePluginFile(ctx context.Context, config system_setting.PluginFileStorageConfig, content io.Reader, mimeType string, maxBytes int64) (string, int64, error) {
	if config.Mode == "disabled" {
		return "", 0, errors.New("plugin file storage is disabled")
	}
	if err := config.Validate(); err != nil {
		return "", 0, err
	}
	if maxBytes <= 0 {
		return "", 0, errors.New("plugin file exceeds the request byte limit")
	}
	var baseURL *url.URL
	if config.Mode == "local" {
		address := PluginFilePublicAddress()
		if err := ValidateTaskArtifactBaseURL(address); err != nil {
			return "", 0, err
		}
		baseURL, _ = url.Parse(address)
	}
	// Keep active content inert, including files with a misleading part Content-Type.
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil || (!strings.HasPrefix(mediaType, "image/") && !strings.HasPrefix(mediaType, "audio/") && !strings.HasPrefix(mediaType, "video/")) || mediaType == "image/svg+xml" {
		mediaType = "application/octet-stream"
	}
	directory := filepath.Join(os.TempDir(), "new-api-plugin-files-staging")
	if config.Mode == "local" {
		directory = config.LocalDirectory
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", 0, errors.New("cannot create plugin file storage directory")
	}
	staging, err := os.MkdirTemp(directory, ".plugin-file-pending-")
	if err != nil {
		return "", 0, errors.New("cannot stage plugin file")
	}
	defer os.RemoveAll(staging)
	file, err := os.OpenFile(filepath.Join(staging, "content"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", 0, errors.New("cannot stage plugin file")
	}
	defer file.Close()
	// ContextReader checks cancellation between reads, including local copies.
	size, err := io.Copy(file, io.LimitReader(&pluginFileContextReader{ctx: ctx, reader: content}, maxBytes+1))
	if err != nil {
		return "", 0, errors.New("cannot read plugin file")
	}
	if size > maxBytes {
		return "", 0, fmt.Errorf("plugin file exceeds the %d byte limit", maxBytes)
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	expires := time.Now().Add(time.Duration(config.TTLHours) * time.Hour).Truncate(time.Second)
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", 0, err
	}
	object := strconv.FormatInt(expires.Unix(), 10) + "-" + hex.EncodeToString(random)
	if config.Mode == "s3" {
		client := pluginFileS3Client(config)
		key := pluginFileS3Prefix(config) + object
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(config.Bucket), Key: aws.String(key), Body: file, ContentLength: aws.Int64(size),
			ContentType: aws.String(mediaType), ContentDisposition: aws.String("attachment"), CacheControl: aws.String("no-store"),
		})
		if err != nil {
			return "", 0, errors.New("plugin file S3 upload failed")
		}
		signed, err := s3.NewPresignClient(client).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(config.Bucket), Key: aws.String(key)}, func(options *s3.PresignOptions) { options.Expires = time.Until(expires) })
		if err != nil {
			return "", 0, errors.New("plugin file S3 URL signing failed")
		}
		return signed.URL, size, nil
	}
	if err := file.Sync(); err != nil {
		return "", 0, errors.New("cannot persist plugin file")
	}
	if err := file.Close(); err != nil {
		return "", 0, errors.New("cannot persist plugin file")
	}
	if err := os.WriteFile(filepath.Join(staging, "type"), []byte(mediaType), 0600); err != nil {
		return "", 0, errors.New("cannot persist plugin file metadata")
	}
	if err := os.Rename(staging, filepath.Join(directory, object)); err != nil {
		return "", 0, errors.New("cannot publish plugin file")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + "/v1/plugin-files/" + object + "/content"
	baseURL.RawPath = ""
	query := baseURL.Query()
	query.Set("access", pluginFileSignature(object, config.SigningKey))
	baseURL.RawQuery = query.Encode()
	return baseURL.String(), size, nil
}

type pluginFileContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *pluginFileContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// Access must be checked by the download middleware before opening any path.
func ServePluginFile(c *gin.Context) {
	config := system_setting.GetPluginFileStorage()
	object := c.Param("object")
	if _, ok := pluginFileExpiry(object); !ok || config.LocalDirectory == "" {
		c.Status(http.StatusNotFound)
		return
	}
	root, err := os.OpenRoot(config.LocalDirectory)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	defer root.Close()
	file, err := root.Open(object + "/content")
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		c.Status(http.StatusNotFound)
		return
	}
	mimeType := "application/octet-stream"
	metadata, err := root.Open(object + "/type")
	if err == nil {
		value, readErr := io.ReadAll(io.LimitReader(metadata, 256))
		metadata.Close()
		if readErr == nil {
			mimeType = string(value)
		}
	}
	c.Header("Content-Type", mimeType)
	c.Header("Content-Disposition", "attachment")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "sandbox; default-src 'none'")
	c.Header("Cache-Control", "private, no-store")
	c.Header("Referrer-Policy", "no-referrer")
	http.ServeContent(c.Writer, c.Request, "content", stat.ModTime(), file)
}

func cleanupPluginFileDirectory(ctx context.Context, directory string, now time.Time) error {
	var failures []error
	if directory != "" {
		root, err := os.OpenRoot(directory)
		if err != nil && !os.IsNotExist(err) {
			failures = append(failures, errors.New("plugin file local cleanup failed"))
		}
		if err == nil {
			defer root.Close()
			dir, openErr := root.Open(".")
			if openErr != nil {
				return errors.New("plugin file local cleanup failed")
			}
			defer dir.Close()
			for {
				entries, readErr := dir.ReadDir(128)
				for _, entry := range entries {
					if err := ctx.Err(); err != nil {
						return err
					}
					expiry, valid := pluginFileExpiry(entry.Name())
					expired := valid && !now.Before(expiry)
					if strings.HasPrefix(entry.Name(), ".plugin-file-pending-") {
						info, statErr := entry.Info()
						expired = statErr == nil && now.Sub(info.ModTime()) > 24*time.Hour
					}
					if expired {
						if err := root.RemoveAll(entry.Name()); err != nil {
							failures = append(failures, errors.New("plugin file local deletion failed"))
						}
					}
				}
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					failures = append(failures, errors.New("plugin file local listing failed"))
					break
				}
			}
		}
	}
	return errors.Join(failures...)
}

// CleanupPluginFiles only removes objects owned by this feature. Disabling new
// uploads still allows local cleanup; retained S3 settings allow S3 cleanup too.
func CleanupPluginFiles(ctx context.Context, config system_setting.PluginFileStorageConfig, now time.Time) error {
	var failures []error
	failures = append(failures, cleanupPluginFileDirectory(ctx, config.LocalDirectory, now))
	failures = append(failures, cleanupPluginFileDirectory(ctx, filepath.Join(os.TempDir(), "new-api-plugin-files-staging"), now))
	s3Config := config
	s3Config.Mode = "s3"
	if s3Config.Validate() == nil {
		client := pluginFileS3Client(config)
		prefix := pluginFileS3Prefix(config)
		pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(config.Bucket), Prefix: aws.String(prefix)})
		for pages.HasMorePages() {
			page, err := pages.NextPage(ctx)
			if err != nil {
				failures = append(failures, errors.New("plugin file S3 listing failed"))
				break
			}
			for _, item := range page.Contents {
				key := aws.ToString(item.Key)
				object, ok := strings.CutPrefix(key, prefix)
				expiry, valid := pluginFileExpiry(object)
				if !ok || !valid || now.Before(expiry) {
					continue
				}
				if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(config.Bucket), Key: item.Key}); err != nil {
					failures = append(failures, errors.New("plugin file S3 deletion failed"))
				}
			}
		}
	}
	return errors.Join(failures...)
}

func StartPluginFileCleanup() {
	pluginFileCleanupOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				err := CleanupPluginFiles(ctx, system_setting.GetPluginFileStorage(), time.Now())
				cancel()
				if err != nil {
					common.SysError(err.Error())
				}
				<-ticker.C
			}
		}()
	})
}
