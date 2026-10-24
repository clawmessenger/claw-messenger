package handler

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RefreshRongCloudToken's failure branch is the only place the server records
// why a node could not get a fresh IM token, so the log line is part of the
// endpoint's contract: a 500 without it leaves the incident with no server-side
// evidence. These tests pin the status, the body text the CLI/front-end prints,
// and the log record together.

// refreshTokenAPIFunc adapts a function to http.RoundTripper. NodeService owns
// its API client (unexported type, built inside the rongcloud package), so from
// this package the only seam is the default transport that client falls back to
// — it is constructed without a transport of its own.
type refreshTokenAPIFunc func(*http.Request) (*http.Response, error)

func (f refreshTokenAPIFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stubRefreshTokenAPI replaces http.DefaultTransport for the duration of the
// test and reports the RongCloud path that was actually hit, so a test can prove
// the response came from the refresh call rather than an earlier guard.
func stubRefreshTokenAPI(t *testing.T, body string) *string {
	t.Helper()
	previous := http.DefaultTransport
	calledPath := ""
	http.DefaultTransport = refreshTokenAPIFunc(func(r *http.Request) (*http.Response, error) {
		calledPath = r.URL.Path
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	return &calledPath
}

// seedRefreshTokenNode inserts the rongcloud_user + rongcloud_node pair
// RefreshToken reads before it calls the REST API, and removes both again.
func seedRefreshTokenNode(t *testing.T, nodeID string) {
	t.Helper()
	rcUserID := "rc_" + nodeID
	dbfx.Insert(t, "rongcloud_user", testutil.Cols{
		"workspace_id":      testWorkspaceID,
		"rongcloud_user_id": rcUserID,
		"name":              "Refresh Token Node",
	})
	dbfx.Cleanup(t, "DELETE FROM rongcloud_user WHERE rongcloud_user_id = $1", rcUserID)
	dbfx.Insert(t, "rongcloud_node", testutil.Cols{
		"workspace_id":      testWorkspaceID,
		"owner_user_id":     testUserID,
		"rongcloud_user_id": rcUserID,
		"node_id":           nodeID,
		"capabilities":      testutil.Raw("'[]'::jsonb"),
	})
	dbfx.Cleanup(t, "DELETE FROM rongcloud_node WHERE node_id = $1", nodeID)
}

// newRefreshTokenHandler wires a real NodeService against the suite database.
// h.RongCloudNode is a concrete *rongcloud.NodeService, so RefreshToken cannot
// be stubbed from this package; the client is created without credentials
// (ensureCreds is a no-op when no loader is set), which keeps the call off the
// real network and points it at whatever transport the test installed. The
// package-level slog default is redirected into buf and restored on cleanup so
// the rest of the package's tests still log where they expect.
func newRefreshTokenHandler(t *testing.T, buf *bytes.Buffer) *Handler {
	t.Helper()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	logger := slog.Default()
	client := rongcloud.NewRongCloudAPIClientForServices(nil, nil, logger)
	return &Handler{
		RongCloudNode: rongcloud.NewNodeService(db.New(testPool), client, nil, logger),
	}
}

func refreshTokenRequest(nodeID string) *http.Request {
	req := testutil.JSONRequest(http.MethodPost, "/api/claw/refresh-token/"+nodeID, nil)
	return testutil.WithURLParams(req, "nodeId", nodeID)
}

func requireLogged(t *testing.T, logs, want string) {
	t.Helper()
	if !strings.Contains(logs, want) {
		t.Fatalf("log output does not contain %q; got:\n%s", want, logs)
	}
}

// A failed refresh must answer 500 with the unchanged public message, and must
// leave one ERROR record naming the node and carrying the error.
func TestRefreshRongCloudTokenOnRefreshErrorReturns500AndLogs(t *testing.T) {
	nodeID := fmt.Sprintf("node_refresh_err_%d", time.Now().UnixNano())
	seedRefreshTokenNode(t, nodeID)
	calledPath := stubRefreshTokenAPI(t, `{"code":500,"errorMessage":"rongcloud is down"}`)

	var logs bytes.Buffer
	h := newRefreshTokenHandler(t, &logs)

	out := testutil.Call(t, h.RefreshRongCloudToken, refreshTokenRequest(nodeID)).
		Want(http.StatusInternalServerError).
		Map()

	// The body text is the contract the CLI/front-end shows a user; it must not
	// drift while this branch gains logging.
	if got, _ := out["error"].(string); got != "failed to refresh token" {
		t.Fatalf(`error = %q, want "failed to refresh token" (full body: %v)`, got, out)
	}
	// Proof the 500 came from the refresh call itself, not from a guard that
	// would keep the assertions below from ever touching the error branch.
	if got := *calledPath; got != "/user/getToken.json" {
		t.Fatalf("RongCloud getUserToken was not reached; called path = %q", got)
	}

	logged := logs.String()
	requireLogged(t, logged, "level=ERROR")
	requireLogged(t, logged, "refresh rongcloud token failed")
	requireLogged(t, logged, "node_id="+nodeID)
	requireLogged(t, logged, "error=")
	requireLogged(t, logged, "rongcloud is down")
	if n := strings.Count(logged, "refresh rongcloud token failed"); n != 1 {
		t.Fatalf("expected exactly one failure record, got %d; log:\n%s", n, logged)
	}
}

// Positive control: the same wiring with a successful refresh answers 200 with
// the token and produces no failure record — otherwise the assertions above
// would pass on any handler invocation.
func TestRefreshRongCloudTokenOnSuccessReturnsTokenWithoutFailureLog(t *testing.T) {
	nodeID := fmt.Sprintf("node_refresh_ok_%d", time.Now().UnixNano())
	seedRefreshTokenNode(t, nodeID)
	calledPath := stubRefreshTokenAPI(t, `{"code":200,"token":"tok_refresh_ok"}`)

	var logs bytes.Buffer
	h := newRefreshTokenHandler(t, &logs)

	out := testutil.Call(t, h.RefreshRongCloudToken, refreshTokenRequest(nodeID)).
		Want(http.StatusOK).
		Map()

	if got := out["token"]; got != "tok_refresh_ok" {
		t.Fatalf("token = %v, want tok_refresh_ok (full body: %v)", got, out)
	}
	if _, isError := out["error"]; isError {
		t.Fatalf("success response carries an error field: %v", out)
	}
	if got := *calledPath; got != "/user/getToken.json" {
		t.Fatalf("RongCloud getUserToken was not reached; called path = %q", got)
	}

	logged := logs.String()
	if strings.Contains(logged, "refresh rongcloud token failed") {
		t.Fatalf("success path logged the failure record; log:\n%s", logged)
	}
	if strings.Contains(logged, "level=ERROR") {
		t.Fatalf("success path logged an ERROR record; log:\n%s", logged)
	}
}
