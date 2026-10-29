package handler

// 设备 exe 升级 —— 服务端侧的「最新版从哪来」。
//
// 目标版本只允许是 GitHub Releases 的 latest：管理端只下发表意（「升到最新」），
// 版本号、资产名、sha256、下载地址全部由服务端自己解析（见 claw_admin_upgrade.go
// 的白名单校验）。前端/设备永远没有机会递一个 URL 进来 —— 这是防 SSRF 的第一道，
// 也是唯一需要的一道。
//
// 缓存 10 分钟：管理端点一次「批量升级」会为 N 台设备解析同一个版本，不缓存就是
// N 次 GitHub API 调用（匿名配额 60 次/小时/IP，几台设备就能烧光）。失败时降级为
// 「上次缓存值 + stale=true」，并且**不刷新**缓存时间 —— 下次点击会再试一次。
// 从没成功过时明确报错，不假装成功：页面上「未知版本」比「假装升到某个版本」诚实。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// clawUpgradeReleasesRepo 与 CLI 内置自更新同源（self-update.ts 的 RELEASES_REPO），
	// 不新增发布方式。
	clawUpgradeReleasesRepo = "clawmessenger/claw-messenger"
	// clawUpgradeWindowsAsset 是 Windows-x64 的资产名。保号是因为测试、提示文案
	// 与历史 token 都按它写；平台到资产的映射统一走 clawUpgradeAssetForPlatform。
	clawUpgradeWindowsAsset = "clawmessenger-windows-x64.exe"
	// clawUpgradePlatformWindows 是设备上报的 "<os>-<arch>" 值。
	clawUpgradePlatformWindows = "win32-x64"
	// clawUpgradeReleaseTTL 是 latest 的缓存时长。
	clawUpgradeReleaseTTL = 10 * time.Minute
	// clawUpgradeAPITimeout / clawUpgradeAssetTimeout 都是「宁可降级也不要挂住请求」
	// 的墙钟：管理端点一次升级按钮，不该因为 GitHub 慢而让管理员等 30 秒。
	clawUpgradeAPITimeout   = 8 * time.Second
	clawUpgradeAssetTimeout = 15 * time.Second
	// clawUpgradeTokenTTL 是下载代理 token 的有效期。设备领到指令后的下载窗口
	// 通常是分钟级，30 分钟足够覆盖「先排队等空闲」的场景，又不至于长到可以
	// 被拿去当免费 CDN 长期刷。
	clawUpgradeTokenTTL = 30 * time.Minute
)

// clawUpgradeVersionPattern 是唯一被接受的版本形态。它同时挡住
// "latest"、"../../"、"http://evil" 这些形状 —— 版本号会被拼进 GitHub 的
// release 路径，不限制字符集就是在拼一个可控 URL。
var clawUpgradeVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// clawUpgradeTagPattern 从 `clawmessenger-v1.2.3` 里抠出版本号（与
// self-update.ts 的 versionFromTag 行为一致：取第一组 x.y.z）。
var clawUpgradeTagPattern = regexp.MustCompile(`(\d+\.\d+\.\d+)`)

// clawUpgradeRelease 是一次 latest 解析的结果。
type clawUpgradeRelease struct {
	Version string
	Tag     string
	// Assets 资产名 -> browser_download_url（GitHub 的下载地址）。
	Assets map[string]string
	// SHA256 资产名 -> 十六进制摘要。两个来源合并后的结果：同一次 Release 的
	// SHA256SUMS 为权威值，Releases API 的 `assets[].digest` 只补 SUMS 缺的资产。
	// 字段含义（「该资产的 sha256」）未变，变的是「从哪来」。
	SHA256 map[string]string
}

func clawUpgradeAPIBase() string {
	if base := strings.TrimRight(strings.TrimSpace(os.Getenv("CLAW_UPGRADE_RELEASE_API")), "/"); base != "" {
		return base
	}
	return "https://api.github.com"
}

// clawUpgradeWebBase 是 release 资产的基址。测试与内网镜像可以覆盖它，
// 与 CLI 的 CLAWMESSENGER_UPDATE_WEB 同义。
func clawUpgradeWebBase() string {
	if base := strings.TrimRight(strings.TrimSpace(os.Getenv("CLAW_UPGRADE_RELEASE_WEB")), "/"); base != "" {
		return base
	}
	return "https://github.com"
}

// clawUpgradeAssetURL 拼出某个 tag 下某个资产的下载地址。
// 入参只能来自服务端解析出的 tag 与白名单资产名。
func clawUpgradeAssetURL(tag, asset string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", clawUpgradeWebBase(), clawUpgradeReleasesRepo, tag, asset)
}

// parseClawSHA256Sums 解析 GitHub Release 里的 `SHA256SUMS`
// （每行 `<hex>  <name>`，与 CLI 的 parseSha256Sums 同格式）。
func parseClawSHA256Sums(text string) map[string]string {
	sums := make(map[string]string)
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != 64 || !isHex(sum) {
			continue
		}
		sums[strings.TrimPrefix(fields[1], "*")] = sum
	}
	return sums
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// parseClawAssetDigest 从 Releases API 的 `assets[].digest` 里取出 sha256。
//
// 只接受 `sha256:<64 hex>`：`sha512:` 之类的其它算法、空值、没有算法前缀的裸串、
// 长度不对的都返回空串（调用方当作「这个资产没有摘要」，绝不猜）。大小写与前后
// 空白都容忍 —— 这是外部输入，不该假设 GitHub 永远只发一种写法。
//
// 为什么要解析它：`SHA256SUMS` 是一份**额外的**下载（第二个 HTTP 请求、可能被
// 镜像站漏同步、可能 404/超时）。它一失败，此前 `release.SHA256` 就是空 map，
// 于是**所有平台**都在下发时被判成「该平台资产缺少校验和」—— 管理端看到的是
// 一个 400，日志里什么都没有。digest 与资产清单在同一个响应里，天然不会漏。
func parseClawAssetDigest(raw string) string {
	algo, value, ok := strings.Cut(strings.TrimSpace(raw), ":")
	if !ok || !strings.EqualFold(strings.TrimSpace(algo), "sha256") {
		return ""
	}
	sum := strings.ToLower(strings.TrimSpace(value))
	if len(sum) != 64 || !isHex(sum) {
		return ""
	}
	return sum
}

// clawUpgradeVersionFromTag 把 tag 归一成 x.y.z。
func clawUpgradeVersionFromTag(tag string) string {
	if m := clawUpgradeTagPattern.FindStringSubmatch(tag); len(m) == 2 {
		return m[1]
	}
	return strings.TrimPrefix(tag, "clawmessenger-v")
}

// fetchClawUpgradeLatest 直连 GitHub 解析 latest：一次 API 调用拿 tag+assets
// （顺带 assets[].digest 里的 sha256），再取一次 SHA256SUMS（小文件）作为权威
// 校验和清单。第一次失败返回错误 —— 由缓存层决定是降级还是报错；第二次失败只
// 记日志（digest 兜底），因为「没有校验和」在下发那一刻是明确的拒绝，不是崩。
func fetchClawUpgradeLatest(ctx context.Context, client *http.Client) (*clawUpgradeRelease, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/releases/latest", clawUpgradeAPIBase(), clawUpgradeReleasesRepo)
	apiCtx, cancel := context.WithTimeout(ctx, clawUpgradeAPITimeout)
	defer cancel()
	res, err := doGitHubAPIGet(apiCtx, client, apiURL)
	if err != nil {
		return nil, fmt.Errorf("github releases/latest: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases/latest: HTTP %d", res.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("github releases/latest: decode: %w", err)
	}
	tag := strings.TrimSpace(body.TagName)
	if tag == "" {
		return nil, fmt.Errorf("github releases/latest: empty tag_name")
	}
	release := &clawUpgradeRelease{
		Version: clawUpgradeVersionFromTag(tag),
		Tag:     tag,
		Assets:  make(map[string]string, len(body.Assets)),
		SHA256:  make(map[string]string),
	}
	digestByAsset := make(map[string]string, len(body.Assets))
	for _, asset := range body.Assets {
		if asset.Name == "" {
			continue
		}
		if asset.URL != "" {
			release.Assets[asset.Name] = asset.URL
		}
		if sum := parseClawAssetDigest(asset.Digest); sum != "" {
			digestByAsset[asset.Name] = sum
		}
	}
	// SHA256SUMS 拿不到不是致命错（比如镜像站只同步了二进制）：它的失败只意味着
	// 少了权威清单，digest 那条兜底还在。但**必须留日志** —— 静默失败的表现是
	// 「管理端点升级，所有平台都报该平台资产缺少校验和」，而日志里一个字都没有
	// （本次 400 就是这么来的）。文案里点名「取校验和」这一步，便于运维直接定位。
	//
	// 日志里刻意**不打 URL**：自建镜像常用预签名地址，把它写进日志等于把可用的
	// 下载凭证抄进日志文件。`tag` + `asset` 已经足够定位到「哪个版本缺清单」。
	sumsURL, hasSums := release.Assets["SHA256SUMS"]
	sums := map[string]string{}
	switch {
	case !hasSums:
		slog.Warn("claw upgrade: 取校验和失败 —— Release 里没有 SHA256SUMS 资产，本次只有资产 digest 兜底",
			"tag", tag, "asset", "SHA256SUMS")
	default:
		sumCtx, sumCancel := context.WithTimeout(ctx, clawUpgradeAssetTimeout)
		defer sumCancel()
		sumRes, sumErr := doGitHubAPIGet(sumCtx, client, sumsURL)
		switch {
		case sumErr != nil:
			slog.Warn("claw upgrade: 取校验和失败 —— SHA256SUMS 下载出错，本次只有资产 digest 兜底",
				"tag", tag, "asset", "SHA256SUMS", "error", sumErr)
		default:
			func() {
				defer sumRes.Body.Close()
				if sumRes.StatusCode != http.StatusOK {
					slog.Warn("claw upgrade: 取校验和失败 —— SHA256SUMS 返回非 200，本次只有资产 digest 兜底",
						"tag", tag, "asset", "SHA256SUMS", "status", sumRes.StatusCode)
					return
				}
				raw, readErr := io.ReadAll(io.LimitReader(sumRes.Body, 1<<20))
				if readErr != nil {
					slog.Warn("claw upgrade: 取校验和失败 —— SHA256SUMS 读取中断，本次只有资产 digest 兜底",
						"tag", tag, "asset", "SHA256SUMS", "error", readErr)
					return
				}
				sums = parseClawSHA256Sums(string(raw))
			}()
		}
	}
	// 合并：SUMS 先入，digest 只补它缺的资产（只增不减）。
	//
	// 两边都有且不同时以 SUMS 为准。真实理由只有两条：
	//  1. 下发值逐字不变 —— 线上设备收到的 sha256 与修复前一模一样（修复前 SUMS 是
	//     唯一来源），回归面最小，这次改动不该顺带改变「已经能正常升级的那部分」；
	//  2. SUMS 是发布者随版本一起签发的**声明**，digest 只是 GitHub 侧对资产文件的
	//     元数据。两者不一致说明发布流程本身有问题，此时信声明、并留痕给人看。
	//
	// 注意：设备端**从不**读 SHA256SUMS（self-update.ts 只用下发指令里的
	// plan.sha256），所以「设备按 SUMS 校验」不是理由 —— 别把不成立的话写进注释。
	//
	// 冲突必须留日志：静默取一边的话，线上会变成设备 checksum_mismatch、服务端零
	// 信号 —— 与本次要修的「静默失败」是同一个病。
	for asset, sum := range sums {
		if digest, ok := digestByAsset[asset]; ok && digest != sum {
			slog.Warn("claw upgrade: 校验和两源不一致，以 SHA256SUMS 为准",
				"tag", tag, "asset", asset, "sums", sum, "digest", digest)
		}
		release.SHA256[asset] = sum
	}
	for asset, sum := range digestByAsset {
		if _, exists := release.SHA256[asset]; !exists {
			release.SHA256[asset] = sum
		}
	}
	// 发布矩阵内的资产仍然没有摘要 = 这次升级一定发不下去。在这里点出来，运维
	// 就不用等到管理端点按钮看到 400 再回来翻日志。
	for _, asset := range clawUpgradeAssetsByPlatform {
		if _, ok := release.SHA256[asset]; ok {
			continue
		}
		if _, published := release.Assets[asset]; !published {
			continue
		}
		slog.Warn("claw upgrade: 该版本资产没有任何校验和来源（SHA256SUMS 与 digest 都缺），将拒绝下发",
			"tag", tag, "asset", asset)
	}
	return release, nil
}

// clawUpgradeCache 是进程级的 latest 缓存。放包级而不是 Handler 字段：Handler
// 结构体不在这次改动的授权范围里，且这份缓存本来就没有「按 handler 实例隔离」的
// 语义 —— 它缓存的是上游 GitHub 的事实。
type clawUpgradeCache struct {
	mu       sync.Mutex
	release  *clawUpgradeRelease
	cachedAt time.Time
	inflight chan struct{}
	client   *http.Client
}

var clawUpgradeLatest = &clawUpgradeCache{
	client: &http.Client{Timeout: clawUpgradeAssetTimeout},
}

// clawUpgradeHTTPClient 让测试能把上游换成一个假 server。
func clawUpgradeHTTPClient() *http.Client {
	clawUpgradeLatest.mu.Lock()
	defer clawUpgradeLatest.mu.Unlock()
	return clawUpgradeLatest.client
}

func clawUpgradeSetHTTPClientForTest(client *http.Client) (restore func()) {
	clawUpgradeLatest.mu.Lock()
	previous := clawUpgradeLatest.client
	clawUpgradeLatest.client = client
	clawUpgradeLatest.mu.Unlock()
	return func() {
		clawUpgradeLatest.mu.Lock()
		clawUpgradeLatest.client = previous
		clawUpgradeLatest.mu.Unlock()
	}
}

// clawUpgradeResetCacheForTest 清空缓存，避免测试之间互相看到对方的 latest。
func clawUpgradeResetCacheForTest() {
	clawUpgradeLatest.mu.Lock()
	clawUpgradeLatest.release = nil
	clawUpgradeLatest.cachedAt = time.Time{}
	clawUpgradeLatest.inflight = nil
	clawUpgradeLatest.mu.Unlock()
}

// resolveClawUpgradeLatest 返回 latest 版本。stale=true 表示这次没打成 GitHub，
// 返回的是上次成功的值（页面必须显式提示「上次已知版本」）。
func resolveClawUpgradeLatest(ctx context.Context) (release *clawUpgradeRelease, stale bool, err error) {
	cache := clawUpgradeLatest
	cache.mu.Lock()
	// 并发点批量升级时只让一个请求去打 GitHub，其余等它的结果。
	for cache.inflight != nil {
		wait := cache.inflight
		cache.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
		cache.mu.Lock()
	}
	if cache.release != nil && time.Since(cache.cachedAt) < clawUpgradeReleaseTTL {
		hit := cache.release
		cache.mu.Unlock()
		return hit, false, nil
	}
	done := make(chan struct{})
	cache.inflight = done
	client := cache.client
	cache.mu.Unlock()

	fetched, fetchErr := fetchClawUpgradeLatest(ctx, client)

	cache.mu.Lock()
	if fetchErr == nil {
		cache.release = fetched
		cache.cachedAt = time.Now()
	}
	previous := cache.release
	cache.inflight = nil
	close(done)
	cache.mu.Unlock()

	if fetchErr != nil {
		if previous != nil {
			// 降级：用上次已知值，但**不改** cachedAt —— 下次点击会重新尝试。
			return previous, true, nil
		}
		return nil, false, fmt.Errorf("无法获取最新版本：%w", fetchErr)
	}
	return fetched, false, nil
}

// ---- 下载代理 token ---------------------------------------------------------

// clawUpgradeProxyNotConfigured 是「服务端没配下载凭证密钥」的错误码，随 503
// 一起返回并打进日志。它是配置问题（该去补 MULTICA_CLAW_UPGRADE_TOKEN_SECRET），
// 与 403 的「token 无效」（攻击、或设备时钟漂移）必须能区分开。
const clawUpgradeProxyNotConfigured = "UPGRADE_PROXY_NOT_CONFIGURED"

// clawUpgradeTokenSecret 是下载代理 token 的签名密钥。
//
// **fail closed**：只认显式配置的 MULTICA_CLAW_UPGRADE_TOKEN_SECRET，未配置时
// 返回 nil，**绝不**回落到 admin JWT 的密钥、更不回落到源码里的常量。
//
// 为什么这一条是硬性的：/api/agent/upgrade/download 是**公网、无 admin 鉴权**的
// 端点，token 是它唯一的门禁。如果密钥就是仓库里人人可见的字面量，任何人都能
// 给自己签一个 `9.9.9|clawmessenger-windows-x64.exe|未来时间` 的 token，让本服务
// 去 GitHub 代拉并落盘 —— 既是免费 CDN，也能把缓存目录写满、挤掉正常工作集（
// 审查报告 S2）。少了这个 secret 的正确降级是「代理不可用、设备直连 GitHub」，
// 而不是「代理可用且人人可签」。
func clawUpgradeTokenSecret() []byte {
	if secret := strings.TrimSpace(os.Getenv("MULTICA_CLAW_UPGRADE_TOKEN_SECRET")); secret != "" {
		return []byte(secret)
	}
	return nil
}

// clawUpgradeTokenSecretConfigured 是上面那条规则的「是/否」形式。
//
// 下载热路径不该靠字符串比较去判断「密钥是不是那个开发默认值」：只要有人把
// 回落逻辑加回来，比较就可能跟着被改成「也算配置过」。这里只回答一个问题 ——
// 运维显式配了吗。没配就 503（UPGRADE_PROXY_NOT_CONFIGURED），设备自然落到
// 直连 GitHub 那一路。
func clawUpgradeTokenSecretConfigured() bool {
	return clawUpgradeTokenSecret() != nil
}

// signClawUpgradeToken 签一个短期下载凭证。绑定 version+asset+过期时间：
// token 泄漏也只能换到「这个版本的这个资产」，不能当通用代理用。
// 未配置密钥时返回空串 —— 调用方（clawUpgradeMirrors）会把代理这一路去掉。
func signClawUpgradeToken(version, asset string, exp time.Time) string {
	secret := clawUpgradeTokenSecret()
	if secret == nil {
		return ""
	}
	payload := fmt.Sprintf("%s|%s|%d", version, asset, exp.Unix())
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + hex.EncodeToString(mac.Sum(nil))
}

// verifyClawUpgradeToken 校验 token，返回 (ok, reason)。
func verifyClawUpgradeToken(token, version, asset string) (bool, string) {
	secret := clawUpgradeTokenSecret()
	if secret == nil {
		// 正常路径上 handler 已经先返回 503 了，这里只是不让「没密钥」被当成
		// 「签名有效」或「签名无效」—— 它是第三种状态：根本没配。
		return false, "服务端未配置下载凭证密钥"
	}
	encoded, signature, ok := strings.Cut(token, ".")
	if !ok || encoded == "" || signature == "" {
		return false, "token 格式不正确"
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return false, "token 格式不正确"
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return false, "token 格式不正确"
	}
	tokenVersion, tokenAsset, expText := parts[0], parts[1], parts[2]
	if tokenVersion != version || tokenAsset != asset {
		return false, "token 与请求的版本/资产不匹配"
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(string(raw)))
	if !hmac.Equal(mac.Sum(nil), mustHexDecode(signature)) {
		return false, "token 签名无效"
	}
	var expUnix int64
	if _, err := fmt.Sscanf(expText, "%d", &expUnix); err != nil {
		return false, "token 过期时间无效"
	}
	if time.Now().Unix() > expUnix {
		return false, "token 已过期"
	}
	return true, ""
}

func mustHexDecode(s string) []byte {
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return raw
}

// clawUpgradeDirectFirst 让运维把下载顺序切回「直连优先」（历史顺序）。
//
// 默认是**代理优先**，这个开关只用于两种情况：服务端本身没有外网出口（代拉不了，
// 只能让设备自己想办法），或者运维明确不希望安装包过服务端。取值 "1" / "true"
// 打开，其它一切值（含空）保持默认。
func clawUpgradeDirectFirst() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CLAW_UPGRADE_DIRECT_FIRST"))) {
	case "1", "true":
		return true
	default:
		return false
	}
}

// clawUpgradeMirrors 给出设备下载该版本资产的顺序：**先服务端代理，再直连**。
// 代理地址是 **服务端相对路径** —— 设备自己知道它的 serverUrl（凭据里就有），
// 不需要服务端配置公网域名。
//
// 为什么默认顺序是代理优先（而不是「CDN 更快所以先直连」）：
//
//  1. 受限出口的设备（内网、白名单出口、只放行业务域名）到 github.com 是**黑洞**
//     而不是「快速失败」。设备先白试一次直连，要一直等到 undici 的默认超时才会
//     换下一个源，页面就长时间停在「下载中」——管理员看到的是「升级卡住了」，而
//     服务端日志一条错误都没有。先走代理，第一跳就是可达的业务域名。
//  2. 带宽是可控的取舍，不是不可控的浪费：代理端点有落盘缓存
//     （<cache>/<version>/<asset>，见 upgrade_download.go），同一版本只回源 GitHub
//     一次，之后 N 台设备共享同一份文件；而 100 台设备各自直连 GitHub 是 100 份
//     88MB 打在同一条企业出口上 —— 那才是会打满带宽的那种用法。
//  3. 直连地址仍然排在后面，不是删掉：服务端自己没外网（代拉必然 502）时，设备
//     的出口反而可能是通的，回退到直连就是那条路的唯一活路。
//
// token 为空（服务端没配 MULTICA_CLAW_UPGRADE_TOKEN_SECRET）时只给直连地址：
// 代理端点此刻是 503，把它排在 mirrors 里只会让设备多等一次必然失败的尝试。
func clawUpgradeMirrors(release *clawUpgradeRelease, asset, token string) (mirrors []string, directURL string) {
	direct := release.Assets[asset]
	if direct == "" {
		direct = clawUpgradeAssetURL(release.Tag, asset)
	}
	if direct == "" {
		// 资产名不在发布矩阵里：连直连地址都拼不出来，此处返回空列表让调用方
		// 按「没有可下载地址」处理，而不是给设备一个 404 的 URL。
		return nil, ""
	}
	if token == "" {
		return []string{direct}, direct
	}
	proxy := fmt.Sprintf("/api/agent/upgrade/download?version=%s&asset=%s&token=%s",
		release.Version, asset, token)
	if clawUpgradeDirectFirst() {
		return []string{direct, proxy}, direct
	}
	return []string{proxy, direct}, direct
}

// clawUpgradeAssetsByPlatform 是「设备上报的 <os>-<arch>」→ Release 资产名的唯一
// 映射，必须与 CLI 的 assetNameForPlatform（self-update.ts）逐字一致。
//
// 两处漂移的后果是静默的：服务端按 A 名签 token，设备按 B 名去下载，结果是
// download_failed（404），而页面上只会显示「下载失败」—— 看不出是名字错了。
// 发布矩阵（.github/workflows/clawmessenger-release.yml）是唯一真相。
var clawUpgradeAssetsByPlatform = map[string]string{
	clawUpgradePlatformWindows: "clawmessenger-windows-x64.exe",
	"linux-x64":                "clawmessenger-linux-x64",
	"linux-arm64":              "clawmessenger-linux-arm64",
	"darwin-arm64":             "clawmessenger-darwin-arm64",
}

// clawUpgradeAssetForPlatform 返回该平台要下载的资产名；空串 = 该平台不在发布
// 矩阵内（含平台未知）。
func clawUpgradeAssetForPlatform(platform string) string {
	return clawUpgradeAssetsByPlatform[strings.TrimSpace(platform)]
}

// clawUpgradeAssetSupported 判断资产名是否属于发布矩阵。下载代理的白名单用它：
// 端点公开，资产名进 URL，不做白名单就等于开了一个「按名字回源任意 release
// 资产」的代理。
func clawUpgradeAssetSupported(asset string) bool {
	asset = strings.TrimSpace(asset)
	if asset == "" {
		return false
	}
	for _, known := range clawUpgradeAssetsByPlatform {
		if known == asset {
			return true
		}
	}
	return false
}

// clawUpgradePlatformSupported 判定该平台能否在后台下发升级。
//
// 非 Windows 的「重启生效」不再是阻碍：设备端的 __relaunch 本来就是
// 「拿端口锁 → spawn 新进程 → 确认站起来 → 放锁」的跨平台交接，与 systemd /
// LaunchAgent 无关；Windows 只是需要额外处理「运行中的 exe 不能删、但能改名」。
//
// **未知平台仍然 = 不支持**（fail closed）。空串表示「设备从没上报过平台」——那
// 多半是 1.3.0 之前的版本，它的 supervisor 根本没有领取升级指令的代码。把它当成
// 可升级会让这一行永远停在 pending，而波次闸会因此把后面所有机器一起卡死（审查
// 报告 S3）。宁可当场说「平台未知，先手动装一次」。
func clawUpgradePlatformSupported(platform string) bool {
	return clawUpgradeAssetForPlatform(platform) != ""
}

// clawUpgradeSupportedPlatformsText 把可升级平台列成人话，供页面/日志解释
// 「为什么这台不行」。顺序固定，避免每次渲染顺序漂移。
func clawUpgradeSupportedPlatformsText() string {
	return "Windows-x64 / Linux-x64 / Linux-arm64 / macOS-arm64"
}
