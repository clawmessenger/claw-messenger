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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
	Session            db.RongcloudPairingSession `json:"session"`
	DeviceCredentialID string                     `json:"device_credential_id"`
	DeviceSecret       string                     `json:"device_secret,omitempty"`
	NodeID             string                     `json:"node_id"`
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

func (s *PairingService) ClaimSession(ctx context.Context, ticket, clientClaimKey, idempotencyKey string) (ClaimResult, error) {
	if s.queries == nil {
		return ClaimResult{}, errors.New("rongcloud: database not configured")
	}
	session, err := s.queries.GetRongCloudPairingSessionByTicket(ctx, ticket)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ClaimResult{}, ErrUnknownPairingTicket
		}
		return ClaimResult{}, fmt.Errorf("rongcloud: get pairing session: %w", err)
	}
	if session.Status != "pending" {
		if session.IdempotencyKey.Valid && session.IdempotencyKey.String == idempotencyKey && session.Status == "claimed" {
			return ClaimResult{Session: session}, nil
		}
		return ClaimResult{}, ErrPairingSessionNotPending
	}
	if session.ExpiresAt.Time.Before(time.Now()) {
		_, _ = s.queries.UpdateRongCloudPairingSessionStatus(ctx, db.UpdateRongCloudPairingSessionStatusParams{
			ID:     session.ID,
			Status: "expired",
		})
		return ClaimResult{}, ErrPairingSessionExpired
	}
	if session.ClientClaimKey.Valid && session.ClientClaimKey.String != clientClaimKey {
		return ClaimResult{}, ErrInvalidClaimKey
	}
	session, err = s.queries.UpdateRongCloudPairingSessionStatus(ctx, db.UpdateRongCloudPairingSessionStatusParams{
		ID:     session.ID,
		Status: "claimed",
	})
	if err != nil {
		return ClaimResult{}, fmt.Errorf("rongcloud: claim pairing session: %w", err)
	}

	var candidateNodeIDs []pgtype.UUID
	if err := json.Unmarshal(session.CandidateNodeIds, &candidateNodeIDs); err != nil || len(candidateNodeIDs) == 0 {
		return ClaimResult{Session: session}, fmt.Errorf("rongcloud: no candidate nodes in session")
	}

	node, err := s.queries.GetRongCloudNodeByID(ctx, candidateNodeIDs[0])
	if err != nil {
		return ClaimResult{Session: session}, fmt.Errorf("rongcloud: get candidate node: %w", err)
	}

	credID, credSecret, err := generateDeviceCredential()
	if err != nil {
		return ClaimResult{Session: session}, fmt.Errorf("rongcloud: generate device credential: %w", err)
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
		return ClaimResult{Session: session}, fmt.Errorf("rongcloud: create device on claim: %w", err)
	}

	return ClaimResult{
		Session:            session,
		DeviceCredentialID: credID,
		DeviceSecret:       credSecret,
		NodeID:             node.NodeID,
	}, nil
}
