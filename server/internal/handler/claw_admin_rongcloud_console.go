package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 融云「内嵌控制台」接入（https://docs.rongcloud.cn/guides/imbedded-console/quick-start）。
//
// 链路：前端只拿到一个「访问令牌 URL」，把它交给 @rongcloud/embed-console-sdk，
// SDK 会把它设成 iframe 的 src。Access Key 是应用级高权限凭证，
// 必须留在服务端换令牌，绝不能下发到浏览器。

const (
	// clawAdminEmbedConsoleConfigKey 密钥配置在 rongcloud_system_config 里的键名。
	// 该键属于敏感键，通用 /system/config 列表接口会跳过它。
	clawAdminEmbedConsoleConfigKey = "rongcloud_embed_console"

	// clawAdminEmbedTokenDefaultURL 融云官方换取令牌的接口。
	clawAdminEmbedTokenDefaultURL = "https://embed-console.rongcloud.cn/embed/access/token"

	// clawAdminEmbedDefaultPageCode 未指定时默认嵌入的页面。
	clawAdminEmbedDefaultPageCode = "user_manage"

	// clawAdminEmbedDefaultUsefulLife 令牌默认有效期（秒），文档建议区间 3600-86400。
	clawAdminEmbedDefaultUsefulLife = 36000

	// 文档给出的 useful_life 取值范围。
	clawAdminEmbedMinUsefulLife = 1
	clawAdminEmbedMaxUsefulLife = 31536000

	// Access Key 也可由环境变量注入（运维场景，优先级低于界面配置）。
	clawAdminEmbedAccessKeyEnv = "MULTICA_RONGCLOUD_EMBED_ACCESS_KEY"
)

// clawAdminEmbedHTTPClient 换成独立客户端，便于单测替换 Transport。
var clawAdminEmbedHTTPClient = &http.Client{Timeout: 15 * time.Second}

// clawAdminEmbedPageCodes 是文档中列出的全部可嵌入页面。
// 它同时充当白名单：只有这些 page_code 会被送去融云，避免前台可传任意值。
var clawAdminEmbedPageCodes = []struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}{
	{"user_manage", "用户管理"},
	{"group_manage", "群组管理"},
	{"blocked_user_manage", "封禁用户管理"},
	{"chatroom_manage", "聊天室管理"},
	{"operation_log", "操作日志"},
	{"recording_manage", "录制文件管理"},
	{"test_user_mgt_embed", "测试用户管理"},
	{"customer_service_manage", "客服管理"},
	{"sensitive_word_settings", "敏感词设置"},
	{"im_server_api", "IM Server API 调试"},
	{"test_push", "推送测试"},
	{"rtc_server_api", "RTC Server API 调试"},
	{"im_service_config", "IM 服务功能配置"},
	{"rtc_service_config", "RTC 服务功能配置"},
}

func clawAdminIsValidEmbedPageCode(code string) bool {
	for _, page := range clawAdminEmbedPageCodes {
		if page.Code == code {
			return true
		}
	}
	return false
}

// clawAdminEmbedConsoleSettings 是运行期配置。
// AccessKey 打上 json:"-" 兜底，保证它不会被顺手序列化进任何响应；
// 也正因为 json 包会同时忽略它，落库才另用 clawAdminEmbedStoredConfig。
type clawAdminEmbedConsoleSettings struct {
	AccessKey   string `json:"-"`
	DefaultPage string `json:"default_page_code"`
	UsefulLife  int    `json:"default_useful_life"`
	TokenAPIURL string `json:"token_api_url"`
	ShowMenu    bool   `json:"show_menu"`
}

// clawAdminEmbedStoredConfig 是 rongcloud_system_config 里的落库形态，密钥在这里持久化。
type clawAdminEmbedStoredConfig struct {
	AccessKey   string `json:"access_key"`
	DefaultPage string `json:"default_page_code"`
	UsefulLife  int    `json:"default_useful_life"`
	TokenAPIURL string `json:"token_api_url"`
	ShowMenu    bool   `json:"show_menu"`
}

func (s clawAdminEmbedConsoleSettings) toStored() clawAdminEmbedStoredConfig {
	return clawAdminEmbedStoredConfig{
		AccessKey:   s.AccessKey,
		DefaultPage: s.DefaultPage,
		UsefulLife:  s.UsefulLife,
		TokenAPIURL: s.TokenAPIURL,
		ShowMenu:    s.ShowMenu,
	}
}

// applyStored 把库里的值合到 settings 上；任何越界/非法值一律退回默认，
// 避免一条脏配置把整个页面卡死。
func (s *clawAdminEmbedConsoleSettings) applyStored(stored clawAdminEmbedStoredConfig) {
	if clawAdminIsValidEmbedPageCode(stored.DefaultPage) {
		s.DefaultPage = stored.DefaultPage
	}
	if stored.UsefulLife >= clawAdminEmbedMinUsefulLife && stored.UsefulLife <= clawAdminEmbedMaxUsefulLife {
		s.UsefulLife = stored.UsefulLife
	}
	if _, err := clawAdminValidateEmbedTokenURL(stored.TokenAPIURL); err == nil {
		s.TokenAPIURL = stored.TokenAPIURL
	}
	s.AccessKey = stored.AccessKey
	s.ShowMenu = stored.ShowMenu
}

func clawAdminDefaultEmbedSettings() clawAdminEmbedConsoleSettings {
	return clawAdminEmbedConsoleSettings{
		DefaultPage: clawAdminEmbedDefaultPageCode,
		UsefulLife:  clawAdminEmbedDefaultUsefulLife,
		TokenAPIURL: clawAdminEmbedTokenDefaultURL,
		ShowMenu:    true,
	}
}

// clawAdminMaskSecret 只留头尾各 4 位，中间用 * 填充。短于 12 位则全部打码，
// 避免「脱敏后反而能推出原文」。
func clawAdminMaskSecret(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) < 12 {
		return strings.Repeat("*", len(secret))
	}
	return secret[:4] + "****" + secret[len(secret)-4:]
}

// clawAdminEmbedSource 说明 Access Key 从哪来，方便界面把「环境变量生效中、
// 界面未配置」这种情况讲清楚。
const (
	clawAdminEmbedSourceNone   = "none"
	clawAdminEmbedSourceConfig = "config"
	clawAdminEmbedSourceEnv    = "env"
)

// clawAdminEmbedKeyEnvLookup 让用例能在不打桩整库的前提下注入环境变量。
var clawAdminEmbedKeyEnvLookup = os.Getenv

// clawAdminLoadEmbedConsoleSettings 读配置。库里的键优先，其次环境变量。
func (h *Handler) clawAdminLoadEmbedConsoleSettings(ctx context.Context) (clawAdminEmbedConsoleSettings, string, error) {
	settings := clawAdminDefaultEmbedSettings()
	source := clawAdminEmbedSourceNone

	var raw []byte
	err := h.DB.QueryRow(ctx,
		"SELECT config FROM rongcloud_system_config WHERE config_key = $1 LIMIT 1",
		clawAdminEmbedConsoleConfigKey).Scan(&raw)
	switch {
	case err == nil:
		var stored clawAdminEmbedStoredConfig
		if json.Unmarshal(raw, &stored) == nil {
			settings.applyStored(stored)
		}
	case errors.Is(err, pgx.ErrNoRows):
		// 未配置过，走环境变量兜底。
	default:
		return settings, source, err
	}

	if settings.AccessKey != "" {
		source = clawAdminEmbedSourceConfig
	} else if envKey := strings.TrimSpace(clawAdminEmbedKeyEnvLookup(clawAdminEmbedAccessKeyEnv)); envKey != "" {
		settings.AccessKey = envKey
		source = clawAdminEmbedSourceEnv
	}
	return settings, source, nil
}

// clawAdminValidateEmbedTokenURL 只允许 https + rongcloud.cn 及其子域，
// 防止把「可配置的接口地址」变成服务端 SSRF 跳板。
func clawAdminValidateEmbedTokenURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("token 接口地址不能为空")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("token 接口地址不是合法 URL")
	}
	if parsed.Scheme != "https" {
		return "", errors.New("token 接口地址必须使用 https")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "rongcloud.cn" && !strings.HasSuffix(host, ".rongcloud.cn") {
		return "", errors.New("token 接口地址必须是 rongcloud.cn 或其子域")
	}
	return parsed.String(), nil
}

// clawAdminEmbedConsolePayload 是 GET 的返回体：只含脱敏信息。
func (h *Handler) clawAdminEmbedConsolePayload(ctx context.Context) (map[string]interface{}, error) {
	settings, source, err := h.clawAdminLoadEmbedConsoleSettings(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"configured":            settings.AccessKey != "",
		"source":                source,
		"access_key_masked":     clawAdminMaskSecret(settings.AccessKey),
		"default_page_code":     settings.DefaultPage,
		"default_useful_life":   settings.UsefulLife,
		"token_api_url":         settings.TokenAPIURL,
		"show_menu":             settings.ShowMenu,
		"pages":                 clawAdminEmbedPageCodes,
		"access_key_env_var":    clawAdminEmbedAccessKeyEnv,
		"min_useful_life":       clawAdminEmbedMinUsefulLife,
		"max_useful_life":       clawAdminEmbedMaxUsefulLife,
		"token_request_timeout": int(clawAdminEmbedHTTPClient.Timeout / time.Second),
	}, nil
}

// ---- /api/admin/rongcloud/console ----

// ClawAdminGetRongCloudConsole 返回内嵌控制台配置（脱敏）。
func (h *Handler) ClawAdminGetRongCloudConsole(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("system:write", w, r) == nil {
		return
	}
	payload, err := h.clawAdminEmbedConsolePayload(r.Context())
	if err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}
	clawAdminOK(w, payload)
}

// ClawAdminPutRongCloudConsole 保存配置。access_key 非空才覆盖，
// 传 clear_access_key=true 显式清空。
func (h *Handler) ClawAdminPutRongCloudConsole(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("system:write", w, r)
	if ident == nil {
		return
	}
	var body struct {
		AccessKey      string `json:"access_key"`
		ClearAccessKey bool   `json:"clear_access_key"`
		DefaultPage    string `json:"default_page_code"`
		UsefulLife     *int   `json:"default_useful_life"`
		TokenAPIURL    string `json:"token_api_url"`
		ShowMenu       *bool  `json:"show_menu"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		clawAdminFail(w, 400, "JSON object is required")
		return
	}

	stored, _, err := h.clawAdminLoadStoredEmbedSettings(r.Context())
	if err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}

	switch {
	case body.ClearAccessKey:
		stored.AccessKey = ""
	case strings.TrimSpace(body.AccessKey) != "":
		stored.AccessKey = strings.TrimSpace(body.AccessKey)
	}

	if body.DefaultPage != "" {
		if !clawAdminIsValidEmbedPageCode(body.DefaultPage) {
			clawAdminFail(w, 400, "unknown page_code")
			return
		}
		stored.DefaultPage = body.DefaultPage
	}
	if body.UsefulLife != nil {
		if *body.UsefulLife < clawAdminEmbedMinUsefulLife || *body.UsefulLife > clawAdminEmbedMaxUsefulLife {
			clawAdminFail(w, 400, "useful_life 取值范围为 1-31536000")
			return
		}
		stored.UsefulLife = *body.UsefulLife
	}
	if body.TokenAPIURL != "" {
		validated, verr := clawAdminValidateEmbedTokenURL(body.TokenAPIURL)
		if verr != nil {
			clawAdminFail(w, 400, verr.Error())
			return
		}
		stored.TokenAPIURL = validated
	}
	if body.ShowMenu != nil {
		stored.ShowMenu = *body.ShowMenu
	}

	if err := h.clawAdminSaveEmbedSettings(r.Context(), stored); err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}
	h.clawAdminAudit(r, ident, "admin_rongcloud_console_config_update", "system_config", clawAdminEmbedConsoleConfigKey, nil)

	payload, err := h.clawAdminEmbedConsolePayload(r.Context())
	if err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}
	clawAdminOK(w, payload)
}

// clawAdminLoadStoredEmbedSettings 只读库里的配置（不含环境变量兜底），
// 用于「读-改-写」时保留原有字段。
func (h *Handler) clawAdminLoadStoredEmbedSettings(ctx context.Context) (clawAdminEmbedConsoleSettings, bool, error) {
	settings := clawAdminDefaultEmbedSettings()
	var raw []byte
	err := h.DB.QueryRow(ctx,
		"SELECT config FROM rongcloud_system_config WHERE config_key = $1 LIMIT 1",
		clawAdminEmbedConsoleConfigKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return settings, false, nil
	}
	if err != nil {
		return settings, false, err
	}
	var stored clawAdminEmbedStoredConfig
	if json.Unmarshal(raw, &stored) == nil {
		settings.applyStored(stored)
	}
	return settings, true, nil
}

func (h *Handler) clawAdminSaveEmbedSettings(ctx context.Context, settings clawAdminEmbedConsoleSettings) error {
	payload, err := json.Marshal(settings.toStored())
	if err != nil {
		return err
	}
	// 与 ClawAdminSystemConfigPut 同一套 upsert：workspace 取该键现有行或默认工作区。
	var wsID string
	if err := h.DB.QueryRow(ctx,
		"SELECT workspace_id::text FROM rongcloud_system_config WHERE config_key=$1 LIMIT 1",
		clawAdminEmbedConsoleConfigKey).Scan(&wsID); err != nil {
		if err := h.DB.QueryRow(ctx,
			"SELECT id::text FROM workspace ORDER BY created_at LIMIT 1").Scan(&wsID); err != nil {
			return fmt.Errorf("no workspace available: %w", err)
		}
	}
	_, err = h.DB.Exec(ctx, `
		INSERT INTO rongcloud_system_config (workspace_id, config_key, config)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, config_key)
		DO UPDATE SET config = $3, config_version = rongcloud_system_config.config_version + 1, updated_at = now()`,
		wsID, clawAdminEmbedConsoleConfigKey, payload)
	return err
}

// ---- /api/admin/rongcloud/console/token ----

type clawAdminEmbedTokenResponse struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// clawAdminExtractEmbedTokenURL 兼容 data 直接是字符串，或包成对象两种返回形态。
func clawAdminExtractEmbedTokenURL(data json.RawMessage) (string, bool) {
	if len(data) == 0 {
		return "", false
	}
	var direct string
	if err := json.Unmarshal(data, &direct); err == nil && strings.TrimSpace(direct) != "" {
		return strings.TrimSpace(direct), true
	}
	var wrapper struct {
		URL      string `json:"url"`
		TokenURL string `json:"token_url"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil {
		for _, candidate := range []string{wrapper.URL, wrapper.TokenURL, wrapper.Token} {
			if strings.TrimSpace(candidate) != "" {
				return strings.TrimSpace(candidate), true
			}
		}
	}
	return "", false
}

// ClawAdminRongCloudConsoleToken 用服务端保存的 Access Key 向融云换取访问令牌 URL。
// 返回给前端的只有一个 URL，不含 Access Key。
func (h *Handler) ClawAdminRongCloudConsoleToken(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("system:write", w, r) == nil {
		return
	}
	var body struct {
		PageCode   string `json:"page_code"`
		UsefulLife int    `json:"useful_life"`
	}
	// body 允许为空：全部走配置里的默认值。
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	}

	settings, _, err := h.clawAdminLoadEmbedConsoleSettings(r.Context())
	if err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}
	if settings.AccessKey == "" {
		clawAdminFail(w, 409, "尚未配置融云 Access Key")
		return
	}

	pageCode := strings.TrimSpace(body.PageCode)
	if pageCode == "" {
		pageCode = settings.DefaultPage
	}
	if !clawAdminIsValidEmbedPageCode(pageCode) {
		clawAdminFail(w, 400, "unknown page_code")
		return
	}
	usefulLife := body.UsefulLife
	if usefulLife == 0 {
		usefulLife = settings.UsefulLife
	}
	if usefulLife < clawAdminEmbedMinUsefulLife || usefulLife > clawAdminEmbedMaxUsefulLife {
		clawAdminFail(w, 400, "useful_life 取值范围为 1-31536000")
		return
	}

	tokenURL, err := h.clawAdminRequestEmbedToken(r.Context(), settings, pageCode, usefulLife)
	if err != nil {
		clawAdminFail(w, 502, err.Error())
		return
	}
	clawAdminOK(w, map[string]interface{}{
		"token_url":   tokenURL,
		"page_code":   pageCode,
		"useful_life": usefulLife,
		"expires_at":  time.Now().Add(time.Duration(usefulLife) * time.Second).UTC().Format(time.RFC3339),
	})
}

// clawAdminRequestEmbedToken 真正去打融云的令牌接口。错误信息里不会带上 Access Key。
func (h *Handler) clawAdminRequestEmbedToken(
	ctx context.Context,
	settings clawAdminEmbedConsoleSettings,
	pageCode string,
	usefulLife int,
) (string, error) {
	requestBody, err := json.Marshal(map[string]interface{}{
		"access_key":  settings.AccessKey,
		"page_code":   pageCode,
		"useful_life": usefulLife,
	})
	if err != nil {
		return "", errors.New("构造请求失败")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, settings.TokenAPIURL, bytes.NewReader(requestBody))
	if err != nil {
		return "", errors.New("构造请求失败")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := clawAdminEmbedHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求融云令牌接口失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", errors.New("读取融云返回失败")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("融云令牌接口返回 HTTP %d", resp.StatusCode)
	}

	var parsed clawAdminEmbedTokenResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", errors.New("融云返回不是合法 JSON")
	}
	if parsed.Code != 10000 {
		message := strings.TrimSpace(parsed.Message)
		if message == "" {
			message = fmt.Sprintf("code=%d", parsed.Code)
		}
		return "", fmt.Errorf("融云返回错误: %s", message)
	}
	tokenURL, ok := clawAdminExtractEmbedTokenURL(parsed.Data)
	if !ok {
		return "", errors.New("融云未返回访问令牌 URL")
	}
	return tokenURL, nil
}

// clawAdminSecretConfigKeys 是 rongcloud_system_config 里含密钥、
// 不允许经通用配置接口读出的键。
func clawAdminSecretConfigKeys() map[string]bool {
	return map[string]bool{clawAdminEmbedConsoleConfigKey: true}
}
