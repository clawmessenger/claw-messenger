package handler

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
)

// 智能体产出文件的上传与读取。
//
// 背景：设备端的 agent（openclaw / opencode / hermes 等）被要求「生成一个 html
// 或一份报告」时，产物落在它自己的工作目录里，stdout 往往只有一句「已生成」甚至
// 什么都没有。此前链路里没有任何地方能把文件本体送到用户面前 —— 用户能拿到的
// 最多是一段被截断的文本。于是有了下面两条接口：
//
//	POST /api/upload           C 端会话（Bearer = claw_im_users.rongcloud_token）
//	POST /api/claw/node-files  设备凭据（nodeId + credentialId + secret）
//	GET  /api/claw/files/{key} 按 key 读回文件（公开、不可枚举）
//
// 为什么读回走 /api/claw/files/{key} 而不是复用现成的 /uploads/*：
// 线上 nginx 只把 /claw-web/api/* 反代到本服务，/claw-web/uploads/* 命中的是
// 前端 SPA 的 index.html，因此存储层返回的相对 URL（/uploads/xxx）在前端根本
// 取不到。挂在 /api 下则无论同源前缀是什么都能被前端直接引用（<img src> /
// iframe / fetch 都不需要额外配置）。key 是 uuid7 + 严格白名单校验，不可枚举、
// 无目录穿越，安全性与既有公开的 /uploads/* 一致。

const (
	// clawFileMaxBytes 是单个产出文件的上限。agent 生成的页面/报告/表格远小于
	// 这个量级，设置上限只为挡住误收的大文件（模型权重、数据集、录屏）。
	clawFileMaxBytes = 25 << 20 // 25 MiB
	// clawFileKeyPrefix 是存储 key 的第一段。带前缀是为了让这些文件与
	// workspaces/ 、users/ 下的既有附件在对象存储里一眼可分。
	clawFileKeyPrefix = "claw-files"
)

// clawFileKeyPattern 严格约束可读回的 key：<prefix>/<scope>/<uuid7><ext>。
// 只允许白名单字符，且 uuid 段必须真的是 uuid 形状 —— 这样 `..`、绝对路径、
// 反斜杠连正则都过不了，不需要再叠一层 filepath.Clean。
var clawFileKeyPattern = regexp.MustCompile(
	`^claw-files/(user|node)-[A-Za-z0-9_-]{1,64}/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}(\.[A-Za-z0-9]{1,8})?$`,
)

func validClawFileKey(key string) bool {
	return clawFileKeyPattern.MatchString(key)
}

// ClawUploadFile — POST /api/upload
//
// 前端（clawmessenger-web）的图片/文件/语音/视频/头像上传入口。契约与旧 Python
// 后端的 /api/upload 一致：multipart 字段 file + type + filename，返回
// {code,data:{url,filename,type,size}}。线上此前没有这个路由（404），前端上传
// 一直是坏的 —— 头像走 dataURL 绕过了它，聊天里的图片/文件则直接失败。
func (h *Handler) ClawUploadFile(w http.ResponseWriter, r *http.Request) {
	if h.Storage == nil {
		clawJSON(w, 503, 503, "文件服务未配置", nil)
		return
	}
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	h.clawStoreUpload(w, r, "user-"+userID, typeFromForm(r))
}

// ClawNodeUploadFile — POST /api/claw/node-files
//
// 设备端（xiachat-cli）把本轮 agent 产出的文件送上来。鉴权沿用同组设备接口的
// 约定：机器节点的 device credential（nodeId + credentialId + secret）以表单
// 字段随 multipart 一起提交。
func (h *Handler) ClawNodeUploadFile(w http.ResponseWriter, r *http.Request) {
	if h.Storage == nil {
		clawJSON(w, 503, 503, "文件服务未配置", nil)
		return
	}
	if h.RongCloudNode == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	// 先解析 multipart 才能读到凭据字段（multipart 里没有 query/header 可用）。
	r.Body = http.MaxBytesReader(w, r.Body, clawFileMaxBytes+1<<20)
	if err := r.ParseMultipartForm(clawFileMaxBytes + 1<<20); err != nil {
		clawJSON(w, 400, 400, "文件过大或表单格式错误", nil)
		return
	}
	defer r.MultipartForm.RemoveAll()

	nodeID := strings.TrimSpace(r.FormValue("nodeId"))
	credentialID := strings.TrimSpace(r.FormValue("credentialId"))
	secret := strings.TrimSpace(r.FormValue("secret"))
	if nodeID == "" || credentialID == "" || secret == "" {
		clawJSON(w, 400, 400, "缺少设备凭据", nil)
		return
	}
	if _, err := h.RongCloudNode.MachineAgentCredentials(r.Context(), nodeID, credentialID, secret); err != nil {
		if errors.Is(err, rongcloud.ErrInvalidDeviceCredential) {
			clawJSON(w, 401, 401, "设备凭据无效", nil)
			return
		}
		slog.Warn("claw: node file upload credential check failed", "nodeId", nodeID, "error", err)
		clawJSON(w, 500, 500, "校验设备凭据失败", nil)
		return
	}
	h.clawStoreUpload(w, r, "node-"+nodeID, typeFromForm(r))
}

// ClawServeFile — GET /api/claw/files/{key...}
//
// 按 key 读回产出文件。故意不加鉴权：这条 URL 要能直接塞进 <img src> / iframe
// / 融云消息体里被浏览器原生消费，而原生请求带不了 Authorization。安全性由
// 「128 位随机 key + 严格 key 形状校验」保证，与既有公开的 /uploads/* 同级。
func (h *Handler) ClawServeFile(w http.ResponseWriter, r *http.Request) {
	if h.Storage == nil {
		http.NotFound(w, r)
		return
	}
	key := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
	if !validClawFileKey(key) {
		http.NotFound(w, r)
		return
	}
	// 与 /api/attachments/{id}/download 同套预览安全头：去掉全局的
	// frame-ancestors 'none'，前端才能在自己的页面里 iframe 预览 PDF/HTML。
	h.setAttachmentPreviewSecurityHeaders(w)
	reader, err := h.Storage.GetReader(r.Context(), key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer reader.Close()
	if ct, ok := extContentTypes[strings.ToLower(path.Ext(key))]; ok {
		w.Header().Set("Content-Type", ct)
	} else if ct := contentTypeFromExt(path.Ext(key)); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = io.Copy(w, reader)
}

// clawStoreUpload 是两条上传入口的公共实现：读包 → 嗅探类型 → 存本地/对象
// 存储 → 返回 {url,key,filename,type,size}。scope 决定 key 的第二段
// （user-xxx / node-xxx），既是归属标记也是命名空间。
func (h *Handler) clawStoreUpload(w http.ResponseWriter, r *http.Request, scope, declaredType string) {
	if r.MultipartForm == nil {
		r.Body = http.MaxBytesReader(w, r.Body, clawFileMaxBytes+1<<20)
		if err := r.ParseMultipartForm(clawFileMaxBytes + 1<<20); err != nil {
			clawJSON(w, 400, 400, "文件过大或表单格式错误", nil)
			return
		}
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		clawJSON(w, 400, 400, fmt.Sprintf("缺少 file 字段: %v", err), nil)
		return
	}
	defer file.Close()
	if header.Size > clawFileMaxBytes {
		clawJSON(w, 413, 413, "文件过大", nil)
		return
	}

	buf := make([]byte, 512)
	n, readErr := file.Read(buf)
	if readErr != nil && readErr != io.EOF {
		clawJSON(w, 400, 400, "读取文件失败", nil)
		return
	}
	contentType := http.DetectContentType(buf[:n])
	ext := strings.ToLower(path.Ext(header.Filename))
	if ct, ok := extContentTypes[ext]; ok {
		contentType = ct
	} else if ct := contentTypeFromExt(ext); ct != "" {
		// 嗅探器对 .html/.md/.csv 之类会一律报 text/plain，按扩展名兜一层。
		contentType = ct
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		clawJSON(w, 500, 500, "读取文件失败", nil)
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, clawFileMaxBytes+1))
	if err != nil {
		clawJSON(w, 400, 400, "读取文件失败", nil)
		return
	}
	if int64(len(data)) > clawFileMaxBytes {
		clawJSON(w, 413, 413, "文件过大", nil)
		return
	}

	id, err := uuid.NewV7()
	if err != nil {
		slog.Error("claw: generate file key", "error", err)
		clawJSON(w, 500, 500, "内部错误", nil)
		return
	}
	sanitized := strings.TrimPrefix(safeExtension(ext), ".")
	key := fmt.Sprintf("%s/%s/%s", clawFileKeyPrefix, scope, id.String())
	if sanitized != "" {
		key += "." + sanitized
	}
	if _, err := h.Storage.Upload(r.Context(), key, data, contentType, header.Filename); err != nil {
		slog.Error("claw: store upload", "key", key, "error", err)
		clawJSON(w, 500, 500, "保存文件失败", nil)
		return
	}
	clawJSON(w, 200, 200, "", map[string]interface{}{
		"url":      "/api/claw/files/" + key,
		"key":      key,
		"filename": header.Filename,
		"type":     contentType,
		"size":     len(data),
		// declaredType 回显前端/设备自称的类别（image/video/audio/file），
		// 前端以 MIME 为准，这里只作兼容旧契约用。
		"declaredType": declaredType,
	})
}

// typeFromForm 读旧契约里的 type 字段（image/video/audio/file）。
func typeFromForm(r *http.Request) string {
	if r.MultipartForm == nil {
		return ""
	}
	return strings.TrimSpace(r.FormValue("type"))
}

// safeExtension 只保留扩展名里的字母数字，挡掉 `../../x` 这种把分隔符塞进
// 扩展名再拼进 key 的尝试。
func safeExtension(ext string) string {
	var b strings.Builder
	for _, c := range ext {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
		if b.Len() >= 8 {
			break
		}
	}
	return b.String()
}

// contentTypeFromExt 补 http.DetectContentType 会判错的常见文本类型。
func contentTypeFromExt(ext string) string {
	switch ext {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".txt", ".log":
		return "text/plain; charset=utf-8"
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	default:
		return ""
	}
}
