package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---- 脚手架 -----------------------------------------------------------------

const embedTestAccessKey = "abcdefghijklmnop1234" // 20 位，走「留头尾各 4 位」分支

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stubEmbedHTTP 顶掉真实出网请求，并把「服务端实际发出去的请求体」交回给用例核对。
func stubEmbedHTTP(t *testing.T, status int, body string) *[]byte {
	t.Helper()
	captured := &[]byte{}
	previous := clawAdminEmbedHTTPClient
	t.Cleanup(func() { clawAdminEmbedHTTPClient = previous })
	clawAdminEmbedHTTPClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Body != nil {
				buf := &bytes.Buffer{}
				_, _ = buf.ReadFrom(req.Body)
				*captured = buf.Bytes()
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		}),
	}
	return captured
}

// embedHandlerOptions 描述一个用例的库状态。
type embedHandlerOptions struct {
	permissions []string
	configJSON  []byte // nil 表示库里没有这条配置
	execHook    func(sql string, args []any)
}

func newEmbedHandler(t *testing.T, opts embedHandlerOptions) *convFakeDB {
	t.Helper()
	permsJSON, _ := json.Marshal(opts.permissions)
	authRow := &convStubRow{values: []any{
		convAdminID, "boss", "老板", "role-1", "active", "超级管理员", permsJSON,
	}}
	fake := &convFakeDB{}
	fake.queryRow = func(sql string, args []any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM claw_admin_users"):
			return authRow
		case strings.Contains(sql, "SELECT config FROM rongcloud_system_config"):
			if opts.configJSON == nil {
				return &convStubRow{err: pgx.ErrNoRows}
			}
			return &convStubRow{values: []any{opts.configJSON}}
		case strings.Contains(sql, "SELECT workspace_id::text FROM rongcloud_system_config"):
			return &convStubRow{values: []any{"ws-1"}}
		case strings.Contains(sql, "SELECT id::text FROM workspace"):
			return &convStubRow{values: []any{"ws-1"}}
		}
		panic("unexpected QueryRow: " + sql)
	}
	fake.exec = func(sql string, args []any) (pgconn.CommandTag, error) {
		if opts.execHook != nil {
			opts.execHook(sql, args)
		}
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}
	return fake
}

func embedConfigJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return raw
}

// storedEmbedConfig 造一条「界面里已经配好」的落库配置。
func storedEmbedConfig(accessKey string) clawAdminEmbedStoredConfig {
	return clawAdminEmbedStoredConfig{
		AccessKey:   accessKey,
		DefaultPage: clawAdminEmbedDefaultPageCode,
		UsefulLife:  clawAdminEmbedDefaultUsefulLife,
		TokenAPIURL: clawAdminEmbedTokenDefaultURL,
		ShowMenu:    true,
	}
}

// embedRequest 包一层 convRequest。
// 注意 convRequest 的 permissions 参数是历史遗留、并不生效——身份权限一律来自
// 库里那条 claw_admin_users → claw_admin_roles 的打桩结果，所以这里显式丢掉，
// 免得用例误以为「传了权限就生效」。
func embedRequest(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	return convRequest(t, method, target, body, nil)
}

// embedRequestWithParams 额外注入 chi 的 URL 参数（{key} / {id} 这类）。
func embedRequestWithParams(t *testing.T, method, target, body string, params map[string]string) *http.Request {
	t.Helper()
	req := embedRequest(t, method, target, body)
	rctx := chi.NewRouteContext()
	for name, value := range params {
		rctx.URLParams.Add(name, value)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// assertNoSecret 确认密钥原文没有出现在任何响应里——这是这一片最重要的断言。
func assertNoSecret(t *testing.T, rr *httptest.ResponseRecorder, secret string) {
	t.Helper()
	if secret != "" && strings.Contains(rr.Body.String(), secret) {
		t.Fatalf("响应体泄露了密钥原文: %s", rr.Body.String())
	}
}

// ---- GET /api/admin/rongcloud/console ---------------------------------------

func TestEmbedConsoleGetMaskesAccessKey(t *testing.T) {
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:write"},
		configJSON:  embedConfigJSON(t, storedEmbedConfig(embedTestAccessKey)),
	})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminGetRongCloudConsole(rr, embedRequest(t, "GET", "/api/admin/rongcloud/console", ""))

	body := decodeConv(t, rr)
	data := dataMap(t, body)
	if data["configured"] != true {
		t.Fatalf("configured = %v, want true", data["configured"])
	}
	if data["source"] != clawAdminEmbedSourceConfig {
		t.Fatalf("source = %v, want %q", data["source"], clawAdminEmbedSourceConfig)
	}
	if got := data["access_key_masked"]; got != "abcd****1234" {
		t.Fatalf("access_key_masked = %v, want abcd****1234", got)
	}
	assertNoSecret(t, rr, embedTestAccessKey)

	pages, ok := data["pages"].([]interface{})
	if !ok || len(pages) == 0 {
		t.Fatalf("pages 未返回: %#v", data["pages"])
	}
	first, _ := pages[0].(map[string]interface{})
	if first["code"] != "user_manage" || first["label"] != "用户管理" {
		t.Fatalf("首个页面不对: %#v", first)
	}
}

func TestEmbedConsoleGetFallsBackToEnv(t *testing.T) {
	previous := clawAdminEmbedKeyEnvLookup
	t.Cleanup(func() { clawAdminEmbedKeyEnvLookup = previous })
	clawAdminEmbedKeyEnvLookup = func(name string) string {
		if name == clawAdminEmbedAccessKeyEnv {
			return "envkey-zzzzzzzzzz"
		}
		return ""
	}

	fake := newEmbedHandler(t, embedHandlerOptions{permissions: []string{"system:write"}})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminGetRongCloudConsole(rr, embedRequest(t, "GET", "/api/admin/rongcloud/console", ""))

	data := dataMap(t, decodeConv(t, rr))
	if data["source"] != clawAdminEmbedSourceEnv {
		t.Fatalf("source = %v, want env", data["source"])
	}
	if data["configured"] != true {
		t.Fatalf("configured = %v, want true", data["configured"])
	}
	if got := data["access_key_masked"]; got != "envk****zzzz" {
		t.Fatalf("access_key_masked = %v", got)
	}
	assertNoSecret(t, rr, "envkey-zzzzzzzzzz")
}

func TestEmbedConsoleGetReportsUnconfigured(t *testing.T) {
	fake := newEmbedHandler(t, embedHandlerOptions{permissions: []string{"system:write"}})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminGetRongCloudConsole(rr, embedRequest(t, "GET", "/api/admin/rongcloud/console", ""))

	data := dataMap(t, decodeConv(t, rr))
	if data["configured"] != false || data["source"] != clawAdminEmbedSourceNone {
		t.Fatalf("未配置状态不对: %#v", data)
	}
	// 未配置时仍要告诉前端默认值，界面才好渲染空态。
	if data["default_page_code"] != clawAdminEmbedDefaultPageCode {
		t.Fatalf("default_page_code = %v", data["default_page_code"])
	}
}

func TestEmbedConsoleGetRequiresSystemWrite(t *testing.T) {
	// 内嵌控制台拿到令牌就是全权限操作，所以 system:read 也不算够——
	// 只读管理员不该能打开融云控制台。
	for _, permission := range []string{"system:read", "user:read", "group:write"} {
		fake := newEmbedHandler(t, embedHandlerOptions{permissions: []string{permission}})
		h := &Handler{DB: fake}
		rr := httptest.NewRecorder()
		h.ClawAdminGetRongCloudConsole(rr, embedRequest(t, "GET", "/api/admin/rongcloud/console", ""))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("permission=%q 时状态码 = %d, want 403", permission, rr.Code)
		}
	}
}

func TestEmbedConsoleGetAllowsWildcardPermission(t *testing.T) {
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"*"},
		configJSON:  embedConfigJSON(t, storedEmbedConfig(embedTestAccessKey)),
	})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminGetRongCloudConsole(rr, embedRequest(t, "GET", "/api/admin/rongcloud/console", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
}

// ---- POST /api/admin/rongcloud/console/token --------------------------------

func TestEmbedConsoleTokenExchangesKeyServerSide(t *testing.T) {
	captured := stubEmbedHTTP(t, http.StatusOK, `{"code":10000,"message":"ok","data":"https://embed-console.rongcloud.cn/embed/access?ticket=abc123&x=1"}`)
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:write"},
		configJSON:  embedConfigJSON(t, storedEmbedConfig(embedTestAccessKey)),
	})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminRongCloudConsoleToken(rr, embedRequest(t, "POST", "/api/admin/rongcloud/console/token",
		`{"page_code":"group_manage","useful_life":7200}`))

	data := dataMap(t, decodeConv(t, rr))
	if data["token_url"] != "https://embed-console.rongcloud.cn/embed/access?ticket=abc123&x=1" {
		t.Fatalf("token_url = %v", data["token_url"])
	}
	if data["page_code"] != "group_manage" {
		t.Fatalf("page_code = %v", data["page_code"])
	}
	if data["useful_life"] != float64(7200) {
		t.Fatalf("useful_life = %v", data["useful_life"])
	}
	if _, ok := data["expires_at"].(string); !ok {
		t.Fatalf("expires_at 缺失: %#v", data)
	}
	assertNoSecret(t, rr, embedTestAccessKey)

	// 服务端必须把 Access Key 交给融云，而不是交给浏览器。
	var sent map[string]interface{}
	if err := json.Unmarshal(*captured, &sent); err != nil {
		t.Fatalf("请求体不是 JSON: %v (%s)", err, string(*captured))
	}
	if sent["access_key"] != embedTestAccessKey {
		t.Fatalf("发给融云的 access_key = %v", sent["access_key"])
	}
	if sent["page_code"] != "group_manage" {
		t.Fatalf("发给融云的 page_code = %v", sent["page_code"])
	}
	if sent["useful_life"] != float64(7200) {
		t.Fatalf("发给融云的 useful_life = %v", sent["useful_life"])
	}
}

func TestEmbedConsoleTokenFallsBackToConfiguredDefaults(t *testing.T) {
	captured := stubEmbedHTTP(t, http.StatusOK, `{"code":10000,"data":"https://embed-console.rongcloud.cn/embed/access?ticket=d"}`)
	settings := storedEmbedConfig(embedTestAccessKey)
	settings.DefaultPage = "chatroom_manage"
	settings.UsefulLife = 10800
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:write"},
		configJSON:  embedConfigJSON(t, settings),
	})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminRongCloudConsoleToken(rr, embedRequest(t, "POST", "/api/admin/rongcloud/console/token", `{}`))

	data := dataMap(t, decodeConv(t, rr))
	if data["page_code"] != "chatroom_manage" || data["useful_life"] != float64(10800) {
		t.Fatalf("默认值未生效: %#v", data)
	}
	var sent map[string]interface{}
	_ = json.Unmarshal(*captured, &sent)
	if sent["page_code"] != "chatroom_manage" {
		t.Fatalf("发给融云的 page_code = %v", sent["page_code"])
	}
}

func TestEmbedConsoleTokenAcceptsWrappedDataPayload(t *testing.T) {
	stubEmbedHTTP(t, http.StatusOK, `{"code":10000,"data":{"token_url":"https://embed-console.rongcloud.cn/embed/access?ticket=w"}}`)
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:write"},
		configJSON:  embedConfigJSON(t, storedEmbedConfig(embedTestAccessKey)),
	})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminRongCloudConsoleToken(rr, embedRequest(t, "POST", "/api/admin/rongcloud/console/token", `{}`))

	data := dataMap(t, decodeConv(t, rr))
	if data["token_url"] != "https://embed-console.rongcloud.cn/embed/access?ticket=w" {
		t.Fatalf("token_url = %v", data["token_url"])
	}
}

func TestEmbedConsoleTokenRejectsUnknownPageCode(t *testing.T) {
	stubEmbedHTTP(t, http.StatusOK, `{"code":10000,"data":"https://x.rongcloud.cn/y"}`)
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:write"},
		configJSON:  embedConfigJSON(t, storedEmbedConfig(embedTestAccessKey)),
	})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminRongCloudConsoleToken(rr, embedRequest(t, "POST", "/api/admin/rongcloud/console/token",
		`{"page_code":"../../etc/passwd"}`))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, want 400 (%s)", rr.Code, rr.Body.String())
	}
}

func TestEmbedConsoleTokenRejectsOutOfRangeUsefulLife(t *testing.T) {
	stubEmbedHTTP(t, http.StatusOK, `{"code":10000,"data":"https://x.rongcloud.cn/y"}`)
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:write"},
		configJSON:  embedConfigJSON(t, storedEmbedConfig(embedTestAccessKey)),
	})
	h := &Handler{DB: fake}
	// 0 是有意义的（表示「用配置里的默认值」），合法区间外的负值与超上限才该被拒。
	for _, life := range []string{"-1", "31536001", "999999999"} {
		rr := httptest.NewRecorder()
		h.ClawAdminRongCloudConsoleToken(rr, embedRequest(t, "POST", "/api/admin/rongcloud/console/token",
			`{"useful_life":`+life+`}`))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("useful_life=%s 时状态码 = %d, want 400", life, rr.Code)
		}
	}
}

func TestEmbedConsoleTokenTreatsZeroUsefulLifeAsDefault(t *testing.T) {
	stubEmbedHTTP(t, http.StatusOK, `{"code":10000,"data":"https://x.rongcloud.cn/y"}`)
	settings := storedEmbedConfig(embedTestAccessKey)
	settings.UsefulLife = 4321
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:write"},
		configJSON:  embedConfigJSON(t, settings),
	})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminRongCloudConsoleToken(rr, embedRequest(t, "POST", "/api/admin/rongcloud/console/token",
		`{"useful_life":0}`))

	data := dataMap(t, decodeConv(t, rr))
	if data["useful_life"] != float64(4321) {
		t.Fatalf("useful_life=0 应回落到配置默认值，实际 = %v", data["useful_life"])
	}
}

func TestEmbedConsoleTokenWithoutAccessKeyIsConflict(t *testing.T) {
	fake := newEmbedHandler(t, embedHandlerOptions{permissions: []string{"system:write"}})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminRongCloudConsoleToken(rr, embedRequest(t, "POST", "/api/admin/rongcloud/console/token", `{}`))

	if rr.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d, want 409 (%s)", rr.Code, rr.Body.String())
	}
}

func TestEmbedConsoleTokenPropagatesRongCloudError(t *testing.T) {
	stubEmbedHTTP(t, http.StatusOK, `{"code":10001,"message":"access key invalid"}`)
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:write"},
		configJSON:  embedConfigJSON(t, storedEmbedConfig(embedTestAccessKey)),
	})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminRongCloudConsoleToken(rr, embedRequest(t, "POST", "/api/admin/rongcloud/console/token", `{}`))

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, want 502", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "access key invalid") {
		t.Fatalf("未透出融云错误信息: %s", rr.Body.String())
	}
	assertNoSecret(t, rr, embedTestAccessKey)
}

func TestEmbedConsoleTokenHandlesTransportFailure(t *testing.T) {
	previous := clawAdminEmbedHTTPClient
	t.Cleanup(func() { clawAdminEmbedHTTPClient = previous })
	clawAdminEmbedHTTPClient = &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, io.ErrUnexpectedEOF
		}),
	}
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:write"},
		configJSON:  embedConfigJSON(t, storedEmbedConfig(embedTestAccessKey)),
	})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminRongCloudConsoleToken(rr, embedRequest(t, "POST", "/api/admin/rongcloud/console/token", `{}`))

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, want 502", rr.Code)
	}
}

func TestEmbedConsoleTokenRequiresSystemWrite(t *testing.T) {
	stubEmbedHTTP(t, http.StatusOK, `{"code":10000,"data":"https://x.rongcloud.cn/y"}`)
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:read"},
		configJSON:  embedConfigJSON(t, storedEmbedConfig(embedTestAccessKey)),
	})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminRongCloudConsoleToken(rr, embedRequest(t, "POST", "/api/admin/rongcloud/console/token", `{}`))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d, want 403", rr.Code)
	}
}

// ---- PUT /api/admin/rongcloud/console ---------------------------------------

func TestEmbedConsolePutKeepsExistingKeyWhenOmitted(t *testing.T) {
	var savedArgs []any
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:write"},
		configJSON:  embedConfigJSON(t, storedEmbedConfig(embedTestAccessKey)),
		execHook: func(sql string, args []any) {
			if strings.Contains(sql, "INSERT INTO rongcloud_system_config") {
				savedArgs = args
			}
		},
	})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminPutRongCloudConsole(rr, embedRequest(t, "PUT", "/api/admin/rongcloud/console",
		`{"default_page_code":"sensitive_word_settings","default_useful_life":1800}`))

	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d (%s)", rr.Code, rr.Body.String())
	}
	if len(savedArgs) != 3 {
		t.Fatalf("落库参数异常: %#v", savedArgs)
	}
	var persisted clawAdminEmbedStoredConfig
	raw, ok := savedArgs[2].([]byte)
	if !ok {
		t.Fatalf("落库的 config 不是 []byte: %T", savedArgs[2])
	}
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("落库的不是 JSON: %v", err)
	}
	if persisted.AccessKey != embedTestAccessKey {
		t.Fatalf("未填 access_key 时应保留原值，实际 = %q", persisted.AccessKey)
	}
	if persisted.DefaultPage != "sensitive_word_settings" || persisted.UsefulLife != 1800 {
		t.Fatalf("其余字段未更新: %#v", persisted)
	}
	assertNoSecret(t, rr, embedTestAccessKey)
}

func TestEmbedConsolePutRejectsNonRongCloudTokenURL(t *testing.T) {
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:write"},
		configJSON:  embedConfigJSON(t, storedEmbedConfig(embedTestAccessKey)),
	})
	h := &Handler{DB: fake}
	for _, target := range []string{
		`{"token_api_url":"http://127.0.0.1:8080/steal"}`,
		`{"token_api_url":"https://evil.example.com/token"}`,
		`{"token_api_url":"http://embed-console.rongcloud.cn/token"}`,
		`{"token_api_url":"https://rongcloud.cn.evil.com/token"}`,
	} {
		rr := httptest.NewRecorder()
		h.ClawAdminPutRongCloudConsole(rr, embedRequest(t, "PUT", "/api/admin/rongcloud/console", target))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s 未被拦截，状态码 = %d (%s)", target, rr.Code, rr.Body.String())
		}
	}
}

func TestEmbedConsolePutRejectsUnknownPageCodeAndBadUsefulLife(t *testing.T) {
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:write"},
		configJSON:  embedConfigJSON(t, storedEmbedConfig(embedTestAccessKey)),
	})
	h := &Handler{DB: fake}
	for _, target := range []string{
		`{"default_page_code":"nope"}`,
		`{"default_useful_life":31536001}`,
		`{"default_useful_life":0}`,
	} {
		rr := httptest.NewRecorder()
		h.ClawAdminPutRongCloudConsole(rr, embedRequest(t, "PUT", "/api/admin/rongcloud/console", target))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s 未被拦截，状态码 = %d (%s)", target, rr.Code, rr.Body.String())
		}
	}
}

func TestEmbedConsolePutCanClearAccessKey(t *testing.T) {
	var persisted clawAdminEmbedStoredConfig
	fake := newEmbedHandler(t, embedHandlerOptions{
		permissions: []string{"system:write"},
		configJSON:  embedConfigJSON(t, storedEmbedConfig(embedTestAccessKey)),
		execHook: func(sql string, args []any) {
			if strings.Contains(sql, "INSERT INTO rongcloud_system_config") && len(args) == 3 {
				if raw, ok := args[2].([]byte); ok {
					_ = json.Unmarshal(raw, &persisted)
				}
			}
		},
	})
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminPutRongCloudConsole(rr, embedRequest(t, "PUT", "/api/admin/rongcloud/console",
		`{"clear_access_key":true}`))

	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d (%s)", rr.Code, rr.Body.String())
	}
	if persisted.AccessKey != "" {
		t.Fatalf("access_key 未被清空: %q", persisted.AccessKey)
	}
}

// ---- 通用 system/config 不能漏出密钥 ----------------------------------------

func TestSystemConfigListHidesSecretKeys(t *testing.T) {
	fake := newEmbedHandler(t, embedHandlerOptions{permissions: []string{"system:read"}})
	now := time.Now()
	fake.query = func(string, []any) (pgx.Rows, error) {
		return &convStubRows{rows: [][]any{
			{clawAdminEmbedConsoleConfigKey, []byte(`{"access_key":"` + embedTestAccessKey + `"}`), now, now},
			{"user_guide", []byte(`{"title":"x"}`), now, now},
		}}, nil
	}
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminSystemConfigList(rr, embedRequest(t, "GET", "/api/admin/system/config", ""))

	items := dataList(t, decodeConv(t, rr))
	if len(items) != 1 {
		t.Fatalf("期望只剩 1 条可见配置，实际 %d 条: %#v", len(items), items)
	}
	kept, _ := items[0].(map[string]interface{})
	if kept["config_key"] != "user_guide" {
		t.Fatalf("剩下的不是 user_guide: %#v", kept)
	}
	assertNoSecret(t, rr, embedTestAccessKey)
}

func TestSystemConfigGetRefusesSecretKey(t *testing.T) {
	fake := newEmbedHandler(t, embedHandlerOptions{permissions: []string{"system:read"}})
	fake.query = func(string, []any) (pgx.Rows, error) {
		return &convStubRows{rows: [][]any{
			{clawAdminEmbedConsoleConfigKey, []byte(`{}`), time.Now(), time.Now()},
		}}, nil
	}
	h := &Handler{DB: fake}
	rr := httptest.NewRecorder()
	h.ClawAdminSystemConfigGet(rr, embedRequestWithParams(t, "GET",
		"/api/admin/system/config/rongcloud_embed_console", "",
		map[string]string{"key": clawAdminEmbedConsoleConfigKey}))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d, want 404", rr.Code)
	}
	if fake.sawStatement("SELECT config_key, config, created_at, updated_at") {
		t.Fatal("密钥配置不应该走到通用配置查询")
	}
}

// ---- 纯函数 -----------------------------------------------------------------

func TestClawAdminValidateEmbedTokenURL(t *testing.T) {
	cases := []struct {
		target string
		ok     bool
	}{
		{"https://embed-console.rongcloud.cn/embed/access/token", true},
		{"https://rongcloud.cn/token", true},
		{"https://foo.bar.rongcloud.cn/token", true},
		{" http://embed-console.rongcloud.cn/token ", false},
		{"https://rongcloud.cn.evil.com/token", false},
		{"https://evil.com/rongcloud.cn", false},
		{"ftp://embed-console.rongcloud.cn/token", false},
		{"", false},
		{"not a url", false},
	}
	for _, tc := range cases {
		_, err := clawAdminValidateEmbedTokenURL(tc.target)
		if tc.ok && err != nil {
			t.Fatalf("%q 应该通过，却报 %v", tc.target, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%q 应该被拒绝", tc.target)
		}
	}
}

func TestClawAdminMaskSecret(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"short":                "*****",
		"12345678901":          "***********",
		embedTestAccessKey:     "abcd****1234",
		"0123456789abcdefghij": "0123****ghij",
	}
	for input, want := range cases {
		if got := clawAdminMaskSecret(input); got != want {
			t.Fatalf("mask(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestClawAdminIsValidEmbedPageCode(t *testing.T) {
	if !clawAdminIsValidEmbedPageCode("user_manage") {
		t.Fatal("user_manage 应当合法")
	}
	if clawAdminIsValidEmbedPageCode("") || clawAdminIsValidEmbedPageCode("USER_MANAGE") {
		t.Fatal("空值 / 大小写不符不应通过")
	}
}
