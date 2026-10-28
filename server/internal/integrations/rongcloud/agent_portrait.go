package rongcloud

import (
	"os"
	"strings"
)

// agent 分类头像（256px PNG）的公网目录。
//
// 这批文件和前端 `public/agent-avatars/<kind>-256.png` 是**同一份字节**：
// 前端 `<img>` 用它当 logo，融云 portraitUri 也指向它。两边同源是刻意的 ——
// 一旦分成两个目录，改图标时必有一边漏掉，各端看到不同头像。
//
// 为什么是 PNG 而不是仓库里的 SVG：融云客户端只按 URL 存字符串，实际渲染在
// 各家客户端和 WebView 里，SVG 的支持面不可控（iOS 原生、安卓旧内核都可能
// 不认），PNG 是唯一安全的交集。
const AgentPortraitBaseURL = "https://newsradar.dreamdt.cn/claw-web/agent-avatars"

// AgentPortraitBaseEnv 允许自建 / 测试环境覆盖头像目录（无尾斜杠）。
const AgentPortraitBaseEnv = "CLAW_AGENT_PORTRAIT_BASE"

// AgentPortraitBase 返回当前生效的头像目录。
func AgentPortraitBase() string {
	if v := strings.TrimSpace(os.Getenv(AgentPortraitBaseEnv)); v != "" {
		return strings.TrimRight(v, "/")
	}
	return strings.TrimRight(AgentPortraitBaseURL, "/")
}

// 融云 userId 的命名空间前缀。包内多处已硬编码这些字面量（`rc_node_` 出现在
// node_service / node_device_auth / group_command），这里只为本文件集中一份，
// 不扩大改动面。
const (
	omUserIDPrefix       = "om_"
	rcNodeIDPrefix       = "rc_node_"
	legacyOpenclawPrefix = "claw_"
)

// agentPortraitKinds 是「有品牌头像的 agent 类型」全集，必须与前端
// `AGENT_LOGO_BY_KIND` 的 25 个键逐项一致（前端那边由 tsc 的
// `Record<AiNodeType, string>` 兜底，这边只能靠测试兜）。
var agentPortraitKinds = map[string]struct{}{
	"openclaw": {}, "opencode": {}, "codearts": {}, "codex": {}, "hermes": {},
	"claude": {}, "kimi": {}, "cursor": {}, "qwen": {}, "qwenpaw": {},
	"grok": {}, "pi": {}, "copilot": {}, "codebuddy": {}, "deveco": {},
	"reasonix": {}, "dsh": {}, "kiro": {}, "antigravity": {}, "qoder": {},
	"qoderclicn": {}, "traecli": {}, "mcode": {}, "dim": {}, "zeroclaw": {},
}

// agentKindByCLIName 是 CLI 命令名 → 前端 kind 的映射。
//
// **两边不是同一套命名**：融云 userId 由 CLI 名拼成（`<cli>_<machineKey>`，
// 见 agentNodeRCUserID），而前端 kind 清单用的是产品名。四组值长得像但不同：
//
//	cursor-agent → cursor      kiro-cli   → kiro
//	agy          → antigravity qodercli  → qoder
//
// 漏掉任何一组，那家的 agent 就永远拿不到品牌头像（会静默落回统一默认图）。
// `omp` 故意不映射：前端没有对应 kind，也就没有它的官方图标 —— 让它保持
// 统一默认图，而不是随便借一张。
var agentKindByCLIName = map[string]string{
	"claude":       "claude",
	"codex":        "codex",
	"opencode":     "opencode",
	"codebuddy":    "codebuddy",
	"codearts":     "codearts",
	"deveco":       "deveco",
	"openclaw":     "openclaw",
	"hermes":       "hermes",
	"pi":           "pi",
	"cursor-agent": "cursor",
	"kimi":         "kimi",
	"reasonix":     "reasonix",
	"dsh":          "dsh",
	"kiro-cli":     "kiro",
	"agy":          "antigravity",
	"qodercli":     "qoder",
	"qoderclicn":   "qoderclicn",
	"traecli":      "traecli",
	"grok":         "grok",
	"qwen":         "qwen",
	"qwenpaw":      "qwenpaw",
	"mcode":        "mcode",
	"dim":          "dim",
	"zeroclaw":     "zeroclaw",
	// CLI 清单（knownAgentCLIs / KNOWN_AGENT_CLIS）里漏了 copilot，但
	// config.go 的候选清单和前端 kind 都有它 —— 这里补上，否则存量 copilot
	// 节点永远拿不到图标。
	"copilot": "copilot",
}

// AgentKindForCLIName 把 CLI 命令名归一成前端 kind。
func AgentKindForCLIName(name string) (string, bool) {
	kind, ok := agentKindByCLIName[strings.TrimSpace(name)]
	return kind, ok
}

// AgentKindFromRCUserID 从融云 userId 反推 agent kind。
//
// 认得的形态（与前端 ai-node-id.ts 的 getAiNodeType 同义）：
//
//	<cli>_<machineKey>              现行 Go agent 节点，如 `kimi_1234`、`cursor-agent_1234`
//	rc_node_<machineKey>_<cli>      旧世代 agent 节点
//	claw_<id>                       远古 openclaw
//	om_<...>                        运维账号前缀，剥掉后再认
//
// 明确不认（返回 false，调用方保持统一默认图）：
//
//	rc_node_<machineKey>            机器主节点，不是 agent
//	..._ops / ops_<key>             运维节点，头像不该跟着 agent 走
//	未知 CLI 名（如 omp）            没有对应官方图标
func AgentKindFromRCUserID(rcUserID string) (string, bool) {
	id := strings.TrimSpace(rcUserID)
	if id == "" {
		return "", false
	}
	// 运维账号（`om_` 前缀）是同一台机器上的另一个人格，本体仍是节点 id。
	id = strings.TrimPrefix(id, omUserIDPrefix)
	// ops 节点有两种在野命名（`ops_<key>` / `<agentRC>_ops`），它们服务 agent
	// 但不是 agent 本人，头像不能跟着换。
	if strings.HasSuffix(id, "_ops") || id == "ops" {
		return "", false
	}

	if strings.HasPrefix(id, rcNodeIDPrefix) {
		rest := strings.TrimPrefix(id, rcNodeIDPrefix)
		separator := strings.LastIndex(rest, "_")
		if separator < 0 {
			// 没有 agent 段：机器主节点。
			return "", false
		}
		return AgentKindForCLIName(rest[separator+1:])
	}

	separator := strings.Index(id, "_")
	if separator < 0 {
		return "", false
	}
	head := id[:separator]
	// 历史 openclaw 曾把节点写成 `claw_<id>`，kind 清单里没有 `claw`。
	if head == strings.TrimSuffix(legacyOpenclawPrefix, "_") {
		return "openclaw", true
	}
	return AgentKindForCLIName(head)
}

// AgentPortraitURI 返回某 kind 的品牌头像地址（256px PNG）。
// kind 没有对应图标时返回 false，调用方应保持统一默认图。
func AgentPortraitURI(kind string) (string, bool) {
	k := strings.TrimSpace(kind)
	if _, ok := agentPortraitKinds[k]; !ok {
		return "", false
	}
	return AgentPortraitBase() + "/" + k + "-256.png", true
}

// rcUserNotFoundCode 是融云「该 userId 不存在」的业务错误码。
//
// 存量库里确实有这种行：`rongcloud_user` 有记录，但融云侧从来没注册过这个
// 账号（设备绑定没走到 getToken、或机器重装后旧节点残留）。这种行调
// `/user/refresh.json` 只会回 1002，头像无从谈起。
//
// 它必须和「推送失败」分开计数：否则一轮批量会被几行历史脏数据判成 failed，
// 让人以为改坏了 —— 而这批行本来就没有可显示头像的地方（融云没账号，就不
// 可能在好友列表里出现）。
const rcUserNotFoundCode = 1002

// IsRCUserNotFound 判断错误是否为「融云侧没有这个用户」。
func IsRCUserNotFound(err error) bool {
	code, _, ok := RongCloudAPIError(err)
	return ok && code == rcUserNotFoundCode
}

// IsAgentPortraitURI 判断一个 portrait 是不是本方案写进去的品牌头像。
//
// 用途有两个，都是「重跑要幂等」的前提：
//  1. 已经是品牌头像（可能来自上一轮、也可能换了图）→ 允许再覆盖成当前目标；
//  2. 别人家的 URL 一律不碰 —— 只认自己 base 下的 25 个固定文件名，
//     绝不用 `%agent%` 这类模糊匹配，否则会误伤用户自定义头像。
func IsAgentPortraitURI(url string) bool {
	clean := strings.TrimSpace(url)
	if clean == "" {
		return false
	}
	if i := strings.IndexAny(clean, "?#"); i >= 0 {
		clean = clean[:i]
	}
	clean = strings.TrimRight(clean, "/")
	prefix := AgentPortraitBase() + "/"
	if !strings.HasPrefix(clean, prefix) {
		return false
	}
	name := strings.TrimPrefix(clean, prefix)
	if name == "" || strings.Contains(name, "/") {
		return false
	}
	const suffix = "-256.png"
	if !strings.HasSuffix(name, suffix) {
		return false
	}
	_, ok := agentPortraitKinds[strings.TrimSuffix(name, suffix)]
	return ok
}
