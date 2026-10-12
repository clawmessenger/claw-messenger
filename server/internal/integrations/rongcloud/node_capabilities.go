package rongcloud

import (
	"encoding/json"
	"sort"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// discussionCapabilities is the capability set the discussion runtime asks a
// bound agent node for.
//
// The Go runtime decides how to run a discussion turn purely from ai_type: a
// node whose ai_type names an agent CLI is executed in-process by
// DiscussionBridge (see discussion_bridge.go), and every other agent node is
// asked over IM with the `discussion/your_turn` command
// (discussion_coordinator.go). The Node CLI answers that command from
// packages/xiachat-cli/src/im.ts and honours the round, speaking_order and
// model parameters it carries — which is exactly what these four capabilities
// advertise:
//
//	discussion_host               the node can carry the moderator turn
//	discussion_roundtable         the node takes part in an ordered round
//	discussion_model_routing      the node honours the per-turn model route
//	discussion_role_recommendation the node can propose the role line-up
//
// Until this set existed the column was never populated: the Node CLI does not
// send `capabilities` at all (packages/xiachat-cli/src/api.ts defaults it to
// []), and ensureAgentNode created every agent node with a hard-coded '[]'.
// Every surface that gates on these values — the admin host picker, the web
// host picker (clawmessenger-web/src/lib/discussion-host.ts) — therefore listed
// nothing. Deriving them from ai_type is what keeps the advertised set equal to
// what the runtime actually asks the node to do.
var discussionCapabilities = []string{
	"discussion_host",
	"discussion_model_routing",
	"discussion_role_recommendation",
	"discussion_roundtable",
}

// DiscussionCapabilities returns a copy of the capability set every bound agent
// node advertises for the discussion runtime.
func DiscussionCapabilities() []string {
	return append([]string(nil), discussionCapabilities...)
}

// NodeCapabilitiesFor derives the capability set an rongcloud node advertises
// from its ai_type, mirroring the runtime's own dispatch contract.
//
// The per-machine infrastructure node (ai_type empty before it is stamped) and
// the built-in ops maintenance agent (OpsAgentType) are internal plumbing and
// advertise nothing: neither is ever asked to speak, and both are hidden from
// every user-facing listing by isHiddenNode.
func NodeCapabilitiesFor(aiType string) []string {
	aiType = strings.TrimSpace(aiType)
	if aiType == "" || aiType == OpsAgentType {
		return nil
	}
	return DiscussionCapabilities()
}

// MergeNodeCapabilities unions the capabilities a device reported with the ones
// the server derives from its ai_type, so fixing the missing discussion set
// never drops a value a device did report. The result is sorted to keep the
// stored JSON stable, which is what lets the write path skip a no-op UPDATE.
func MergeNodeCapabilities(reported []string, aiType string) []string {
	all := make([]string, 0, len(reported)+len(discussionCapabilities))
	all = append(all, reported...)
	all = append(all, NodeCapabilitiesFor(aiType)...)

	seen := make(map[string]struct{}, len(all))
	merged := make([]string, 0, len(all))
	for _, value := range all {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		merged = append(merged, trimmed)
	}
	sort.Strings(merged)
	if len(merged) == 0 {
		return []string{}
	}
	return merged
}

// nodeCapabilitiesJSON renders the merged capability set for the jsonb column.
func nodeCapabilitiesJSON(reported []string, aiType string) []byte {
	encoded, err := json.Marshal(MergeNodeCapabilities(reported, aiType))
	if err != nil {
		return []byte("[]")
	}
	return encoded
}

// nodeCapabilitiesJSONFromStored re-derives the capability set from whatever a
// node row already holds. A stored value that is not a JSON array (the column
// default is '{}') degrades to no reported capabilities rather than failing.
func nodeCapabilitiesJSONFromStored(stored []byte, aiType string) []byte {
	var reported []string
	if len(stored) > 0 {
		if err := json.Unmarshal(stored, &reported); err != nil {
			reported = nil
		}
	}
	return nodeCapabilitiesJSON(reported, aiType)
}

// IsDisplayNode reports whether n belongs in a user-facing node listing. The
// per-machine infrastructure node and the ops maintenance agent stay internal;
// this is the same contract the device list, friend list and admin pickers use.
func IsDisplayNode(n db.RongcloudNode) bool {
	return !isHiddenNode(n)
}
