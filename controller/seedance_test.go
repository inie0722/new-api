package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestSeedanceIndependentProviderAndPersistedQuery(t *testing.T) {
	registry := pluginruntime.NewRegistry()
	plugin, err := registry.Register(`
 export const meta = {apiVersion:1,key:"provider-c",name:"Provider C",version:"1.0.0",author:{name:"Test"},models:["existing-model"],fetchMode:"per_task",protocols:["seedance_video"]};
 export function buildSubmitRequest(){return {};}
 export function parseSubmitResponse(){return {};}
 export function buildQueryRequest(){throw new Error("query must not contact upstream");}
 export function parseTaskResult(){return {};}
 export const protocols = {seedance_video:{
 decodeRequest:function(ctx){return {kind:"submit",model:ctx.body.value.model,requestBody:ctx.body.value};},
 render:function(ctx,task){return {id:"private",model:"private",status:"succeeded",created_at:1,updated_at:2,private_key:"secret",content:{video_url:"https://example.com/video.mp4"},usage:{total_tokens:0,completion_tokens:0},generate_audio:false,seed:0};}
 }};`, pluginruntime.Options{})
	require.NoError(t, err)
	candidates := registry.Generation().LookupEndpointCandidates("POST", "/seedance/api/v3/contents/generations/tasks", "existing-model")
	require.Len(t, candidates, 1)
	assert.Equal(t, "provider-c", candidates[0].Plugin.Meta.Key)
	events := []string{}
	db := setupTaskSubmissionDatabase(t, true, &events)
	if dialect := os.Getenv("TEST_TASK_DB_DIALECT"); dialect != "" && dialect != "sqlite" {
		var driver gorm.Dialector
		switch dialect {
		case "mysql":
			require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
			driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
		case "postgres":
			require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
			driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
		default:
			t.Fatalf("unsupported dialect %s", dialect)
		}
		db, err = gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("seedance_%d_", time.Now().UnixNano())}})
		require.NoError(t, err)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(&model.Task{})); require.NoError(t, sqlDB.Close()) })
		require.NoError(t, db.AutoMigrate(&model.Task{}))
		require.NoError(t, db.Callback().Create().Before("gorm:create").Register("seedance:inserts", func(*gorm.DB) { events = append(events, "insert") }))
		model.DB = db
	}
	var version string
	if dialect := os.Getenv("TEST_TASK_DB_DIALECT"); dialect == "" || dialect == "sqlite" {
		require.NoError(t, db.Raw("SELECT sqlite_version()").Scan(&version).Error)
	} else {
		require.NoError(t, db.Raw("SELECT version()").Scan(&version).Error)
	}
	t.Logf("Seedance task database: %s", version)
	task := &model.Task{TaskID: "task_public", UserId: 71, Platform: constant.TaskPlatform("provider-c"), Status: model.TaskStatusSuccess, CreatedAt: 123, UpdatedAt: 456, Properties: model.Properties{OriginModelName: "existing-model"}}
	require.NoError(t, db.Create(task).Error)
	deps := pluginProtocolBridgeDeps{
		getByTaskId: model.GetByTaskId,
		resolvePlugin: func(platform constant.TaskPlatform) (*pluginruntime.LoadedPlugin, *pluginruntime.RoutingGeneration, bool) {
			assert.Equal(t, task.Platform, platform)
			return plugin, registry.Generation(), true
		},
	}
	for _, tc := range []struct {
		name   string
		user   int
		id     string
		status model.TaskStatus
		want   int
		state  string
	}{
		{"success", 71, "task_public", model.TaskStatusSuccess, 200, "succeeded"},
		{"repeat", 71, "task_public", model.TaskStatusSuccess, 200, "succeeded"},
		{"queued", 71, "task_public", model.TaskStatusSubmitted, 200, "queued"},
		{"running", 71, "task_public", model.TaskStatusInProgress, 200, "running"},
		{"failed", 71, "task_public", model.TaskStatusFailure, 200, "failed"},
		{"other user", 72, "task_public", model.TaskStatusSuccess, 404, ""},
		{"missing", 71, "task_missing", model.TaskStatusSuccess, 404, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, db.Model(task).UpdateColumn("status", tc.status).Error)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/seedance/api/v3/contents/generations/tasks/"+tc.id, nil)
			c.Params = gin.Params{{Key: "task_id", Value: tc.id}}
			common.SetContextKey(c, constant.ContextKeyUserId, tc.user)
			retrieveSeedanceTask(c, deps)
			require.Equal(t, tc.want, recorder.Code, recorder.Body.String())
			if tc.want == 404 {
				assert.JSONEq(t, `{"error":{"code":"task_not_found","message":"Task not found","type":"new_api_error"}}`, recorder.Body.String())
				return
			}
			var body map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
			assert.Equal(t, "task_public", body["id"])
			assert.Equal(t, "existing-model", body["model"])
			assert.Equal(t, tc.state, body["status"])
			assert.Equal(t, float64(123), body["created_at"])
			assert.Equal(t, float64(task.UpdatedAt), body["updated_at"])
			assert.Equal(t, false, body["generate_audio"])
			assert.Equal(t, float64(0), body["seed"])
			assert.NotContains(t, body, "private_key")
			if tc.status == model.TaskStatusSuccess {
				assert.Equal(t, map[string]any{"video_url": "https://example.com/video.mp4"}, body["content"])
				assert.Equal(t, map[string]any{"total_tokens": float64(0), "completion_tokens": float64(0)}, body["usage"])
			} else {
				assert.NotContains(t, body, "content")
				assert.NotContains(t, body, "usage")
			}
		})
	}
	assert.Equal(t, []string{"insert"}, events, "retrieval must not submit, reserve or settle")
}

func TestSeedanceSubmissionReceipt(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(pluginruntime.ContextKeyPinnedEndpoint, pluginruntime.PinnedEndpoint{Protocol: "seedance_video", Operation: pluginruntime.HostProtocolOperation{Name: "create"}})
	presentTaskSubmission(c, &taskSubmissionOutcome{Task: &model.Task{TaskID: "task_public"}, Result: &relay.TaskSubmitResult{}, RelayInfo: &relaycommon.RelayInfo{}})
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.JSONEq(t, `{"id":"task_public"}`, recorder.Body.String())
}

// SEEDANCE_ARK_PYTHON optionally runs the same HTTP lifecycle using the official
// SDK from an isolated Python environment, without adding a runtime dependency.
func TestSeedanceHTTPSubmissionAndPolling(t *testing.T) {
	service.InitHttpClient()
	events := []string{}
	db := setupTaskSubmissionDatabase(t, true, &events)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Channel{}, &model.Log{}))
	oldLog := model.LOG_DB
	oldMemory, oldRedis, oldBatch, oldConsume := common.MemoryCacheEnabled, common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled
	model.LOG_DB = db
	common.MemoryCacheEnabled, common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled = false, false, false, false
	t.Cleanup(func() {
		model.LOG_DB = oldLog
		common.MemoryCacheEnabled, common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled = oldMemory, oldRedis, oldBatch, oldConsume
	})
	const modelName = "seedance-test-model"
	_, err := pluginruntime.DefaultRegistry.Register(`
 export const meta={apiVersion:1,key:"seedance-http-test",name:"Seedance HTTP test",version:"1.0.0",author:{name:"Test"},models:["seedance-test-model"],fetchMode:"per_task",protocols:["seedance_video"],usageExamples:[{label:"Example",facts:{tokens:200}}],usageSchema:{tokens:{type:"number",unit:"token",description:{en:"Video generation token unit price",zh:"视频生成 Token 单价"}}}};
 export const protocols={seedance_video:{
 decodeRequest:function(ctx){return {kind:"submit",model:ctx.model,requestBody:{model:ctx.model,metadata:ctx.body.value}};},
 render:function(ctx,task){return task.data;}
 }};
 export function buildSubmitRequest(ctx){return {url:ctx.baseUrl+"/api/v3/contents/generations/tasks",method:"POST",headers:{Authorization:"Bearer "+ctx.apiKey},body:ctx.requestBody.metadata};}
 export function parseSubmitResponse(ctx,resp){return {taskId:resp.body.id,taskData:resp.body};}
 export function extractUsage(){return {tokens:200};}
 export function buildQueryRequest(ctx){return {url:ctx.baseUrl+"/api/v3/contents/generations/tasks/"+ctx.taskId,method:"GET",headers:{Authorization:"Bearer "+ctx.apiKey}};}
 export function parseTaskResult(ctx,body){return {status:"SUCCESS",progress:"100%",url:body.content.video_url};}
 export function extractUsageOnComplete(ctx,result,body){return {tokens:body.usage.total_tokens};}
 `, pluginruntime.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pluginruntime.DefaultRegistry.Unregister("seedance-http-test")) })
	withTieredBillingConfig(t, map[string]string{modelName: "tiered_expr"}, map[string]string{modelName: `u("tokens") * 0.000001`})
	require.NoError(t, db.Create(&model.User{Id: 71, Username: "seedance-http", Group: "default", Quota: 1000000}).Error)
	var submits, queries atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.Equal(t, "Bearer supplier-key", r.Header.Get("Authorization"))
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v3/contents/generations/tasks":
			submits.Add(1)
			var body map[string]any
			require.NoError(t, common.DecodeJson(r.Body, &body))
			assert.Equal(t, modelName, body["model"])
			assert.Equal(t, false, body["generate_audio"])
			assert.Equal(t, float64(0), body["seed"])
			_, _ = io.WriteString(w, `{"id":"private-upstream"}`)
		case "GET /api/v3/contents/generations/tasks/private-upstream":
			queries.Add(1)
			_, _ = io.WriteString(w, `{"id":"private-upstream","status":"succeeded","content":{"video_url":"https://example.com/video.mp4"},"usage":{"completion_tokens":100,"total_tokens":100},"seed":0,"generate_audio":false}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Setting: common.GetPointer(`{"task_plugin_key":"seedance-http-test"}`), Name: "seedance-http", Key: "supplier-key", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, Models: modelName, Group: "default"}
	require.NoError(t, db.Create(&channel).Error)
	oldFactory := service.GetTaskAdaptorFunc
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
	t.Cleanup(func() { service.GetTaskAdaptorFunc = oldFactory })
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		assert.Equal(t, "Bearer customer-key", c.GetHeader("Authorization"))
		common.SetContextKey(c, constant.ContextKeyUserId, 71)
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserQuota, 1000000)
		c.Next()
	})
	const path = "/seedance/api/v3/contents/generations/tasks"
	engine.POST(path, middleware.PinTaskPluginEndpoint(), middleware.PrepareTaskPluginEndpoint(), func(c *gin.Context) {
		require.Nil(t, middleware.SetupContextForSelectedChannel(c, &channel, modelName))
		billing := &nativeRouteBilling{userID: 71}
		info := &relaycommon.RelayInfo{UserId: 71, UserGroup: "default", UsingGroup: "default", UserQuota: 1000000, TokenGroup: "default", OriginModelName: modelName, Billing: billing, TaskRelayInfo: &relaycommon.TaskRelayInfo{Action: c.GetString("task_action"), PublicTaskID: "task_seedance_http", LockedChannel: &channel}}
		outcome, taskErr := executeTaskSubmissionWith(c, info, relay.RelayTaskSubmit)
		require.Nil(t, taskErr)
		require.NotNil(t, outcome)
		presentTaskSubmission(c, outcome)
		var persisted model.Task
		require.NoError(t, db.Where("task_id = ?", outcome.Task.TaskID).First(&persisted).Error)
		assert.Equal(t, channel.Id, persisted.ChannelId)
		service.DispatchPlatformUpdate(context.Background(), persisted.Platform, map[int][]string{channel.Id: {"private-upstream"}}, map[string]*model.Task{"private-upstream": &persisted})
	})
	engine.GET(path+"/:task_id", RetrieveSeedanceTask)
	server := httptest.NewServer(engine)
	defer server.Close()
	if python := os.Getenv("SEEDANCE_ARK_PYTHON"); python != "" {
		command := exec.Command(python, "-c", `
import sys
from volcenginesdkarkruntime import Ark
client=Ark(base_url=sys.argv[1]+"/seedance/api/v3",api_key="customer-key",max_retries=0)
created=client.content_generation.tasks.create(model="seedance-test-model",content=[{"type":"text","text":"A cat"}],duration=5,seed=0,generate_audio=False)
assert created.id=="task_seedance_http",created
for _ in range(2):
    result=client.content_generation.tasks.get(task_id=created.id)
    assert result.id==created.id and result.status=="succeeded",result
    assert result.content.video_url=="https://example.com/video.mp4",result
    assert result.seed==0 and result.generate_audio is False,result
print("Official Ark SDK create and repeated get passed")
`, server.URL)
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
		t.Log(string(output))
	} else {
		request, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(`{"model":"seedance-test-model","content":[{"type":"text","text":"A cat"}],"duration":5,"seed":0,"generate_audio":false}`))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer customer-key")
		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, response.Body.Close())
		require.NoError(t, err)
		require.Equal(t, 200, response.StatusCode, string(body))
		assert.JSONEq(t, `{"id":"task_seedance_http"}`, string(body))
		for range 2 {
			request, err = http.NewRequest(http.MethodGet, server.URL+path+"/task_seedance_http", nil)
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer customer-key")
			response, err = http.DefaultClient.Do(request)
			require.NoError(t, err)
			body, err = io.ReadAll(response.Body)
			require.NoError(t, response.Body.Close())
			require.NoError(t, err)
			require.Equal(t, 200, response.StatusCode, string(body))
			assert.Contains(t, string(body), `"status":"succeeded"`)
			assert.NotContains(t, string(body), "private-upstream")
		}
	}
	assert.Equal(t, int32(1), submits.Load())
	assert.Equal(t, int32(1), queries.Load(), "client queries must not re-poll upstream")
}
