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

// addFriend creates a one-way RongCloud friend relation from userID to
// friendID with optType=2 (direct add, no approval step). The legacy Python
// backend used the same call when binding a device so the node user showed up
// in the owner's IM friend list. RongCloud answers 25460 when the two are
// already friends, which is reported as alreadyFriends=true and counts as
// success so re-binding (and the friend backfill) stays idempotent.
func (c *rongcloudAPIClient) addFriend(ctx context.Context, userID, friendID, remark string) (alreadyFriends bool, err error) {
	form := url.Values{
		"userId":   {userID},
		"targetId": {friendID},
		"optType":  {"2"},
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
func (c *rongcloudAPIClient) getUserInfo(ctx context.Context, userID string) (userInfoResult, error) {
	form := url.Values{"userId": {userID}}
	result, err := c.postForm(ctx, "/user/info.json", form)
	if err != nil {
		return userInfoResult{}, err
	}
	info := userInfoResult{
		UserID:      getString(result, "userId"),
		Name:        getString(result, "name"),
		PortraitURI: getString(result, "portraitUri"),
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
