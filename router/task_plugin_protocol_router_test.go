package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostProtocolRegistryDrivesProtocolRoutesOnce(t *testing.T) {
	engine := gin.New()
	SetTaskPluginProtocolRouter(engine)

	expected := []string{
		"POST /seedance/api/v3/contents/generations/tasks",
		"GET /seedance/api/v3/contents/generations/tasks/:task_id",
		"POST /v1/responses",
		"GET /v1/responses/:response_id",
		"POST /v1/videos",
		"GET /v1/videos/:task_id",
		"GET /v1/videos/:task_id/content",
		"HEAD /v1/videos/:task_id/content",
	}
	actual := make([]string, 0, len(engine.Routes()))
	for _, route := range engine.Routes() {
		actual = append(actual, fmt.Sprintf("%s %s", route.Method, route.Path))
	}
	sort.Strings(expected)
	sort.Strings(actual)
	assert.Equal(t, expected, actual)
}

func TestSeedanceRoutesAuthenticateAndRejectUnavailableRequests(t *testing.T) {
	setupRelayRouterTestDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Task{}))
	user := model.User{Username: "seedance-router", Status: common.UserStatusEnabled, Group: "default", Quota: 100}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{UserId: user.Id, Key: "seedancerouter", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}).Error)
	engine := gin.New()
	// Use the production API registration order to catch Gin route conflicts.
	SetApiRouter(engine)
	SetDashboardRouter(engine)
	SetRelayRouter(engine)
	SetTaskPluginProtocolRouter(engine)
	SetVideoRouter(engine)
	SetTaskRouter(engine)
	const path = "/seedance/api/v3/contents/generations/tasks"
	for _, tc := range []struct {
		name, method, path, token, body string
		status                          int
	}{
		{"missing create token", http.MethodPost, path, "", "", 401},
		{"missing query token", http.MethodGet, path + "/task_public", "", "", 401},
		{"invalid token", http.MethodGet, path + "/task_public", "invalidseedance", "", 401},
		{"unknown task", http.MethodGet, path + "/task_missing", "seedancerouter", "", 404},
		{"unknown model", http.MethodPost, path, "seedancerouter", `{"model":"unknown","content":[]}`, 400},
		{"builtin does not claim Seedance", http.MethodPost, path, "seedancerouter", `{"model":"doubao-seedance-1-0-pro-250528","content":[{"type":"text","text":"cat"}]}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			if tc.token != "" {
				request.Header.Set("Authorization", "Bearer sk-"+tc.token)
			}
			engine.ServeHTTP(recorder, request)
			assert.Equal(t, tc.status, recorder.Code, recorder.Body.String())
			assert.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
		})
	}
}
