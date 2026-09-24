package rongcloud

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var knownAgentCLIs = []string{
	"claude", "codex", "opencode", "codebuddy", "codearts", "deveco",
	"openclaw", "hermes", "pi", "omp", "cursor-agent", "kimi",
	"reasonix", "dsh", "kiro-cli", "agy", "qodercli", "qoderclicn",
	"traecli", "grok", "qwen", "qwenpaw", "mcode", "dim", "zeroclaw",
}

type DiscussionBridge struct {
	queries *db.Queries
	logger  *slog.Logger
	agents  map[string]string
}

func NewDiscussionBridge(queries *db.Queries, logger *slog.Logger) *DiscussionBridge {
	if logger == nil {
		logger = slog.Default()
	}
	b := &DiscussionBridge{
		queries: queries,
		logger:  logger,
		agents:  make(map[string]string),
	}
	b.RefreshAgents()
	return b
}

func (b *DiscussionBridge) RefreshAgents() {
	b.agents = make(map[string]string)
	for _, name := range knownAgentCLIs {
		if path, err := exec.LookPath(name); err == nil {
			b.agents[name] = path
			b.logger.Debug("agent CLI discovered", "name", name, "path", path)
		}
	}
	if len(b.agents) == 0 {
		b.logger.Warn("no agent CLIs found on PATH; server-managed turns will fail")
	}
}

func (b *DiscussionBridge) IsServerManaged(node db.RongcloudNode) bool {
	if !node.AiType.Valid {
		return false
	}
	_, ok := b.agents[node.AiType.String]
	return ok
}

func (b *DiscussionBridge) IsServerManagedByType(aiType string) bool {
	if aiType == "" {
		return false
	}
	_, ok := b.agents[aiType]
	return ok
}

func (b *DiscussionBridge) ExecuteTurn(ctx context.Context, chatroomID, nodeID pgtype.UUID, prompt, model string) (string, error) {
	node, err := b.queries.GetRongCloudNodeByID(ctx, nodeID)
	if err != nil {
		return "", fmt.Errorf("bridge: lookup node: %w", err)
	}
	execPath, ok := b.agents[node.AiType.String]
	if !ok {
		return "", fmt.Errorf("bridge: agent CLI %q not available", node.AiType.String)
	}

	execCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(execCtx, execPath)
	if model != "" {
		cmd.Args = append(cmd.Args, "--model", model)
	}
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		b.logger.Error("agent CLI failed",
			"node_id", nodeID.String(),
			"ai_type", node.AiType.String,
			"error", err,
			"stderr", stderr.String(),
		)
		return "", fmt.Errorf("bridge: agent %q failed: %w (stderr: %s)", node.AiType.String, err, stderr.String())
	}

	b.logger.Debug("agent CLI turn completed",
		"node_id", nodeID.String(),
		"ai_type", node.AiType.String,
		"stdout_len", stdout.Len(),
	)
	return stdout.String(), nil
}
