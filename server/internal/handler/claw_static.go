package handler

import (
	"embed"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
)

// claw_static.go 提供 C 端（虾说）默认头像的托管与 URL 决议。
//
// 背景：注册（web/uniapp）与 AI 节点绑定在融云侧只写昵称、不写头像 ——
// getToken 对已存在用户不更新资料，于是融云好友列表/会话里这些账号长期显示
// 裸 id 或空白。两端客户端各有自己的本地默认图兜底，但融云端只会回显它自己
// 存的 portraitUri，客户端兜底在「融云已知资料」场景下不生效。
//
// 方案：
//   - 真人默认头像：embed 进二进制，经 /api/static/claw/avatar-default.png
//     免鉴权公开（与 /uploads/* 同一开放等级）。注册与资料更新链路把它写进
//     融云 portraitUri，同时落 claw_im_users.portrait_uri。
//   - AI 节点默认头像：沿用两端前端既定约定（newsradar.dreamdt.cn 托管的
//     openclaw.png），ensureAgentNode 绑定时写入融云。
//
// URL 决议优先级（clawDefaultPortraitURL）：
//   1. CLAW_DEFAULT_PORTRAIT_URI 环境变量（自建域名/CDN 时覆盖）
//   2. MULTICA_PUBLIC_URL + 免鉴权静态路径（同域部署的默认形态）
//   3. 线上 openclaw 托管域名的兜底（newsradar.dreamdt.cn）——与前端
//     ChatAvatar.tsx / conversation-list.vue 的硬编码一致

//go:embed clawstatic/avatar-default.png
var clawStaticAssets embed.FS

// clawDefaultAvatarPath 是真人默认头像的免鉴权路由（不含前缀，router 挂载）。
const clawDefaultAvatarPath = "/api/static/claw/avatar-default.png"

// ClawDefaultUserAvatarURL 返回真人默认头像的公网 URL。
// 线上部署经 nginx 剥掉 /im-test/ 前缀，服务自身路径即完整公开路径。
func (h *Handler) ClawDefaultUserAvatarURL() string {
	return clawDefaultPortraitURL()
}

// clawDefaultPortraitURL 按优先级决议默认头像 URL（见文件头注释）。
func clawDefaultPortraitURL() string {
	if v := strings.TrimSpace(os.Getenv("CLAW_DEFAULT_PORTRAIT_URI")); v != "" {
		return v
	}
	if base := strings.TrimRight(strings.TrimSpace(os.Getenv("MULTICA_PUBLIC_URL")), "/"); base != "" {
		return base + clawDefaultAvatarPath
	}
	// 兜底：线上部署的公开路径（nginx 剥 /im-test/ 前缀后即本服务的
	// clawDefaultAvatarPath）。与前端硬编码 openclaw 域名的做法一致；自建
	// 部署用 CLAW_DEFAULT_PORTRAIT_URI 覆盖。
	return "https://newsradar.dreamdt.cn/im-test" + clawDefaultAvatarPath
}

// ClawDefaultAINodeAvatarURL 返回 AI 节点默认头像的公网 URL（两端前端既定约定）。
func ClawDefaultAINodeAvatarURL() string {
	if v := strings.TrimSpace(os.Getenv("CLAW_AI_NODE_PORTRAIT_URI")); v != "" {
		return v
	}
	return "https://newsradar.dreamdt.cn/newschat/static/imgs/rongyun/openclaw.png"
}

// ServeClawStaticAssets 挂载免鉴权静态资源（当前只有默认头像）。
func ServeClawStaticAssets(mux chi.Router) {
	mux.Get(clawDefaultAvatarPath, func(w http.ResponseWriter, r *http.Request) {
		data, err := clawStaticAssets.ReadFile("clawstatic/avatar-default.png")
		if err != nil {
			http.Error(w, "avatar asset missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(data)
	})
}
