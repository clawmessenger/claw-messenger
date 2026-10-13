package rongcloud

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// chatroomInfoResult holds the response from getChatroomInfo.
type chatroomInfoResult struct {
	ChatroomID string `json:"chatRoomId"`
	Name       string `json:"name"`
}

// chatroomMember represents a single chatroom member in a query response.
type chatroomMember struct {
	UserID string `json:"userId"`
}

// chatroomMembersResult holds the response from getChatroomMembers.
type chatroomMembersResult struct {
	Users []chatroomMember `json:"users"`
	Total int              `json:"total"`
}

// createChatroom creates a RongCloud chatroom.
func (c *rongcloudAPIClient) createChatroom(ctx context.Context, chatroomID string) error {
	form := url.Values{
		"chatroomId":  {chatroomID},
		"destroyType": {"0"},
		"destroyTime": {"10080"},
	}
	_, err := c.postForm(ctx, "/chatroom/create_new.json", form)
	return err
}

// destroyChatroom destroys a RongCloud chatroom.
func (c *rongcloudAPIClient) destroyChatroom(ctx context.Context, chatroomID string) error {
	form := url.Values{"chatroomId": {chatroomID}}
	_, err := c.postForm(ctx, "/chatroom/destroy.json", form)
	return err
}

// joinChatroom adds users to a chatroom.
func (c *rongcloudAPIClient) joinChatroom(ctx context.Context, chatroomID string, userIDs []string) error {
	form := url.Values{
		"chatroomId": {chatroomID},
		"userId":     {strings.Join(userIDs, ",")},
	}
	_, err := c.postForm(ctx, "/chatroom/join.json", form)
	return err
}

// quitChatroom removes users from a chatroom.
func (c *rongcloudAPIClient) quitChatroom(ctx context.Context, chatroomID string, userIDs []string) error {
	form := url.Values{
		"chatroomId": {chatroomID},
		"userId":     {strings.Join(userIDs, ",")},
	}
	_, err := c.postForm(ctx, "/chatroom/quit.json", form)
	return err
}

// getChatroomInfo retrieves chatroom information.
func (c *rongcloudAPIClient) getChatroomInfo(ctx context.Context, chatroomID string) (chatroomInfoResult, error) {
	form := url.Values{"chatroomId": {chatroomID}}
	result, err := c.postForm(ctx, "/chatroom/get.json", form)
	if err != nil {
		return chatroomInfoResult{}, err
	}
	return chatroomInfoResult{
		ChatroomID: getString(result, "chatRoomId"),
		Name:       getString(result, "name"),
	}, nil
}

// getChatroomMembers queries chatroom members.
func (c *rongcloudAPIClient) getChatroomMembers(ctx context.Context, chatroomID string, count int) (chatroomMembersResult, error) {
	if count <= 0 {
		count = 20
	}
	form := url.Values{
		"chatroomId": {chatroomID},
		"count":      {fmt.Sprintf("%d", count)},
	}
	result, err := c.postForm(ctx, "/chatroom/user/query.json", form)
	if err != nil {
		return chatroomMembersResult{}, err
	}
	members := chatroomMembersResult{}
	if users, ok := result["users"].([]interface{}); ok {
		for _, u := range users {
			if m, ok := u.(map[string]interface{}); ok {
				members.Users = append(members.Users, chatroomMember{
					UserID: getString(m, "userId"),
				})
			}
		}
	}
	if total, ok := result["total"].(float64); ok {
		members.Total = int(total)
	}
	return members, nil
}

// ensureChatroom creates a chatroom if it does not exist (code 23410 = not found).
func (c *rongcloudAPIClient) ensureChatroom(ctx context.Context, chatroomID, name string) error {
	_, err := c.getChatroomInfo(ctx, chatroomID)
	if err == nil {
		return nil
	}
	if createErr := c.createChatroom(ctx, chatroomID); createErr != nil {
		return fmt.Errorf("rongcloud: ensure chatroom (create): %w", createErr)
	}
	return nil
}

// ---- 拉黑 / 禁言 / 白名单 -------------------------------------------------
//
// 这三个能力融云都以「聊天室 + 逗号分隔的 userId 列表」建模，端点一一对应旧后端
// rongcloud/chatroom.py 的 block_chatroom_member / mute_chatroom_member /
// add_chatroom_whitelist 等。拉黑与禁言的写动作支持 minute（0=永久），白名单没有
// 时长概念。列表查询统一返回 `{"users":[{"id","time"}]}`，这里解析成通用 map 列表，
// 字段位置对融云的历史变体宽容（users 数组缺省时返回空而非报错）。

// blockChatroomMember 把用户加入聊天室黑名单（minute=0 永久）。
func (c *rongcloudAPIClient) blockChatroomMember(ctx context.Context, chatroomID string, userIDs []string, minute int) error {
	form := url.Values{
		"chatroomId": {chatroomID},
		"userId":     {strings.Join(userIDs, ",")},
		"minute":     {strconv.Itoa(minute)},
	}
	_, err := c.postForm(ctx, "/chatroom/user/block/add.json", form)
	return err
}

// unblockChatroomMember 把用户移出聊天室黑名单。
func (c *rongcloudAPIClient) unblockChatroomMember(ctx context.Context, chatroomID string, userIDs []string) error {
	form := url.Values{
		"chatroomId": {chatroomID},
		"userId":     {strings.Join(userIDs, ",")},
	}
	_, err := c.postForm(ctx, "/chatroom/user/block/remove.json", form)
	return err
}

// listChatroomBlocklist 查询聊天室黑名单。
func (c *rongcloudAPIClient) listChatroomBlocklist(ctx context.Context, chatroomID string) ([]map[string]interface{}, error) {
	result, err := c.postForm(ctx, "/chatroom/user/block/list.json", url.Values{"chatroomId": {chatroomID}})
	if err != nil {
		return nil, err
	}
	return parseModerationUserList(result), nil
}

// gagChatroomMember 禁言聊天室成员（minute=0 永久）。
func (c *rongcloudAPIClient) gagChatroomMember(ctx context.Context, chatroomID string, userIDs []string, minute int) error {
	form := url.Values{
		"chatroomId": {chatroomID},
		"userId":     {strings.Join(userIDs, ",")},
		"minute":     {strconv.Itoa(minute)},
	}
	_, err := c.postForm(ctx, "/chatroom/user/gag/add.json", form)
	return err
}

// ungagChatroomMember 解除聊天室成员禁言。
func (c *rongcloudAPIClient) ungagChatroomMember(ctx context.Context, chatroomID string, userIDs []string) error {
	form := url.Values{
		"chatroomId": {chatroomID},
		"userId":     {strings.Join(userIDs, ",")},
	}
	_, err := c.postForm(ctx, "/chatroom/user/gag/remove.json", form)
	return err
}

// listChatroomGaglist 查询聊天室禁言列表。
func (c *rongcloudAPIClient) listChatroomGaglist(ctx context.Context, chatroomID string) ([]map[string]interface{}, error) {
	result, err := c.postForm(ctx, "/chatroom/user/gag/list.json", url.Values{"chatroomId": {chatroomID}})
	if err != nil {
		return nil, err
	}
	return parseModerationUserList(result), nil
}

// addChatroomWhitelist 把用户加入聊天室白名单。
func (c *rongcloudAPIClient) addChatroomWhitelist(ctx context.Context, chatroomID string, userIDs []string) error {
	form := url.Values{
		"chatroomId": {chatroomID},
		"userId":     {strings.Join(userIDs, ",")},
	}
	_, err := c.postForm(ctx, "/chatroom/user/whitelist/add.json", form)
	return err
}

// removeChatroomWhitelist 把用户移出聊天室白名单。
func (c *rongcloudAPIClient) removeChatroomWhitelist(ctx context.Context, chatroomID string, userIDs []string) error {
	form := url.Values{
		"chatroomId": {chatroomID},
		"userId":     {strings.Join(userIDs, ",")},
	}
	_, err := c.postForm(ctx, "/chatroom/user/whitelist/remove.json", form)
	return err
}

// listChatroomWhitelist 查询聊天室白名单。
func (c *rongcloudAPIClient) listChatroomWhitelist(ctx context.Context, chatroomID string) ([]map[string]interface{}, error) {
	result, err := c.postForm(ctx, "/chatroom/user/whitelist/query.json", url.Values{"chatroomId": {chatroomID}})
	if err != nil {
		return nil, err
	}
	return parseModerationUserList(result), nil
}

// parseModerationUserList 从黑名单/禁言/白名单查询响应里取出 users 数组。每一项
// 至少是 {"id": ...}，融云还会带 time；其余字段原样保留。对字段名宽容（id /
// userId / user_id），缺省时跳过该项，保证前端拿到的都是可用记录。
func parseModerationUserList(result map[string]interface{}) []map[string]interface{} {
	raw, ok := result["users"].([]interface{})
	if !ok {
		return []map[string]interface{}{}
	}
	out := make([]map[string]interface{}, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		id := firstNonEmptyString(m["id"], m["userId"], m["user_id"])
		if id == "" {
			continue
		}
		rec := make(map[string]interface{}, len(m)+1)
		for k, v := range m {
			rec[k] = v
		}
		rec["id"] = id
		out = append(out, rec)
	}
	return out
}
