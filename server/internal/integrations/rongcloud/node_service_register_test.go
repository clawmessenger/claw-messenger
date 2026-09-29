package rongcloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// registerTestEnv bundles the DB pool, fixtures, mock RongCloud API server and
// services a Register test needs. The suite skips when no database is
// reachable, mirroring the handler package's TestMain convention.
type registerTestEnv struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	dbfx    *testutil.Fixture
	nodes   *NodeService
	pairing *PairingService
	wsID    string
	userID  string
}

func newRegisterTestEnv(t *testing.T) *registerTestEnv {
	t.Helper()

	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("skipping: could not connect to database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skipping: database not reachable: %v", err)
	}
	t.Cleanup(pool.Close)

	queries := db.New(pool)

	// Mock RongCloud API: every token request succeeds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":200,"token":"test-token"}`))
	}))
	t.Cleanup(srv.Close)

	// Seed a workspace, its owner user and the owner membership. Rows are
	// removed again by the fixture's cleanup hooks.
	seed := testutil.New(pool, "", "")
	wsID := seed.Workspace(t, "Register test workspace", "register-test-ws", testutil.Cols{
		"issue_prefix": "RGW",
	})
	userID := seed.User(t, "Register Test User", "register-test-user@multica.ai")
	seed.Member(t, wsID, userID, "owner")

	return &registerTestEnv{
		pool:    pool,
		queries: queries,
		dbfx:    testutil.New(pool, wsID, userID),
		nodes:   NewNodeService(queries, newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger()), nil, testLogger()),
		pairing: NewPairingService(queries, nil, testLogger()),
		wsID:    wsID,
		userID:  userID,
	}
}

// cleanupRegisteredNode removes the rows Register created for the given
// RongCloud user id. Tables carry no foreign keys, so the order is cosmetic.
func (e *registerTestEnv) cleanupRegisteredNode(t *testing.T, macAddress string) {
	t.Helper()
	rcUserID := "rc_node_" + macAddress
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.pool.Exec(ctx, `DELETE FROM rongcloud_device WHERE node_id IN (SELECT id FROM rongcloud_node WHERE rongcloud_user_id = $1)`, rcUserID)
		_, _ = e.pool.Exec(ctx, `DELETE FROM rongcloud_node WHERE rongcloud_user_id = $1`, rcUserID)
		_, _ = e.pool.Exec(ctx, `DELETE FROM rongcloud_user WHERE rongcloud_user_id = $1`, rcUserID)
	})
}

// createPendingPairingTicket creates a pending pairing session bound to the
// env workspace through the pairing service, mirroring what the Web UI does.
func (e *registerTestEnv) createPendingPairingTicket(t *testing.T, expiresIn time.Duration) string {
	t.Helper()
	wsUUID, err := parseUUIDParam(e.wsID)
	if err != nil {
		t.Fatalf("parse workspace uuid: %v", err)
	}
	session, err := e.pairing.CreateSession(context.Background(), PairingCreateParams{
		WorkspaceID: wsUUID,
		ExpiresIn:   expiresIn,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return session.Ticket
}

func registerParams(macAddress, ticket string) NodeRegisterParams {
	return NodeRegisterParams{
		Name:          "register-test-node",
		MacAddress:    macAddress,
		NodeType:      "ai",
		AIType:        "claude",
		Capabilities:  []string{"chat"},
		PairingTicket: ticket,
	}
}

// Register without a pairing ticket cannot attribute the node to a workspace,
// and the rongcloud tables require a workspace, so it must fail with the
// explicit ErrWorkspaceAttributionRequired instead of a database error.
func TestRegisterWithoutPairingTicketRequiresAttribution(t *testing.T) {
	env := newRegisterTestEnv(t)

	_, err := env.nodes.Register(context.Background(), registerParams("aa:bb:cc:dd:ee:01", ""))
	if err == nil {
		t.Fatal("expected error for registration without workspace attribution, got nil")
	}
	if want := ErrWorkspaceAttributionRequired; !containsError(err, want) {
		t.Fatalf("expected ErrWorkspaceAttributionRequired, got %v", err)
	}
}

// A pairing ticket that does not exist must be rejected clearly rather than
// silently dropping the attribution.
func TestRegisterWithUnknownTicketFails(t *testing.T) {
	env := newRegisterTestEnv(t)

	_, err := env.nodes.Register(context.Background(), registerParams("aa:bb:cc:dd:ee:02", "pt_does_not_exist"))
	if !containsError(err, ErrInvalidPairingTicket) {
		t.Fatalf("expected ErrInvalidPairingTicket, got %v", err)
	}
}

// An expired ticket must be rejected the same way as an unknown one.
func TestRegisterWithExpiredTicketFails(t *testing.T) {
	env := newRegisterTestEnv(t)
	ticket := env.createPendingPairingTicket(t, -time.Minute)

	_, err := env.nodes.Register(context.Background(), registerParams("aa:bb:cc:dd:ee:03", ticket))
	if !containsError(err, ErrInvalidPairingTicket) {
		t.Fatalf("expected ErrInvalidPairingTicket for expired ticket, got %v", err)
	}
}

// A valid pending ticket attributes the node (and its user/device rows) to the
// ticket's workspace with a workspace manager as owner.
func TestRegisterWithValidTicketAttributesWorkspace(t *testing.T) {
	env := newRegisterTestEnv(t)
	ticket := env.createPendingPairingTicket(t, 5*time.Minute)
	env.cleanupRegisteredNode(t, "aa:bb:cc:dd:ee:04")

	result, err := env.nodes.Register(context.Background(), registerParams("aa:bb:cc:dd:ee:04", ticket))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if result.NodeID == "" {
		t.Fatalf("expected node_id in result, got %+v", result)
	}

	wsUUID, err := parseUUIDParam(env.wsID)
	if err != nil {
		t.Fatalf("parse workspace uuid: %v", err)
	}
	ownerUUID, err := parseUUIDParam(env.userID)
	if err != nil {
		t.Fatalf("parse owner uuid: %v", err)
	}

	node, err := env.queries.GetRongCloudNodeByNodeID(context.Background(), result.NodeID)
	if err != nil {
		t.Fatalf("GetRongCloudNodeByNodeID: %v", err)
	}
	if node.WorkspaceID != wsUUID {
		t.Fatalf("node workspace = %v, want %s", node.WorkspaceID, env.wsID)
	}
	if node.OwnerUserID != ownerUUID {
		t.Fatalf("node owner = %v, want %s", node.OwnerUserID, env.userID)
	}

	user, err := env.queries.GetRongCloudUserByRongCloudID(context.Background(), "rc_node_aa:bb:cc:dd:ee:04")
	if err != nil {
		t.Fatalf("GetRongCloudUserByRongCloudID: %v", err)
	}
	if user.WorkspaceID != wsUUID {
		t.Fatalf("rongcloud user workspace = %v, want %s", user.WorkspaceID, env.wsID)
	}
}

func containsError(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
			continue
		}
		return false
	}
	return false
}
