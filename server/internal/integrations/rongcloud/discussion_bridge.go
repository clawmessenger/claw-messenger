package rongcloud

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
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

// The web stores a model as a `provider/model` route. Most agent CLIs take
// that verbatim, but two split it:
//
//	codex  — `-m` takes a bare slug and the provider comes from config, so the
//	         route's provider rides on `-c model_provider=<id>`. Passing the
//	         full route fails with model_not_found.
//	hermes — `-m <slug> --provider <id>`.
//
// Keep in sync with buildModelArgs in packages/xiachat-cli/src/agents.ts.
var modelRouteRE = regexp.MustCompile(
	`^([A-Za-z0-9][A-Za-z0-9._-]{0,127})/([A-Za-z0-9][A-Za-z0-9._:-]{0,127})$`,
)

// splitModelRoute splits a `provider/model` route. ok=false means the value is
// not a route, so legacy bare model ids stay bare.
func splitModelRoute(route string) (provider, model string, ok bool) {
	match := modelRouteRE.FindStringSubmatch(route)
	if match == nil {
		return "", "", false
	}
	return match[1], match[2], true
}

// modelArgs renders the model-selection flags for one agent.
func modelArgs(name, model string) []string {
	if model == "" {
		return nil
	}
	provider, slug, ok := splitModelRoute(model)
	switch name {
	case "codex":
		if ok {
			return []string{"-c", "model_provider=" + provider, "-m", slug}
		}
		return []string{"-m", model}
	case "hermes":
		if ok {
			return []string{"-m", slug, "--provider", provider}
		}
		return []string{"-m", model}
	default:
		return []string{"--model", model}
	}
}

// Agents known to reject or hang on stdin-mode invocation get an argv
// strategy: the prompt rides as a CLI argument instead of stdin.
//
//	claude:   print mode (-p), non-interactive
//	codex:    `exec` subcommand (stdin-mode invocation is rejected)
//	opencode: `run` subcommand (stdin-mode invocation hangs)
//	openclaw: `agent --local -m` one-shot message (stdin-mode unsupported)
//	hermes:   `-z` one-shot prompt (stdin-mode unsupported)
//
// Keep in sync with buildAgentArgs in packages/xiachat-cli/src/agents.ts.
func agentArgvArgs(name, prompt, model string) []string {
	switch name {
	case "claude":
		return append([]string{"-p", prompt}, modelArgs(name, model)...)
	case "codex":
		return append([]string{"exec", prompt}, modelArgs(name, model)...)
	case "opencode":
		return append([]string{"run", prompt}, modelArgs(name, model)...)
	case "openclaw":
		return append([]string{"agent", "--local", "-m", prompt}, modelArgs(name, model)...)
	case "hermes":
		return append([]string{"-z", prompt}, modelArgs(name, model)...)
	}
	return nil
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
		return invocationSpec{argv: modelArgs(name, model), stdinPrompt: true}
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
