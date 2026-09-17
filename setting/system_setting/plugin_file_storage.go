package system_setting

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/QuantumNous/new-api/common"
)

// The complete configuration is one option so readers never see a partial update.
// Its Secret suffix also excludes credentials from the generic options response.
const PluginFileStorageOption = "PluginFileStorageSecret"

type PluginFileStorageConfig struct {
	SigningKey     string `json:"signing_key,omitempty"`
	Mode           string `json:"mode"`
	TTLHours       int    `json:"ttl_hours"`
	LocalDirectory string `json:"local_directory"`
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix"`
	AccessKey      string `json:"access_key"`
	SecretKey      string `json:"secret_key"`
	PathStyle      bool   `json:"path_style"`
}

var pluginFileStorage atomic.Pointer[PluginFileStorageConfig]

func GetPluginFileStorage() PluginFileStorageConfig {
	if config := pluginFileStorage.Load(); config != nil {
		return *config
	}
	return PluginFileStorageConfig{Mode: "disabled", TTLHours: 24, LocalDirectory: "./data/plugin-files", Region: "us-east-1", PathStyle: true}
}

func (config PluginFileStorageConfig) Validate() error {
	if config.Mode != "disabled" && config.Mode != "local" && config.Mode != "s3" {
		return errors.New("invalid plugin file storage mode")
	}
	if config.TTLHours < 1 || config.TTLHours > 168 {
		return errors.New("file URL lifetime must be between 1 and 168 hours")
	}
	if config.SigningKey != "" || config.Mode == "local" {
		key, err := hex.DecodeString(config.SigningKey)
		if err != nil || len(key) != 32 {
			return errors.New("invalid plugin file signing key")
		}
	}
	if config.Mode == "local" && config.LocalDirectory == "" {
		return errors.New("a dedicated local storage directory is required")
	}
	if config.LocalDirectory != "" {
		directory := filepath.Clean(config.LocalDirectory)
		if config.LocalDirectory != strings.TrimSpace(config.LocalDirectory) || directory == "." || directory == string(filepath.Separator) || strings.ContainsRune(directory, 0) {
			return errors.New("a dedicated local storage directory is required")
		}
	}
	if config.Mode == "s3" {
		return ValidateTaskArtifactStoreConfig(TaskArtifactStoreConfig{Mode: TaskArtifactStoreModeS3, S3Endpoint: config.Endpoint, S3Bucket: config.Bucket, S3Region: config.Region, S3AccessKey: config.AccessKey, S3SecretKey: config.SecretKey, S3Prefix: config.Prefix, S3PresignTTLSeconds: config.TTLHours * 3600})
	}
	return nil
}

func ParsePluginFileStorage(value string) (PluginFileStorageConfig, error) {
	var config PluginFileStorageConfig
	if err := common.UnmarshalJsonStr(value, &config); err != nil {
		return config, errors.New("invalid plugin file storage configuration")
	}
	return config, config.Validate()
}

func SetPluginFileStorage(value string) error {
	config, err := ParsePluginFileStorage(value)
	if err != nil {
		return err
	}
	pluginFileStorage.Store(&config)
	return nil
}
