package rongcloud

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	"github.com/multica-ai/multica/server/pkg/db/generated"
)

// Claim-verdict sentinels. They let the handler separate client-fixable
// rejections (unknown ticket, session not claimable, bad claim key) from
// database faults, which stay wrapped errors mapped to 500.
var (
	// ErrUnknownPairingTicket is returned by ClaimSession when no session
	// exists for the ticket.
	ErrUnknownPairingTicket = errors.New("rongcloud: unknown pairing ticket")
	// ErrPairingSessionNotPending is returned when the session was already
	// claimed, expired or cancelled and the request is not an idempotent
	// replay of the original claim.
	ErrPairingSessionNotPending = errors.New("rongcloud: pairing session is not pending")
	// ErrPairingSessionExpired is returned when the session's expiry passed;
	// the session is marked expired before the error is returned.
	ErrPairingSessionExpired = errors.New("rongcloud: pairing session expired")
	// ErrInvalidClaimKey is returned when the request's claim key does not
	// match the key the workspace bound to the session at creation.
	ErrInvalidClaimKey = errors.New("rongcloud: invalid client claim key")
)

type PairingCreateParams struct {
	WorkspaceID      pgtype.UUID
	CandidateNodeIDs []pgtype.UUID
	ClientClaimKey   string
	ExpiresIn        time.Duration
}

type ClaimResult struct {
	Session            PairingSessionView `json:"session"`
	DeviceCredentialID string             `json:"device_credential_id"`
	DeviceSecret       string             `json:"device_secret,omitempty"`
	NodeID             string             `json:"node_id"`
}

// PairingSessionView is the caller-facing projection of a pairing session.
// The full row carries the client claim key and the idempotency key — both
// credentials — so responses echo only what a caller needs after a claim.
type PairingSessionView struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Ticket      string `json:"ticket"`
	Status      string `json:"status"`
	ExpiresAt   string `json:"expires_at"`
}

func newPairingSessionView(session db.RongcloudPairingSession) PairingSessionView {
	return PairingSessionView{
		ID:          util.UUIDToString(session.ID),
		WorkspaceID: util.UUIDToString(session.WorkspaceID),
		Ticket:      session.Ticket,
		Status:      session.Status,
		ExpiresAt:   session.ExpiresAt.Time.Format(time.RFC3339),
	}
}

type PairingService struct {
	queries *db.Queries
	box     *secretbox.Box
	logger  *slog.Logger
}

func NewPairingService(queries *db.Queries, box *secretbox.Box, logger *slog.Logger) *PairingService {
	if logger == nil {
		logger = slog.Default()
	}
	return &PairingService{queries: queries, box: box, logger: logger}
}

func (s *PairingService) generateTicket() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "pt_" + hex.EncodeToString(b)
}

// safeTicketPrefix returns the first 6 characters of a ticket ("pt_" plus
// three hex chars). Pairing tickets are bearer credentials, so logs get only
// this non-identifying prefix — enough to correlate lines, not to claim.
func safeTicketPrefix(ticket string) string {
	if len(ticket) > 6 {
		return ticket[:6]
	}
	return ticket
}

func (s *PairingService) CreateSession(ctx context.Context, params PairingCreateParams) (db.RongcloudPairingSession, error) {
	if s.queries == nil {
		return db.RongcloudPairingSession{}, errors.New("rongcloud: database not configured")
	}
	if params.ExpiresIn == 0 {
		params.ExpiresIn = 5 * time.Minute
	}
	ticket := s.generateTicket()
	candidateJSON, _ := json.Marshal(params.CandidateNodeIDs)
	clientClaimKey := pgtype.Text{}
	if params.ClientClaimKey != "" {
		clientClaimKey = pgtype.Text{String: params.ClientClaimKey, Valid: true}
	}
	return s.queries.CreateRongCloudPairingSession(ctx, db.CreateRongCloudPairingSessionParams{
		WorkspaceID:      params.WorkspaceID,
		Ticket:           ticket,
		Status:           "pending",
		ClientClaimKey:   clientClaimKey,
		CandidateNodeIds: candidateJSON,
		ExpiresAt:        pgtype.Timestamptz{Time: time.Now().Add(params.ExpiresIn), Valid: true},
	})
}

func (s *PairingService) GetSession(ctx context.Context, ticket string) (db.RongcloudPairingSession, error) {
	if s.queries == nil {
		return db.RongcloudPairingSession{}, errors.New("rongcloud: database not configured")
	}
	return s.queries.GetRongCloudPairingSessionByTicket(ctx, ticket)
}

// SessionClaimedNodeAIType returns the ai_type of the node that claimed the
// session (candidates[0], backfilled by register). Empty string when the
// session is not claimed or the node lookup fails — callers treat it as
// optional display metadata.
func (s *PairingService) SessionClaimedNodeAIType(ctx context.Context, session db.RongcloudPairingSession) string {
	if s.queries == nil || session.Status != "claimed" {
		return ""
	}
	var candidateNodeIDs []pgtype.UUID
	if err := json.Unmarshal(session.CandidateNodeIds, &candidateNodeIDs); err != nil || len(candidateNodeIDs) == 0 {
		return ""
	}
	node, err := s.queries.GetRongCloudNodeByID(ctx, candidateNodeIDs[0])
	if err != nil {
		s.logger.Warn("rongcloud: claimed node lookup failed for ai_type", "ticket_prefix", safeTicketPrefix(session.Ticket), "error", err)
		return ""
	}
	return node.AiType.String
}

func (s *PairingService) ClaimSession(ctx context.Context, ticket, clientClaimKey, idempotencyKey string) (ClaimResult, error) {
	if s.queries == nil {
		return ClaimResult{}, errors.New("rongcloud: database not configured")
	}
	session, err := s.queries.GetRongCloudPairingSessionByTicket(ctx, ticket)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ClaimResult{}, ErrUnknownPairingTicket
		}
		s.logger.Error("rongcloud: pairing session lookup failed", "ticket_prefix", safeTicketPrefix(ticket), "error", err)
		return ClaimResult{}, fmt.Errorf("rongcloud: get pairing session: %w", err)
	}
	if session.Status != "pending" {
		if session.IdempotencyKey.Valid && session.IdempotencyKey.String == idempotencyKey && session.Status == "claimed" {
			// Idempotent replay of a successful claim: the secret was handed
			// out once and is not stored in plaintext, so only the session
			// comes back. Callers detect the replay via the empty secret.
			return ClaimResult{Session: newPairingSessionView(session)}, nil
		}
		return ClaimResult{}, ErrPairingSessionNotPending
	}
	if session.ExpiresAt.Time.Before(time.Now()) {
		if _, err := s.queries.UpdateRongCloudPairingSessionStatus(ctx, db.UpdateRongCloudPairingSessionStatusParams{
			ID:     session.ID,
			Status: "expired",
		}); err != nil {
			// Marking expiry is best-effort bookkeeping; the session is
			// already rejected either way, so log instead of failing.
			s.logger.Error("rongcloud: failed to mark pairing session expired", "error", err)
		}
		return ClaimResult{}, ErrPairingSessionExpired
	}
	if session.ClientClaimKey.Valid && session.ClientClaimKey.String != clientClaimKey {
		return ClaimResult{}, ErrInvalidClaimKey
	}
	session, err = s.queries.UpdateRongCloudPairingSessionClaim(ctx, db.UpdateRongCloudPairingSessionClaimParams{
		ID:             session.ID,
		IdempotencyKey: pgText(idempotencyKey),
	})
	if err != nil {
		s.logger.Error("rongcloud: pairing session claim write failed", "error", err)
		return ClaimResult{}, fmt.Errorf("rongcloud: claim pairing session: %w", err)
	}

	var candidateNodeIDs []pgtype.UUID
	if err := json.Unmarshal(session.CandidateNodeIds, &candidateNodeIDs); err != nil || len(candidateNodeIDs) == 0 {
		// The status flip already persisted; log so the half-claimed session
		// is diagnosable from server logs alone.
		s.logger.Error("rongcloud: claimed pairing session has no usable candidate nodes", "ticket_prefix", safeTicketPrefix(ticket), "error", err)
		return ClaimResult{Session: newPairingSessionView(session)}, fmt.Errorf("rongcloud: no candidate nodes in session")
	}

	node, err := s.queries.GetRongCloudNodeByID(ctx, candidateNodeIDs[0])
	if err != nil {
		s.logger.Error("rongcloud: candidate node lookup failed on claim", "error", err)
		return ClaimResult{Session: newPairingSessionView(session)}, fmt.Errorf("rongcloud: get candidate node: %w", err)
	}

	credID, credSecret, err := generateDeviceCredential()
	if err != nil {
		s.logger.Error("rongcloud: device credential generation failed", "error", err)
		return ClaimResult{Session: newPairingSessionView(session)}, fmt.Errorf("rongcloud: generate device credential: %w", err)
	}

	encSecret := ""
	if s.box != nil {
		sealed, err := s.box.Seal([]byte(credSecret))
		if err == nil {
			encSecret = base64.StdEncoding.EncodeToString(sealed)
		}
	}

	_, err = s.queries.CreateRongCloudDevice(ctx, db.CreateRongCloudDeviceParams{
		WorkspaceID:               node.WorkspaceID,
		OwnerUserID:               node.OwnerUserID,
		NodeID:                    node.ID,
		DeviceName:                node.NodeID,
		DeviceType:                node.AiType,
		CredentialID:              pgText(credID),
		CredentialSecretEncrypted: pgText(encSecret),
		Status:                    "active",
	})
	if err != nil {
		s.logger.Error("rongcloud: create device on claim failed", "error", err)
		return ClaimResult{Session: newPairingSessionView(session)}, fmt.Errorf("rongcloud: create device on claim: %w", err)
	}

	return ClaimResult{
		Session:            newPairingSessionView(session),
		DeviceCredentialID: credID,
		DeviceSecret:       credSecret,
		NodeID:             node.NodeID,
	}, nil
}

// =============== device-code pairing (browser authorization) ===============

// userCodeAlphabet is a 31-char alphabet with the visually ambiguous
// characters (I, O, 0, 1) removed, so a user can hand-type the code from a
// terminal into a browser without mistaking glyphs.
const userCodeAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"

const userCodeLen = 8

// generateUserCode returns an 8-char user code drawn from userCodeAlphabet
// with crypto/rand. Uniqueness is enforced by a partial unique index; the
// caller retries on collision.
func generateUserCode() (string, error) {
	out := make([]byte, userCodeLen)
	max := big.NewInt(int64(len(userCodeAlphabet)))
	for i := range out {
		// 256 % 31 leaves a small modular bias; acceptable for a
		// non-secret, collision-retried lookup key.
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("rongcloud: user code entropy: %w", err)
		}
		out[i] = userCodeAlphabet[idx.Int64()]
	}
	return string(out), nil
}

// DeviceSessionParams describes one terminal-initiated pairing session. It is
type DeviceSessionParams struct {
	WorkspaceID pgtype.UUID
	MachineID   string
	Hostname    string
	Platform    string
	Agents      []string
	ExpiresIn   time.Duration
}

// CreateDeviceSession starts a pairing session on behalf of a terminal
// client: the device reports its identity and discovered agent CLIs, and
// gets back a ticket (opaque to the user) plus an 8-char user code that the
// human types into the browser verify page. The workspace is resolved by
// the caller (single-install assumption), matching ClawCreatePairing.
func (s *PairingService) CreateDeviceSession(ctx context.Context, p DeviceSessionParams) (db.RongcloudPairingSession, error) {
	if s.queries == nil {
		return db.RongcloudPairingSession{}, errors.New("rongcloud: database not configured")
	}
	if p.ExpiresIn == 0 {
		p.ExpiresIn = 15 * time.Minute
	}
	// Dedupe and cap the reported agent list: it is echoed to the verify
	// page and bounded JSON, not free-form storage.
	seen := map[string]bool{}
	agents := make([]string, 0, len(p.Agents))
	for _, a := range p.Agents {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] || len(agents) >= 64 {
			continue
		}
		seen[a] = true
		agents = append(agents, a)
	}
	agentsJSON, err := json.Marshal(agents)
	if err != nil {
		return db.RongcloudPairingSession{}, fmt.Errorf("rongcloud: marshal reported agents: %w", err)
	}
	infoJSON, err := json.Marshal(map[string]string{
		"machine_id": p.MachineID,
		"hostname":   p.Hostname,
		"platform":   p.Platform,
	})
	if err != nil {
		return db.RongcloudPairingSession{}, fmt.Errorf("rongcloud: marshal device info: %w", err)
	}

	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		code, err := generateUserCode()
		if err != nil {
			return db.RongcloudPairingSession{}, err
		}
		session, err := s.queries.CreateRongCloudPairingDeviceSession(ctx, db.CreateRongCloudPairingDeviceSessionParams{
			WorkspaceID:    p.WorkspaceID,
			Ticket:         s.generateTicket(),
			UserCode:       pgText(code),
			ReportedAgents: agentsJSON,
			DeviceInfo:     infoJSON,
			ExpiresAt:      pgtype.Timestamptz{Time: time.Now().Add(p.ExpiresIn), Valid: true},
		})
		if err == nil {
			return session, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// user_code collision on the partial unique index; draw again.
			lastErr = err
			continue
		}
		s.logger.Error("rongcloud: create device pairing session failed", "error", err)
		return db.RongcloudPairingSession{}, fmt.Errorf("rongcloud: create device session: %w", err)
	}
	s.logger.Error("rongcloud: user_code collision retries exhausted", "error", lastErr)
	return db.RongcloudPairingSession{}, fmt.Errorf("rongcloud: create device session: %w", lastErr)
}

// GetSessionByUserCode resolves a session by its 8-char user code. The code
// is normalized (trim + upper) so hand-typed input is forgiving. An unknown
// code maps to ErrUnknownPairingTicket so handlers share the 404 semantics.
func (s *PairingService) GetSessionByUserCode(ctx context.Context, code string) (db.RongcloudPairingSession, error) {
	if s.queries == nil {
		return db.RongcloudPairingSession{}, errors.New("rongcloud: database not configured")
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	session, err := s.queries.GetRongCloudPairingSessionByUserCode(ctx, pgText(code))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.RongcloudPairingSession{}, ErrUnknownPairingTicket
		}
		s.logger.Error("rongcloud: user code lookup failed", "error", err)
		return db.RongcloudPairingSession{}, fmt.Errorf("rongcloud: get session by user code: %w", err)
	}
	return session, nil
}

// DevicePairLimiter caps how many device-code sessions one machine may open
// inside a window. It lives with the pairing domain rather than the HTTP
// layer so the window semantics are testable without a database. Single
// process scope is enough: the deployment runs one server binary, and a
// multi-replica rollout only softens the limit, it does not open a hole.
type DevicePairLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	max    int
	window time.Duration
}

// NewDevicePairLimiter builds a limiter allowing max sessions per window per
// key (machine_id).
func NewDevicePairLimiter(max int, window time.Duration) *DevicePairLimiter {
	return &DevicePairLimiter{hits: map[string][]time.Time{}, max: max, window: window}
}

// Allow records one attempt for key and reports whether it is within budget.
func (l *DevicePairLimiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	// Periodic pruning so an anonymous endpoint cannot grow the map forever.
	if len(l.hits) > 500 {
		for k, ts := range l.hits {
			if kept := pruneSince(ts, now, l.window); len(kept) == 0 {
				delete(l.hits, k)
			} else {
				l.hits[k] = kept
			}
		}
	}
	kept := pruneSince(l.hits[key], now, l.window)
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

func pruneSince(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	kept := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	return kept
}

// SessionEffectiveStatus applies read-time expiry. The database only flips
// pending→expired when someone attempts a claim, so a poller would otherwise
// see "pending" forever once ExpiresAt passed.
//
// claimed counts as expirable too: expires_at bounds the *authorization
// window*, not the device's credentials (those are issued at claim and live
// independently). A claimed session past its window means the browser link is
// dead — a leaked URL must not stay usable for the rest of the day.
func SessionEffectiveStatus(session db.RongcloudPairingSession, now time.Time) string {
	switch session.Status {
	case "pending", "claimed":
		if !session.ExpiresAt.Time.After(now) {
			return "expired"
		}
	}
	return session.Status
}
