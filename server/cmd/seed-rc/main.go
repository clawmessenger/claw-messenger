// 一次性工具：为本地 Go 后端创建融云 channel installation。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

func main() {
	appKey := os.Getenv("RC_APP_KEY")
	appSecret := os.Getenv("RC_APP_SECRET")
	masterKey := os.Getenv("MULTICA_RONGCLOUD_SECRET_KEY")
	dsn := os.Getenv("DATABASE_URL")
	if appKey == "" || appSecret == "" || masterKey == "" || dsn == "" {
		fmt.Println("need RC_APP_KEY RC_APP_SECRET MULTICA_RONGCLOUD_SECRET_KEY DATABASE_URL")
		os.Exit(1)
	}

	key, err := secretbox.LoadKey("MULTICA_RONGCLOUD_SECRET_KEY")
	if err != nil {
		fmt.Println("load key:", err)
		os.Exit(1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		fmt.Println("new box:", err)
		os.Exit(1)
	}
	sealed, err := box.Seal([]byte(appSecret))
	if err != nil {
		fmt.Println("seal:", err)
		os.Exit(1)
	}
	encSecret := base64.StdEncoding.EncodeToString(sealed)

	cfg := map[string]string{
		"app_key":              appKey,
		"app_secret_encrypted": encSecret,
		"system_node_id":       "system",
	}
	cfgJSON, _ := json.Marshal(cfg)

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	var workspaceID string
	err = conn.QueryRow(ctx, "SELECT id::text FROM workspace ORDER BY created_at LIMIT 1").Scan(&workspaceID)
	if err != nil {
		fmt.Println("workspace:", err)
		os.Exit(1)
	}

	// 设备注册要求 workspace 有 owner/admin：node_service.go 的
	// resolveWorkspaceAttribution 查 member 表，空了就返回
	// "workspace ... has no manager to own the node"，而 /api/ai/register
	// 只把它翻成 500，日志里没有任何线索。走 POST /api/workspaces 建
	// workspace 会在同一事务里写 owner，但本工具是直接写库、绕过了那层，
	// 所以这里补一条幂等的 owner 关系，避免 seed 完的环境绑不了设备。
	var managers int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM member WHERE workspace_id = $1::uuid AND role IN ('owner', 'admin')`,
		workspaceID).Scan(&managers); err != nil {
		fmt.Println("member count:", err)
		os.Exit(1)
	}
	if managers == 0 {
		var ownerID string
		if err := conn.QueryRow(ctx, `SELECT id::text FROM "user" ORDER BY created_at LIMIT 1`).Scan(&ownerID); err != nil {
			fmt.Println(`workspace has no owner and "user" is empty — create a user first:`, err)
			os.Exit(1)
		}
		if _, err := conn.Exec(ctx, `
			INSERT INTO member (workspace_id, user_id, role)
			VALUES ($1::uuid, $2::uuid, 'owner')
			ON CONFLICT (workspace_id, user_id) DO UPDATE SET role = 'owner'`,
			workspaceID, ownerID); err != nil {
			fmt.Println("ensure workspace owner:", err)
			os.Exit(1)
		}
		fmt.Println("workspace owner ensured:", ownerID)
	}

	// channel_installation.agent_id 非空，指向 agent 表；确保存在一个可用 agent。
	var agentID string
	err = conn.QueryRow(ctx, "SELECT id::text FROM agent WHERE workspace_id = $1::uuid ORDER BY created_at LIMIT 1", workspaceID).Scan(&agentID)
	if err != nil {
		_, err = conn.Exec(ctx, `
			INSERT INTO agent (workspace_id, name, runtime_mode, owner_id)
			VALUES ($1::uuid, 'rongcloud-seed', 'local',
			        (SELECT id FROM "user" LIMIT 1))`, workspaceID)
		if err != nil {
			fmt.Println("create agent:", err)
			os.Exit(1)
		}
		err = conn.QueryRow(ctx, "SELECT id::text FROM agent WHERE workspace_id = $1::uuid ORDER BY created_at LIMIT 1", workspaceID).Scan(&agentID)
		if err != nil {
			fmt.Println("agent after insert:", err)
			os.Exit(1)
		}
	}

	tag, err := conn.Exec(ctx, `
		INSERT INTO channel_installation (workspace_id, agent_id, channel_type, config, status, installer_user_id, installed_at)
		VALUES ($1::uuid, $2::uuid, 'rongcloud', $3, 'active',
		        (SELECT id FROM "user" LIMIT 1), now())
		ON CONFLICT DO NOTHING`, workspaceID, agentID, cfgJSON)
	if err != nil {
		fmt.Println("insert:", err)
		os.Exit(1)
	}
	fmt.Println("installation created, rows:", tag.RowsAffected(), "workspace:", workspaceID)
}
