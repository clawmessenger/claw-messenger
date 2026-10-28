package handler

// claw_account_security_test.go 钉住三条账号安全接口的**判定边界**。
//
// 这三条都在改别人的登录凭据，所以用例的重心不是「接口能返回 200」，而是：
//
//  1. 改密码必须验旧密码 —— 只有 Bearer token 就能改密的接口等于「谁捡到 token
//     谁就是主人」；
//  2. 微信-only 账号（哨兵哈希）**不能**被要求旧密码，否则它永远设不了密码；
//  3. 绑定手机/邮箱要挡住格式错误、挡住已被别人占用的号，并支持传空串解绑；
//  4. 微信绑定在没配 WECHAT_MP_APPID/SECRET 时明确回 503，不能静默当成功。
//
// 用桩 DB（`groupStubRow`，见 claw_admin_groups_test.go）而不是真库：这些断言
// 只跟 SQL 的**形状**有关（哪条 UPDATE、参数是什么），起一个库反而更脆弱。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// accountSecDB 是只认本文件这几条 SQL 的最小桩。
type accountSecDB struct {
	// user 是 clawQueryUser 的 13 列（顺序见 claw_compat.go 的 SELECT）。
	user []any
	// taken 按列名给出「是否已被别人占用」。
	taken map[string]bool
	// openIDOwner 非空表示 openid 已属于这个 user_id。
	openIDOwner string

	execSQL  []string
	execArgs [][]any
}

func (f *accountSecDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "WHERE rongcloud_token="):
		return &groupStubRow{values: []any{"721913"}}
	case strings.Contains(sql, "SELECT EXISTS"):
		// 两种 EXISTS：手机号（phone=$1）与邮箱（lower(email)=lower($1)）。
		key := "phone"
		if strings.Contains(sql, "lower(email)") {
			key = "email"
		}
		return &groupStubRow{values: []any{f.taken[key]}}
	case strings.Contains(sql, "wechat_openid=$1"):
		if f.openIDOwner != "" {
			return &groupStubRow{values: []any{f.openIDOwner}}
		}
		return &groupStubRow{err: pgx.ErrNoRows}
	default:
		if f.user == nil {
			return &groupStubRow{err: pgx.ErrNoRows}
		}
		return &groupStubRow{values: f.user}
	}
}

func (f *accountSecDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("not stubbed")
}

func (f *accountSecDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execSQL = append(f.execSQL, sql)
	f.execArgs = append(f.execArgs, args)
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

// accountUserRow 拼一行 claw_im_users（13 列）。
func accountUserRow(passwordHash, wechatOpenID string) []any {
	return []any{
		"721913", "11257339", "小酒", "", "jiu@example.com", "13800000000",
		"签名", "", "", "active", passwordHash, "im-token", wechatOpenID,
	}
}

func postAccountSec(t *testing.T, h *Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer im-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	switch path {
	case "/api/user/password":
		h.ClawUserPassword(rr, req)
	case "/api/user/binding":
		h.ClawUserBinding(rr, req)
	case "/api/user/wechat-bind":
		h.ClawUserWeChatBind(rr, req)
	default:
		t.Fatalf("unknown path %s", path)
	}
	return rr
}

func decodeEnvelope(t *testing.T, rr *httptest.ResponseRecorder) (int, string, map[string]any) {
	t.Helper()
	var env struct {
		Code int            `json:"code"`
		Msg  string         `json:"message"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rr.Body.String())
	}
	return env.Code, env.Msg, env.Data
}

func TestClawUserPasswordRejectsMissingOrWrongOldPassword(t *testing.T) {
	// 真哈希：旧密码 "old-secret" 能通过 clawVerifyPassword。
	realHash := clawHashPassword("old-secret")
	fake := &accountSecDB{user: accountUserRow(realHash, "")}
	h := &Handler{DB: fake}

	// 1) 只给新密码 → 必须拒
	rr := postAccountSec(t, h, "/api/user/password", `{"newPassword":"new-secret"}`)
	if code, msg, _ := decodeEnvelope(t, rr); code != 400 || !strings.Contains(msg, "原密码") {
		t.Fatalf("missing old password: code=%d msg=%q", code, msg)
	}
	// 2) 旧密码错 → 必须拒
	rr = postAccountSec(t, h, "/api/user/password", `{"oldPassword":"nope","newPassword":"new-secret"}`)
	if code, msg, _ := decodeEnvelope(t, rr); code != 400 || !strings.Contains(msg, "原密码不正确") {
		t.Fatalf("wrong old password: code=%d msg=%q", code, msg)
	}
	// 3) 两次都不该写库
	if len(fake.execSQL) != 0 {
		t.Fatalf("rejected requests must not write, got %v", fake.execSQL)
	}
}

func TestClawUserPasswordHashesNewPassword(t *testing.T) {
	realHash := clawHashPassword("old-secret")
	fake := &accountSecDB{user: accountUserRow(realHash, "")}
	h := &Handler{DB: fake}

	rr := postAccountSec(t, h, "/api/user/password", `{"oldPassword":"old-secret","newPassword":"new-secret"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	if len(fake.execArgs) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(fake.execArgs))
	}
	args := fake.execArgs[0]
	if args[1] != "721913" {
		t.Fatalf("update target = %v, want 721913", args[1])
	}
	// 落库的必须是**新密码**的哈希（而不是明文、也不是旧哈希）
	stored, _ := args[0].(string)
	if !clawVerifyPassword("new-secret", stored) {
		t.Fatalf("stored hash does not verify new password: %q", stored)
	}
	if clawVerifyPassword("old-secret", stored) {
		t.Fatalf("stored hash still verifies old password")
	}
}

func TestClawUserPasswordSetsForWeChatOnlyAccount(t *testing.T) {
	// 微信登录建出来的账号：哨兵哈希，从来没设过密码。
	fake := &accountSecDB{user: accountUserRow("wechat_only$no_password_set", "openid-1")}
	h := &Handler{DB: fake}

	rr := postAccountSec(t, h, "/api/user/password", `{"newPassword":"first-secret"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("wechat-only account must be able to set a password, got %d (%s)", rr.Code, rr.Body.String())
	}
	stored, _ := fake.execArgs[0][0].(string)
	if !clawVerifyPassword("first-secret", stored) {
		t.Fatalf("stored hash does not verify new password: %q", stored)
	}
}

func TestClawUserPasswordRejectsSamePasswordAndShortPassword(t *testing.T) {
	realHash := clawHashPassword("old-secret")
	fake := &accountSecDB{user: accountUserRow(realHash, "")}
	h := &Handler{DB: fake}

	rr := postAccountSec(t, h, "/api/user/password", `{"oldPassword":"old-secret","newPassword":"old-secret"}`)
	if code, msg, _ := decodeEnvelope(t, rr); code != 400 || !strings.Contains(msg, "不能与原密码相同") {
		t.Fatalf("same password: code=%d msg=%q", code, msg)
	}
	rr = postAccountSec(t, h, "/api/user/password", `{"oldPassword":"old-secret","newPassword":"123"}`)
	if code, _, _ := decodeEnvelope(t, rr); code != 400 {
		t.Fatalf("short password should be rejected, got %d", code)
	}
	if len(fake.execSQL) != 0 {
		t.Fatalf("rejected requests must not write")
	}
}

func TestClawUserBindingValidatesFormatAndUniqueness(t *testing.T) {
	fake := &accountSecDB{user: accountUserRow(clawHashPassword("x"), ""), taken: map[string]bool{"phone": true}}
	h := &Handler{DB: fake}

	cases := []struct {
		name   string
		body   string
		want   int
		expect string
	}{
		{"手机号格式", `{"phone":"12345"}`, 400, "手机号格式不正确"},
		{"邮箱格式", `{"email":"not-an-email"}`, 400, "邮箱格式不正确"},
		{"手机号被占用", `{"phone":"13800000000"}`, 409, "已绑定其他账号"},
		{"两个都没传", `{}`, 400, "请提供"},
	}
	for _, tc := range cases {
		rr := postAccountSec(t, h, "/api/user/binding", tc.body)
		code, msg, _ := decodeEnvelope(t, rr)
		if code != tc.want || !strings.Contains(msg, tc.expect) {
			t.Fatalf("%s: code=%d msg=%q want %d/%q", tc.name, code, msg, tc.want, tc.expect)
		}
	}
	if len(fake.execSQL) != 0 {
		t.Fatalf("rejected requests must not write")
	}
}

func TestClawUserBindingWritesAndCanClear(t *testing.T) {
	fake := &accountSecDB{user: accountUserRow(clawHashPassword("x"), ""), taken: map[string]bool{}}
	h := &Handler{DB: fake}

	rr := postAccountSec(t, h, "/api/user/binding", `{"phone":"13900000000","email":"new@example.com"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	if len(fake.execArgs) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(fake.execArgs))
	}
	sql := fake.execSQL[0]
	// 空串走 NULLIF → 解绑；非空原样落库。两条语义都必须出现在同一条 UPDATE 里，
	// 否则「清空手机号」会变成写入空字符串（登录时再按空串匹配就乱套了）。
	if !strings.Contains(sql, "NULLIF($1, '')") || !strings.Contains(sql, "NULLIF($2, '')") {
		t.Fatalf("update sql = %q", sql)
	}
	if got := fake.execArgs[0][:2]; got[0] != "13900000000" || got[1] != "new@example.com" {
		t.Fatalf("args = %v", fake.execArgs[0])
	}
	// 回包要带上账号安全面板要用的两个布尔位
	_, _, data := decodeEnvelope(t, rr)
	if _, ok := data["hasPassword"]; !ok {
		t.Fatalf("response should carry hasPassword, got %v", data)
	}
	if data["wechatBound"] != false {
		t.Fatalf("wechatBound = %v, want false", data["wechatBound"])
	}
}

func TestClawUserWeChatBindWithoutConfig(t *testing.T) {
	// 显式清空两个配置：没配微信时必须明确 503，不能让前端以为绑上了。
	t.Setenv("WECHAT_MP_APPID", "")
	t.Setenv("WECHAT_MP_SECRET", "")

	fake := &accountSecDB{user: accountUserRow(clawHashPassword("x"), "")}
	h := &Handler{DB: fake}

	rr := postAccountSec(t, h, "/api/user/wechat-bind", `{"code":"any-code"}`)
	code, msg, _ := decodeEnvelope(t, rr)
	if code != 503 || !strings.Contains(msg, "微信登录未配置") {
		t.Fatalf("code=%d msg=%q", code, msg)
	}
	if len(fake.execSQL) != 0 {
		t.Fatalf("must not write when wechat is not configured")
	}
}

// TestClawLoginAcceptsAnyBoundIdentifier 钉住「登录框只给一个输入框」这件事：
// 虾说号 / 手机号 / 邮箱都必须能登录，且优先级是 username 最高（老用户不受影响）。
//
// 桩 DB 只关心 SQL 里有没有那四列 —— 真正的优先级由 ORDER BY 保证，那条语句
// 在集成环境里跑（这里断言的是「查询确实把这四列都放进 WHERE 了」，防止有人
// 把登录改回只查 username）。
func TestClawLoginAcceptsAnyBoundIdentifier(t *testing.T) {
	var gotSQL string
	var gotArg string
	fake := &accountSecDB{user: accountUserRow(clawHashPassword("pw-123456"), "")}
	// 拦一层：记录登录实际发出的那一条 SQL。
	rec := &recordingDB{inner: fake, onQueryRow: func(sql string, args []any) {
		gotSQL = sql
		if len(args) > 0 {
			gotArg, _ = args[0].(string)
		}
	}}
	h := &Handler{DB: rec}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"username":"13800000000","password":"pw-123456"}`))
	h.ClawLogin(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	if gotArg != "13800000000" {
		t.Fatalf("login lookup arg = %q", gotArg)
	}
	for _, col := range []string{"username=$1", "user_id=$1", "phone=$1", "lower(email)=lower($1)"} {
		if !strings.Contains(gotSQL, col) {
			t.Fatalf("login lookup must match %s, sql = %q", col, gotSQL)
		}
	}
	// 优先级必须写在同一句的 ORDER BY 里：username 先于 user_id、先于 phone。
	if !strings.Contains(gotSQL, "ORDER BY (username=$1) DESC, (user_id=$1) DESC") {
		t.Fatalf("login lookup lost its priority order: %q", gotSQL)
	}
}

// TestClawLoginRejectsPasswordLoginForWeChatOnlyAccount 说明「微信账号没有密码」
// 这条边界：哨兵哈希验证必然失败，接口回 401，而不是让它用空密码登进去。
func TestClawLoginRejectsPasswordLoginForWeChatOnlyAccount(t *testing.T) {
	fake := &accountSecDB{user: accountUserRow("wechat_only$no_password_set", "openid-1")}
	h := &Handler{DB: fake}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"username":"721913","password":"wechat_only$no_password_set"}`))
	h.ClawLogin(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (%s)", rr.Code, rr.Body.String())
	}
}

// recordingDB 把 QueryRow 透传给内层桩，同时记录 SQL —— 用来断言「查了哪几列」。
type recordingDB struct {
	inner      *accountSecDB
	onQueryRow func(sql string, args []any)
}

func (r *recordingDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if r.onQueryRow != nil {
		r.onQueryRow(sql, args)
	}
	return r.inner.QueryRow(ctx, sql, args...)
}

func (r *recordingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return r.inner.Query(ctx, sql, args...)
}

func (r *recordingDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return r.inner.Exec(ctx, sql, args...)
}

// ---- /api/user/info 的账号状态可见性 ----------------------------------------

// infoViewDB 让「查谁」和「谁在查」解耦：viewer 是 Bearer 换出来的 user_id，
// row 是被查的那个人的 13 列。
type infoViewDB struct {
	viewer string
	row    []any
}

func (f *infoViewDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "WHERE rongcloud_token="):
		if f.viewer == "" {
			return &groupStubRow{err: pgx.ErrNoRows}
		}
		return &groupStubRow{values: []any{f.viewer}}
	case strings.Contains(sql, "WHERE user_id = $1"):
		return &groupStubRow{values: f.row}
	default:
		// username 兜底、rongcloud_user 回落都当作查不到。
		return &groupStubRow{err: pgx.ErrNoRows}
	}
}

func (f *infoViewDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("not stubbed")
}

func (f *infoViewDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 0"), nil
}

func getInfo(rr http.ResponseWriter, h *Handler, userID, token string) {
	req := httptest.NewRequest(http.MethodGet, "/api/user/info?userId="+userID, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	h.ClawUserInfo(rr, req)
}

// 别人查我：公开资料照给，账号安全状态（有没有密码、绑没绑微信）必须剥掉。
// 这个接口不校验登录态，剥不掉等于把「谁设了密码」全量公开。
func TestClawUserInfoHidesSecurityFlagsFromOthers(t *testing.T) {
	fake := &infoViewDB{viewer: "300001", row: accountUserRow(clawHashPassword("x"), "openid-abc")}
	h := &Handler{DB: fake}

	rr := httptest.NewRecorder()
	getInfo(rr, h, "721913", "im-token")
	code, _, data := decodeEnvelope(t, rr)
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	if data["nickname"] != "小酒" {
		t.Fatalf("公开字段丢了: %v", data)
	}
	if _, ok := data["hasPassword"]; ok {
		t.Fatalf("别人的 hasPassword 不该回: %v", data)
	}
	if _, ok := data["wechatBound"]; ok {
		t.Fatalf("别人的 wechatBound 不该回: %v", data)
	}
}

// 匿名查我：同样剥掉（未登录也要走这条分支，靠的是「viewer 空串 ≠ 目标」）。
func TestClawUserInfoHidesSecurityFlagsFromAnonymous(t *testing.T) {
	fake := &infoViewDB{viewer: "", row: accountUserRow(clawHashPassword("x"), "openid-abc")}
	h := &Handler{DB: fake}

	rr := httptest.NewRecorder()
	getInfo(rr, h, "721913", "")
	code, _, data := decodeEnvelope(t, rr)
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	if _, ok := data["hasPassword"]; ok {
		t.Fatalf("匿名不该拿到 hasPassword: %v", data)
	}
}

// 查自己：账号安全面板靠这两个布尔决定「设置/修改」与「已绑定/去绑定」，
// 所以本人必须拿到，且微信-only 账号的哨兵哈希不能被当成「有密码」。
func TestClawUserInfoShowsSecurityFlagsToSelf(t *testing.T) {
	fake := &infoViewDB{viewer: "721913", row: accountUserRow("wechat_only$no_password_set", "openid-abc")}
	h := &Handler{DB: fake}

	rr := httptest.NewRecorder()
	getInfo(rr, h, "721913", "im-token")
	code, _, data := decodeEnvelope(t, rr)
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	if data["hasPassword"] != false {
		t.Fatalf("微信-only 账号 hasPassword 应为 false，got %v", data["hasPassword"])
	}
	if data["wechatBound"] != true {
		t.Fatalf("wechatBound 应为 true，got %v", data["wechatBound"])
	}
}
