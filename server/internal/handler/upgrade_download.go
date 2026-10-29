package handler

// 服务端中转下载 —— 设备下载安装包的**首选**路径（直连 GitHub 是回退）。
//
// 为什么需要它：企业出口常常允许访问业务域名却打不通 github.com（或被限速到
// 不可用）。设备先走这里（业务域名、出口白名单里通常就在），失败再回退直连，
// 于是「GitHub 不可达」不会变成「设备永远升不上去」。
//
// 这个顺序（代理优先）是刻意的，不是历史包袱：受限出口打到 github.com 往往是
// 黑洞而不是拒绝，设备先直连会一直等到它的默认超时才换源，页面就长时间停在
// 「下载中」；而代理第一跳就是设备自己正在用的业务域名，必定可达。顺序以及
// CLAW_UPGRADE_DIRECT_FIRST 回退开关见 upgrade_release.go 的 clawUpgradeMirrors。
//
// 两个安全问题在这里解决：
//  1. **不做开放代理**：URL 里的版本号必须匹配 x.y.z，资产名必须在白名单里，
//     并且带服务端签发的短期 token（绑定 version+asset+过期时间）。地址是服务端
//     按 `releases/download/clawmessenger-v<version>/<asset>` 自己拼出来的，
//     请求方没有任何机会指定主机或路径。
//  2. **不重复下载**：同一版本只回源一次，落盘缓存后所有设备共享（100 台设备
//     从这里下载时不该打 100 次 GitHub）。缓存按版本目录保留最近若干份。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"log/slog"
)

const (
	// clawUpgradeCacheKeep 是保留的历史版本目录数（按目录修改时间）。
	clawUpgradeCacheKeep = 3
)

// clawUpgradeCacheDir 是代理下载的落盘目录。默认放在系统临时目录下：
// 这些文件是「从 GitHub 抄一份」的缓存，丢了下次再下就是了，不需要持久卷。
func clawUpgradeCacheDir() string {
	if dir := strings.TrimSpace(os.Getenv("CLAW_UPGRADE_CACHE_DIR")); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "clawmessenger-upgrades")
}

// clawUpgradeDownloadLocks 按 <version>/<asset> 串行化回源：同一版本并发到达时
// 只有一个 goroutine 真的去 GitHub 拉，其余的等它落盘后直接读文件。
var clawUpgradeDownloadLocks = struct {
	sync.Mutex
	byKey map[string]*sync.Mutex
}{byKey: make(map[string]*sync.Mutex)}

func clawUpgradeDownloadLock(key string) *sync.Mutex {
	clawUpgradeDownloadLocks.Lock()
	defer clawUpgradeDownloadLocks.Unlock()
	lock, ok := clawUpgradeDownloadLocks.byKey[key]
	if !ok {
		lock = &sync.Mutex{}
		clawUpgradeDownloadLocks.byKey[key] = lock
	}
	return lock
}

// clawUpgradeTagForVersion 把版本号还原成发布 tag。发布链路固定是
// `clawmessenger-v*.*.*`（.github/workflows/clawmessenger-release.yml），与 CLI
// 的 versionFromTag 互逆。
func clawUpgradeTagForVersion(version string) string {
	return "clawmessenger-v" + version
}

// ClawUpgradeDownload GET /api/agent/upgrade/download?version=&asset=&token=
//
// 公开端点（不挂 admin 鉴权）：设备手里只有设备凭据，而它拉的是**公开的
// release 资产**。鉴权靠 token —— 只有服务端自己签发的、针对该版本该资产的
// 短期凭证才能下载，所以它不是一个可以随便拿来当中转的开放代理。
func (h *Handler) ClawUpgradeDownload(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	version := strings.TrimSpace(query.Get("version"))
	asset := strings.TrimSpace(query.Get("asset"))
	token := strings.TrimSpace(query.Get("token"))
	if !clawUpgradeVersionPattern.MatchString(version) {
		http.Error(w, "版本号不合法", http.StatusBadRequest)
		return
	}
	// 白名单按发布矩阵校验，而不是「只允许 Windows 那一个名字」：端点公开，
	// 资产名进 URL，不设白名单就等于开了一个「按名字回源任意 release 资产」的代理。
	if !clawUpgradeAssetSupported(asset) {
		http.Error(w, "不支持的资产", http.StatusBadRequest)
		return
	}
	// fail closed：没配 MULTICA_CLAW_UPGRADE_TOKEN_SECRET 就没有可信的 token 门禁。
	// 此时**不能**退化成「不校验 token」（那是把一个公网端点变成开放代理），也
	// 不能回落到 admin JWT 的密钥（源码里有默认常量，等于人人可签）。直接 503，
	// 并把原因写进日志 —— 运维看到这个 503 就知道该去配 secret。
	//
	// 刻意如此的行为：dev 默认密钥（以及任何隐式回落）下服务端代理不可用，设备
	// 会落到「直连 GitHub 失败 → 这轮升不上去」。宁可升不上去，也不能开一个
	// 公开的 GitHub 中转。错误码 UPGRADE_PROXY_NOT_CONFIGURED 由运维/监控抓取，
	// 与「token 无效」的 403 区分开（后者是攻击或时钟漂移，前者是没配）。
	if !clawUpgradeTokenSecretConfigured() {
		slog.Error("claw: upgrade download unavailable, MULTICA_CLAW_UPGRADE_TOKEN_SECRET is not set",
			"code", clawUpgradeProxyNotConfigured)
		http.Error(w, clawUpgradeProxyNotConfigured+": 升级下载未配置签名密钥（MULTICA_CLAW_UPGRADE_TOKEN_SECRET）",
			http.StatusServiceUnavailable)
		return
	}
	if ok, reason := verifyClawUpgradeToken(token, version, asset); !ok {
		http.Error(w, "下载凭证无效："+reason, http.StatusForbidden)
		return
	}

	file, err := clawUpgradeCachedAsset(r.Context(), version, asset)
	if err != nil {
		var upstream *clawUpgradeUpstreamError
		if errors.As(err, &upstream) {
			http.Error(w, fmt.Sprintf("回源下载失败：HTTP %d", upstream.Status), http.StatusBadGateway)
			return
		}
		slog.Warn("claw: upgrade download failed", "version", version, "error", err)
		http.Error(w, "回源下载失败", http.StatusBadGateway)
		return
	}
	handle, err := os.Open(file)
	if err != nil {
		http.Error(w, "缓存文件不可读", http.StatusInternalServerError)
		return
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		http.Error(w, "缓存文件不可读", http.StatusInternalServerError)
		return
	}
	// ServeContent 负责 Range / If-Modified-Since / HEAD：设备中途断线后重试
	// 可以直接从断点续传（缓存已经在盘上，不必回源）。
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, asset))
	http.ServeContent(w, r, asset, info.ModTime(), handle)
}

// clawUpgradeUpstreamError 区分「回源返回了非 200」与「网络层失败」，好让调用方
// 给出准确的 HTTP 状态。
type clawUpgradeUpstreamError struct {
	Status int
}

func (e *clawUpgradeUpstreamError) Error() string {
	return fmt.Sprintf("upstream HTTP %d", e.Status)
}

// clawUpgradeCachedAsset 保证 <cache>/<version>/<asset> 存在，返回其路径。
func clawUpgradeCachedAsset(ctx context.Context, version, asset string) (string, error) {
	dir := filepath.Join(clawUpgradeCacheDir(), version)
	dest := filepath.Join(dir, asset)
	if info, err := os.Stat(dest); err == nil && info.Size() > 0 {
		return dest, nil
	}
	lock := clawUpgradeDownloadLock(version + "/" + asset)
	lock.Lock()
	defer lock.Unlock()
	// 双检：等锁期间别人可能已经下完了。
	if info, err := os.Stat(dest); err == nil && info.Size() > 0 {
		return dest, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	url := clawUpgradeAssetURL(clawUpgradeTagForVersion(version), asset)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "clawmessenger-server")
	client := &http.Client{Timeout: 10 * time.Minute}
	res, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", &clawUpgradeUpstreamError{Status: res.StatusCode}
	}
	partial := dest + ".part"
	out, err := os.Create(partial)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(out, hasher), res.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(partial)
		return "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(partial)
		return "", closeErr
	}
	// 有校验和就顺手校验一次：设备自己也会校验，但服务端缓存里留一个坏文件
	// 会让所有回退到代理的设备一起失败，代价太大。
	if expected := clawUpgradeExpectedSum(version, asset); expected != "" {
		actual := hex.EncodeToString(hasher.Sum(nil))
		if actual != expected {
			_ = os.Remove(partial)
			return "", fmt.Errorf("checksum mismatch: expected %s, got %s", expected, actual)
		}
	}
	if err := os.Rename(partial, dest); err != nil {
		_ = os.Remove(partial)
		return "", err
	}
	slog.Info("claw: upgrade asset cached", "version", version, "asset", asset, "bytes", written)
	clawUpgradeCleanupCache()
	return dest, nil
}

// clawUpgradeExpectedSum 从 latest 缓存里取该版本该资产的摘要；版本不是当前
// latest（或缓存里没有）时返回空，此时只做传输完整性以外的信任（设备端仍会
// 用指令里的 sha256 校验）。
func clawUpgradeExpectedSum(version, asset string) string {
	clawUpgradeLatest.mu.Lock()
	defer clawUpgradeLatest.mu.Unlock()
	if clawUpgradeLatest.release == nil || clawUpgradeLatest.release.Version != version {
		return ""
	}
	return clawUpgradeLatest.release.SHA256[asset]
}

// clawUpgradeCleanupCache 只保留最近 clawUpgradeCacheKeep 个版本目录：
// 88MB × N 会慢慢吃满 /tmp，而旧版本在「只升不降」的口径下不会再被请求。
func clawUpgradeCleanupCache() {
	entries, err := os.ReadDir(clawUpgradeCacheDir())
	if err != nil {
		return
	}
	type versioned struct {
		path    string
		modTime time.Time
	}
	dirs := make([]versioned, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !clawUpgradeVersionPattern.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		dirs = append(dirs, versioned{path: filepath.Join(clawUpgradeCacheDir(), entry.Name()), modTime: info.ModTime()})
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].modTime.After(dirs[j].modTime) })
	for i, dir := range dirs {
		if i < clawUpgradeCacheKeep {
			continue
		}
		if err := os.RemoveAll(dir.path); err != nil {
			continue
		}
	}
}
