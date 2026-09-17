package controller

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

var pluginFileConfigUpdate sync.Mutex

func GetPluginFileStorage(c *gin.Context) {
	config := system_setting.GetPluginFileStorage()
	accessConfigured, secretConfigured := config.AccessKey != "", config.SecretKey != ""
	config.AccessKey, config.SecretKey, config.SigningKey = "", "", ""
	common.ApiSuccess(c, gin.H{"config": config, "access_key_configured": accessConfigured, "secret_key_configured": secretConfigured, "public_address": service.PluginFilePublicAddress()})
}

func UpdatePluginFileStorage(c *gin.Context) {
	var config system_setting.PluginFileStorageConfig
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10), &config); err != nil {
		common.ApiErrorMsg(c, "invalid plugin file storage configuration")
		return
	}
	pluginFileConfigUpdate.Lock()
	defer pluginFileConfigUpdate.Unlock()
	previous := system_setting.GetPluginFileStorage()
	// Only the host issues signing keys; callers cannot replace them through settings.
	config.SigningKey = previous.SigningKey
	if config.SigningKey == "" && config.Mode == "local" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			common.ApiErrorMsg(c, "cannot create plugin file signing key")
			return
		}
		config.SigningKey = hex.EncodeToString(key)
	}
	if config.AccessKey == "" {
		config.AccessKey = previous.AccessKey
	}
	if config.SecretKey == "" {
		config.SecretKey = previous.SecretKey
	}
	if err := config.Validate(); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	if config.Mode == "local" {
		if err := service.ValidateTaskArtifactBaseURL(service.PluginFilePublicAddress()); err != nil {
			common.ApiErrorMsg(c, err.Error())
			return
		}
	}
	value, err := common.Marshal(config)
	if err == nil {
		err = model.UpdateOption(system_setting.PluginFileStorageOption, string(value))
	}
	if err != nil {
		common.ApiErrorMsg(c, "failed to save plugin file storage configuration")
		return
	}
	recordManageAudit(c, "option.update", map[string]any{"key": system_setting.PluginFileStorageOption})
	GetPluginFileStorage(c)
}
