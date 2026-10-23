package handler

// claw_register_nickname_test.go 钉住 A2 的数据契约：
//
//	「用户没填昵称」在库里必须是 NULL，在接口里必须是空串 —— 账号名只属于
//	username 一列，不能再被当成昵称写进库、读出来。
//
// 注意验收边界：本项修的是**数据污染**，不是 UI。界面上仍然可能看到账号名 ——
// 融云 /user/getToken.json 的 name 是必填项，好友列表的权威源又是融云，所以
// 注册时仍要把账号名推过去当显示兜底（见 TestClawRegisterPushesUsername...）；

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func uniqueClawUsername(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// 注册不填昵称：库里是 NULL（不是空串），接口返回 nickname: ""。
func TestClawRegisterWithoutNicknameStoresNullAndReturnsEmpty(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database-backed handler tests are disabled")
	}
	username := uniqueClawUsername("nick-null")
	dbfx.Cleanup(t, `DELETE FROM claw_im_users WHERE username = $1`, username)

	req := testutil.JSONRequest(http.MethodPost, "/api/register", map[string]string{
		"username": username,
		"password": "secret123",
	})
	env := testutil.Call(t, testHandler.ClawRegister, req).Want(http.StatusOK).Map()
	data, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("register response has no data object: %#v", env)
	}
	userID, _ := data["userId"].(string)
	if userID == "" {
		t.Fatalf("register response has no userId: %#v", data)
	}
	if got, _ := data["nickname"].(string); got != "" {
		t.Errorf("register response nickname = %q, want empty (never the account name)", got)
	}

	var isNull bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT nickname IS NULL FROM claw_im_users WHERE username = $1`, username).Scan(&isNull); err != nil {
		t.Fatalf("read back nickname: %v", err)
	}
	if !isNull {
		var stored string
		_ = testPool.QueryRow(context.Background(),
			`SELECT COALESCE(nickname,'<null>') FROM claw_im_users WHERE username = $1`, username).Scan(&stored)
		t.Fatalf("claw_im_users.nickname = %q, want NULL (account name must not be written as a nickname)", stored)
	}

	// 读路径：ClawUserInfo 必须照实返回空串，不能再用 COALESCE 把账号名吐出来。
	infoReq := httptest.NewRequest(http.MethodGet, "/api/user/info?userId="+userID, nil)
	info := testutil.Call(t, testHandler.ClawUserInfo, infoReq).Want(http.StatusOK).Map()
	infoData, ok := info["data"].(map[string]any)
	if !ok {
		t.Fatalf("user info has no data object: %#v", info)
	}
	if got, _ := infoData["nickname"].(string); got != "" {
		t.Errorf("user info nickname = %q, want empty for a user who never set one", got)
	}
	if got, _ := infoData["username"].(string); got != username {
		t.Errorf("user info username = %q, want %q", got, username)
	}
}

// 注册填了昵称：原样保存，不能被账号名覆盖。
func TestClawRegisterKeepsProvidedNickname(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database-backed handler tests are disabled")
	}
	username := uniqueClawUsername("nick-keep")
	dbfx.Cleanup(t, `DELETE FROM claw_im_users WHERE username = $1`, username)

	req := testutil.JSONRequest(http.MethodPost, "/api/register", map[string]string{
		"username": username,
		"password": "secret123",
		"nickname": "小明",
	})
	env := testutil.Call(t, testHandler.ClawRegister, req).Want(http.StatusOK).Map()
	data, _ := env["data"].(map[string]any)
	if got, _ := data["nickname"].(string); got != "小明" {
		t.Fatalf("register response nickname = %q, want 小明", got)
	}

	var stored string
	if err := testPool.QueryRow(context.Background(),
		`SELECT COALESCE(nickname,'') FROM claw_im_users WHERE username = $1`, username).Scan(&stored); err != nil {
		t.Fatalf("read back nickname: %v", err)
	}
	if stored != "小明" {
		t.Fatalf("claw_im_users.nickname = %q, want 小明", stored)
	}
}

// 融云投影：昵称为空时推账号名（name 必填，空值会让 token mint 失败 → 登录 500）；
// 昵称非空时推昵称。用假融云服务直接断言请求表单。
func TestClawRegisterPushesUsernameToRongCloudWhenNicknameEmpty(t *testing.T) {
	var gotForm map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":200,"token":"tok-abc"}`))
	}))
	defer srv.Close()
	t.Setenv("CLAW_RONG_API_BASE", srv.URL)

	rc := &clawRongClient{appKey: "key", appSecret: "secret"}
	token := clawRegisterRongToken(context.Background(), rc, "100001", "", "acct-name", "https://x/a.png")
	if token != "tok-abc" {
		t.Fatalf("token = %q, want tok-abc", token)
	}
	if got := gotForm["name"][0]; got != "acct-name" {
		t.Fatalf("rongcloud name = %q, want the account name as the display fallback", got)
	}
	if got := gotForm["userId"][0]; got != "100001" {
		t.Fatalf("rongcloud userId = %q, want 100001", got)
	}
}

func TestClawRegisterPushesNicknameToRongCloudWhenSet(t *testing.T) {
	var gotForm map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		_, _ = w.Write([]byte(`{"code":200,"token":"tok-abc"}`))
	}))
	defer srv.Close()
	t.Setenv("CLAW_RONG_API_BASE", srv.URL)

	rc := &clawRongClient{appKey: "key", appSecret: "secret"}
	if token := clawRegisterRongToken(context.Background(), rc, "100002", "小明", "acct-name", ""); token != "tok-abc" {
		t.Fatalf("token = %q, want tok-abc", token)
	}
	if got := gotForm["name"][0]; got != "小明" {
		t.Fatalf("rongcloud name = %q, want the nickname", got)
	}
}

// 融云失败不能拖垮注册（best-effort）：返回空串由调用方跳过写 token。
func TestClawRegisterRongTokenFailureIsBestEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":1000,"errorMessage":"boom"}`))
	}))
	defer srv.Close()
	t.Setenv("CLAW_RONG_API_BASE", srv.URL)

	rc := &clawRongClient{appKey: "key", appSecret: "secret"}
	if token := clawRegisterRongToken(context.Background(), rc, "100003", "", "acct-name", ""); token != "" {
		t.Fatalf("token = %q, want empty on failure", token)
	}
}
