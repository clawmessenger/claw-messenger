// backfill-friends repairs the RongCloud friend edges for devices that were
// bound before the bind flow started writing them, and can re-sync node
// nicknames to RongCloud (-sync-names) for devices renamed before the rename
// flow started pushing to RongCloud.
//
// The web client's 好友列表 (contact list) reads the RongCloud friend list, so a
// device bound before BindAgents called /friend/add.json never showed up there.
// This command walks the visible agent nodes of a workspace (the same nodes the
// device list shows: the per-machine infrastructure node and the internal ops
// node are skipped) and friends each one to every target claw/IM user.
//
// It is safe to rerun: RongCloud answers 25460 for an existing edge, which is
// reported as "already" and counts as success. Nothing is written unless
// -commit is passed — the default is a dry run that only prints the plan.
//
// Typical use (from the server/ directory):
//
//	go run ./cmd/backfill-friends -env-file ../.env -commit -all-users
//	go run ./cmd/backfill-friends -env-file ../.env -commit -user 100123,100456
//	go run ./cmd/backfill-friends -env-file ../.env -commit -sync-names
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func main() {
	logger.Init()
	if err := run(); err != nil {
		slog.Error("friend backfill failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	userFlag := flag.String("user", "", "comma-separated claw_im_users.user_id values to backfill")
	allUsers := flag.Bool("all-users", false, "backfill every active claw_im_users account")
	workspaceFlag := flag.String("workspace", "", "workspace UUID to backfill (default: every workspace that has nodes)")
	commit := flag.Bool("commit", false, "write the friend edges (default is a dry run)")
	includeOps := flag.Bool("include-ops", false, "also friend the internal ops node (hidden from the friend list by default)")
	bidirectional := flag.Bool("both", false, "also write the reverse edge (agent -> owner)")
	syncNames := flag.Bool("sync-names", false, "push every node's database nickname to RongCloud (/user/refresh.json) instead of, or in addition to, friending")
	dsnFlag := flag.String("dsn", "", "postgres DSN (default: $DATABASE_URL, then $MULTICA_DATABASE_URL)")
	envFile := flag.String("env-file", "", "load KEY=VALUE environment variables from this file before reading configuration")
	flag.Parse()

	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			return fmt.Errorf("load env file: %w", err)
		}
	}

	if *userFlag == "" && !*allUsers && !*syncNames {
		return fmt.Errorf("specify -user <id[,id...]>, -all-users, or -sync-names to choose what to repair")
	}

	dsn := firstNonEmpty(*dsnFlag, os.Getenv("DATABASE_URL"), os.Getenv("MULTICA_DATABASE_URL"))
	if dsn == "" {
		return fmt.Errorf("no database DSN: pass -dsn or set DATABASE_URL")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	queries := db.New(pool)

	key, err := secretbox.LoadKey("MULTICA_RONGCLOUD_SECRET_KEY")
	if err != nil {
		return fmt.Errorf("load MULTICA_RONGCLOUD_SECRET_KEY: %w", err)
	}
	box, err := secretbox.New(key)
	if err != nil {
		return fmt.Errorf("init secretbox: %w", err)
	}
	client := rongcloud.NewRongCloudAPIClientForServices(box, queries, slog.Default())
	svc := rongcloud.NewNodeService(queries, client, box, slog.Default())

	workspaces, err := resolveWorkspaces(ctx, pool, *workspaceFlag)
	if err != nil {
		return err
	}
	users, err := resolveUsers(ctx, pool, *userFlag, *allUsers)
	if err != nil {
		return err
	}
	if len(workspaces) == 0 {
		return fmt.Errorf("no rongcloud workspace found to backfill")
	}
	if len(users) == 0 && !*syncNames {
		return fmt.Errorf("no claw user matched; nothing to do")
	}

	opts := rongcloud.BackfillFriendsOptions{
		IncludeOps:    *includeOps,
		Bidirectional: *bidirectional,
		DryRun:        !*commit,
	}
	if opts.DryRun {
		fmt.Println("DRY RUN — nothing will be written. Re-run with -commit to apply.")
	}
	fmt.Printf("workspaces=%d users=%d include_ops=%v bidirectional=%v sync_names=%v\n\n",
		len(workspaces), len(users), opts.IncludeOps, opts.Bidirectional, *syncNames)

	failed := 0

	if *syncNames {
		for _, ws := range workspaces {
			outcomes, err := svc.SyncNodeNicknames(ctx, ws, opts)
			if err != nil {
				return fmt.Errorf("sync nicknames workspace %s: %w", uuidString(ws), err)
			}
			for _, o := range outcomes {
				if o.Status == "failed" {
					failed++
				}
				line := fmt.Sprintf("%-9s node=%-46s name=%s", o.Status, o.NodeRC, o.Name)
				if o.Error != "" {
					line += " error=" + o.Error
				}
				fmt.Println(line)
			}
		}
		fmt.Println()
	}

	if len(users) > 0 {
		var added, already, planned int
		for _, ws := range workspaces {
			outcomes, err := svc.BackfillAgentFriends(ctx, ws, users, opts)
			if err != nil {
				return fmt.Errorf("backfill workspace %s: %w", uuidString(ws), err)
			}
			for _, o := range outcomes {
				switch o.Status {
				case "added":
					added++
				case "already":
					already++
				case "failed":
					failed++
				case "planned":
					planned++
				}
				line := fmt.Sprintf("%-7s owner=%s agent=%-10s node=%s", o.Status, o.Owner, o.Agent, o.NodeRC)
				if o.Error != "" {
					line += " error=" + o.Error
				}
				fmt.Println(line)
			}
		}
		fmt.Printf("friends summary: added=%d already=%d failed=%d planned=%d\n", added, already, failed, planned)
	}

	if failed > 0 {
		return fmt.Errorf("%d operation(s) failed", failed)
	}
	return nil
}

// resolveWorkspaces returns the workspace UUIDs to scan: the explicit flag when
// given, otherwise every workspace that currently has a rongcloud node.
func resolveWorkspaces(ctx context.Context, pool *pgxpool.Pool, explicit string) ([]pgtype.UUID, error) {
	if strings.TrimSpace(explicit) != "" {
		var wsID pgtype.UUID
		if err := pool.QueryRow(ctx, "SELECT $1::uuid", strings.TrimSpace(explicit)).Scan(&wsID); err != nil {
			return nil, fmt.Errorf("parse -workspace %q: %w", explicit, err)
		}
		return []pgtype.UUID{wsID}, nil
	}
	rows, err := pool.Query(ctx, "SELECT DISTINCT workspace_id FROM rongcloud_node ORDER BY 1")
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rows.Close()
	var out []pgtype.UUID
	for rows.Next() {
		var wsID pgtype.UUID
		if err := rows.Scan(&wsID); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		out = append(out, wsID)
	}
	return out, rows.Err()
}

// resolveUsers returns the claw user ids to repair: the comma-separated flag
// when given, otherwise every active account.
func resolveUsers(ctx context.Context, pool *pgxpool.Pool, userFlag string, all bool) ([]string, error) {
	if strings.TrimSpace(userFlag) != "" {
		var out []string
		for _, id := range strings.Split(userFlag, ",") {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
		return out, nil
	}
	if !all {
		return nil, nil
	}
	rows, err := pool.Query(ctx, "SELECT user_id FROM claw_im_users WHERE status = 'active' ORDER BY user_id")
	if err != nil {
		return nil, fmt.Errorf("list claw users: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan claw user: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// loadEnvFile sets KEY=VALUE pairs from path into the process environment
// without overwriting variables that are already set. It understands '#'
// comments, a leading "export ", and single/double quoted values.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
	return scanner.Err()
}

func uuidString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
