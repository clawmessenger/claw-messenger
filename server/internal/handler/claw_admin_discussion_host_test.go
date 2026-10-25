package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 管理端「讨论主持 AI → 供应商 + 模型」（task 2026-10-25/task-001）。
//
// 真库集成测试，连不上就 Skip（复用 quotaTestPool 的同一套约定）。
//
// 每个用例跑在**一个事务**里并用 Rollback 收尾，原因是这组接口直接读写
// rongcloud_system_config 的 system_host 行 —— 开发库里那一行是运维的真实
// 配置，测试既不能覆盖它，也不能为了造干净状态把它 DELETE 掉。

const discussionHostTestCaps = `[
	"discussion_host",
	"discussion_roundtable",
	"discussion_model_routing",
	"discussion_role_recommendation"
]`

// catalogProviders 是一份「目录里含 openai/gpt-5.6-sol」的设备应答。
const catalogProviders = `[
	{"id":"openai","name":"OpenAI","models":[
		{"id":"gpt-5.6-sol","name":"GPT 5.6 Sol"},
		{"id":"gpt-5-mini","name":"GPT-5 mini"}
	]}
]`

// otherCatalogProviders 是一份不含目标模型的目录（用于「目录可取但不命中」）。
const otherCatalogProviders = `[
	{"id":"anthropic","name":"Anthropic","models":[{"id":"claude-x","name":"Claude X"}]}
]`

// switchableCatalogProviders 是「同一台设备上有两个供应商」的目录：用来验证
// 「只改 provider」与「只改 model」都必须重新核验（同一模型 id 在两个供应商下
// 都存在，这样两种改动都是合法保存，能把「跳过核验」的判据钉死在两个字段上）。
const switchableCatalogProviders = `[
	{"id":"openai","name":"OpenAI","models":[
		{"id":"gpt-5.6-sol","name":"GPT 5.6 Sol"},
		{"id":"gpt-5-mini","name":"GPT-5 mini"}
	]},
	{"id":"anthropic","name":"Anthropic","models":[
		{"id":"gpt-5.6-sol","name":"GPT 5.6 Sol"},
		{"id":"claude-x","name":"Claude X"}
	]}
]`

type discussionHostFixture struct {
	h             *Handler
	tx            pgx.Tx
	nodeUUID      string
	nodeRc        string
	token         string
	readonlyToken string
	// adminID / readonlyAdminID 用于精确定位本用例写的审计行（审计表是全库共享的）。
	adminID         string
	readonlyAdminID string
	// sendCount 记录向设备发了多少次目录/推荐请求（M2「模型没改就别再问一遍」
	// 的观测口）。
	sendCount *int32
}

// setupDiscussionHostFixture 备一个事务内的干净现场：角色/管理员、合成 workspace、
// 能力位齐备的主持节点。providersJSON 非空时装一个假 broker，它会在 send 时
// 立刻把一份目录回包塞回 broker（模拟设备端应答，无网络、无重试）。
func setupDiscussionHostFixture(t *testing.T, providersJSON string) *discussionHostFixture {
	t.Helper()
	pool := quotaTestPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	f := &discussionHostFixture{
		tx:        tx,
		nodeUUID:  uuid.NewString(),
		nodeRc:    "rc-host-" + uuid.NewString(),
		sendCount: new(int32),
	}

	// 角色 + 管理员：requireClawAdmin → loadClawAdmin 要联表取 permissions。
	roleID := "role-" + uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO claw_admin_roles (role_id, name, role_name, permissions)
		VALUES ($1, $1, 'host test', '{system:read,system:write}')`, roleID); err != nil {
		t.Fatalf("seed admin role: %v", err)
	}
	adminID := "adm-" + uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO claw_admin_users (admin_id, username, nickname, password_hash, role_id)
		VALUES ($1, $2, 'host test', 'x', $3)`, adminID, adminID, roleID); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	token, err := issueClawAdminJWT(adminID, "access", time.Hour)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	f.token = token
	f.adminID = adminID

	// 只读管理员：目录接口是新开的有副作用通道，必须证明它只对 system:write 开放。
	readonlyRoleID := "role-ro-" + uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO claw_admin_roles (role_id, name, role_name, permissions)
		VALUES ($1, $1, 'host test readonly', '{system:read}')`, readonlyRoleID); err != nil {
		t.Fatalf("seed readonly role: %v", err)
	}
	readonlyID := "adm-ro-" + uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO claw_admin_users (admin_id, username, nickname, password_hash, role_id)
		VALUES ($1, $2, 'host test ro', 'x', $3)`, readonlyID, readonlyID, readonlyRoleID); err != nil {
		t.Fatalf("seed readonly admin: %v", err)
	}
	f.readonlyToken, err = issueClawAdminJWT(readonlyID, "access", time.Hour)
	if err != nil {
		t.Fatalf("issue readonly jwt: %v", err)
	}
	f.readonlyAdminID = readonlyID

	ws := uuid.NewString()
	// 清掉既有 system_host 行（**只在事务内**，回滚即还原），否则
	// clawAdminDiscussionHostWorkspaceID 会落到运维真实的 workspace 上。
	if _, err := tx.Exec(ctx,
		`DELETE FROM rongcloud_system_config WHERE config_key = $1`,
		clawAdminDiscussionHostKey); err != nil {
		t.Fatalf("clear system_host: %v", err)
	}
	// 一条合成 workspace 的 system_config 占位行：workspace 回落取「最近更新
	// 的那一行」，于是本用例的 system_host 落在合成 workspace 里。
	if _, err := tx.Exec(ctx, `
		INSERT INTO rongcloud_system_config (workspace_id, config_key, config)
		VALUES ($1, $2, '{}'::jsonb)`,
		ws, "zz_discussion_host_test_"+uuid.NewString()); err != nil {
		t.Fatalf("seed workspace marker: %v", err)
	}
	f.nodeUUID, f.nodeRc = f.seedNode(t, ws, "codex", discussionHostTestCaps)

	f.h = &Handler{DB: tx, Queries: db.New(tx)}
	if providersJSON != "" {
		broker := rongcloud.NewNodeRequestBroker(nil)
		broker.BindSender(func(sendCtx context.Context, toUserID, content string) error {
			atomic.AddInt32(f.sendCount, 1)
			var request struct {
				RequestID string `json:"requestId"`
			}
			if err := json.Unmarshal([]byte(content), &request); err != nil {
				return err
			}
			response := fmt.Sprintf(
				`{"msg_type":"discussion_model_catalog_response","protocolVersion":2,`+
					`"requestId":%q,"defaultModel":null,"providers":%s,"timestamp":%d}`,
				request.RequestID, providersJSON, time.Now().UnixMilli())
			broker.AcceptCatalogResponse(toUserID, response)
			return nil
		})
		f.h.RongCloudNodeRequests = broker
	}
	return f
}

// seedNode 在给定 workspace 里插一台节点，返回 (uuid 文本, 融云 user id)。
func (f *discussionHostFixture) seedNode(t *testing.T, ws, aiType, capabilities string) (string, string) {
	t.Helper()
	nodeUUID := uuid.NewString()
	nodeRc := "rc-node-" + nodeUUID
	if _, err := f.tx.Exec(context.Background(), `
		INSERT INTO rongcloud_node
			(id, workspace_id, owner_user_id, rongcloud_user_id, node_id, ai_type, capabilities)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
		nodeUUID, ws, uuid.NewString(), nodeRc, "node-"+nodeUUID, aiType, capabilities); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return nodeUUID, nodeRc
}

func (f *discussionHostFixture) put(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/system/discussion-host", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.token)
	rr := httptest.NewRecorder()
	f.h.ClawAdminDiscussionHostPut(rr, req)
	return rr
}

// catalog 调一次目录接口，token 与 node_id 由调用方给（空 node_id 表示不带这个参数）。
func (f *discussionHostFixture) catalog(t *testing.T, token, nodeID string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/admin/system/discussion-host/model-catalog"
	if nodeID != "" {
		path += "?node_id=" + nodeID
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	f.h.ClawAdminDiscussionHostModelCatalog(rr, req)
	return rr
}

// putBody 造一份完整的 PUT payload；model 相关三段的取值交给调用方。
func (f *discussionHostFixture) putBody(version int, modelProvider, model *string) string {
	body := map[string]interface{}{
		"expected_config_version":    version,
		"enabled":                    true,
		"node_id":                    f.nodeUUID,
		"display_name":               "虾说讨论主持人",
		"avatar":                     "https://example.test/host.png",
		"role_recommendation_prompt": "推荐互补角色",
		"discussion_host_prompt":     "你是中立主持人",
		"timeout_seconds":            60,
	}
	if modelProvider != nil {
		body["model_provider"] = *modelProvider
	}
	if model != nil {
		body["model"] = *model
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

func cloneString(value string) *string { return &value }

// auditRow 是一条审计记录里本组用例关心的部分。
type auditRow struct {
	action     string
	resourceID string
	detail     map[string]interface{}
	nodeID     string
	errorCode  string
	reason     string
}

// auditRows 读**本事务内、该管理员**写的审计行（审计表是全库共享的，不能按
// 全表计数断言）。
func (f *discussionHostFixture) auditRows(t *testing.T, adminID string) []auditRow {
	t.Helper()
	rows, err := f.tx.Query(context.Background(), `
		SELECT action, resource_id, COALESCE(detail, '{}'::jsonb)
		FROM claw_admin_audit_logs
		WHERE admin_id = $1
		ORDER BY created_at, id`, adminID)
	if err != nil {
		t.Fatalf("read audit logs: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var row auditRow
		var raw []byte
		if err := rows.Scan(&row.action, &row.resourceID, &raw); err != nil {
			t.Fatalf("scan audit log: %v", err)
		}
		_ = json.Unmarshal(raw, &row.detail)
		row.nodeID, _ = row.detail["node_id"].(string)
		row.errorCode, _ = row.detail["error_code"].(string)
		row.reason, _ = row.detail["reason"].(string)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit logs: %v", err)
	}
	return out
}

// storedConfig 直接读表里的 config jsonb，用于断言「写了什么 / 没写什么」。
func (f *discussionHostFixture) storedConfig(t *testing.T) map[string]interface{} {
	t.Helper()
	var raw []byte
	if err := f.tx.QueryRow(context.Background(), `
		SELECT COALESCE(config, '{}'::jsonb) FROM rongcloud_system_config
		WHERE config_key = $1`, clawAdminDiscussionHostKey).Scan(&raw); err != nil {
		t.Fatalf("read stored config: %v", err)
	}
	var stored map[string]interface{}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("stored config is not JSON: %v (%s)", err, raw)
	}
	return stored
}

func dataOf(t *testing.T, rr *httptest.ResponseRecorder) map[string]interface{} {	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rr.Body.String())
	}
	data, _ := body["data"].(map[string]interface{})
	return data
}

// TestClawAdminDiscussionHostPutModelRoundTrip：带模型保存后，jsonb 里两个键都在、
// GET 读回一致、运行时视图拼出 route。节点离线（未装 broker）时**允许保存**并
// 标记 model_verified=false。
func TestClawAdminDiscussionHostPutModelRoundTrip(t *testing.T) {
	f := setupDiscussionHostFixture(t, "")

	rr := f.put(t, f.putBody(0, cloneString("openai"), cloneString("gpt-5.6-sol")))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", rr.Code, rr.Body.String())
	}
	data := dataOf(t, rr)
	if data["model_provider"] != "openai" || data["model"] != "gpt-5.6-sol" {
		t.Fatalf("response model = %v/%v, want openai/gpt-5.6-sol", data["model_provider"], data["model"])
	}
	if verified, ok := data["model_verified"].(bool); !ok || verified {
		t.Fatalf("model_verified = %v, want false（节点离线/目录不可取时必须放行且标记未核验）", data["model_verified"])
	}

	stored := f.storedConfig(t)
	if stored["model_provider"] != "openai" || stored["model"] != "gpt-5.6-sol" {
		t.Fatalf("stored config = %#v, want model_provider=openai model=gpt-5.6-sol", stored)
	}

	// GET 读回一致（applyClawAdminDiscussionHostStored 的类型断言必须认得新键）。
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/system/discussion-host", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	f.h.ClawAdminDiscussionHostGet(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET = %d (%s)", rr.Code, rr.Body.String())
	}
	data = dataOf(t, rr)
	if data["model_provider"] != "openai" || data["model"] != "gpt-5.6-sol" {
		t.Fatalf("GET model = %v/%v, want openai/gpt-5.6-sol", data["model_provider"], data["model"])
	}
	if _, present := data["model_verified"]; present {
		t.Fatalf("GET must not report model_verified: %v", data)
	}

	// 运行时视图：两段拼成一条 route，三处 host 快照与设备端 CLI 都按它工作。
	cfg, found, err := f.h.ClawDiscussionSystemHostConfig(context.Background())
	if err != nil || !found {
		t.Fatalf("ClawDiscussionSystemHostConfig = (%v, %v)", found, err)
	}
	if cfg.Model != "openai/gpt-5.6-sol" {
		t.Fatalf("runtime Model = %q, want openai/gpt-5.6-sol", cfg.Model)
	}
}

// TestClawAdminDiscussionHostPutOmitsModelKeysWhenUnset：不选模型时不写键
// （不是写空串），且运行时视图为空 —— 这是「行为与改造前完全一致」的落点。
func TestClawAdminDiscussionHostPutOmitsModelKeysWhenUnset(t *testing.T) {
	f := setupDiscussionHostFixture(t, "")

	rr := f.put(t, f.putBody(0, nil, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", rr.Code, rr.Body.String())
	}
	if _, present := dataOf(t, rr)["model_verified"]; present {
		t.Fatal("未指定模型时不应出现 model_verified")
	}

	stored := f.storedConfig(t)
	if _, present := stored["model"]; present {
		t.Fatalf("stored config must not carry a model key: %#v", stored)
	}
	if _, present := stored["model_provider"]; present {
		t.Fatalf("stored config must not carry a model_provider key: %#v", stored)
	}

	cfg, _, err := f.h.ClawDiscussionSystemHostConfig(context.Background())
	if err != nil {
		t.Fatalf("ClawDiscussionSystemHostConfig: %v", err)
	}
	if cfg.Model != "" {
		t.Fatalf("runtime Model = %q, want empty", cfg.Model)
	}
}

// TestClawAdminDiscussionHostPutRejectsInvalidModel：格式非法一律 400，且不落库。
func TestClawAdminDiscussionHostPutRejectsInvalidModel(t *testing.T) {
	cases := []struct {
		name     string
		provider *string
		model    *string
	}{
		{"provider only", cloneString("openai"), nil},
		// 「有模型没供应商」会退化成裸 id：设备端拆不出 provider，codex 会用自己
		// 配置里的 provider，表现为「选了但模型不对」。两个键必须同进同出。
		{"model only", nil, cloneString("gpt-5.6-sol")},
		{"whitespace in model", cloneString("openai"), cloneString("gpt 5")},
		{"illegal character", cloneString("openai"), cloneString("gpt$5")},
		{"too long", cloneString("openai"), cloneString(strings.Repeat("m", 129))},
		{"model with slash", cloneString("openai"), cloneString("gpt-5/extra")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setupDiscussionHostFixture(t, "")
			rr := f.put(t, f.putBody(0, tc.provider, tc.model))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d (%s), want 400", rr.Code, rr.Body.String())
			}
			var count int
			if err := f.tx.QueryRow(context.Background(), `
				SELECT COUNT(*) FROM rongcloud_system_config WHERE config_key = $1`,
				clawAdminDiscussionHostKey).Scan(&count); err != nil {
				t.Fatalf("count rows: %v", err)
			}
			if count != 0 {
				t.Fatalf("invalid model must not be persisted, found %d row(s)", count)
			}
		})
	}
}

// TestClawAdminDiscussionHostPutVerifiesModelAgainstCatalog：目录可取且命中 →
// 保存成功并标记 model_verified=true。
func TestClawAdminDiscussionHostPutVerifiesModelAgainstCatalog(t *testing.T) {
	f := setupDiscussionHostFixture(t, catalogProviders)

	rr := f.put(t, f.putBody(0, cloneString("openai"), cloneString("gpt-5.6-sol")))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", rr.Code, rr.Body.String())
	}
	if verified, ok := dataOf(t, rr)["model_verified"].(bool); !ok || !verified {
		t.Fatalf("model_verified = %v, want true", dataOf(t, rr)["model_verified"])
	}
}

// TestClawAdminDiscussionHostPutRejectsModelMissingFromCatalog：目录可取但里面
// 没有这个模型 → 400（唯一一种应当拒绝保存的模型错误），且不落库。
func TestClawAdminDiscussionHostPutRejectsModelMissingFromCatalog(t *testing.T) {
	f := setupDiscussionHostFixture(t, otherCatalogProviders)

	rr := f.put(t, f.putBody(0, cloneString("openai"), cloneString("gpt-5.6-sol")))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d (%s), want 400", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), rongcloud.NodeRequestCodeModelNotAvailable) {
		t.Fatalf("body = %s, want %s", rr.Body.String(), rongcloud.NodeRequestCodeModelNotAvailable)
	}
	var count int
	if err := f.tx.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM rongcloud_system_config WHERE config_key = $1`,
		clawAdminDiscussionHostKey).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Fatal("a model missing from the catalog must not be persisted")
	}
}

// TestClawAdminDiscussionHostModelCatalog 覆盖目录接口：可取时回 providers，
// 不可取（broker 未装配 / 节点离线）时 503 + 统一 code，页面据此降级手填。
func TestClawAdminDiscussionHostModelCatalog(t *testing.T) {
	f := setupDiscussionHostFixture(t, catalogProviders)

	rr := f.catalog(t, f.token, f.nodeUUID)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET model-catalog = %d (%s)", rr.Code, rr.Body.String())
	}
	data := dataOf(t, rr)
	providers, _ := data["providers"].([]interface{})
	if len(providers) != 1 {
		t.Fatalf("providers = %v, want one provider", data["providers"])
	}
	provider, _ := providers[0].(map[string]interface{})
	if provider["id"] != "openai" || provider["name"] != "OpenAI" {
		t.Fatalf("provider = %#v", provider)
	}
	models, _ := provider["models"].([]interface{})
	if len(models) != 2 {
		t.Fatalf("models = %v, want 2", provider["models"])
	}
	if data["source"] != "device" || data["node_id"] != f.nodeUUID {
		t.Fatalf("envelope = %#v", data)
	}
}

func TestClawAdminDiscussionHostModelCatalogOffline(t *testing.T) {
	// providersJSON 为空 → 未装配 broker，等价于「目录取不到」。
	f := setupDiscussionHostFixture(t, "")

	rr := f.catalog(t, f.token, f.nodeUUID)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET model-catalog = %d (%s), want 503", rr.Code, rr.Body.String())
	}
	// 对外的失败码只有这一个：具体原因（离线 / 超时 / 协议不符）只写日志与审计。
	if !strings.Contains(rr.Body.String(), rongcloud.NodeRequestCodeCatalogUnavailable) {
		t.Fatalf("body = %s, want code %s", rr.Body.String(), rongcloud.NodeRequestCodeCatalogUnavailable)
	}
}

// TestClawAdminDiscussionHostModelCatalogRequiresWritePermission：这条接口会以
// 系统账号向一台设备私发 IM、并让它跑一次本机 agent 探测（有副作用），因此必须
// 挂 system:write。只读管理员调用应当 403，且**一次设备往返都不该发生**。
func TestClawAdminDiscussionHostModelCatalogRequiresWritePermission(t *testing.T) {
	f := setupDiscussionHostFixture(t, catalogProviders)

	rr := f.catalog(t, f.readonlyToken, f.nodeUUID)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("只读管理员 GET model-catalog = %d (%s), want 403", rr.Code, rr.Body.String())
	}
	if got := atomic.LoadInt32(f.sendCount); got != 0 {
		t.Fatalf("只读管理员触发了 %d 次设备往返，want 0", got)
	}
}

// TestClawAdminDiscussionHostModelCatalogHidesNodeExistence：三种「不能给你目录」
// 的原因（node_id 不存在 / 节点不可展示 / 节点不具备主持能力）必须给出**逐字节
// 相同**的响应。否则这条接口就成了「某个 node_id 是否存在」的探测预言机 —— 它
// 同时还能让那台设备跑一次本机探测，不能兼职做存在性检查。
func TestClawAdminDiscussionHostModelCatalogHidesNodeExistence(t *testing.T) {
	f := setupDiscussionHostFixture(t, catalogProviders)
	ws, err := f.h.clawAdminDiscussionHostWorkspaceID(context.Background())
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	// ops 维护 agent：isHiddenNode 判它不可展示。
	hiddenNode, _ := f.seedNode(t, ws, "ops", discussionHostTestCaps)
	// 可展示但能力位不全（缺 discussion_host 等）→ 不是主持节点。
	partialNode, _ := f.seedNode(t, ws, "codex", `["discussion_roundtable"]`)

	cases := map[string]string{
		"unknown node":    uuid.NewString(),
		"hidden node":     hiddenNode,
		"not a host node": partialNode,
	}
	var firstBody string
	for name, nodeID := range cases {
		rr := f.catalog(t, f.token, nodeID)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: GET model-catalog = %d (%s), want 503", name, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), rongcloud.NodeRequestCodeCatalogUnavailable) {
			t.Fatalf("%s: body = %s, want %s", name, rr.Body.String(), rongcloud.NodeRequestCodeCatalogUnavailable)
		}
		if firstBody == "" {
			firstBody = rr.Body.String()
		} else if rr.Body.String() != firstBody {
			t.Fatalf("%s: 响应与其它失败原因不一致，泄露了节点是否存在\n got: %s\nwant: %s",
				name, rr.Body.String(), firstBody)
		}
	}
	if got := atomic.LoadInt32(f.sendCount); got != 0 {
		t.Fatalf("被拒的节点不该被惊动，实际发了 %d 次设备往返", got)
	}
}

// TestClawAdminDiscussionHostPutSkipsVerifyWhenModelUnchanged：模型**完全没变**
// 时跳过设备核验。运维最常见的动作是「只改人设提示词」，那不该为此多等一次
// 往返（最多 8s），也不该每次都弹「未能核验」黄条。
//
// 反向的三条同样重要：判据是「provider 与 model 都相同」，只改其中一个必须
// 重新核验 —— 只看 model、或只看 provider 的实现都会在这里露馅。
func TestClawAdminDiscussionHostPutSkipsVerifyWhenModelUnchanged(t *testing.T) {
	const firstProvider, firstModel = "openai", "gpt-5.6-sol"
	cases := []struct {
		name           string
		provider       string
		model          string
		wantExtraSends int32
		wantVerified   bool
	}{
		{"both unchanged", "openai", "gpt-5.6-sol", 0, false},
		{"only provider changed", "anthropic", "gpt-5.6-sol", 1, true},
		{"only model changed", "openai", "gpt-5-mini", 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setupDiscussionHostFixture(t, switchableCatalogProviders)

			rr := f.put(t, f.putBody(0, cloneString(firstProvider), cloneString(firstModel)))
			if rr.Code != http.StatusOK {
				t.Fatalf("首次 PUT = %d (%s)", rr.Code, rr.Body.String())
			}
			if got := atomic.LoadInt32(f.sendCount); got == 0 {
				t.Fatal("首次保存必须核验一次（目录可取）")
			}
			afterFirst := atomic.LoadInt32(f.sendCount)

			rr = f.put(t, f.putBody(1, cloneString(tc.provider), cloneString(tc.model)))
			if rr.Code != http.StatusOK {
				t.Fatalf("二次 PUT = %d (%s)", rr.Code, rr.Body.String())
			}
			if got := atomic.LoadInt32(f.sendCount) - afterFirst; got != tc.wantExtraSends {
				t.Fatalf("二次保存发了 %d 次设备往返，want %d", got, tc.wantExtraSends)
			}
			_, present := dataOf(t, rr)["model_verified"]
			if present != tc.wantVerified {
				t.Fatalf("model_verified 出现=%v, want %v（%v）", present, tc.wantVerified, dataOf(t, rr))
			}
			// 配置仍然完整（模型没被这次保存弄丢）。
			stored := f.storedConfig(t)
			if stored["model_provider"] != tc.provider || stored["model"] != tc.model {
				t.Fatalf("stored config = %#v, want %s/%s", stored, tc.provider, tc.model)
			}
		})
	}
}

// TestClawAdminDiscussionHostModelCatalogAudits：这条接口会让一台设备跑一次本机
// agent 探测，收紧到 system:write 之后，审计是回答「谁枚举了哪台节点」的唯一
// 证据 —— 成功与失败都必须留痕；被 403 挡在门外的请求不产生记录（ident 为 nil）。
func TestClawAdminDiscussionHostModelCatalogAudits(t *testing.T) {
	f := setupDiscussionHostFixture(t, catalogProviders)
	ws, err := f.h.clawAdminDiscussionHostWorkspaceID(context.Background())
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	partialNode, _ := f.seedNode(t, ws, "codex", `["discussion_roundtable"]`)

	if rows := f.auditRows(t, f.adminID); len(rows) != 0 {
		t.Fatalf("开局不该有审计行: %#v", rows)
	}

	// 成功：记一条，带 node_id 与来源。
	if rr := f.catalog(t, f.token, f.nodeUUID); rr.Code != http.StatusOK {
		t.Fatalf("GET model-catalog = %d (%s)", rr.Code, rr.Body.String())
	}
	rows := f.auditRows(t, f.adminID)
	if len(rows) != 1 {
		t.Fatalf("成功拉取应留 1 条审计，实际 %d 条: %#v", len(rows), rows)
	}
	if rows[0].action != "fetch_discussion_host_model_catalog" || rows[0].resourceID != clawAdminDiscussionHostKey {
		t.Fatalf("审计行 = %#v", rows[0])
	}
	if rows[0].nodeID != f.nodeUUID || rows[0].errorCode != "" {
		t.Fatalf("成功审计 detail = %#v（node_id=%q error_code=%q）", rows[0].detail, rows[0].nodeID, rows[0].errorCode)
	}

	// 失败：同样留痕，但要带上错误码与原因，否则事后分不清「被拒」与「没调」。
	if rr := f.catalog(t, f.token, partialNode); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("非主持节点 = %d (%s), want 503", rr.Code, rr.Body.String())
	}
	rows = f.auditRows(t, f.adminID)
	if len(rows) != 2 {
		t.Fatalf("失败也应留审计，实际 %d 条: %#v", len(rows), rows)
	}
	failed := rows[len(rows)-1]
	if failed.errorCode != rongcloud.NodeRequestCodeCatalogUnavailable || failed.reason == "" {
		t.Fatalf("失败审计 detail = %#v（error_code=%q reason=%q）", failed.detail, failed.errorCode, failed.reason)
	}
	if failed.nodeID != partialNode {
		t.Fatalf("失败审计 node_id = %q, want %q", failed.nodeID, partialNode)
	}

	// 只读管理员被 403 挡在门外：不该产生审计行。
	if rr := f.catalog(t, f.readonlyToken, f.nodeUUID); rr.Code != http.StatusForbidden {
		t.Fatalf("只读管理员 = %d, want 403", rr.Code)
	}
	if rows := f.auditRows(t, f.readonlyAdminID); len(rows) != 0 {
		t.Fatalf("403 不该留审计: %#v", rows)
	}
}

// TestClawAdminDiscussionHostGetDropsIllegalStoredModel：写入口不止管理端 PUT
// 一个 —— 旁路 PUT /api/workspaces/{id}/rongcloud/system-host 会把 config 整块
// 覆盖且不做任何字段校验。读路径必须自己 clamp 一次，否则非法串会一路进角色
// 快照 → IM payload → 设备 argv。
func TestClawAdminDiscussionHostGetDropsIllegalStoredModel(t *testing.T) {
	f := setupDiscussionHostFixture(t, "")
	ws, err := f.h.clawAdminDiscussionHostWorkspaceID(context.Background())
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	if _, err := f.tx.Exec(context.Background(), `
		INSERT INTO rongcloud_system_config (workspace_id, config_key, node_id, config, config_version)
		VALUES ($1, $2, $3, $4::jsonb, 1)`,
		ws, clawAdminDiscussionHostKey, f.nodeUUID,
		`{"enabled":true,"display_name":"主持人","model_provider":"open ai","model":"gpt-5.6-sol"}`); err != nil {
		t.Fatalf("seed sideloaded config: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/system/discussion-host", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	f.h.ClawAdminDiscussionHostGet(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET = %d (%s)", rr.Code, rr.Body.String())
	}
	data := dataOf(t, rr)
	if data["model_provider"] != "" || data["model"] != "" {
		t.Fatalf("非法存量模型必须被读路径丢弃，got %#v", data)
	}
	cfg, _, err := f.h.ClawDiscussionSystemHostConfig(context.Background())
	if err != nil {
		t.Fatalf("ClawDiscussionSystemHostConfig: %v", err)
	}
	if cfg.Model != "" {
		t.Fatalf("运行时 Model = %q, want empty", cfg.Model)
	}
}
