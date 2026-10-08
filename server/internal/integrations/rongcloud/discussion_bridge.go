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

// Agents known to reject or hang on stdin-mode invocation get an argv
// strategy: the prompt rides as a CLI argument instead of stdin.
//   claude:   print mode (-p), non-interactive
//   codex:    `exec` subcommand (stdin-mode invocation is rejected)
//   opencode: `run` subcommand (stdin-mode invocation hangs)
//   openclaw: `agent --local -m` one-shot message (stdin-mode unsupported)
//   hermes:   `-z` one-shot prompt (stdin-mode unsupported)
// Keep in sync with buildAgentArgs in packages/xiachat-cli/src/agents.ts.
func agentArgvArgs(name, prompt, model string) []string {
	var args []string
	switch name {
	case "claude":
		args = append(args, "-p", prompt)
		if model != "" {
			args = append(args, "--model", model)
		}
	case "codex":
		args = append(args, "exec", prompt)
		if model != "" {
			args = append(args, "-m", model)
		}
	case "opencode":
		args = append(args, "run", prompt)
		if model != "" {
			args = append(args, "--model", model)
		}
	case "openclaw":
		args = append(args, "agent", "--local", "-m", prompt)
		if model != "" {
			args = append(args, "--model", model)
		}
	case "hermes":
		args = append(args, "-z", prompt)
		if model != "" {
			args = append(args, "-m", model)
		}
	default:
		return nil
	}
	return args
}

// Windows CreateProcess caps the whole command line near 32k chars, so
// very long prompts cannot ride as argv — past this cap every agent falls
// back to stdin mode regardless of strategy.
const argvPromptMaxChars = 8000

type invocationSpec struct {
	// argv holds the full agent argument list.
	argv []string
	// stdinPrompt reports whether the prompt must ride via Stdin (unknown
	// agent, or a prompt past the argv length cap).
	stdinPrompt bool
}

// buildInvocation mirrors buildAgentArgs in packages/xiachat-cli: agents
// with a known strategy take the prompt as argv; unknown agents (or a
// prompt past the argv length cap) keep the stdin invocation with the
// model flag as the only argument.
func buildInvocation(name, prompt, model string) invocationSpec {
	argv := agentArgvArgs(name, prompt, model)
	if argv != nil && len(prompt) <= argvPromptMaxChars {
		return invocationSpec{argv: argv}
	}
	if model != "" {
		return invocationSpec{argv: []string{"--model", model}, stdinPrompt: true}
	}
	return invocationSpec{stdinPrompt: true}
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

	spec := buildInvocation(node.AiType.String, prompt, model)
	cmd := exec.CommandContext(execCtx, execPath, spec.argv...)
	if spec.stdinPrompt {
		cmd.Stdin = strings.NewReader(prompt)
	}

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
