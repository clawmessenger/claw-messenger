package handler

import (
	"embed"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
)

// claw_static.go 提供 C 端（虾说）默认头像的托管与 URL 决议。
//
// 背景：注册（web/uniapp）与 AI 节点绑定在融云侧只写昵称、不写头像 ——
// getToken 对已存在用户不更新资料，于是融云好友列表/会话里这些账号长期显示
// 裸 id 或空白。两端客户端各有自己的本地默认图兜底，但融云端只会回显它自己
// 存的 portraitUri，客户端兜底在「融云已知资料」场景下不生效。
//
// 方案：全平台（真人 / AI agent / 机器主节点 / 群 / 讨论组 / 聊天室成员）统一
// 使用同一张默认头像，图源与 env 优先级的唯一定义在
// rongcloud.DefaultUserPortrait()，本文件只做转发，避免两处漂移：
//  1. CLAW_DEFAULT_PORTRAIT_URI 环境变量（自建域名/CDN 时覆盖）
//  2. 统一图源 newsradar.dreamdt.cn/newschat/static/imgs/rongyun/default_user.png
//
// （CLAW_AI_NODE_PORTRAIT_URI 已废弃删除：它会让 AI 与真人各用一张默认图。）
//
// /api/static/claw/avatar-default.png 路由保留（旧数据/自建部署兼容），但不再
// 作为默认值 —— 否则线上设了 MULTICA_PUBLIC_URL 就仍会推旧灰图。

//go:embed clawstatic/avatar-default.png
var clawStaticAssets embed.FS

// clawDefaultAvatarPath 是旧默认头像的免鉴权路由（不含前缀，router 挂载）。
// 仅作兼容保留，不再参与默认值决议。
const clawDefaultAvatarPath = "/api/static/claw/avatar-default.png"

// clawDefaultPortraitURL 返回统一默认头像 URL（决议逻辑见
// rongcloud.DefaultUserPortrait）。
func clawDefaultPortraitURL() string {
	return rongcloud.DefaultUserPortrait()
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
