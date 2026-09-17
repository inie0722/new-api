package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	pluginadaptor "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func pluginFileConfigFixture(t *testing.T) system_setting.PluginFileStorageConfig {
	t.Helper()
	previous := system_setting.GetPluginFileStorage()
	secret, public, server := common.CryptoSecret, system_setting.TaskPublicAddress, system_setting.ServerAddress
	common.CryptoSecret = "plugin-file-test-signing-secret"
	system_setting.TaskPublicAddress = "https://files.example.test/base"
	system_setting.ServerAddress = "https://fallback.example.test"
	t.Cleanup(func() {
		value, err := common.Marshal(previous)
		require.NoError(t, err)
		require.NoError(t, system_setting.SetPluginFileStorage(string(value)))
		common.CryptoSecret, system_setting.TaskPublicAddress, system_setting.ServerAddress = secret, public, server
	})
	config := system_setting.PluginFileStorageConfig{SigningKey: strings.Repeat("ab", 32), Mode: "local", TTLHours: 24, LocalDirectory: t.TempDir(), Region: "us-east-1", PathStyle: true}
	setPluginFileConfig(t, config)
	return config
}

func setPluginFileConfig(t *testing.T, config system_setting.PluginFileStorageConfig) {
	t.Helper()
	value, err := common.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, system_setting.SetPluginFileStorage(string(value)))
}

func pluginFileDownloadRouter() http.Handler {
	router := gin.New()
	middleware.SetUpLogger(router)
	router.GET("/v1/plugin-files/:object/content", middleware.PluginFileAccess(), service.ServePluginFile)
	router.HEAD("/v1/plugin-files/:object/content", middleware.PluginFileAccess(), service.ServePluginFile)
	return http.StripPrefix("/base", router)
}

func TestPluginFilesLocalAccessAndCleanup(t *testing.T) {
	config := pluginFileConfigFixture(t)
	var accessLogs bytes.Buffer
	previousWriter := gin.DefaultWriter
	gin.DefaultWriter = &accessLogs
	t.Cleanup(func() { gin.DefaultWriter = previousWriter })
	// A base path is retained; a spoofed request Host is never used in issuance.
	fileURL, size, err := service.StorePluginFile(t.Context(), config, strings.NewReader("reference-image"), "image/png", 64)
	require.NoError(t, err)
	assert.EqualValues(t, 15, size)
	parsed, err := url.Parse(fileURL)
	require.NoError(t, err)
	assert.Equal(t, "files.example.test", parsed.Host)
	object := strings.Split(parsed.Path, "/")[4]
	signature := parsed.Query().Get("access")
	assert.True(t, service.VerifyPluginFileAccess(object, signature, time.Now()))
	assert.False(t, service.VerifyPluginFileAccess(object, signature, time.Now().Add(25*time.Hour)))

	router := pluginFileDownloadRouter()
	for _, tc := range []struct {
		name, method, query, rangeHeader string
		status                           int
		body                             string
	}{
		{"get", "GET", parsed.RawQuery, "", 200, "reference-image"},
		{"head", "HEAD", parsed.RawQuery, "", 200, ""},
		{"range", "GET", parsed.RawQuery, "bytes=0-8", 206, "reference"},
		{"missing signature", "GET", "", "", 404, ""},
		{"tampered signature", "GET", "access=" + strings.Repeat("a", 43), "", 404, ""},
		{"duplicate signature", "GET", parsed.RawQuery + "&" + parsed.RawQuery, "", 404, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, parsed.Path+"?"+tc.query, nil)
			request.Header.Set("Range", tc.rangeHeader)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			assert.Equal(t, tc.status, response.Code)
			assert.Equal(t, tc.body, response.Body.String())
			if tc.status < 300 {
				assert.Equal(t, "attachment", response.Header().Get("Content-Disposition"))
				assert.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
			}
		})
	}
	// Reconstructing configuration is sufficient after a process restart. Disabling
	// new uploads must not revoke already-issued links or stop local cleanup.
	config.Mode = "disabled"
	setPluginFileConfig(t, config)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", fileURL, nil))
	assert.Equal(t, 200, response.Code)
	// A signed object cannot be used to follow a symlink outside the storage root.
	outside := filepath.Join(t.TempDir(), "private")
	require.NoError(t, os.WriteFile(outside, []byte("private-server-data"), 0600))
	require.NoError(t, os.Remove(filepath.Join(config.LocalDirectory, object, "content")))
	require.NoError(t, os.Symlink(outside, filepath.Join(config.LocalDirectory, object, "content")))
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", fileURL, nil))
	assert.Equal(t, 404, response.Code)
	assert.NotContains(t, response.Body.String(), "private-server-data")
	assert.False(t, service.VerifyPluginFileAccess("../"+object, signature, time.Now()))
	require.NoError(t, os.WriteFile(filepath.Join(config.LocalDirectory, "unrelated"), []byte("keep"), 0600))
	require.NoError(t, service.CleanupPluginFiles(t.Context(), config, time.Now().Add(25*time.Hour)))
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", fileURL, nil))
	assert.Equal(t, 404, response.Code)
	assert.NotContains(t, accessLogs.String(), signature)
	kept, err := os.ReadFile(filepath.Join(config.LocalDirectory, "unrelated"))
	require.NoError(t, err)
	assert.Equal(t, "keep", string(kept))
	private, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, "private-server-data", string(private))
}

func TestPluginFilesRejectIncompleteWrites(t *testing.T) {
	config := pluginFileConfigFixture(t)
	fileURL, _, err := service.StorePluginFile(t.Context(), config, strings.NewReader("oversized"), "text/html", 3)
	require.ErrorContains(t, err, "3 byte limit")
	assert.Empty(t, fileURL)
	entries, err := os.ReadDir(config.LocalDirectory)
	require.NoError(t, err)
	assert.Empty(t, entries)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fileURL, _, err = service.StorePluginFile(ctx, config, strings.NewReader("data"), "image/png", 64)
	require.Error(t, err)
	assert.Empty(t, fileURL)
	config.Mode = "disabled"
	_, _, err = service.StorePluginFile(t.Context(), config, strings.NewReader("data"), "image/png", 64)
	require.ErrorContains(t, err, "disabled")
}

func TestPluginFilesPlaceholderToURLOnlyUpstream(t *testing.T) {
	config := pluginFileConfigFixture(t)
	files := httptest.NewServer(pluginFileDownloadRouter())
	defer files.Close()
	system_setting.TaskPublicAddress = files.URL + "/base"
	source := `
export const meta={apiVersion:1,key:"file-url-test",name:"File URL test",version:"1.0.0",author:{name:"Test"},models:["m"],fetchMode:"per_task",requiredCapabilities:["file-url@1"]};
export function buildSubmitRequest(ctx){return {url:ctx.baseUrl+"/submit",body:{images:[{url:{__fileRef:ctx.files[0].ref,encoding:"url"}},{url:{__fileRef:ctx.files[0].ref,encoding:"url",maxBytes:64}}]}}}
export function parseSubmitResponse(){return {taskId:"1"}}
export function buildQueryRequest(){return {url:"https://example.com"}}
export function parseTaskResult(){return {status:"SUCCESS"}}
`
	plugin, err := pluginruntime.NewRegistry().Register(source, pluginruntime.Options{})
	require.NoError(t, err)
	var received []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Images []struct {
				URL string `json:"url"`
			} `json:"images"`
		}
		require.NoError(t, common.DecodeJson(r.Body, &body))
		require.Len(t, body.Images, 2)
		assert.Equal(t, body.Images[0].URL, body.Images[1].URL)
		for _, image := range body.Images {
			response, err := http.Get(image.URL)
			require.NoError(t, err)
			content, err := io.ReadAll(response.Body)
			response.Body.Close()
			require.NoError(t, err)
			assert.Equal(t, 200, response.StatusCode)
			received = append(received, string(content))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1"}`))
	}))
	defer upstream.Close()
	adaptor := pluginadaptor.New(plugin)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: upstream.URL}, TaskRelayInfo: &relaycommon.TaskRelayInfo{}}
	adaptor.Init(info)
	var inbound bytes.Buffer
	writer := multipart.NewWriter(&inbound)
	part, err := writer.CreateFormFile("image", "input.png")
	require.NoError(t, err)
	_, err = part.Write([]byte("image-bytes"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/submit", bytes.NewReader(inbound.Bytes()))
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	c.Set("task_request", map[string]any{"model": "m"})
	body, err := adaptor.BuildRequestBody(c, info)
	require.NoError(t, err)
	response, err := http.Post(upstream.URL+"/submit", "application/json", body)
	require.NoError(t, err)
	response.Body.Close()
	assert.Equal(t, []string{"image-bytes", "image-bytes"}, received)
	entries, err := os.ReadDir(config.LocalDirectory)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestPluginFileSettingsPersistAcrossDatabases(t *testing.T) {
	config := pluginFileConfigFixture(t)
	previousDB := model.DB
	previousOptions := common.OptionMap
	t.Cleanup(func() { model.DB = previousDB; common.OptionMap = previousOptions })
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "settings.db"))
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			model.DB = db
			var version string
			versionSQL := "SELECT version()"
			if dialect == "sqlite" {
				versionSQL = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionSQL).Scan(&version).Error)
			t.Logf("%s version: %s", dialect, version)
			common.OptionMap = map[string]string{}
			require.NoError(t, db.AutoMigrate(&model.Option{}))
			value, err := common.Marshal(config)
			require.NoError(t, err)
			require.NoError(t, model.UpdateOption(system_setting.PluginFileStorageOption, string(value)))
			config.TTLHours = 48
			value, err = common.Marshal(config)
			require.NoError(t, err)
			require.NoError(t, model.UpdateOption(system_setting.PluginFileStorageOption, string(value)))
			// Existing rows survive repeated startup migration and options reload.
			require.NoError(t, db.AutoMigrate(&model.Option{}))
			var stored model.Option
			require.NoError(t, db.Where(&model.Option{Key: system_setting.PluginFileStorageOption}).First(&stored).Error)
			require.NoError(t, system_setting.SetPluginFileStorage(stored.Value))
			assert.Equal(t, config, system_setting.GetPluginFileStorage())
			invalid := config
			invalid.TTLHours = 0
			invalidValue, err := common.Marshal(invalid)
			require.NoError(t, err)
			require.Error(t, model.UpdateOption(system_setting.PluginFileStorageOption, string(invalidValue)))
			require.NoError(t, db.Where(&model.Option{Key: system_setting.PluginFileStorageOption}).First(&stored).Error)
			assert.Equal(t, string(value), stored.Value)
			assert.Equal(t, config, system_setting.GetPluginFileStorage())
		})
	}
}

func TestPluginFileSettingsRedactionAndAuthorization(t *testing.T) {
	config := pluginFileConfigFixture(t)
	user, token := setupAccessTokenAudit(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Option{}))
	previousOptions := common.OptionMap
	common.OptionMap = map[string]string{}
	t.Cleanup(func() { common.OptionMap = previousOptions })
	config.AccessKey = "private-access-key"
	config.SecretKey = "private-secret-key"
	config.Mode = "disabled"
	config.SigningKey = ""
	setPluginFileConfig(t, config)
	router := gin.New()
	router.GET("/api/option/plugin_file_storage", middleware.RootAuth(), GetPluginFileStorage)
	router.PUT("/api/option/plugin_file_storage", middleware.RootAuth(), middleware.SessionCookieOriginGuard(), UpdatePluginFileStorage)
	for _, method := range []string{"GET", "PUT"} {
		assert.Equal(t, 401, auditRequest(router, method, "/api/option/plugin_file_storage", "").Code)
		assert.Equal(t, 403, auditRequest(router, method, "/api/option/plugin_file_storage", token).Code)
	}
	// Exercise the root handler with a verified identity after testing the real
	// authorization middleware above. Ordinary administrators cannot reach it.
	authenticated := gin.New()
	authenticated.Use(func(c *gin.Context) { c.Set("id", user.Id); c.Set("role", common.RoleRootUser); c.Next() })
	authenticated.GET("/settings", GetPluginFileStorage)
	authenticated.PUT("/settings", UpdatePluginFileStorage)
	authenticated.PUT("/generic", UpdateOption)
	config.AccessKey = ""
	config.SecretKey = ""
	config.Mode = "local"
	config.TTLHours = 72
	value, err := common.Marshal(config)
	require.NoError(t, err)
	response := httptest.NewRecorder()
	authenticated.ServeHTTP(response, httptest.NewRequest("PUT", "/settings", bytes.NewReader(value)))
	require.Contains(t, response.Body.String(), `"success":true`)
	assert.NotContains(t, response.Body.String(), "private-access-key")
	assert.NotContains(t, response.Body.String(), "private-secret-key")
	assert.NotContains(t, response.Body.String(), strings.Repeat("ab", 32))
	actual := system_setting.GetPluginFileStorage()
	assert.Equal(t, 72, actual.TTLHours)
	require.Len(t, actual.SigningKey, 64)
	assert.NotContains(t, response.Body.String(), actual.SigningKey)
	localURL, _, err := service.StorePluginFile(t.Context(), actual, strings.NewReader("retained"), "image/png", 64)
	require.NoError(t, err)
	stored := model.Option{Key: system_setting.PluginFileStorageOption}
	require.NoError(t, model.DB.First(&stored).Error)
	require.NoError(t, system_setting.SetPluginFileStorage(stored.Value))
	parsed, err := url.Parse(localURL)
	require.NoError(t, err)
	object := strings.Split(parsed.Path, "/")[4]
	common.CryptoSecret = "different-process-secret"
	assert.True(t, service.VerifyPluginFileAccess(object, parsed.Query().Get("access"), time.Now()))
	assert.Equal(t, "private-secret-key", actual.SecretKey)
	response = httptest.NewRecorder()
	authenticated.ServeHTTP(response, httptest.NewRequest("PUT", "/generic", strings.NewReader(`{"key":"PluginFileStorageSecret","value":"{}"}`)))
	assert.Contains(t, response.Body.String(), "use the plugin file storage settings endpoint")
	assert.Equal(t, actual, system_setting.GetPluginFileStorage())
	secure := common.SessionCookieSecure
	common.SessionCookieSecure = true
	t.Cleanup(func() { common.SessionCookieSecure = secure })
	guarded := gin.New()
	guarded.PUT("/settings", middleware.SessionCookieOriginGuard(), UpdatePluginFileStorage)
	request := httptest.NewRequest("PUT", "https://panel.example.test/settings", bytes.NewReader(value))
	request.Header.Set("Origin", "https://attacker.example.test")
	response = httptest.NewRecorder()
	guarded.ServeHTTP(response, request)
	assert.Equal(t, 403, response.Code)
	assert.Equal(t, actual, system_setting.GetPluginFileStorage())
}

func TestPluginFilesS3UploadReadAndCleanup(t *testing.T) {
	endpoint := os.Getenv("TEST_PLUGIN_FILE_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_PLUGIN_FILE_S3_ENDPOINT is not configured")
	}
	config := pluginFileConfigFixture(t)
	config.Mode = "s3"
	config.Endpoint = endpoint
	config.Bucket = "plugin-file-tests"
	config.Prefix = "test-scope"
	config.AccessKey = "filetest"
	config.SecretKey = "file-test-only-secret"
	client := s3.New(s3.Options{Region: config.Region, BaseEndpoint: aws.String(endpoint), UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider(config.AccessKey, config.SecretKey, "")})
	_, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(config.Bucket)})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := client.DeleteBucket(context.Background(), &s3.DeleteBucketInput{Bucket: aws.String(config.Bucket)})
		require.NoError(t, err)
	})
	fileURL, size, err := service.StorePluginFile(t.Context(), config, strings.NewReader("s3-image"), "image/png", 64)
	require.NoError(t, err)
	assert.EqualValues(t, 8, size)
	response, err := http.Get(fileURL)
	require.NoError(t, err)
	content, err := io.ReadAll(response.Body)
	response.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, 200, response.StatusCode)
	assert.Equal(t, "s3-image", string(content))
	parsed, err := url.Parse(fileURL)
	require.NoError(t, err)
	seconds, err := strconv.Atoi(parsed.Query().Get("X-Amz-Expires"))
	require.NoError(t, err)
	assert.Positive(t, seconds)
	assert.LessOrEqual(t, seconds, 86400)
	parsed.RawQuery = ""
	response, err = http.Get(parsed.String())
	require.NoError(t, err)
	response.Body.Close()
	assert.Equal(t, 403, response.StatusCode)
	// Cleanup must retain unrelated objects even when they are in the same bucket.
	_, err = client.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String(config.Bucket), Key: aws.String("unrelated"), Body: strings.NewReader("keep")})
	require.NoError(t, err)
	require.NoError(t, service.CleanupPluginFiles(t.Context(), config, time.Now().Add(25*time.Hour)))
	_, err = client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(config.Bucket), Key: aws.String("unrelated")})
	require.NoError(t, err)
	response, err = http.Get(fileURL)
	require.NoError(t, err)
	response.Body.Close()
	assert.Equal(t, 404, response.StatusCode)
	_, err = client.DeleteObject(t.Context(), &s3.DeleteObjectInput{Bucket: aws.String(config.Bucket), Key: aws.String("unrelated")})
	require.NoError(t, err)
}

func TestPluginFilePlaceholderLimitsAndCapabilities(t *testing.T) {
	config := pluginFileConfigFixture(t)
	previousLimit := constant.MaxFileDownloadMB
	constant.MaxFileDownloadMB = 1
	t.Cleanup(func() { constant.MaxFileDownloadMB = previousLimit })
	for _, tc := range []struct {
		name, expression, mode, wantError string
		capability                        bool
		size                              int
	}{
		{"requires declared capability", `{__fileRef:ctx.files[0].ref,encoding:"url"}`, "local", "require file-url@1", false, 4},
		{"disabled storage", `{__fileRef:ctx.files[0].ref,encoding:"url"}`, "disabled", "disabled", true, 4},
		{"missing file", `{__fileRef:"request_file:missing",encoding:"url"}`, "local", "unknown file reference", true, 4},
		{"request-scoped reference", `{__fileRef:"image",encoding:"url"}`, "local", "unknown file reference", true, 4},
		{"vendor size limit", `{__fileRef:ctx.files[0].ref,encoding:"url",maxBytes:3}`, "local", "3 byte limit", true, 4},
		{"fractional size limit", `{__fileRef:ctx.files[0].ref,encoding:"url",maxBytes:1.5}`, "local", "invalid file placeholder", true, 4},
		{"invalid placeholder property", `{__fileRef:ctx.files[0].ref,encoding:"url",extra:true}`, "local", "invalid file placeholder", true, 4},
		{"cumulative size", `[{__fileRef:ctx.files[0].ref,encoding:"url"},{__fileRef:ctx.files[1].ref,encoding:"url"}]`, "local", "1048576 byte limit", true, 700 << 10},
		{"duplicate reuse within limit", `[{__fileRef:ctx.files[0].ref,encoding:"url"},{__fileRef:ctx.files[0].ref,encoding:"url"}]`, "local", "", true, 700 << 10},
		{"later duplicate vendor limit", `[{__fileRef:ctx.files[0].ref,encoding:"url"},{__fileRef:ctx.files[0].ref,encoding:"url",maxBytes:3}]`, "local", "3 byte limit", true, 4},
		{"huge vendor cap retains host limit", `{__fileRef:ctx.files[0].ref,encoding:"url",maxBytes:1e30}`, "local", "1048576 byte limit", true, (1 << 20) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config.Mode = tc.mode
			setPluginFileConfig(t, config)
			capability := ""
			if tc.capability {
				capability = `,requiredCapabilities:["file-url@1"]`
			}
			source := fmt.Sprintf(`export const meta={apiVersion:1,key:"file-limit",name:"File Limit",version:"1.0.0",author:{name:"Test"},models:["m"],fetchMode:"per_task"%s};
export function buildSubmitRequest(ctx){return {url:ctx.baseUrl+"/submit",body:{input:%s}}}
export function parseSubmitResponse(){return {taskId:"1"}} export function buildQueryRequest(){return {url:"https://example.com"}} export function parseTaskResult(){return {status:"SUCCESS"}}`, capability, tc.expression)
			plugin, err := pluginruntime.NewRegistry().Register(source, pluginruntime.Options{})
			require.NoError(t, err)
			adaptor := pluginadaptor.New(plugin)
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: "https://provider.example"}, TaskRelayInfo: &relaycommon.TaskRelayInfo{}}
			adaptor.Init(info)
			var inbound bytes.Buffer
			writer := multipart.NewWriter(&inbound)
			for _, name := range []string{"image", "second"} {
				part, err := writer.CreateFormFile(name, "input.png")
				require.NoError(t, err)
				_, err = part.Write(bytes.Repeat([]byte("x"), tc.size))
				require.NoError(t, err)
			}
			require.NoError(t, writer.Close())
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/submit", bytes.NewReader(inbound.Bytes()))
			c.Request.Header.Set("Content-Type", writer.FormDataContentType())
			c.Set("task_request", map[string]any{"model": "m"})
			body, err := adaptor.BuildRequestBody(c, info)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				assert.Nil(t, body)
				return
			}
			require.NoError(t, err)
			var decoded struct {
				Input []string `json:"input"`
			}
			require.NoError(t, common.DecodeJson(body, &decoded))
			require.Len(t, decoded.Input, 2)
			assert.Equal(t, decoded.Input[0], decoded.Input[1])
		})
	}
}
