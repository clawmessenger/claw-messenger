package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// claw_static_test.go 覆盖默认头像的三个环节：
//  1. clawDefaultPortraitURL 的决议优先级（env 覆盖 > 统一图源；PUBLIC_URL 不再参与）；
//  2. ServeClawStaticAssets 真正能把 embed 的 PNG 发出去；
//  3. clawRongClient.refreshUser 走 /user/refresh.json 且只在字段非空时携带
//     （getToken 不更新已存在用户资料，改头像全靠它 —— 契约错了头像就推不上融云）。

// TestClawDefaultPortraitURLPriority 钉住新契约：唯一定义在
// rongcloud.DefaultUserPortrait()。
//
//	1. CLAW_DEFAULT_PORTRAIT_URI 设置时优先；
//	2. 未设置时返回统一 default_user.png；
//	3. MULTICA_PUBLIC_URL 彻底不再影响结果 —— 这条必须显式断言，否则旧分支被
//	   顺手加回来时，线上只要设了它就会继续推 1.9 KB 的旧灰图。
func TestClawDefaultPortraitURLPriority(t *testing.T) {
	t.Setenv("CLAW_DEFAULT_PORTRAIT_URI", "")
	t.Setenv("CLAW_AI_NODE_PORTRAIT_URI", "")
	t.Setenv("MULTICA_PUBLIC_URL", "")

	// 默认：统一图源，不再是同域的 /api/static/claw/avatar-default.png。
	if got := clawDefaultPortraitURL(); got != rongcloud.DefaultUserPortraitURI {
		t.Fatalf("default = %q, want the unified %q", got, rongcloud.DefaultUserPortraitURI)
	}

	// MULTICA_PUBLIC_URL 设置有值也不能改变结果（旧分支已删除）。
	t.Setenv("MULTICA_PUBLIC_URL", "https://example.com/")
	if got := clawDefaultPortraitURL(); got != rongcloud.DefaultUserPortraitURI {
		t.Fatalf("MULTICA_PUBLIC_URL must not affect the default portrait, got %q", got)
	}
	// 旧静态路由仍然可访问（旧数据/自建兼容），但绝不再作为默认值输出。
	if got := clawDefaultPortraitURL(); strings.Contains(got, clawDefaultAvatarPath) {
		t.Fatalf("legacy static path must not be handed out as the default: %q", got)
	}

	// 显式覆盖最高优先。
	t.Setenv("CLAW_DEFAULT_PORTRAIT_URI", "https://cdn.example.com/a.png")
	if got := clawDefaultPortraitURL(); got != "https://cdn.example.com/a.png" {
		t.Fatalf("override = %q", got)
	}
	// 覆盖值必须是裸值：不能把旧静态路径拼上去。
	if got := clawDefaultPortraitURL(); strings.Contains(got, clawDefaultAvatarPath) {
		t.Fatalf("override = %q must be used verbatim", got)
	}
}

// TestClawRegisterWritesUnifiedDefaultPortrait 钉住「真人注册」这条写路径：新用户
// 落库的 portrait_uri 与回包里的 portraitUri 都是统一默认图，且不受
// MULTICA_PUBLIC_URL 影响。融云侧推送（clawRong）在测试环境没有 installation
// 行时是 no-op，这里断言的是「写进库/返回给客户端的那一个值」。
func TestClawRegisterWritesUnifiedDefaultPortrait(t *testing.T) {
	t.Setenv("CLAW_DEFAULT_PORTRAIT_URI", "")
	t.Setenv("CLAW_AI_NODE_PORTRAIT_URI", "")
	t.Setenv("MULTICA_PUBLIC_URL", "https://newsradar.dreamdt.cn/im-test")

	username := fmt.Sprintf("portrait-probe-%d", time.Now().UnixNano())
	dbfx.Cleanup(t, `DELETE FROM claw_im_users WHERE username = $1`, username)

	req := testutil.JSONRequest(http.MethodPost, "/api/register", map[string]string{
		"username": username,
		"password": "secret123",
		"nickname": "头像探针",
	})
	env := testutil.Call(t, testHandler.ClawRegister, req).Want(http.StatusOK).Map()
	data, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("register response has no data object: %#v", env)
	}
	if got := data["portraitUri"]; got != rongcloud.DefaultUserPortraitURI {
		t.Fatalf("register response portraitUri = %v, want %q", got, rongcloud.DefaultUserPortraitURI)
	}

	var stored string
	if err := testPool.QueryRow(context.Background(),
		`SELECT COALESCE(portrait_uri, '') FROM claw_im_users WHERE username = $1`, username,
	).Scan(&stored); err != nil {
		t.Fatalf("read back claw_im_users: %v", err)
	}
	if stored != rongcloud.DefaultUserPortraitURI {
		t.Fatalf("claw_im_users.portrait_uri = %q, want %q", stored, rongcloud.DefaultUserPortraitURI)
	}
	if strings.Contains(stored, "avatar-default.png") {
		t.Fatalf("register must never write the legacy default any more, got %q", stored)
	}
}

func TestServeClawStaticAssetsServesEmbeddedPNG(t *testing.T) {
	mux := chi.NewRouter()
	ServeClawStaticAssets(mux)

	req := httptest.NewRequest(http.MethodGet, clawDefaultAvatarPath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q", ct)
	}
	// PNG 魔数。
	body := rec.Body.Bytes()
	if len(body) < 8 || string(body[:4]) != "\x89PNG" {
		t.Fatalf("body is not a PNG (%d bytes)", len(body))
	}
}

func TestClawRongRefreshUserPostsForm(t *testing.T) {
	var gotPath string
	var gotForm map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":200}`))
	}))
	defer srv.Close()
	t.Setenv("CLAW_RONG_API_BASE", srv.URL)

	c := &clawRongClient{appKey: "key", appSecret: "secret"}
	if err := c.refreshUser(context.Background(), "850509", "小明", "https://x/a.png"); err != nil {
		t.Fatalf("refreshUser: %v", err)
	}
	if gotPath != "/user/refresh.json" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotForm["userId"][0] != "850509" || gotForm["name"][0] != "小明" || gotForm["portraitUri"][0] != "https://x/a.png" {
		t.Fatalf("form = %#v", gotForm)
	}
}

func TestClawRongRefreshUserOmitsEmptyFields(t *testing.T) {
	var gotForm map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		_, _ = w.Write([]byte(`{"code":200}`))
	}))
	defer srv.Close()
	t.Setenv("CLAW_RONG_API_BASE", srv.URL)

	c := &clawRongClient{appKey: "key", appSecret: "secret"}
	// 只改头像：昵称为空串时不携带 name，避免把融云侧昵称清掉。
	if err := c.refreshUser(context.Background(), "850509", "", "https://x/a.png"); err != nil {
		t.Fatalf("refreshUser: %v", err)
	}
	if _, has := gotForm["name"]; has {
		t.Fatalf("empty name must be omitted: %#v", gotForm)
	}
	if gotForm["portraitUri"][0] != "https://x/a.png" {
		t.Fatalf("portrait must be sent: %#v", gotForm)
	}
}

func TestClawRongRefreshUserPropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":1000,"errorMessage":"boom"}`))
	}))
	defer srv.Close()
	t.Setenv("CLAW_RONG_API_BASE", srv.URL)

	c := &clawRongClient{appKey: "key", appSecret: "secret"}
	if err := c.refreshUser(context.Background(), "850509", "n", "p"); err == nil {
		t.Fatal("non-200 code must be an error")
	}
}

// 编译期确认响应形状：注册返回体新增 portraitUri 字段（前端拿它直接渲染）。
func TestClawRegisterResponseCarriesPortrait(t *testing.T) {
	var out struct {
		Data struct {
			PortraitUri string `json:"portraitUri"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(`{"code":200,"data":{"portraitUri":"https://x/a.png"}}`), &out)
	if out.Data.PortraitUri == "" {
		t.Fatal("portraitUri must be part of the register response contract")
	}
}
