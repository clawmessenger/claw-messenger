package rongcloud

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// chatroomFixture bundles everything a test needs after setup.
type chatroomFixture struct {
	h          *systemHandler
	pool       *pgxpool.Pool
	queries    *db.Queries
	wsID       pgtype.UUID
	owner      db.RongcloudUser
	hostNode   db.RongcloudNode
	memberNode db.RongcloudNode
	roomRCID   string
}

func setupChatroomFixtureFull(t *testing.T) *chatroomFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("skipping: cannot build pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skipping: db unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	queries := db.New(pool)
	wsID := seedWorkspaceRow(t, pool)
	owner := seedUser(t, queries, wsID, "owner-rc-"+uniqueID("t"))
	hostNode := seedNode(t, queries, wsID, "host-node-"+uniqueID("t"), `["discussion_host","discussion_roundtable","discussion_model_routing"]`)
	memberNode := seedNode(t, queries, wsID, "member-node-"+uniqueID("t"), `["discussion_participant","discussion_roundtable","discussion_model_routing"]`)
	h := &systemHandler{queries: queries, nodeID: "rc_test_sys", logger: testLogger()}
	roomRCID := "rc_test_room_" + uniqueID("t")
	r := h.actionChatroomCreate(ctx, CommandContent{
		Service: "chatroom", Action: "create", WebEnvelope: true,
		Params: map[string]interface{}{
			"chatroomId":        roomRCID,
			"chatroomName":      "fixture-room",
			"openclawMaxRounds": 10,
			"hostNodeId":        hostNode.NodeID,
			"aiMembers": []interface{}{
				map[string]interface{}{"nodeId": hostNode.NodeID, "roleName": "host"},
				map[string]interface{}{"nodeId": memberNode.NodeID, "roleName": "analyst", "roleInstructions": "analyze"},
			},
		},
	}, owner)
	if r.code != 200 {
		t.Fatalf("fixture create failed: %d %s", r.code, r.message)
	}
	return &chatroomFixture{
		h: h, pool: pool, queries: queries, wsID: wsID,
		owner: owner, hostNode: hostNode, memberNode: memberNode,
		roomRCID: roomRCID,
	}
}

// newChatroomTestHandler builds a handler over a real DB; nil if DB unreachable.
func newChatroomTestHandler(t *testing.T) *systemHandler {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil
	}
	t.Cleanup(pool.Close)
	return &systemHandler{queries: db.New(pool), nodeID: "rc_test_sys", logger: testLogger()}
}

func (f *chatroomFixture) seedUser(t *testing.T, rcID string) db.RongcloudUser {
	t.Helper()
	return seedUser(t, f.queries, f.wsID, rcID)
}

func seedWorkspaceRow(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	var id pgtype.UUID
	slug := uniqueID("ws")
	err := pool.QueryRow(ctx,
		`INSERT INTO workspace (name, slug, description) VALUES ($1, $2, $3) RETURNING id`,
		"ws-"+slug, slug, "",
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	return id
}

func seedUser(t *testing.T, q *db.Queries, ws pgtype.UUID, rcID string) db.RongcloudUser {
	t.Helper()
	u, err := q.CreateRongCloudUser(context.Background(), db.CreateRongCloudUserParams{
		WorkspaceID:     ws,
		RongcloudUserID: rcID,
		Name:            pgtype.Text{String: "u-" + rcID, Valid: true},
		NodeType:        "human",
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u
}

func seedNode(t *testing.T, q *db.Queries, ws pgtype.UUID, nodeID, capsJSON string) db.RongcloudNode {
	t.Helper()
	n, err := q.CreateRongCloudNode(context.Background(), db.CreateRongCloudNodeParams{
		WorkspaceID:     ws,
		OwnerUserID:     ws, // workspace owns the test node; any valid UUID works
		RongcloudUserID: "rc_" + nodeID,
		NodeID:          nodeID,
		DeployStatus:    "online",
		Capabilities:    []byte(capsJSON),
		MachineID:       "machine-" + nodeID,
	})
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return n
}

func memberPresent(members []map[string]interface{}, uid string) bool {
	for _, m := range members {
		if s, ok := m["userId"].(string); ok && s == uid {
			return true
		}
	}
	return false
}

// --- tests ---

func TestChatroomCommandGetInfoOnMissingRoom(t *testing.T) {
	h := newChatroomTestHandler(t)
	if h == nil {
		t.Skip("database not available")
	}
	r := h.actionChatroomGetInfo(context.Background(), CommandContent{
		Service: "chatroom", Action: "getInfo",
		Params: map[string]interface{}{"chatroomId": "rc_missing_" + uniqueID("t")},
	}, "")
	if r.code != 404 {
		t.Fatalf("expected 404, got %d (%s)", r.code, r.message)
	}
}

func TestChatroomCommandCreateAndGetInfo(t *testing.T) {
	f := setupChatroomFixtureFull(t)
	if f == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	r := f.h.actionChatroomGetInfo(ctx, CommandContent{
		Service: "chatroom", Action: "getInfo",
		Params: map[string]interface{}{"chatroomId": f.roomRCID},
	}, f.owner.RongcloudUserID)
	if r.code != 200 {
		t.Fatalf("getInfo failed: %d %s", r.code, r.message)
	}
	data, _ := r.data.(map[string]interface{})
	if data["chatroomId"] != f.roomRCID {
		t.Fatalf("chatroomId mismatch: %v", data["chatroomId"])
	}
	if data["ownerId"] != f.owner.RongcloudUserID {
		t.Fatalf("ownerId mismatch: got %v want %s", data["ownerId"], f.owner.RongcloudUserID)
	}
	if data["hostNodeId"] != f.hostNode.NodeID {
		t.Fatalf("hostNodeId mismatch: got %v want %s", data["hostNodeId"], f.hostNode.NodeID)
	}
	members, _ := data["members"].([]map[string]interface{})
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}
	aiMembers, _ := data["aiMembers"].([]map[string]interface{})
	if len(aiMembers) != 2 {
		t.Fatalf("expected 2 aiMembers, got %d", len(aiMembers))
	}
	if data["memberCount"].(int) != 3 {
		t.Fatalf("memberCount mismatch: %v", data["memberCount"])
	}
	found := false
	for _, m := range aiMembers {
		if m["nodeId"] == f.memberNode.NodeID {
			if m["memberType"] != "ai" {
				t.Fatalf("ordinary member memberType wrong: %v", m["memberType"])
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("ordinary member %s not in aiMembers", f.memberNode.NodeID)
	}
}

func TestChatroomCommandJoinLeaveRemoveMember(t *testing.T) {
	f := setupChatroomFixtureFull(t)
	if f == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	joiner := f.seedUser(t, "joiner-rc-"+uniqueID("t"))
	r := f.h.actionChatroomJoin(ctx, CommandContent{
		Service: "chatroom", Action: "join",
		Params: map[string]interface{}{"chatroomId": f.roomRCID},
	}, joiner)
	if r.code != 200 {
		t.Fatalf("join failed: %d %s", r.code, r.message)
	}
	r = f.h.actionChatroomGetMembers(ctx, CommandContent{
		Service: "chatroom", Action: "getMembers",
		Params: map[string]interface{}{"chatroomId": f.roomRCID},
	})
	data, _ := r.data.(map[string]interface{})
	users, _ := data["users"].([]map[string]interface{})
	if !memberPresent(users, joiner.RongcloudUserID) {
		t.Fatalf("joiner not in members after join")
	}
	r = f.h.actionChatroomRemoveMember(ctx, CommandContent{
		Service: "chatroom", Action: "removeMember",
		Params: map[string]interface{}{"chatroomId": f.roomRCID, "memberId": joiner.RongcloudUserID},
	}, f.owner)
	if r.code != 200 {
		t.Fatalf("removeMember failed: %d %s", r.code, r.message)
	}
	r = f.h.actionChatroomGetMembers(ctx, CommandContent{
		Service: "chatroom", Action: "getMembers",
		Params: map[string]interface{}{"chatroomId": f.roomRCID},
	})
	data, _ = r.data.(map[string]interface{})
	users, _ = data["users"].([]map[string]interface{})
	if memberPresent(users, joiner.RongcloudUserID) {
		t.Fatalf("joiner still present after removeMember")
	}
	// non-owner cannot remove
	joiner2 := f.seedUser(t, "joiner2-rc-"+uniqueID("t"))
	_ = f.h.actionChatroomJoin(ctx, CommandContent{
		Service: "chatroom", Action: "join",
		Params: map[string]interface{}{"chatroomId": f.roomRCID},
	}, joiner2)
	r = f.h.actionChatroomRemoveMember(ctx, CommandContent{
		Service: "chatroom", Action: "removeMember",
		Params: map[string]interface{}{"chatroomId": f.roomRCID, "memberId": joiner2.RongcloudUserID},
	}, joiner)
	if r.code != 403 {
		t.Fatalf("expected 403 for non-owner remove, got %d", r.code)
	}
}

func TestChatroomCommandUpdateProfile(t *testing.T) {
	f := setupChatroomFixtureFull(t)
	if f == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	r := f.h.actionChatroomUpdate(ctx, CommandContent{
		Service: "chatroom", Action: "update",
		Params: map[string]interface{}{
			"chatroomId":        f.roomRCID,
			"chatroomName":      "renamed-room",
			"portraitUri":       "https://example.org/p.png",
			"openclawMaxRounds": 42,
		},
	}, f.owner)
	if r.code != 200 {
		t.Fatalf("update failed: %d %s", r.code, r.message)
	}
	view, err := f.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, f.roomRCID)
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if view.Name != "renamed-room" || view.PortraitUri != "https://example.org/p.png" || view.MaxRounds != 42 {
		t.Fatalf("update did not persist: name=%q portrait=%q max=%d", view.Name, view.PortraitUri, view.MaxRounds)
	}
	intruder := f.seedUser(t, "intruder-rc-"+uniqueID("t"))
	r = f.h.actionChatroomUpdate(ctx, CommandContent{
		Service: "chatroom", Action: "update",
		Params: map[string]interface{}{"chatroomId": f.roomRCID, "chatroomName": "hacked"},
	}, intruder)
	if r.code != 403 {
		t.Fatalf("expected 403 for non-owner, got %d", r.code)
	}
}

func TestChatroomCommandDestroy(t *testing.T) {
	f := setupChatroomFixtureFull(t)
	if f == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	intruder := f.seedUser(t, "intruder-rc-"+uniqueID("t"))
	r := f.h.actionChatroomDestroy(ctx, CommandContent{
		Service: "chatroom", Action: "destroy",
		Params: map[string]interface{}{"chatroomId": f.roomRCID},
	}, intruder)
	if r.code != 403 {
		t.Fatalf("expected 403, got %d", r.code)
	}
	r = f.h.actionChatroomDestroy(ctx, CommandContent{
		Service: "chatroom", Action: "destroy",
		Params: map[string]interface{}{"chatroomId": f.roomRCID},
	}, f.owner)
	if r.code != 200 {
		t.Fatalf("destroy failed: %d %s", r.code, r.message)
	}
	r = f.h.actionChatroomGetInfo(ctx, CommandContent{
		Service: "chatroom", Action: "getInfo",
		Params: map[string]interface{}{"chatroomId": f.roomRCID},
	}, f.owner.RongcloudUserID)
	if r.code != 404 {
		t.Fatalf("expected 404 after destroy, got %d", r.code)
	}
}

func TestChatroomCommandGetList(t *testing.T) {
	f := setupChatroomFixtureFull(t)
	if f == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	r := f.h.actionChatroomGetList(ctx, CommandContent{
		Service: "chatroom", Action: "getList",
		Params: map[string]interface{}{"page": 1, "pageSize": 100},
	}, f.owner)
	if r.code != 200 {
		t.Fatalf("getList failed: %d %s", r.code, r.message)
	}
	data, _ := r.data.(map[string]interface{})
	list, _ := data["list"].([]map[string]interface{})
	total, _ := data["total"].(int)
	if total < 1 {
		t.Fatalf("expected total >= 1, got %d", total)
	}
	found := false
	for _, room := range list {
		if room["chatroomId"] == f.roomRCID {
			found = true
			if _, has := room["members"]; has {
				t.Fatalf("getList should not include members")
			}
		}
	}
	if !found {
		t.Fatalf("owner's room %q not in list", f.roomRCID)
	}
}

func TestChatroomCommandSetMemberRoleHostGuard(t *testing.T) {
	f := setupChatroomFixtureFull(t)
	if f == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	r := f.h.actionChatroomSetMemberRole(ctx, CommandContent{
		Service: "chatroom", Action: "setMemberRole",
		Params: map[string]interface{}{
			"chatroomId": f.roomRCID, "nodeId": f.hostNode.NodeID,
			"roleName": "x", "roleInstructions": "", "enabled": false,
		},
	}, f.owner)
	if r.code != 409 {
		t.Fatalf("expected 409 (cannot disable host), got %d", r.code)
	}
}
