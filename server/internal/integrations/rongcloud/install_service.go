package rongcloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type InstallService struct {
	queries *db.Queries
	box     *secretbox.Box
	logger  *slog.Logger
}

func NewInstallService(queries *db.Queries, box *secretbox.Box, logger *slog.Logger) *InstallService {
	if logger == nil {
		logger = slog.Default()
	}
	return &InstallService{queries: queries, box: box, logger: logger}
}

func (s *InstallService) GetAppKey(ctx context.Context) string {
	if s.queries == nil {
		return ""
	}
	insts, err := s.queries.ListActiveChannelInstallations(ctx, string(TypeRongCloud))
	if err != nil || len(insts) == 0 {
		return ""
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(insts[0].Config, &cfg); err != nil {
		return ""
	}
	appKey, _ := cfg["app_key"].(string)
	return appKey
}

func (s *InstallService) GetInstallation(ctx context.Context, instID pgtype.UUID) (channel.Config, error) {
	inst, err := s.queries.GetChannelInstallation(ctx, db.GetChannelInstallationParams{
		ID:          instID,
		ChannelType: string(TypeRongCloud),
	})
	if err != nil {
		return channel.Config{}, err
	}
	return channel.Config{
		Type: TypeRongCloud,
		Raw:  inst.Config,
		ID:   inst.ID,
	}, nil
}

func (s *InstallService) CreateInstallation(ctx context.Context, workspaceID, agentID pgtype.UUID, appKey, appSecret, systemNodeID string, installerUserID pgtype.UUID) (channel.Config, error) {
	encSecret := ""
	if s.box != nil && appSecret != "" {
		sealed, err := s.box.Seal([]byte(appSecret))
		if err != nil {
			return channel.Config{}, err
		}
		encSecret = base64.StdEncoding.EncodeToString(sealed)
	}
	configJSON, err := json.Marshal(installConfig{
		AppKey:             appKey,
		AppSecretEncrypted: encSecret,
		SystemNodeID:       systemNodeID,
	})
	if err != nil {
		return channel.Config{}, err
	}
	inst, err := s.queries.UpsertChannelInstallation(ctx, db.UpsertChannelInstallationParams{
		WorkspaceID:    workspaceID,
		AgentID:        agentID,
		ChannelType:    string(TypeRongCloud),
		Config:         configJSON,
		InstallerUserID: installerUserID,
	})
	if err != nil {
		return channel.Config{}, err
	}
	return channel.Config{
		Type: TypeRongCloud,
		Raw:  configJSON,
		ID:   inst.ID,
	}, nil
}

func (s *InstallService) RevokeInstallation(ctx context.Context, instID pgtype.UUID) error {
	return s.queries.SetChannelInstallationStatus(ctx, db.SetChannelInstallationStatusParams{
		ID:     instID,
		Status: "revoked",
	})
}

func (s *InstallService) UpsertSystemConfig(ctx context.Context, workspaceID pgtype.UUID, configKey string, nodeID pgtype.UUID, config json.RawMessage) (db.RongcloudSystemConfig, error) {
	return s.queries.UpsertRongCloudSystemConfig(ctx, db.UpsertRongCloudSystemConfigParams{
		WorkspaceID:  workspaceID,
		ConfigKey:     configKey,
		NodeID:        nodeID,
		Config:        config,
		ConfigVersion: 1,
	})
}

func (s *InstallService) GetSystemConfig(ctx context.Context, workspaceID pgtype.UUID, configKey string) (db.RongcloudSystemConfig, error) {
	return s.queries.GetRongCloudSystemConfig(ctx, db.GetRongCloudSystemConfigParams{
		WorkspaceID: workspaceID,
		ConfigKey:   configKey,
	})
}

func (s *InstallService) ListSystemConfigs(ctx context.Context, workspaceID pgtype.UUID) ([]db.RongcloudSystemConfig, error) {
	return s.queries.ListRongCloudSystemConfigs(ctx, workspaceID)
}

func (s *InstallService) DeleteSystemConfig(ctx context.Context, workspaceID pgtype.UUID, configKey string) error {
	return s.queries.DeleteRongCloudSystemConfig(ctx, db.DeleteRongCloudSystemConfigParams{
		WorkspaceID: workspaceID,
		ConfigKey:   configKey,
	})
}

// NewRongCloudAPIClientForServices creates an API client for service-layer
// use. The client starts with empty credentials; the loader below fills them
// lazily from the encrypted installation config on first request. Returns a
// client with no loader if the logger is nil (caller should guard).
func NewRongCloudAPIClientForServices(box *secretbox.Box, queries *db.Queries, logger *slog.Logger) *rongcloudAPIClient {
	c := newRongCloudAPIClient("", "", "", nil, logger)
	if queries != nil && box != nil {
		c.loadCreds = func(ctx context.Context) (string, string, error) {
			insts, err := queries.ListActiveChannelInstallations(ctx, string(TypeRongCloud))
			if err != nil {
				return "", "", err
			}
			if len(insts) == 0 {
				return "", "", fmt.Errorf("no active rongcloud channel installation found")
			}
			cfg, err := decodeCredentials(insts[0].Config, box.Open)
			if err != nil {
				return "", "", err
			}
			return cfg.AppKey, cfg.AppSecret, nil
		}
	}
	return c
}
