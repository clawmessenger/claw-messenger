package rongcloud

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// claimCacheTTL bounds how often the friend-list inversion is repeated. The
// admin device list is the only caller and it renders a page at a time, so a
// short window is enough to keep a single page load from re-fetching.
const claimCacheTTL = 15 * time.Second

// NodeClaimants maps an agent node's RongCloud user id to the claw/IM account
// that claims it.
//
// The mapping is derived from the RongCloud friend edges, because nothing local
// records IM ownership: rongcloud_node.owner_user_id points at the multica
// workspace manager (resolveWorkspaceAttribution picks the workspace's first
// manager), and rongcloud_pairing_session never captured the claiming account.
// The friend edge written when a device was claimed
// (BackfillAgentFriends with Bidirectional) is therefore the only 融云-side
// truth, and inverting it is what "按融云侧归属" means in practice.
//
// RongCloud friend relations are one-way and the platform API exposes no
// "who friended me" query, so inversion costs one /friend/get.json call per
// account. The whole map is memoised for claimCacheTTL.
//
// An account that fails to answer is skipped rather than failing the call: one
// bad token must not blank out the entire归属 column. The error is only
// returned when nothing at all could be resolved.
func (s *NodeService) NodeClaimants(ctx context.Context, imUserIDs []string) (map[string]string, error) {
	if s.client == nil {
		return nil, errors.New("rongcloud: API client not configured")
	}
	s.claimMu.Lock()
	if s.claimCache != nil && time.Since(s.claimCacheAt) < claimCacheTTL {
		cached := s.claimCache
		s.claimMu.Unlock()
		return cached, nil
	}
	s.claimMu.Unlock()

	claimants := make(map[string]string)
	var lastErr error
	answered := 0
	for _, userID := range imUserIDs {
		userID = strings.TrimSpace(userID)
		if userID == "" {
			continue
		}
		friends, err := s.client.FriendIDs(ctx, userID)
		if err != nil {
			lastErr = err
			continue
		}
		answered++
		for _, friend := range friends {
			// RongCloud friend edges are not unique across accounts in theory;
			// first writer wins so a page render stays deterministic.
			if _, taken := claimants[friend]; !taken {
				claimants[friend] = userID
			}
		}
	}
	if answered == 0 && lastErr != nil {
		return nil, lastErr
	}

	s.claimMu.Lock()
	s.claimCache = claimants
	s.claimCacheAt = time.Now()
	s.claimMu.Unlock()
	return claimants, nil
}

// InvalidateNodeClaimants drops the memoised friend-edge inversion. Claim
// changes must call this so the next read reflects the new edges instead of
// serving the pre-change mapping for up to claimCacheTTL.
func (s *NodeService) InvalidateNodeClaimants() {
	s.claimMu.Lock()
	s.claimCache = nil
	s.claimCacheAt = time.Time{}
	s.claimMu.Unlock()
}

// GrantFriendship adds targetID to userID's RongCloud friend list, reporting
// whether the edge already existed. Mirrors the edge binding writes, so an
// ownership change leaves the node reachable in the new owner's contact list.
func (s *NodeService) GrantFriendship(ctx context.Context, userID, targetID, remark string) (bool, error) {
	if s.client == nil {
		return false, errors.New("rongcloud: API client not configured")
	}
	return s.client.addFriend(ctx, userID, targetID, remark)
}

// RevokeFriendship removes the given edges from userID's RongCloud friend list.
func (s *NodeService) RevokeFriendship(ctx context.Context, userID string, targetIDs ...string) error {
	if s.client == nil {
		return errors.New("rongcloud: API client not configured")
	}
	if len(targetIDs) == 0 {
		return nil
	}
	return s.client.RemoveFriends(ctx, userID, targetIDs...)
}

// friendEdgeNodeOwner lists the nodes a user owns, derived from the user's
// RongCloud friend edges — the same 融云好友边 truth NodeClaimants inverts.
// It is the P5 roundtable creation service's nodeOwner: for each friend rc
// account, GetRongCloudNodeByRongCloudUserID maps it back to its node row.
// A friend id with no node row (installation accounts, other claw users) is
// skipped; the per-machine infrastructure node never qualifies as a discussion
// participant (ai_type empty → no discussion capabilities), so it never enters
// the eligible set downstream.
func friendEdgeNodeOwner(queries *db.Queries, client *rongcloudAPIClient, logger *slog.Logger) func(ctx context.Context, userID string) ([]db.RongcloudNode, error) {
	return func(ctx context.Context, userID string) ([]db.RongcloudNode, error) {
		if queries == nil {
			return nil, errors.New("rongcloud: database not available")
		}
		if client == nil {
			return nil, errors.New("rongcloud: API client not configured")
		}
		friends, err := client.FriendIDs(ctx, userID)
		if err != nil {
			return nil, err
		}
		nodes := make([]db.RongcloudNode, 0, len(friends))
		for _, friend := range friends {
			friend = strings.TrimSpace(friend)
			if friend == "" {
				continue
			}
			node, err := queries.GetRongCloudNodeByRongCloudUserID(ctx, friend)
			if err != nil {
				// 好友里混着安装账号/真人 claw 用户：查不到节点行属正常，
				// 单个失败只跳过，不让整个归属列表失败。
				continue
			}
			nodes = append(nodes, node)
		}
		return nodes, nil
	}
}
