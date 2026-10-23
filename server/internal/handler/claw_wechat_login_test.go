package handler

// claw_wechat_login_test.go 覆盖微信登录 HTTP 通道
// （POST /api/wechat/login）。
//
// 为什么要这个通道：微信登录发生在「用户还没有任何凭据」的时刻，而融云
// command 通道（service=user/action=wxLogin）的前提是「已连上 IM」。只有
// command 时构成死锁：客户端 sendRequest 先 checkConnection，未登录连不上 →
// 等 10 秒 → 抛「融云连接失败，请检查网络」。这些用例锁住 HTTP 通路的四条
// 主要出口，避免以后又被人把唯一入口挪回 command。
//
// 业务逻辑本身（建号、并发幂等、token 换发）在
// internal/integrations/rongcloud/im_service_commands_test.go 里已被覆盖；
// 这里只验传输层映射：入参校验、状态码、回包形状。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const (
	wechatLoginTestAppID  = "wx-app-id"
	wechatLoginTestSecret = "wx-secret"
)

// newWeChatLoginTestHandler 组装一个只接了微信登录所需依赖的 Handler。
// 数据库用项目里现成的 groupFakeDB（脚本化，未打桩的分支直接 panic）。
func newWeChatLoginTestHandler(fake *groupFakeDB, wechat *httptest.Server) *Handler {
	h := &Handler{DB: fake}
	if wechat != nil {
		h.wechatAPIBase = wechat.URL
		h.wechatHTTPClient = wechat.Client()
	}
	return h
}

// wechatWithSession 起一个假的微信 jscode2session 服务。
func wechatJSCode2Session(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("js_code"); got == "" {
			t.Errorf("js_code 不应为空")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// registeredWeChatUser 是已在库里的微信账号行
// （列顺序与 UserStore.FindByOpenID 的 SELECT 一致）。
func registeredWeChatUser(token string) []any {
	return []any{"102001", "102001", "微信用户2001", "", token, "active"}
}

func postWeChatLogin(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/wechat/login", strings.NewReader(payload))
	rr := httptest.NewRecorder()
	h.ClawWeChatLogin(rr, req)
	return rr
}

func TestWeChatLoginHTTPRejectsEmptyCode(t *testing.T) {
	// 没有 DB 打桩：缺 code 必须在碰数据库之前就被挡住（打桩缺失会 panic）。
	h := newWeChatLoginTestHandler(&groupFakeDB{}, nil)

	rr := postWeChatLogin(t, h, `{"code":"  "}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	body := decodeGroupBody(t, rr)
	if body["code"].(float64) != 400 {
		t.Errorf("envelope code = %v, want 400", body["code"])
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "code") {
		t.Errorf("message = %q, want mention of code", msg)
	}
}

func TestWeChatLoginHTTPReportsMissingWeChatConfiguration(t *testing.T) {
	t.Setenv("WECHAT_MP_APPID", "")
	t.Setenv("WECHAT_MP_SECRET", "")
	h := newWeChatLoginTestHandler(&groupFakeDB{}, nil)

	rr := postWeChatLogin(t, h, `{"code":"code-1"}`)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
	body := decodeGroupBody(t, rr)
	if msg, _ := body["message"].(string); !strings.Contains(msg, "微信登录未配置") {
		t.Errorf("message = %q, want 未配置提示", msg)
	}
}

func TestWeChatLoginHTTPSurfacesWeChatRejectionAs400(t *testing.T) {
	t.Setenv("WECHAT_MP_APPID", wechatLoginTestAppID)
	t.Setenv("WECHAT_MP_SECRET", wechatLoginTestSecret)
	wechat := wechatJSCode2Session(t, `{"errcode":40163,"errmsg":"code been used"}`)
	h := newWeChatLoginTestHandler(&groupFakeDB{}, wechat)

	rr := postWeChatLogin(t, h, `{"code":"used-code"}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	body := decodeGroupBody(t, rr)
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "微信登录失败") || !strings.Contains(msg, "code been used") {
		t.Errorf("message = %q, want 微信登录失败 + 微信原因", msg)
	}
}

func TestWeChatLoginHTTPReturnsTheStoredToken(t *testing.T) {
	t.Setenv("WECHAT_MP_APPID", wechatLoginTestAppID)
	t.Setenv("WECHAT_MP_SECRET", wechatLoginTestSecret)
	wechat := wechatJSCode2Session(t, `{"openid":"openid-1","session_key":"sk"}`)

	fake := &groupFakeDB{
		queryRow: func(sql string, args []any) pgx.Row {
			if !strings.Contains(sql, "wechat_openid") {
				t.Fatalf("unexpected QueryRow: %s", sql)
			}
			// 库内已有 token：复用而不是换发（否则会把其它端踢下线）。
			return &groupStubRow{values: registeredWeChatUser("stored-rc-token")}
		},
	}
	h := newWeChatLoginTestHandler(fake, wechat)

	rr := postWeChatLogin(t, h, `{"code":"good-code"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	body := decodeGroupBody(t, rr)
	data, ok := body["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("data 不是对象: %T", body["data"])
	}
	if data["token"] != "stored-rc-token" {
		t.Errorf("token = %v, want stored-rc-token（应复用库内 token）", data["token"])
	}
	if data["userId"] != "102001" {
		t.Errorf("userId = %v, want 102001", data["userId"])
	}
	if data["isNewUser"] != false {
		t.Errorf("isNewUser = %v, want false", data["isNewUser"])
	}
	// 客户端按这个形状写本地会话（login-popup.vue），字段缺一个都会让登录后掉线。
	for _, key := range []string{"username", "nickname", "portraitUri"} {
		if _, exist := data[key]; !exist {
			t.Errorf("回包缺少 %s 字段", key)
		}
	}
}

func TestWeChatLoginHTTPRejectsDisabledAccount(t *testing.T) {
	t.Setenv("WECHAT_MP_APPID", wechatLoginTestAppID)
	t.Setenv("WECHAT_MP_SECRET", wechatLoginTestSecret)
	wechat := wechatJSCode2Session(t, `{"openid":"openid-2"}`)
	row := registeredWeChatUser("stored-rc-token")
	row[len(row)-1] = "disabled"

	fake := &groupFakeDB{
		queryRow: func(string, []any) pgx.Row {
			return &groupStubRow{values: row}
		},
	}
	h := newWeChatLoginTestHandler(fake, wechat)

	rr := postWeChatLogin(t, h, `{"code":"good-code"}`)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rr.Code, rr.Body.String())
	}
	body := decodeGroupBody(t, rr)
	if msg, _ := body["message"].(string); !strings.Contains(msg, "禁用") {
		t.Errorf("message = %q, want 账户已被禁用", msg)
	}
}

// TestWeChatLoginHTTPUnknownAccountWithoutRongCloudIs503 锁住「融云未配置」的
// 降级语义：新账号需要换发 IM token，没有融云安装配置时不能假装成功 ——
// 否则客户端会拿着空 token 去 connect，表现成更难查的「登录成功但连不上」。
func TestWeChatLoginHTTPUnknownAccountWithoutRongCloudIs503(t *testing.T) {
	t.Setenv("WECHAT_MP_APPID", wechatLoginTestAppID)
	t.Setenv("WECHAT_MP_SECRET", wechatLoginTestSecret)
	wechat := wechatJSCode2Session(t, `{"openid":"openid-3"}`)

	fake := &groupFakeDB{
		queryRow: func(sql string, args []any) pgx.Row {
			if strings.Contains(sql, "wechat_openid =") && !strings.Contains(sql, "INSERT") {
				// openid 未入库 → 交给建号分支。
				return &groupStubRow{err: pgx.ErrNoRows}
			}
			if strings.Contains(sql, "INSERT INTO claw_im_users") {
				return &groupStubRow{values: registeredWeChatUser("")}
			}
			t.Fatalf("unexpected QueryRow: %s", sql)
			return nil
		},
	}
	h := newWeChatLoginTestHandler(fake, wechat)

	rr := postWeChatLogin(t, h, `{"code":"good-code"}`)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
	body := decodeGroupBody(t, rr)
	if msg, _ := body["message"].(string); msg != "user service is unavailable" {
		t.Errorf("message = %q, want user service is unavailable", msg)
	}
}

// TestWeChatLoginHTTPRejectsMalformedJSON 防一个实际踩过的坑：客户端发
// undefined 时 body 会退化成空串，直接 Decode 会 400 —— 这是对的，但必须是
// 可读的 400 而不是 500。
func TestWeChatLoginHTTPRejectsMalformedJSON(t *testing.T) {
	h := newWeChatLoginTestHandler(&groupFakeDB{}, nil)

	rr := postWeChatLogin(t, h, `not-json`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
}
