package controller

import (
	"net/http"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// RetrieveSeedanceTask presents a persisted task without issuing upstream work.
func RetrieveSeedanceTask(c *gin.Context) {
	retrieveSeedanceTask(c, defaultPluginProtocolBridgeDeps())
}

func retrieveSeedanceTask(c *gin.Context, deps pluginProtocolBridgeDeps) {
	deps = deps.withDefaults()
	taskID := strings.TrimSpace(c.Param("task_id"))
	userID := common.GetContextKeyInt(c, constant.ContextKeyUserId)
	task, exists, err := deps.getByTaskId(userID, taskID)
	if err != nil {
		respondPluginProtocolError(c, http.StatusInternalServerError, "task_lookup_failed", "Task lookup failed")
		return
	}
	if !exists || task == nil {
		respondPluginProtocolError(c, http.StatusNotFound, "task_not_found", "Task not found")
		return
	}
	plugin, _, available := deps.resolvePlugin(task.Platform)
	if !available || plugin == nil {
		respondPluginProtocolError(c, http.StatusNotFound, "task_not_found", "Task not found")
		return
	}
	claimed := slices.ContainsFunc(plugin.Meta.Protocols, func(claim pluginruntime.ProtocolClaim) bool {
		if claim.Name != "seedance_video" {
			return false
		}
		models := claim.Models
		if len(models) == 0 {
			models = plugin.Meta.Models
		}
		return slices.Contains(models, task.Properties.OriginModelName) || slices.Contains(models, task.Properties.UpstreamModelName)
	})
	if !claimed {
		respondPluginProtocolError(c, http.StatusNotFound, "task_not_found", "Task not found")
		return
	}
	view, err := service.BuildTaskPluginView(task)
	if err != nil {
		respondPluginProtocolError(c, http.StatusInternalServerError, "task_protocol_error", "Task data is invalid")
		return
	}
	viewValue, err := taskPluginProtocolJSONValue(view)
	if err != nil {
		respondPluginProtocolError(c, http.StatusInternalServerError, "task_protocol_error", "Task data is invalid")
		return
	}
	value, err := plugin.Engine.CallPath(c.Request.Context(), "protocols", []string{"seedance_video", "render"},
		map[string]any{"protocol": "seedance_video", "operation": "retrieve", "model": task.Properties.OriginModelName}, viewValue)
	if err != nil {
		respondPluginProtocolError(c, http.StatusInternalServerError, "task_protocol_error", "Task presentation failed")
		return
	}
	// Decode only official response fields. Provider envelopes and private fields
	// must never become public merely because a renderer returns them.
	data, err := common.Marshal(value)
	if err != nil {
		respondPluginProtocolError(c, http.StatusInternalServerError, "task_protocol_error", "Task presentation failed")
		return
	}
	var response *dto.SeedanceTaskResponse
	if err = common.Unmarshal(data, &response); err != nil || response == nil {
		respondPluginProtocolError(c, http.StatusInternalServerError, "task_protocol_error", "Invalid Seedance task response")
		return
	}
	response.ID = task.TaskID
	response.Model = task.Properties.OriginModelName
	response.CreatedAt = task.CreatedAt
	if response.CreatedAt == 0 {
		response.CreatedAt = task.SubmitTime
	}
	response.UpdatedAt = max(task.UpdatedAt, response.CreatedAt)
	response.Error = nil
	switch task.Status {
	case model.TaskStatusNotStart, model.TaskStatusSubmitted, model.TaskStatusQueued:
		response.Status = "queued"
	case model.TaskStatusInProgress:
		response.Status = "running"
	case model.TaskStatusSuccess:
		response.Status = "succeeded"
	case model.TaskStatusFailure:
		if response.Status != "cancelled" && response.Status != "expired" {
			response.Status = "failed"
		}
		response.Error = &dto.SeedanceTaskError{Code: "task_failed", Message: task.FailReason}
		if response.Error.Message == "" {
			response.Error.Message = "Video generation failed"
		}
	default:
		respondPluginProtocolError(c, http.StatusInternalServerError, "task_protocol_error", "Task status is unavailable")
		return
	}
	if task.Status != model.TaskStatusSuccess {
		response.Content = nil
		response.Usage = nil
	}
	c.JSON(http.StatusOK, response)
}
