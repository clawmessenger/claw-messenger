package rongcloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// deviceCodeEnv is the device-code flow's integration harness: a real
// database, the real pairing + node services, and a RongCloud API stub that
// records the friend edges the authorization step writes.
type deviceCodeEnv struct {
	pool      *pgxpool.Pool
	queries   *db.Queries
	nodes     *NodeService
	pairing   *PairingService
	workspace string
	owner     string
	// friendEdges records "owner->target" for every /friend/add.json call,
	// which is the only durable proof that an authorized agent belongs to a
	// user (rongcloud_pairing_session never records the claiming account).
	friendMu    sync.Mutex
	friendEdges []string
}

func newDeviceCodeEnv(t *testing.T) *deviceCodeEnv {
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

	env := &deviceCodeEnv{pool: pool, queries: db.New(pool)}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/friend/add.json") {
			_ = r.ParseForm()
			env.friendMu.Lock()
			env.friendEdges = append(env.friendEdges, r.Form.Get("userId")+"->"+r.Form.Get("targetId"))
			env.friendMu.Unlock()
			_, _ = w.Write([]byte(`{"code":200}`))
			return
		}
		// Token issuance for every node/user the flow creates.
		_, _ = w.Write([]byte(`{"code":200,"token":"test-token"}`))
	}))
	t.Cleanup(srv.Close)

	seed := testutil.New(pool, "", "")
	env.workspace = seed.Workspace(t, "Device code integration", "device-code-it-"+time.Now().Format("150405.000000"), testutil.Cols{
		"issue_prefix": "DCI",
	})
	env.owner = seed.User(t, "Device Code Owner", "device-code-owner-"+time.Now().Format("150405.000000")+"@multica.ai")
	seed.Member(t, env.workspace, env.owner, "owner")

	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	env.nodes = NewNodeService(env.queries, client, nil, testLogger())
	env.pairing = NewPairingService(env.queries, nil, testLogger())
	return env
}

func (e *deviceCodeEnv) friendEdgeSnapshot() []string {
	e.friendMu.Lock()
	defer e.friendMu.Unlock()
	return append([]string(nil), e.friendEdges...)
}

func (e *deviceCodeEnv) cleanup(t *testing.T, machineID, ticket string) {
	t.Helper()
	rcUserID := "rc_node_" + machineID
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.pool.Exec(ctx, `DELETE FROM rongcloud_device WHERE node_id IN (SELECT id FROM rongcloud_node WHERE machine_id = $1)`, machineID)
		_, _ = e.pool.Exec(ctx, `DELETE FROM rongcloud_node WHERE machine_id = $1`, machineID)
		_, _ = e.pool.Exec(ctx, `DELETE FROM rongcloud_node WHERE rongcloud_user_id = $1`, rcUserID)
		_, _ = e.pool.Exec(ctx, `DELETE FROM rongcloud_user WHERE rongcloud_user_id = $1`, rcUserID)
		_, _ = e.pool.Exec(ctx, `DELETE FROM rongcloud_pairing_session WHERE ticket = $1`, ticket)
	})
}

// TestDeviceCodePairingEndToEnd walks the exact sequence the CLI and the
// browser perform and asserts the observable outcomes at each hop:
//
//	terminal: create device session -> register with its ticket -> claim
//	browser:  bind the agents the device reported
//	terminal: poll sees the bound agents
//
// The last assertion is the important one: the friend edge is the only place
// that records "this agent now belongs to that user", so a bind that silently
// skips it looks successful in every other table.
func TestDeviceCodePairingEndToEnd(t *testing.T) {
	env := newDeviceCodeEnv(t)
	ctx := context.Background()

	wsUUID, err := parseUUIDParam(env.workspace)
	if err != nil {
		t.Fatalf("parse workspace uuid: %v", err)
	}

	machineID := "it-machine-" + time.Now().Format("150405.000000")
	reported := []string{"opencode", "hermes"}

	// 1. Terminal asks for a device-code session. No user is involved yet.
	session, err := env.pairing.CreateDeviceSession(ctx, DeviceSessionParams{
		WorkspaceID: wsUUID,
		MachineID:   machineID,
		Hostname:    "it-host",
		Platform:    "darwin",
		Agents:      reported,
		ExpiresIn:   10 * time.Minute,
	})
	env.cleanup(t, machineID, session.Ticket)
	if err != nil {
		t.Fatalf("CreateDeviceSession: %v", err)
	}
	if len(session.UserCode.String) != 8 {
		t.Fatalf("user_code = %q, want 8 chars", session.UserCode.String)
	}
	// The browser can resolve the code before the terminal even connects.
	if _, err := env.pairing.GetSessionByUserCode(ctx, strings.ToLower(session.UserCode.String)); err != nil {
		t.Fatalf("GetSessionByUserCode(lowercased): %v", err)
	}

	// 2. Terminal registers the machine against that ticket and claims it.
	reg, err := env.nodes.Register(ctx, NodeRegisterParams{
		Name:          "it-host opencode",
		MacAddress:    machineID,
		NodeType:      "ai",
		AIType:        "opencode",
		PairingTicket: session.Ticket,
		Agents:        reported,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if reg.NodeID == "" || reg.Token == "" {
		t.Fatalf("register returned an empty node id/token: %+v", reg)
	}
	claim, err := env.pairing.ClaimSession(ctx, session.Ticket, "", "idem-it-"+machineID)
	if err != nil {
		t.Fatalf("ClaimSession: %v", err)
	}
	if claim.DeviceCredentialID == "" || claim.DeviceSecret == "" {
		t.Fatalf("claim did not issue a device credential: %+v", claim)
	}

	// The reported agents must survive register (it rewrites the column), or
	// the verify page would show nothing to tick.
	afterRegister, err := env.pairing.GetSession(ctx, session.Ticket)
	if err != nil {
		t.Fatalf("GetSession after register: %v", err)
	}
	var reportedAfter []string
	if err := json.Unmarshal(afterRegister.ReportedAgents, &reportedAfter); err != nil {
		t.Fatalf("decode reported_agents: %v", err)
	}
	if len(reportedAfter) != 2 {
		t.Fatalf("reported_agents after register = %v, want 2", reportedAfter)
	}
	if got := SessionEffectiveStatus(afterRegister, time.Now()); got != "claimed" {
		t.Fatalf("status after claim = %q, want claimed", got)
	}

	// 3. The human authorizes in the browser: bind exactly what the device
	// reported, attributed to the owner's RongCloud identity.
	ownerRongCloudID := "rc_owner_" + strings.ReplaceAll(machineID, ".", "")
	before := len(env.friendEdgeSnapshot())
	bound, err := env.nodes.BindAgents(ctx, afterRegister, reported, ownerRongCloudID)
	if err != nil {
		t.Fatalf("BindAgents: %v", err)
	}
	if len(bound) != 2 {
		t.Fatalf("bound %d agents, want 2: %+v", len(bound), bound)
	}
	agents := map[string]bool{}
	for _, b := range bound {
		agents[b.Agent] = true
		if b.NodeID == "" {
			t.Fatalf("bound agent %q has no node id", b.Agent)
		}
	}
	for _, want := range reported {
		if !agents[want] {
			t.Fatalf("agent %q missing from bind result %+v", want, bound)
		}
	}

	// 4. The friend edges were written — one per agent, owner -> agent.
	edges := env.friendEdgeSnapshot()[before:]
	if len(edges) != len(reported) {
		t.Fatalf("friend edges = %v, want %d entries", edges, len(reported))
	}
	for _, edge := range edges {
		if !strings.HasPrefix(edge, ownerRongCloudID+"->") {
			t.Fatalf("friend edge %q is not attributed to the authorizing user %q", edge, ownerRongCloudID)
		}
	}

	// 5. The terminal's poll sees the authorization and can stop waiting.
	afterBind, err := env.pairing.GetSession(ctx, session.Ticket)
	if err != nil {
		t.Fatalf("GetSession after bind: %v", err)
	}
	var boundAgents []BoundAgentResult
	if err := json.Unmarshal(afterBind.BoundAgents, &boundAgents); err != nil {
		t.Fatalf("decode bound_agents: %v", err)
	}
	if len(boundAgents) != len(reported) {
		t.Fatalf("bound_agents = %+v, want %d entries", boundAgents, len(reported))
	}
}

// TestDeviceCodePairingRejectsBindBeforeClaim pins the ordering the browser
// page relies on: a code that no terminal has claimed yet cannot be
// authorized, so the page shows "waiting for the device" instead of a ticking
// form that would fail on submit.
func TestDeviceCodePairingRejectsBindBeforeClaim(t *testing.T) {
	env := newDeviceCodeEnv(t)
	ctx := context.Background()
	wsUUID, err := parseUUIDParam(env.workspace)
	if err != nil {
		t.Fatalf("parse workspace uuid: %v", err)
	}
	machineID := "it-unclaimed-" + time.Now().Format("150405.000000")
	session, err := env.pairing.CreateDeviceSession(ctx, DeviceSessionParams{
		WorkspaceID: wsUUID,
		MachineID:   machineID,
		Hostname:    "it-host",
		Platform:    "linux",
		Agents:      []string{"hermes"},
		ExpiresIn:   10 * time.Minute,
	})
	env.cleanup(t, machineID, session.Ticket)
	if err != nil {
		t.Fatalf("CreateDeviceSession: %v", err)
	}

	_, err = env.nodes.BindAgents(ctx, session, []string{"hermes"}, "rc_owner_unclaimed")
	if err == nil {
		t.Fatal("BindAgents on a pending session must fail")
	}
	if !strings.Contains(err.Error(), "not claimed") {
		// ErrPairingSessionNotClaimed is what the handler maps to HTTP 409.
		if err != ErrPairingSessionNotClaimed {
			t.Fatalf("error = %v, want ErrPairingSessionNotClaimed", err)
		}
	}
}
