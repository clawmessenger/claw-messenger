// retire-legacy-nodes removes the per-machine node groups that were registered
// under the pre-v0.2.3 machine id format, so the device can pair again and come
// back with a short, readable node id.
//
// Why re-pairing is the only route: a RongCloud user id is the account's
// primary key, so "rc_node_m_e99ad19f108d9dad78ad07e3_codex" cannot be renamed
// into a shorter one — the account has to be replaced. The alternative, keeping
// the account and only shrinking the local row, would leave the device deriving
// the old id on its next register and recreate the very group we removed.
//
// The CLI used to mint "clawmessenger-<uuid>" (50 chars). RongCloud caps a user
// id at 64 and the server reserves 33 of them for "rc_node_" + "_" + the
// longest agent name, leaving 31 for the machine segment; anything longer was
// sha256-ed into an opaque "m_<24 hex>" key. v0.2.3 mints 10 base36 chars
// instead, so a device that pairs again gets rc_node_7che3nsv0n_codex.
//
// Nothing is written unless -commit is passed. RongCloud account deactivation
// is irreversible and therefore needs a second, separate opt-in
// (-purge-accounts).
//
// Typical use (from the server/ directory):
//
//	go run ./cmd/retire-legacy-nodes -env-file ../.env
//	go run ./cmd/retire-legacy-nodes -env-file ../.env -commit
//	go run ./cmd/retire-legacy-nodes -env-file ../.env -commit -purge-accounts
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// rongCloudBatchSize is the largest number of ids RongCloud accepts in one
// deactivate call.
const rongCloudBatchSize = 100

func main() {
	logger.Init()
	if err := run(); err != nil {
		slog.Error("retire legacy nodes failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	workspaceFlag := flag.String("workspace", "", "workspace UUID to scan (default: every workspace that has nodes)")
	maxMachineID := flag.Int("max-machine-id", rongcloud.MachineIdentityBudget,
		"retire nodes whose machine id is longer than this; pass 11 to also sweep ids outside the 8-11 character format")
	commit := flag.Bool("commit", false, "delete the legacy node rows (default is a dry run)")
	keepFriends := flag.Bool("keep-friends", false, "do not remove the owners' RongCloud friend edges to the retired nodes")
	purgeAccounts := flag.Bool("purge-accounts", false,
		"also deactivate the retired nodes' RongCloud accounts; irreversible, deletes their message history")
	ownerFlag := flag.String("owner", "", "comma-separated claw_im_users.user_id values whose friend edges to clean (default: every active account)")
	dsnFlag := flag.String("dsn", "", "postgres DSN (default: $DATABASE_URL, then $MULTICA_DATABASE_URL)")
	envFile := flag.String("env-file", "", "load KEY=VALUE environment variables from this file before reading configuration")
	flag.Parse()

	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			return fmt.Errorf("load env file: %w", err)
		}
	}
	if *purgeAccounts && !*commit {
		return fmt.Errorf("-purge-accounts requires -commit")
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

	workspaces, err := resolveWorkspaces(ctx, pool, *workspaceFlag)
	if err != nil {
		return err
	}
	if len(workspaces) == 0 {
		return fmt.Errorf("no rongcloud workspace found")
	}

	var nodes []db.RongcloudNode
	for _, ws := range workspaces {
		wsNodes, err := queries.ListRongCloudNodesByWorkspace(ctx, ws)
		if err != nil {
			return fmt.Errorf("list nodes of workspace %s: %w", uuidString(ws), err)
		}
		nodes = append(nodes, wsNodes...)
	}

	groups := rongcloud.PlanLegacyNodeRetirement(nodes, *maxMachineID)
	fmt.Printf("scanned %d node(s) across %d workspace(s); %d machine group(s) are legacy\n\n",
		len(nodes), len(workspaces), len(groups))
	if len(groups) == 0 {
		fmt.Println("nothing to retire — every node already has a short machine id.")
		return nil
	}

	retired := make([]db.RongcloudNode, 0, len(groups))
	for _, g := range groups {
		machine := g.MachineID
		if machine == "" {
			machine = "(no machine id recorded)"
		}
		fmt.Printf("machine %s\n", machine)
		for _, n := range g.Nodes {
			retired = append(retired, n)
			fmt.Printf("    %-52s ai_type=%-9s node_id=%s\n",
				n.RongcloudUserID, n.AiType.String, n.NodeID)
		}
	}
	fmt.Println()

	if !*commit {
		fmt.Println("DRY RUN — nothing was written. Re-run with -commit to retire these nodes.")
		fmt.Println("After that, on each affected device: stop it, delete ~/.clawmessenger/machine_id, and run `clawmessenger pair` again.")
		return nil
	}

	failed := 0

	if !*keepFriends {
		owners, err := resolveOwners(ctx, pool, *ownerFlag)
		if err != nil {
			return err
		}
		targets := rcUserIDs(retired)
		for _, owner := range owners {
			edges, err := client.FriendIDs(ctx, owner)
			if err != nil {
				slog.Warn("read friend list failed", "owner", owner, "error", err)
				failed++
				continue
			}
			var doomed []string
			for _, edge := range edges {
				for _, t := range targets {
					if edge == t {
						doomed = append(doomed, edge)
						break
					}
				}
			}
			if len(doomed) == 0 {
				continue
			}
			if err := client.RemoveFriends(ctx, owner, doomed...); err != nil {
				slog.Warn("remove friend edges failed", "owner", owner, "targets", len(doomed), "error", err)
				failed++
				continue
			}
			fmt.Printf("friends   owner=%-14s removed=%d\n", owner, len(doomed))
		}
	}

	if *purgeAccounts {
		targets := rcUserIDs(retired)
		for start := 0; start < len(targets); start += rongCloudBatchSize {
			end := start + rongCloudBatchSize
			if end > len(targets) {
				end = len(targets)
			}
			batch := targets[start:end]
			if err := client.DeactivateUser(ctx, batch...); err != nil {
				slog.Warn("deactivate accounts failed", "count", len(batch), "error", err)
				failed++
				continue
			}
			fmt.Printf("accounts  deactivated=%d\n", len(batch))
		}
	}

	for _, n := range retired {
		if err := deleteNodeRows(ctx, pool, queries, n); err != nil {
			slog.Warn("delete node rows failed", "node", n.RongcloudUserID, "error", err)
			failed++
			continue
		}
		fmt.Printf("rows      deleted node=%s\n", n.RongcloudUserID)
	}

	fmt.Println()
	if failed > 0 {
		return fmt.Errorf("%d operation(s) failed", failed)
	}
	fmt.Println("done. Re-pair each affected device to register it under a short machine id:")
	fmt.Println("  1. stop the device process")
	fmt.Println("  2. delete ~/.clawmessenger/machine_id (and ~/.clawmessenger/processed-uids-*.json)")
	fmt.Println("  3. run `clawmessenger pair --ticket <pt_...>` again")
	return nil
}

// deleteNodeRows removes everything the database holds for a retired node: its
// device credentials, model catalog, chatroom memberships, system config, the
// node row, and the RongCloud user row that owns the account.
func deleteNodeRows(ctx context.Context, pool *pgxpool.Pool, queries *db.Queries, n db.RongcloudNode) error {
	if device, err := queries.GetRongCloudDeviceByNodeID(ctx, n.ID); err == nil {
		if err := queries.DeleteRongCloudDevice(ctx, device.ID); err != nil {
			return fmt.Errorf("delete device: %w", err)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lookup device: %w", err)
	}

	catalogs, err := queries.ListRongCloudNodeModelCatalogsByNode(ctx, n.ID)
	if err != nil {
		return fmt.Errorf("list model catalogs: %w", err)
	}
	for _, c := range catalogs {
		if err := queries.DeleteRongCloudNodeModelCatalog(ctx, c.ID); err != nil {
			return fmt.Errorf("delete model catalog: %w", err)
		}
	}

	// These two tables hang off node_id, which no generated delete covers, so
	// they are cleared directly. No foreign keys exist on either table.
	for _, table := range []string{"rongcloud_chatroom_member", "rongcloud_system_config"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+table+" WHERE node_id = $1", n.ID); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}

	if err := queries.DeleteRongCloudNode(ctx, n.ID); err != nil {
		return fmt.Errorf("delete node: %w", err)
	}
	if user, err := queries.GetRongCloudUserByRongCloudID(ctx, n.RongcloudUserID); err == nil {
		if err := queries.DeleteRongCloudUser(ctx, user.ID); err != nil {
			return fmt.Errorf("delete user: %w", err)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lookup user: %w", err)
	}
	return nil
}

func rcUserIDs(nodes []db.RongcloudNode) []string {
	out := make([]string, 0, len(nodes))
	seen := map[string]bool{}
	for _, n := range nodes {
		if n.RongcloudUserID == "" || seen[n.RongcloudUserID] {
			continue
		}
		seen[n.RongcloudUserID] = true
		out = append(out, n.RongcloudUserID)
	}
	return out
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

// resolveOwners returns the claw accounts whose friend lists should be cleaned:
// the comma-separated flag when given, otherwise every active account.
func resolveOwners(ctx context.Context, pool *pgxpool.Pool, ownerFlag string) ([]string, error) {
	if strings.TrimSpace(ownerFlag) != "" {
		var out []string
		for _, id := range strings.Split(ownerFlag, ",") {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
		return out, nil
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
