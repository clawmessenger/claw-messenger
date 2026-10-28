package rongcloud

import (
	"log/slog"
	"net/url"
	"os"
	"strings"
)

// portrait_group.go 是「群类会话默认头像」的唯一定义处。
//
// 背景：真人 / AI 节点 / 机器主节点共用一张 default_user.png，群聊、讨论组、
// 聊天室原本也落在同一张图上 —— 会话列表里一排群长得完全一样，用户在 web 端
// 已经按类型换成了三张自绘图（两人剪影 / 圆桌三席 / 两个气泡）。
//
// 但移动端（小程序、App）不读 web 的这份映射：
//   - 群聊读融云群资料的 portraitUri；
//   - 讨论组 / 聊天室读 Go 侧 rongcloud_chatroom.portrait_uri（融云聊天室 API
//     根本没有头像字段）。
//
// 所以图源必须由服务端按 kind 决议并写下去，否则「web 换了图、手机还是灰人形」。
//
// 两条硬约束：
//  1. 必须是 PNG。原生 App 用 Glide / SDWebImage 加载头像，解不了 SVG；web 用的
//     是同一套图形的 SVG 版本（public/group-avatars/*.svg）。
//  2. 图源要能被 env 覆盖。换域名 / 走 CDN 时不该改代码，与
//     CLAW_DEFAULT_PORTRAIT_URI 同一套思路。图必须公网可读 —— 融云只存 URL，
//     真去拉图的是客户端。

const (
	// GroupPortraitKindGroup 群聊（融云群）。
	GroupPortraitKindGroup = "group"
	// GroupPortraitKindDiscussion 讨论组（圆桌，落在 rongcloud_chatroom）。
	GroupPortraitKindDiscussion = "discussion"
	// GroupPortraitKindChatroom 聊天室（落在 rongcloud_chatroom）。
	GroupPortraitKindChatroom = "chatroom"
)

// DefaultGroupPortraitBaseURL 是三张群类默认图的发布位置（web 产物的
// public/group-avatars/，部署后即 /claw-web/group-avatars/）。
const DefaultGroupPortraitBaseURL = "https://newsradar.dreamdt.cn/claw-web/group-avatars"

// GroupPortraitBaseEnv 覆盖三张群类默认图的基址（自建域名 / CDN）。
const GroupPortraitBaseEnv = "CLAW_GROUP_PORTRAIT_BASE"

// groupPortraitKinds 是允许拼进 URL 的 kind 全集。除此之外一律回落，绝不把
// 调用方传来的任意字符串拼成地址（那只会换回一个 404 的头像）。
var groupPortraitKinds = []string{
	GroupPortraitKindGroup,
	GroupPortraitKindDiscussion,
	GroupPortraitKindChatroom,
}

// GroupPortraitBase 返回群类默认图的基址。
func GroupPortraitBase() string {
	if v := strings.TrimSpace(os.Getenv(GroupPortraitBaseEnv)); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultGroupPortraitBaseURL
}

// GroupPortraitURI 返回某类群会话的默认头像。未知 kind 回落到统一默认图
// （default_user.png）—— 宁可显示一张灰人形，也不要写进一个 404 的 URL。
func GroupPortraitURI(kind string) string {
	normalized := strings.ToLower(strings.TrimSpace(kind))
	for _, known := range groupPortraitKinds {
		if normalized == known {
			return GroupPortraitBase() + "/" + known + ".png"
		}
	}
	return DefaultUserPortrait()
}

// GroupPortraitIfEmpty 保留调用方给的群头像，空串时按 kind 落默认图。建群 / 建
// 聊天室的两条写路径都走它，保证 portrait_uri 不会被写成空串。
func GroupPortraitIfEmpty(kind, portrait string) string {
	if trimmed := strings.TrimSpace(portrait); trimmed != "" {
		return trimmed
	}
	return GroupPortraitURI(kind)
}

// ChatroomPortraitKind 把 rongcloud_chatroom.conversation_kind 映射成群类图 kind。
//
// 空值按聊天室处理：与 web 端「room 缺 conversationKind 时算聊天室」的判定保持
// 一致（Home.tsx 的 knownKind ?? 'chatroom'），也让历史行有确定归属，不会因为
// 少一个字段就落回灰人形。
func ChatroomPortraitKind(conversationKind string) string {
	if strings.EqualFold(strings.TrimSpace(conversationKind), GroupPortraitKindDiscussion) {
		return GroupPortraitKindDiscussion
	}
	return GroupPortraitKindChatroom
}

// GroupDefaultPortraitURIs 返回「可以被我方改写的默认图」全集：统一默认图 +
// 三张群类图。存量回填的 SQL 预筛用它做等值匹配（只认这几种，用户自定义图绝不
// 入选）。
func GroupDefaultPortraitURIs() []string {
	out := make([]string, 0, len(groupPortraitKinds)+1)
	out = append(out, DefaultUserPortrait())
	for _, kind := range groupPortraitKinds {
		out = append(out, GroupPortraitURI(kind))
	}
	return out
}

// IsGroupDefaultPortrait 判断一条头像是不是三张群类默认图之一。
//
// 判定是「URL path 与我方基址**全等**」+ 文件名精确等于 <kind>.png，不是
// `contains("group-avatars")` 这类模糊匹配：用户自己传的群头像完全可能叫
// `uploads/group-avatars/group.png`，模糊匹配会把它当成默认图覆盖掉 —— 这与
// NeedsDefaultPortrait 对旧静态路径的处理是同一条纪律。
//
// 基址同时认当前 env 与线上常量：换域名时两种写法都算我方发布的图。更早的历史
// 域名不在识别范围内（写下去之后没人再改基址），需要时把旧基址填进 env 即可。
func IsGroupDefaultPortrait(portrait string) bool {
	parsed, err := url.Parse(strings.TrimSpace(portrait))
	if err != nil {
		return false
	}
	path := strings.TrimRight(parsed.Path, "/")
	bases := []string{DefaultGroupPortraitBaseURL, GroupPortraitBase()}
	for _, kind := range groupPortraitKinds {
		for _, base := range bases {
			if path == groupPortraitPath(base, kind) {
				return true
			}
		}
	}
	return false
}

// groupPortraitPath 返回基址对应的 URL path + 文件名（不含 host）。
func groupPortraitPath(base, kind string) string {
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return ""
	}
	return strings.TrimRight(parsed.Path, "/") + "/" + kind + ".png"
}

// NeedsGroupPortraitFix 判断一条群 / 聊天室头像是否应当改写成 target。
//
// 命中条件（其余一律视为用户自定义，绝不覆盖）：
//   - 空值；
//   - 统一默认图 default_user.png（换图之前的群全落在这里，也是最主要的存量）；
//   - 历史默认图（openclaw.png、旧静态路径）；
//   - 三张群类默认图中的任意一张 —— 类型判错或后补的图要能被纠正。
//
// 已经等于 target 的直接返回 false：回填要幂等，重跑不能每次都往融云打一遍。
func NeedsGroupPortraitFix(current, target string) bool {
	trimmed := strings.TrimSpace(current)
	if trimmed == target {
		return false
	}
	if trimmed == "" {
		return true
	}
	if trimmed == DefaultUserPortrait() {
		return true
	}
	for _, legacy := range legacyDefaultPortraitURIs {
		if trimmed == legacy {
			return true
		}
	}
	if parsed, err := url.Parse(trimmed); err == nil && strings.HasSuffix(parsed.Path, LegacyDefaultPortraitPath) {
		return true
	}
	return IsGroupDefaultPortrait(trimmed)
}

// LogGroupPortraitSource 在服务启动时打一条群类默认图图源日志，并在 env 覆盖时
// Warn 一次：群头像写进融云后各端都会长期回显这个 URL，配错了很难一眼看出来。
func LogGroupPortraitSource(logger *slog.Logger) {
	if logger == nil {
		return
	}
	logger.Info("群类默认头像图源",
		"base", GroupPortraitBase(),
		"canonical", DefaultGroupPortraitBaseURL,
		"group", GroupPortraitURI(GroupPortraitKindGroup),
		"discussion", GroupPortraitURI(GroupPortraitKindDiscussion),
		"chatroom", GroupPortraitURI(GroupPortraitKindChatroom))
	if v := strings.TrimSpace(os.Getenv(GroupPortraitBaseEnv)); v != "" && strings.TrimRight(v, "/") != DefaultGroupPortraitBaseURL {
		logger.Warn("CLAW_GROUP_PORTRAIT_BASE 覆盖了群类默认头像基址，请确认这是期望的图源",
			"value", v, "canonical", DefaultGroupPortraitBaseURL)
	}
}
