package rongcloud

import (
	"context"
	"net/url"
)

// createGroup creates a RongCloud group. userId is a repeated form param.
func (c *rongcloudAPIClient) createGroup(ctx context.Context, groupID, groupName string, userIDs []string) error {
	form := url.Values{
		"groupId":   {groupID},
		"groupName": {groupName},
	}
	for _, uid := range userIDs {
		form.Add("userId", uid)
	}
	_, err := c.postForm(ctx, "/group/create.json", form)
	return err
}

// dismissGroup dismisses a RongCloud group.
func (c *rongcloudAPIClient) dismissGroup(ctx context.Context, groupID, memberID string) error {
	form := url.Values{
		"groupId":  {groupID},
		"memberId": {memberID},
	}
	_, err := c.postForm(ctx, "/group/dismiss.json", form)
	return err
}

// joinGroup adds users to a group. userId is a repeated form param.
func (c *rongcloudAPIClient) joinGroup(ctx context.Context, groupID string, userIDs []string) error {
	form := url.Values{"groupId": {groupID}}
	for _, uid := range userIDs {
		form.Add("userId", uid)
	}
	_, err := c.postForm(ctx, "/group/join.json", form)
	return err
}

// quitGroup removes users from a group. userId is a repeated form param.
func (c *rongcloudAPIClient) quitGroup(ctx context.Context, groupID string, userIDs []string) error {
	form := url.Values{"groupId": {groupID}}
	for _, uid := range userIDs {
		form.Add("userId", uid)
	}
	_, err := c.postForm(ctx, "/group/quit.json", form)
	return err
}

// refreshGroupInfo refreshes a group's name and portrait.
func (c *rongcloudAPIClient) refreshGroupInfo(ctx context.Context, groupID, groupName, portraitURI string) error {
	form := url.Values{"groupId": {groupID}}
	if groupName != "" {
		form.Set("groupName", groupName)
	}
	if portraitURI != "" {
		form.Set("portraitUri", portraitURI)
	}
	_, err := c.postForm(ctx, "/group/refresh.json", form)
	return err
}
