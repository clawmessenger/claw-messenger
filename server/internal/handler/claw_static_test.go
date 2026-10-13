package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// claw_static_test.go 覆盖默认头像的三个环节：
//  1. clawDefaultPortraitURL 的决议优先级（env 覆盖 > PUBLIC_URL > 线上兜底）；
//  2. ServeClawStaticAssets 真正能把 embed 的 PNG 发出去；
//  3. clawRongClient.refreshUser 走 /user/refresh.json 且只在字段非空时携带
//     （getToken 不更新已存在用户资料，改头像全靠它 —— 契约错了头像就推不上融云）。

func TestClawDefaultPortraitURLPriority(t *testing.T) {
	t.Setenv("CLAW_DEFAULT_PORTRAIT_URI", "")
	t.Setenv("MULTICA_PUBLIC_URL", "")

	// 兜底：线上公网路径。
	got := clawDefaultPortraitURL()
	want := "https://newsradar.dreamdt.cn/im-test/api/static/claw/avatar-default.png"
	if got != want {
		t.Fatalf("fallback = %q, want %q", got, want)
	}

	// PUBLIC_URL 优先于兜底。
	t.Setenv("MULTICA_PUBLIC_URL", "https://example.com/")
	if got = clawDefaultPortraitURL(); got != "https://example.com"+clawDefaultAvatarPath {
		t.Fatalf("PUBLIC_URL form = %q", got)
	}

	// 显式覆盖最高优先。
	t.Setenv("CLAW_DEFAULT_PORTRAIT_URI", "https://cdn.example.com/a.png")
	if got = clawDefaultPortraitURL(); got != "https://cdn.example.com/a.png" {
		t.Fatalf("override = %q", got)
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
