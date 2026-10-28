package handler

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

// claw_user_info_test.go 覆盖 /api/user/info 的节点回落链：
//
// 前端（clawmessenger-web 的 loadAuthoritativeChatroom / loadGroupMembers /
// handleAddMember）会对群聊/聊天室的**每个成员**调这个接口取昵称头像。AI 成员
// 的 userId 是 rc_node_*（rongcloud_user 表），不在 claw_im_users —— 修复前
// 全部 404，气泡退化成裸 id。回落顺序：claw_im_users(user_id) →
// claw_im_users(username) → rongcloud_user(rongcloud_user_id)，最后一步不命中
// 才 404。

type userInfoFakeDB struct {
	queryRow func(sql string, args []any) pgx.Row
}

func (f *userInfoFakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return f.queryRow(sql, args)
}

func (f *userInfoFakeDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("not stubbed")
}

func (f *userInfoFakeDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag(""), errors.New("not stubbed")
}

func TestClawUserInfoFallsBackToRongCloudNode(t *testing.T) {
	fake := &userInfoFakeDB{}
	fake.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "FROM rongcloud_user") {
			if args[0] != "rc_node_3300776275_openclaw" {
				t.Errorf("node lookup args = %v", args)
			}
			return &groupStubRow{values: []any{
				"rc_node_3300776275_openclaw", "openclaw", "https://cdn.example/a.png", true, false,
			}}
		}
		// claw_im_users 两次都查不到（user_id + username）。
		return &groupStubRow{err: pgx.ErrNoRows}
	}
	h := &Handler{DB: fake}

	req := httptest.NewRequest(http.MethodGet, "/api/user/info?userId=rc_node_3300776275_openclaw", nil)
	rr := httptest.NewRecorder()
	h.ClawUserInfo(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			UserId      string `json:"userId"`
			Username    string `json:"username"`
			Nickname    string `json:"nickname"`
			PortraitUri string `json:"portraitUri"`
			IsAiNode    bool   `json:"isAiNode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != 200 || env.Data.Nickname != "openclaw" || !env.Data.IsAiNode {
		t.Fatalf("envelope = %#v", env)
	}
	if env.Data.PortraitUri != "https://cdn.example/a.png" {
		t.Errorf("portrait = %q", env.Data.PortraitUri)
	}
}

func TestClawUserInfoReservedNodeStays404(t *testing.T) {
	// 机器主节点（is_system_reserved）是身份派生的内部锚点，不对外当资料暴露。
	fake := &userInfoFakeDB{}
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		if strings.Contains(sql, "FROM rongcloud_user") {
			return &groupStubRow{values: []any{"rc_node_3300776275", "机器", "", false, true}}
		}
		return &groupStubRow{err: pgx.ErrNoRows}
	}
	h := &Handler{DB: fake}

	req := httptest.NewRequest(http.MethodGet, "/api/user/info?userId=rc_node_3300776275", nil)
	rr := httptest.NewRecorder()
	h.ClawUserInfo(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("reserved node must stay 404, got %d (%s)", rr.Code, rr.Body.String())
	}
}

func TestClawUserInfoHumanStillWins(t *testing.T) {
	// 人类账号必须优先于节点回落，且不会被 rongcloud_user 的同名行劫走。
	fake := &userInfoFakeDB{}
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		if strings.Contains(sql, "FROM rongcloud_user") {
			t.Fatal("human hit must never reach the rongcloud_user fallback")
		}
		// 13 个值对应 clawQueryUser 的 13 个扫描目标（claw_compat.go 的 SELECT）：
		// user_id, username, nickname, portrait_uri, email, phone, signature,
		// gender, birthday, status, password_hash, rongcloud_token,
		// wechat_openid。少一个就会撞上 assignGroupScan 的 arity 检查 → 被当成
		// 「查不到」→ 落到 rongcloud_user 回落，测试红在一个跟断言无关的地方。
		return &groupStubRow{values: []any{
			"897610", "probe040318", "绑定探针", "", "", "", "", "", "", "active", "hash", "token", "",
		}}
	}
	h := &Handler{DB: fake}

	req := httptest.NewRequest(http.MethodGet, "/api/user/info?userId=897610", nil)
	rr := httptest.NewRecorder()
	h.ClawUserInfo(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "probe040318") {
		t.Fatalf("body = %s", rr.Body.String())
	}
}
