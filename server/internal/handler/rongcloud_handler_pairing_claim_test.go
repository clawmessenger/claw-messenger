package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// newPairingClaimHandler wires only the pairing service, the one dependency
// ClaimRongCloudPairing reads. The suite's TestMain never wires RongCloud
// services into testHandler, and NodeService cannot be constructed outside
// the rongcloud package (unexported API client parameter), so the claim tests
// seed the candidate node row directly through the fixture instead of going
// through NodeService.Register.
func newPairingClaimHandler() *Handler {
	return &Handler{RongCloudPairing: rongcloud.NewPairingService(db.New(testPool), nil, nil)}
}

// seedPairingCandidateNode inserts one rongcloud_node row for the suite
// workspace and returns its primary key UUID, ready for CandidateNodeIDs.
func seedPairingCandidateNode(t *testing.T) pgtype.UUID {
	t.Helper()
	rowID := dbfx.Insert(t, "rongcloud_node", testutil.Cols{
		"workspace_id":      testWorkspaceID,
		"owner_user_id":     testUserID,
		"rongcloud_user_id": "rc_node_pairing_claim_test",
		"node_id":           "node_pairing_claim_test",
		"capabilities":      testutil.Raw("'[]'::jsonb"),
	})
	// ClaimSession inserts a rongcloud_device row for the claimed node; no
	// foreign keys exist, but remove it so the suite database stays clean.
	dbfx.Cleanup(t, "DELETE FROM rongcloud_device WHERE node_id = $1", rowID)
	return parseUUID(rowID)
}

// createClaimableSession opens a pending pairing session with one candidate
// node so the claim succeeds and returns a real device credential.
func createClaimableSession(t *testing.T, h *Handler, candidate pgtype.UUID, clientClaimKey string) string {
	t.Helper()
	session, err := h.RongCloudPairing.CreateSession(context.Background(), rongcloud.PairingCreateParams{
		WorkspaceID:      parseUUID(testWorkspaceID),
		CandidateNodeIDs: []pgtype.UUID{candidate},
		ClientClaimKey:   clientClaimKey,
		ExpiresIn:        5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	dbfx.Cleanup(t, "DELETE FROM rongcloud_pairing_session WHERE ticket = $1", session.Ticket)
	return session.Ticket
}

// A device claiming a pending session with a candidate node receives the
// device credential and the node the session bound it to.
func TestClaimRongCloudPairing(t *testing.T) {
	h := newPairingClaimHandler()
	candidate := seedPairingCandidateNode(t)
	ticket := createClaimableSession(t, h, candidate, "")

	req := testutil.JSONRequest(http.MethodPost, "/api/claw/pairing/"+ticket+"/claim", map[string]string{
		"client_claim_key": "",
		"idempotency_key":  "idem-claim-1",
	})
	req = testutil.WithURLParams(req, "ticket", ticket)
	out := testutil.Call(t, h.ClaimRongCloudPairing, req).Want(http.StatusOK).Map()

	if got, _ := out["node_id"].(string); got != "node_pairing_claim_test" {
		t.Fatalf("node_id = %q, want %q (full response: %v)", got, "node_pairing_claim_test", out)
	}
	if got, _ := out["device_credential_id"].(string); got == "" {
		t.Fatalf("expected non-empty device_credential_id, got %v", out)
	}
	if got, _ := out["device_secret"].(string); got == "" {
		t.Fatalf("expected non-empty device_secret, got %v", out)
	}
	session, _ := out["session"].(map[string]any)
	if session == nil || session["status"] != "claimed" {
		t.Fatalf("expected claimed session in response, got %v", out["session"])
	}
}

// An unknown ticket is a client-side dead end, not a server fault: it maps to
// 404 like GetRongCloudPairing, consistent with the Task 1 error-mapping
// convention (user-fixable input never yields 500).
func TestClaimRongCloudPairingUnknownTicket(t *testing.T) {
	h := newPairingClaimHandler()

	req := testutil.JSONRequest(http.MethodPost, "/api/claw/pairing/pt_unknown/claim", map[string]string{
		"idempotency_key": "idem-1",
	})
	req = testutil.WithURLParams(req, "ticket", "pt_unknown")
	testutil.Call(t, h.ClaimRongCloudPairing, req).Want(http.StatusNotFound)
}

// A claim key that does not match the session's stored key is rejected as a
// 400-class input error and leaves the session pending.
func TestClaimRongCloudPairingWrongClaimKey(t *testing.T) {
	h := newPairingClaimHandler()
	candidate := seedPairingCandidateNode(t)
	ticket := createClaimableSession(t, h, candidate, "expected-key")

	req := testutil.JSONRequest(http.MethodPost, "/api/claw/pairing/"+ticket+"/claim", map[string]string{
		"client_claim_key": "wrong-key",
		"idempotency_key":  "idem-2",
	})
	req = testutil.WithURLParams(req, "ticket", ticket)
	testutil.Call(t, h.ClaimRongCloudPairing, req).Want(http.StatusBadRequest)

	var status string
	dbfx.QueryRow(t, "SELECT status FROM rongcloud_pairing_session WHERE ticket = $1", ticket).Scan(&status)
	if status != "pending" {
		t.Fatalf("session status after rejected claim = %q, want pending", status)
	}
}

// Retrying a successful claim with the same idempotency key is a replay, not
// a conflict: the response is a 200 carrying the claimed session, the secret
// is empty (it was handed out exactly once), and no second device row exists.
func TestClaimRongCloudPairingReplaySameKey(t *testing.T) {
	h := newPairingClaimHandler()
	candidate := seedPairingCandidateNode(t)
	ticket := createClaimableSession(t, h, candidate, "")

	claimReq := func(idem string) map[string]any {
		req := testutil.JSONRequest(http.MethodPost, "/api/claw/pairing/"+ticket+"/claim", map[string]string{
			"idempotency_key": idem,
		})
		req = testutil.WithURLParams(req, "ticket", ticket)
		return testutil.Call(t, h.ClaimRongCloudPairing, req).Want(http.StatusOK).Map()
	}

	first := claimReq("idem-replay-1")
	if got, _ := first["device_secret"].(string); got == "" {
		t.Fatalf("first claim: expected non-empty device_secret, got %v", first)
	}

	second := claimReq("idem-replay-1")
	if got, _ := second["device_secret"].(string); got != "" {
		t.Fatalf("replay: expected empty device_secret, got %q", got)
	}
	if got, _ := second["device_credential_id"].(string); got != "" {
		t.Fatalf("replay: expected empty device_credential_id, got %q", got)
	}
	if got, _ := second["node_id"].(string); got != "" {
		t.Fatalf("replay: expected empty node_id, got %q", got)
	}
	session, _ := second["session"].(map[string]any)
	if session == nil || session["status"] != "claimed" {
		t.Fatalf("replay: expected claimed session in response, got %v", second["session"])
	}
	if _, leaked := session["client_claim_key"]; leaked {
		t.Fatalf("replay: session payload leaked client_claim_key: %v", session)
	}
	if _, leaked := session["idempotency_key"]; leaked {
		t.Fatalf("replay: session payload leaked idempotency_key: %v", session)
	}

	// Exactly one device row for the candidate node: the replay must not
	// mint a second credential.
	if n := dbfx.Count(t, "SELECT count(*) FROM rongcloud_device WHERE node_id = $1", util.UUIDToString(candidate)); n != 1 {
		t.Fatalf("device rows after replay = %d, want 1", n)
	}
}

// Retrying a successful claim with a DIFFERENT idempotency key is a genuine
// conflict over an already-claimed session, not a replay.
func TestClaimRongCloudPairingReplayDifferentKey(t *testing.T) {
	h := newPairingClaimHandler()
	candidate := seedPairingCandidateNode(t)
	ticket := createClaimableSession(t, h, candidate, "")

	first := testutil.JSONRequest(http.MethodPost, "/api/claw/pairing/"+ticket+"/claim", map[string]string{
		"idempotency_key": "idem-original",
	})
	first = testutil.WithURLParams(first, "ticket", ticket)
	testutil.Call(t, h.ClaimRongCloudPairing, first).Want(http.StatusOK).Map()

	retry := testutil.JSONRequest(http.MethodPost, "/api/claw/pairing/"+ticket+"/claim", map[string]string{
		"idempotency_key": "idem-other",
	})
	retry = testutil.WithURLParams(retry, "ticket", ticket)
	testutil.Call(t, h.ClaimRongCloudPairing, retry).Want(http.StatusConflict)
}

// A session whose expiry already passed is a user-fixable dead end: the claim
// returns 410 and the session is marked expired in the database.
func TestClaimRongCloudPairingExpiredSession(t *testing.T) {
	h := newPairingClaimHandler()
	candidate := seedPairingCandidateNode(t)
	ticket := createClaimableSession(t, h, candidate, "")

	// Backdate expiry the way time passing would.
	dbfx.Exec(t, "UPDATE rongcloud_pairing_session SET expires_at = now() - interval '1 minute' WHERE ticket = $1", ticket)

	req := testutil.JSONRequest(http.MethodPost, "/api/claw/pairing/"+ticket+"/claim", map[string]string{
		"idempotency_key": "idem-expired",
	})
	req = testutil.WithURLParams(req, "ticket", ticket)
	testutil.Call(t, h.ClaimRongCloudPairing, req).Want(http.StatusGone)

	var status string
	dbfx.QueryRow(t, "SELECT status FROM rongcloud_pairing_session WHERE ticket = $1", ticket).Scan(&status)
	if status != "expired" {
		t.Fatalf("session status after expired claim = %q, want expired", status)
	}
}
