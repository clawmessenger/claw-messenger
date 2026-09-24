package rongcloud

import (
	"context"
	"fmt"
	"net/url"
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

// refreshUser refreshes a user's info in RongCloud.
func (c *rongcloudAPIClient) refreshUser(ctx context.Context, userID, name, portraitURI string) (string, error) {
	form := url.Values{
		"userId":      {userID},
		"name":        {name},
		"portraitUri": {portraitURI},
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
