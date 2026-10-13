package rongcloud

import (
	"context"
	"net/url"
	"strconv"
	"strings"
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

// queryGroupMembers lists the RongCloud ids of a group's members.
//
// 融云 /group/user/query.json 是分页接口（count 上限 100，nextId 游标）。旧后端只
// 读首页（Python group.py 默认 count=20），超过 20 人的群会漏人 —— 这里按游标翻页
// 补齐。群成员是 C 端「@ 谁」「群管理」的数据源，少一半成员比慢一点糟糕得多。
func (c *rongcloudAPIClient) queryGroupMembers(ctx context.Context, groupID string) ([]string, error) {
	const pageSize = 100
	// 2000 人，远超本平台单群规模；纯粹是游标异常时的止损，不是业务上限。
	const maxPages = 20

	ids := make([]string, 0, pageSize)
	seen := make(map[string]struct{}, pageSize)
	cursor := ""
	for page := 0; page < maxPages; page++ {
		form := url.Values{
			"groupId": {groupID},
			"count":   {strconv.Itoa(pageSize)},
		}
		if cursor != "" {
			form.Set("nextId", cursor)
		}
		result, err := c.postForm(ctx, "/group/user/query.json", form)
		if err != nil {
			return nil, err
		}
		batch, next := parseGroupMemberPage(result)
		for _, id := range batch {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		if next == "" || len(batch) == 0 {
			break
		}
		cursor = next
	}
	return ids, nil
}

// parseGroupMemberPage 从 /group/user/query.json 的响应里取出本页成员 id 与下一页
// 游标。
//
// 字段位置按 v3 文档是 data.members，但线上见过 data 直接就是数组、以及顶层
// members/users 三种形态（旧后端同样是「先看 data 再看是不是 list」的写法）。这里
// 全部认下来：成员列表静默变空是最难排查的一类故障。
func parseGroupMemberPage(result map[string]interface{}) ([]string, string) {
	container := result
	if data, ok := result["data"]; ok {
		switch typed := data.(type) {
		case []interface{}:
			return groupMemberIDs(typed), groupCursor(result)
		case map[string]interface{}:
			container = typed
		}
	}
	for _, key := range []string{"members", "users", "list"} {
		if list, ok := container[key].([]interface{}); ok {
			return groupMemberIDs(list), groupCursor(container)
		}
	}
	return nil, ""
}

func groupMemberIDs(items []interface{}) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		switch typed := item.(type) {
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				ids = append(ids, trimmed)
			}
		case map[string]interface{}:
			id := firstNonEmptyString(typed["id"], typed["userId"], typed["user_id"])
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func groupCursor(result map[string]interface{}) string {
	return firstNonEmptyString(result["next"], result["nextId"], result["next_id"])
}

// firstNonEmptyString 返回第一个非空字符串，非字符串（JSON 数字 id 之类）也接住。
func firstNonEmptyString(values ...interface{}) string {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				return trimmed
			}
		case float64:
			// 融云偶有把 id 当数字返回的历史版本。
			return strconv.FormatInt(int64(typed), 10)
		}
	}
	return ""
}

// transferGroup hands a group over to a new owner.
//
// 融云把转让群主放在 /entrust 命名空间下（不在 /group/* 里），旧后端
// rongcloud/group.py:transfer_group 用的就是这个路径。
func (c *rongcloudAPIClient) transferGroup(ctx context.Context, groupID, newOwnerID string) error {
	form := url.Values{
		"groupId":    {groupID},
		"newOwnerId": {newOwnerID},
	}
	_, err := c.postForm(ctx, "/entrust/group/transfer/owner.json", form)
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
