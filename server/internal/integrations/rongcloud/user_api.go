package rongcloud

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// userInfoResult holds the response from getUserInfo.
type userInfoResult struct {
	UserID      string `json:"userId"`
	Name        string `json:"name"`
	PortraitURI string `json:"portraitUri"`
}

// getUserToken registers or retrieves a RongCloud IM token for a user.
func (c *rongcloudAPIClient) getUserToken(ctx context.Context, userID, name, portraitURI string) (string, error) {
	form := url.Values{
		"userId":      {userID},
		"name":        {name},
		"portraitUri": {portraitURI},
	}
	result, err := c.postForm(ctx, "/user/getToken.json", form)
	if err != nil {
		return "", err
	}
	token, _ := result["token"].(string)
	return token, nil
}

// refreshUser refreshes a user's info in RongCloud. Empty name/portraitURI
// fields are omitted (RongCloud leaves them untouched), matching the legacy
// Python client's optional-field semantics.
func (c *rongcloudAPIClient) refreshUser(ctx context.Context, userID, name, portraitURI string) (string, error) {
	form := url.Values{"userId": {userID}}
	if name != "" {
		form.Set("name", name)
	}
	if portraitURI != "" {
		form.Set("portraitUri", portraitURI)
	}
	result, err := c.postForm(ctx, "/user/refresh.json", form)
	if err != nil {
		return "", err
	}
	token, _ := result["token"].(string)
	return token, nil
}

// checkOnline returns true if the user is currently online.
func (c *rongcloudAPIClient) checkOnline(ctx context.Context, userID string) (bool, error) {
	form := url.Values{"userId": {userID}}
	result, err := c.postForm(ctx, "/user/checkOnline.json", form)
	if err != nil {
		return false, err
	}
	status, _ := result["status"].(string)
	return status == "1", nil
}

// expireToken invalidates tokens issued before the given timestamp (ms).
func (c *rongcloudAPIClient) expireToken(ctx context.Context, userID string, timestampMs int64) error {
	form := url.Values{
		"userId": {userID},
		"time":   {fmt.Sprintf("%d", timestampMs)},
	}
	_, err := c.postForm(ctx, "/user/token/expire.json", form)
	return err
}

// RongCloud /friend/add.json optType values. The endpoint carries both
// behaviours through one parameter: 2 creates the edge immediately, 1 sends a
// friend *application* that the target has to accept (the edge only exists
// after that).
const (
	friendOptTypeRequest = "1"
	friendOptTypeDirect  = "2"
)

// addFriend creates a one-way RongCloud friend relation from userID to
// friendID with optType=2 (direct add, no approval step). The legacy Python
// backend used the same call when binding a device so the node user showed up
// in the owner's IM friend list. RongCloud answers 25460 when the two are
// already friends, which is reported as alreadyFriends=true and counts as
// success so re-binding (and the friend backfill) stays idempotent.
func (c *rongcloudAPIClient) addFriend(ctx context.Context, userID, friendID, remark string) (alreadyFriends bool, err error) {
	return c.addFriendWithOptType(ctx, userID, friendID, remark, friendOptTypeDirect)
}

// addFriendWithOptType is addFriend with the optType spelled out, so the
// friend command channel can forward the client's choice (1 = ask for
// approval, 2 = direct add). Callers that don't care keep using addFriend,
// whose default stays 2 — every existing binding path must keep adding
// directly, otherwise re-binding suddenly needs an approval nobody can grant.
//
// An alreadyFriends result only makes sense for optType=2: with optType=1 the
// edge is not created by this call, so 25460 can only mean the two are already
// friends from before.
func (c *rongcloudAPIClient) addFriendWithOptType(ctx context.Context, userID, friendID, remark, optType string) (alreadyFriends bool, err error) {
	if optType != friendOptTypeRequest && optType != friendOptTypeDirect {
		return false, fmt.Errorf("rongcloud: unsupported friend optType %q", optType)
	}
	form := url.Values{
		"userId":   {userID},
		"targetId": {friendID},
		"optType":  {optType},
	}
	if remark != "" {
		form.Set("name", remark)
	}
	result, err := c.postForm(ctx, "/friend/add.json", form)
	if err != nil {
		if code, ok := result["code"].(float64); ok && code == 25460 {
			return true, nil // already friends
		}
		return false, err
	}
	return false, nil
}

// friendEntry is one entry from the RongCloud friend list.
type friendEntry struct {
	UserID      string
	Name        string
	PortraitURI string
}

// getFriends fetches a user's RongCloud friend list. RongCloud nests the
// entries under "friendList"; the legacy Python backend also tolerated "data"
// and "users", so accept those shapes too.
func (c *rongcloudAPIClient) getFriends(ctx context.Context, userID string) ([]friendEntry, error) {
	form := url.Values{"userId": {userID}}
	result, err := c.postForm(ctx, "/friend/get.json", form)
	if err != nil {
		return nil, err
	}
	raw, ok := result["friendList"].([]interface{})
	if !ok {
		if raw, ok = result["data"].([]interface{}); !ok {
			raw, _ = result["users"].([]interface{})
		}
	}
	friends := make([]friendEntry, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		id := getString(entry, "userId")
		if id == "" {
			id = getString(entry, "id")
		}
		if id == "" {
			id = getString(entry, "friendId")
		}
		if id == "" {
			continue
		}
		friends = append(friends, friendEntry{
			UserID:      id,
			Name:        getString(entry, "name"),
			PortraitURI: getString(entry, "portraitUri"),
		})
	}
	return friends, nil
}

// getUserInfo retrieves a user's info from RongCloud.
//
// 响应有两种形状，**必须都收**：
//
//	{"code":200,"data":{"userId":"u1","name":"Alice","portraitUri":"http://..."}}
//	{"code":200,"userName":"Alice","userPortrait":"http://...","createTime":"..."}
//
// 前者是线上实际返回（字段包在 data 子字典里）—— 旧 Python 后端的命令通道就是
// 按 `result["data"]["portraitUri"]` 读的，注释见
// clawmessenger-server/rongcloud/command_handler.py:1973；后者是官方英文文档
// （platform-chat-api/user/get）给出的扁平结构，且字段名是 userName / userPortrait。
//
// 之前这里只认扁平 `name` / `portraitUri`，两种形状都对不上：postForm 只看
// `code`，200 就返回成功，于是 getUserInfo 会**静默**回一个空名字 + 空头像，
// 调用方拿不到任何错误信号。它在 2026-10-29 之前没有任何生产调用点（只有
// rongcloud_test.go 里一个扁平 mock），所以这个错位一直没暴露；搜索回填接上
// 来之后就是「永远补不到东西」的哑故障，因此在这里一次性收口。
//
// userId 只在老结构里回传，没回就退回入参 —— 调用方关心的是「这次问的是谁」。
func (c *rongcloudAPIClient) getUserInfo(ctx context.Context, userID string) (userInfoResult, error) {
	form := url.Values{"userId": {userID}}
	result, err := c.postForm(ctx, "/user/info.json", form)
	if err != nil {
		return userInfoResult{}, err
	}
	// 优先 data 子字典；它是空对象或不是对象时退回顶层，两种形状共用下面的取值。
	fields := result
	if data, ok := result["data"].(map[string]interface{}); ok && len(data) > 0 {
		fields = data
	}
	info := userInfoResult{
		UserID:      getStringAny(fields, "userId"),
		Name:        getStringAny(fields, "name", "userName"),
		PortraitURI: getStringAny(fields, "portraitUri", "userPortrait"),
	}
	if info.UserID == "" {
		info.UserID = userID
	}
	return info, nil
}

// RemoveFriends deletes the friend edges from userID to targetIDs.
//
// The endpoint replaces the whole target set it is given, so passing an empty
// list is a no-op rather than a "delete everything". RongCloud's hosted friend
// service counts one call per target, and caps a single request at 100.
func (c *rongcloudAPIClient) RemoveFriends(ctx context.Context, userID string, targetIDs ...string) error {
	if len(targetIDs) == 0 {
		return nil
	}
	form := url.Values{
		"userId":    {userID},
		"targetIds": {strings.Join(targetIDs, ",")},
	}
	_, err := c.postForm(ctx, "/friend/delete.json", form)
	return err
}

// FriendIDs returns the ids in a user's RongCloud friend list.
func (c *rongcloudAPIClient) FriendIDs(ctx context.Context, userID string) ([]string, error) {
	friends, err := c.getFriends(ctx, userID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(friends))
	for _, f := range friends {
		ids = append(ids, f.UserID)
	}
	return ids, nil
}

// DeactivateUser permanently deletes the given users' RongCloud data.
//
// RongCloud drops the profile, message history, conversations and push
// registrations; the user id can only be reactivated empty, so this cannot be
// undone. It is offered separately from the local row cleanup for that reason,
// and callers must gate it behind an explicit opt-in.
func (c *rongcloudAPIClient) DeactivateUser(ctx context.Context, userIDs ...string) error {
	if len(userIDs) == 0 {
		return nil
	}
	form := url.Values{"userId": {strings.Join(userIDs, ",")}}
	_, err := c.postForm(ctx, "/user/deactivate.json", form)
	return err
}
