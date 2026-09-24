package rongcloud

import (
	"context"
	"fmt"
	"net/url"
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
