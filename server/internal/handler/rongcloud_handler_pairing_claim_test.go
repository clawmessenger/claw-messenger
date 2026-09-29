package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	"github.com/multica-ai/multica/server/internal/testutil"
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
