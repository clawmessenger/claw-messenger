#!/usr/bin/env bash
# claw-cli-mirror-sync.sh — 把 GitHub Release 里的 CLI 安装包镜像到本机，
# 给「访问不到 GitHub」的用户留一条备用下载路（见使用指南「接入提示词」第 3.5 节）。
#
# 为什么是这个形状：
#   1) 文件名与 GitHub 完全一致 → SHA256SUMS 原样复用。用户从镜像下的包，
#      仍然用 GitHub 发布的哈希校验，镜像本身不需要再签一套。
#   2) 先下到临时目录、逐个校验、全部通过后才原子切换。中途断网/校验失败
#      不会把正在服务的旧版本换成一堆坏文件。
# 3) 安装包放在 webroot 之外（与 webroot 同级），webroot 里只放一个符号链接（默认 dl）。
#      重新部署前端（tar 解包、甚至 rm -rf webroot）都不会误删几百 MB 的包，
#      重跑本脚本即可恢复链接。反过来把 400MB 放在 webroot 里，一次清理就全没了。
#      放同级还有个好处：SELinux Enforcing 时与 webroot 同父目录，上下文好对齐。
#   4) 服务器自己连不上 GitHub 时（本项目当前就是这样），走 --from-dir：
#      本机跑 claw-cli-mirror-fetch.ps1 下好打包 → 上传 → 服务器解包后 --from-dir 激活。
#      这是主路径，不是应急方案；不带 --from-dir 的联网模式只是「服务器恰好能出网」时的便利。
#
# 不提供的东西：签名。镜像只能防传输损坏和本地篡改，不能防上游被投毒。
# 哈希来自 GitHub 的 SHA256SUMS（TLS 传输），信任锚仍是 GitHub。
#
# 用法：
#   bash scripts/claw-cli-mirror-sync.sh --bundle /tmp/claw-cli-mirror-1.3.4.tar.gz
#       ↑ 主路径。本机 fetch 打好的包直接喂进来：自动解包、读包内 MANIFEST/VERSION、校验后激活。
#   bash scripts/claw-cli-mirror-sync.sh --from-dir /tmp/clawmirror    # 已自己解好包时用
#   bash scripts/claw-cli-mirror-sync.sh --from-dir /tmp/pkgs --tag clawmessenger-v1.3.4   # 手动标版本
#   bash scripts/claw-cli-mirror-sync.sh                     # 服务器能出网时，直接拉 latest 并激活
#   bash scripts/claw-cli-mirror-sync.sh --dry-run           # 只打印计划，不下载不改动
#   bash scripts/claw-cli-mirror-sync.sh --only clawmessenger-windows-x64.exe
#   bash scripts/claw-cli-mirror-sync.sh --verify-only       # 只校验当前 latest，不下载
#   bash scripts/claw-cli-mirror-sync.sh --mode copy         # webroot 里放真实目录（nginx 禁符号链接时用）
#
# 默认目录（按线上实际布局）：
#   --dir      /data/im/shrimp-says-test/claw-mirror        安装包存放处（webroot 同级，不在其内）
#   --webroot  /data/im/shrimp-says-test/claw-web-product   nginx 上 /claw-web/ 的 alias 目标
#   --link     dl                                          webroot 里的链接名 → <webroot>/dl
#   对外 URL： https://newsradar.dreamdt.cn/claw-web/dl/<asset>
#   （nginx 给 /claw-web/ 与 /claw-web/assets/ 各配了 alias；dl/ 走前者，无需改 nginx）
#
# 可用环境变量：CLAWMESSENGER_MIRROR_GH_BASE（默认 https://github.com）

set -euo pipefail

REPO="clawmessenger/claw-messenger"
GH_BASE="${CLAWMESSENGER_MIRROR_GH_BASE:-https://github.com}"
GH_LATEST="$GH_BASE/$REPO/releases/latest"
GH_DOWNLOAD="$GH_BASE/$REPO/releases/latest/download"

DIR="/data/im/shrimp-says-test/claw-mirror"
WEBROOT="/data/im/shrimp-says-test/claw-web-product"
LINK="dl"
MODE="link"
FROM_DIR=""
BUNDLE=""
SUMS=""
ONLY=""
FORCED_TAG=""
DRY_RUN=0
VERIFY_ONLY=0
KEEP_PREV=1

DEFAULT_ASSETS="clawmessenger-windows-x64.exe
clawmessenger-windows-x64-daemon-scripts.zip
clawmessenger-linux-x64
clawmessenger-linux-arm64
clawmessenger-linux-daemon-scripts.tar.gz
clawmessenger-darwin-arm64"

log()  { printf '%s\n' "$*"; }
warn() { printf 'warn: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dir)         DIR="${2:?--dir 需要参数}"; shift 2 ;;
    --webroot)     WEBROOT="${2:?--webroot 需要参数}"; shift 2 ;;
    --link)        LINK="${2:?--link 需要参数}"; shift 2 ;;
    --mode)        MODE="${2:?--mode 需要参数（link|copy）}"; shift 2 ;;
    --from-dir)    FROM_DIR="${2:?--from-dir 需要参数}"; shift 2 ;;
    --bundle)      BUNDLE="${2:?--bundle 需要参数}"; shift 2 ;;
    --sums)        SUMS="${2:?--sums 需要参数}"; shift 2 ;;
    --only)        ONLY="${2:?--only 需要参数}"; shift 2 ;;
    --tag)         FORCED_TAG="${2:?--tag 需要参数}"; shift 2 ;;
    --dry-run)     DRY_RUN=1; shift ;;
    --verify-only) VERIFY_ONLY=1; shift ;;
    --no-keep-prev) KEEP_PREV=0; shift ;;
    -h|--help)     sed -n '2,/^set -euo pipefail$/p' "$0" | sed '$d'; exit 0 ;;
    *) die "不认识的参数：$1（--help 看用法）" ;;
  esac
done

command -v curl >/dev/null 2>&1 || die "需要 curl"
if command -v sha256sum >/dev/null 2>&1; then
  SHA256_OF() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  SHA256_OF() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "需要 sha256sum 或 shasum"
fi

# 同一个脚本会被复制到 /usr/local/bin。跑的是 A、PATH 里是 B 时，报错会指向完全不相关的
# 旧行为（比如旧版不认 --from-dir、默认路径也不一样），所以先比一下。
INSTALLED="/usr/local/bin/claw-cli-mirror-sync"
if [ -f "$INSTALLED" ] && [ "$INSTALLED" != "$0" ]; then
  if [ "$(SHA256_OF "$INSTALLED")" != "$(SHA256_OF "$0")" ]; then
    warn "$INSTALLED 与当前脚本版本不同。建议先同步：install -m 755 \"$0\" $INSTALLED"
  fi
fi

TARGET="$DIR/latest"
PREV="$DIR/prev"

# ---------------------------------------------------------------- 吃本机打好的包
# --bundle 是主路径：服务器出不了网，包由本机 fetch 打好后上传。
# 解到 $DIR 下的临时目录（与镜像同分区），随后用硬链接拷进 staging，避免 432MB 被写两遍。
BUNDLE_ROOT=""
cleanup_bundle() { if [ -n "${BUNDLE_ROOT:-}" ]; then rm -rf "$BUNDLE_ROOT"; fi; return 0; }

if [ -n "$BUNDLE" ]; then
  [ -f "$BUNDLE" ] || die "--bundle 指向的文件不存在：$BUNDLE
      这个包由本机跑 claw-cli-mirror-fetch.ps1 产出，必须先上传到服务器（例如 /tmp）。"
  command -v tar >/dev/null 2>&1 || die "需要 tar"
  if [ "$DRY_RUN" -eq 1 ]; then
    log "dry-run：$BUNDLE 里是这些文件——"
    tar tzf "$BUNDLE" 2>/dev/null | sed 's/^/  /' || die "$BUNDLE 不是有效的 tar.gz（上传不完整？）"
    log ""
    log "确认无误后去掉 --dry-run 重跑：会自动解包、校验、激活。"
    exit 0
  fi
  mkdir -p "$DIR"
  BUNDLE_ROOT="$(mktemp -d "$DIR/.bundle.XXXXXX")"
  trap cleanup_bundle EXIT INT TERM
  log "解包 $BUNDLE → $BUNDLE_ROOT"
  tar xzf "$BUNDLE" -C "$BUNDLE_ROOT" \
    || die "解包失败：$BUNDLE
      多半是上传不完整（比对一下大小与 sha256），或包不是 tar.gz。"
  FROM_DIR="$BUNDLE_ROOT"
  if [ ! -f "$FROM_DIR/SHA256SUMS" ]; then
    # 包里多套了一层目录时往下找一层
    for d in "$BUNDLE_ROOT"/*/; do
      if [ -f "${d}SHA256SUMS" ]; then FROM_DIR="${d%/}"; break; fi
    done
  fi
  [ -f "$FROM_DIR/SHA256SUMS" ] || die "包里没有 SHA256SUMS，不是 fetch 脚本打出来的包：$BUNDLE"
fi

LATEST_ASSETS="$DEFAULT_ASSETS"
if [ -n "$ONLY" ]; then
  LATEST_ASSETS="$(printf '%s' "$ONLY" | tr ',' '\n')"
elif [ -n "$FROM_DIR" ] && [ -f "$FROM_DIR/MANIFEST" ]; then
  # 本机 fetch 脚本打出来的包自带 MANIFEST（只列它下到的那几个资产）。
  # 不读它的话，部分平台上线时必须两边手抄同一份 --only，抄漏一次就是"资产不齐"。
  MANIFEST_ASSETS="$(tr -d '\r' < "$FROM_DIR/MANIFEST" | sed '/^[[:space:]]*#/d; /^[[:space:]]*$/d')"
  if [ -n "$MANIFEST_ASSETS" ]; then
    LATEST_ASSETS="$MANIFEST_ASSETS"
    log "资产清单来自 $FROM_DIR/MANIFEST（$(printf '%s\n' "$LATEST_ASSETS" | wc -l | tr -d ' ') 个）"
  fi
fi

# 「最新版本」的两条来路：先读 releases/latest 的跳转（不消耗 API 额度），
# 失败再退回 assets 直链的跳转，两条都不行就报错——绝不在版本不明时切目录。
resolve_tag() {
  local url tag
  url="$(curl -sIL -o /dev/null -w '%{url_effective}' --max-time 25 "$GH_LATEST" 2>/dev/null || true)"
  tag="$(printf '%s' "$url" | sed -n 's#.*/releases/tag/\([^/?#]*\).*#\1#p')"
  if [ -n "$tag" ]; then printf '%s' "$tag"; return 0; fi
  url="$(curl -sIL -o /dev/null -w '%{url_effective}' --max-time 25 "$GH_DOWNLOAD/SHA256SUMS" 2>/dev/null || true)"
  tag="$(printf '%s' "$url" | sed -n 's#.*/releases/download/\([^/?#]*\)/.*#\1#p')"
  [ -n "$tag" ] || return 1
  printf '%s' "$tag"
}

# 校验一个目录里的资产。只校验「清单里有、目录里也有」的文件；
# 缺文件是否算错由调用方通过 required 列表判断。
verify_dir() {
  local d="$1" sums="$2" asset expected actual bad=0
  [ -f "$sums" ] || { warn "找不到哈希清单：$sums"; return 1; }
  while IFS= read -r asset; do
    [ -n "$asset" ] || continue
    [ -f "$d/$asset" ] || continue
    expected="$(awk -v a="$asset" '$2 == a { print $1; exit }' "$sums")"
    [ -n "$expected" ] || { warn "清单里没有 $asset，无法校验"; bad=1; continue; }
    actual="$(SHA256_OF "$d/$asset")"
    if [ "$actual" != "$expected" ]; then
      warn "哈希不符：$asset"
      warn "  expected $expected"
      warn "  actual   $actual"
      bad=1
    fi
  done <<EOF
$LATEST_ASSETS
EOF
  return $bad
}

require_dir() {
  local d="$1" missing=0 asset
  while IFS= read -r asset; do
    [ -n "$asset" ] || continue
    if [ ! -f "$d/$asset" ]; then warn "缺少 ${asset}（清单要求每个默认资产都存在）"; missing=1; fi
  done <<EOF
$LATEST_ASSETS
EOF
  return $missing
}

# 从 --from-dir / --bundle 取文件进 staging。优先硬链接：两处都在 $DIR 下、
# 同一文件系统，432MB 只需一份真实数据；跨设备或权限不允许时退回普通复制。
copy_in() {
  cp -l "$1" "$2" 2>/dev/null || cp "$1" "$2"
}

say_dir() {
  log "目录配置："
  log "  安装包  $DIR"
  log "  对外链接 $WEBROOT/$LINK -> $TARGET（$MODE）"
  log "  资产    $(printf '%s' "$LATEST_ASSETS" | tr '\n' ' ')"
}

case "$MODE" in
  link|copy) ;;
  *) die "--mode 只支持 link 或 copy" ;;
esac

# 把 latest 暴露到 webroot 下。默认符号链接（不占盘、切换即生效）；
# 有些 nginx 开了 disable_symlinks，访问会 403 —— 那种机器改用 --mode copy
# （优先硬链接，跨文件系统或权限不允许时退回复制）。
publish() {
  mkdir -p "$WEBROOT"
  if [ "$MODE" = "link" ]; then
    if ! ln -sfn "$TARGET" "$WEBROOT/$LINK"; then
      die "创建符号链接失败：$WEBROOT/$LINK。文件已就位在 $TARGET；
     可以改用 --mode copy 发布，或手动建链接后重跑。"
    fi
    log "已发布：$WEBROOT/$LINK -> $TARGET"
    return 0
  fi
  rm -rf "$WEBROOT/$LINK"
  mkdir -p "$WEBROOT/$LINK"
  if ! cp -al "$TARGET/." "$WEBROOT/$LINK/" 2>/dev/null; then
    cp -a "$TARGET/." "$WEBROOT/$LINK/"
  fi
  log "已发布（复制）：$WEBROOT/$LINK"
}

# ---------------------------------------------------------------- verify-only
if [ "$VERIFY_ONLY" -eq 1 ]; then
  [ -d "$TARGET" ] || die "$TARGET 不存在，先跑一次同步"
  [ -f "$TARGET/SHA256SUMS" ] || die "$TARGET/SHA256SUMS 不存在"
  say_dir
  if verify_dir "$TARGET" "$TARGET/SHA256SUMS"; then
    log "OK  $(cat "$TARGET/VERSION" 2>/dev/null || echo '未知版本') 全部资产校验通过"
    exit 0
  fi
  die "校验未通过"
fi

# ---------------------------------------------------------------- 取版本号
TAG=""
if [ -z "$FROM_DIR" ]; then
  log "查询 GitHub 最新版本…"
  TAG="$(resolve_tag || true)"
  if [ -z "$TAG" ]; then
    warn "拿不到最新版本号 —— 这台机器出不了网，联网模式不可用（不是包的问题）。"
    log ""
    if ls /tmp/claw-cli-mirror-*.tar.gz >/dev/null 2>&1; then
      log "不过 /tmp 里已经有打好的包，直接喂给它就行，不用手工解包："
      for f in /tmp/claw-cli-mirror-*.tar.gz; do
        log "  $0 --bundle $f"
      done
      log ""
      die "把上面那条命令改成带 --bundle 重跑即可（本次未做任何改动）"
    fi
    log "当前这台机器上没有可用的安装包。正确顺序是："
    log "  1) 本机（能上网的那台）跑 claw-cli-mirror-fetch.ps1，产出 claw-cli-mirror-<版本>.tar.gz"
    log "  2) 把这个包上传到服务器，例如 /tmp/"
    log "  3) $0 --bundle /tmp/claw-cli-mirror-<版本>.tar.gz"
    die "没有可用的安装包，未做任何改动"
  fi
  log "  latest = $TAG"
fi

if [ -n "$FROM_DIR" ]; then
  [ -d "$FROM_DIR" ] || die "--from-dir 指向的目录不存在：$FROM_DIR"
  [ -f "$FROM_DIR/SHA256SUMS" ] || die "$FROM_DIR 里没有 SHA256SUMS（可用 --sums 指定别处的清单）"
fi

# ---------------------------------------------------------------- 幂等短路
CURRENT_TAG="$(cat "$TARGET/VERSION" 2>/dev/null || true)"
if [ -z "$FROM_DIR" ] && [ "$CURRENT_TAG" = "$TAG" ] && [ -d "$TARGET" ]; then
  if verify_dir "$TARGET" "$TARGET/SHA256SUMS" && require_dir "$TARGET"; then
    log "已是最新（$TAG），资产完好，无需重复下载。"
    if [ ! -e "$WEBROOT/$LINK" ] && [ "$DRY_RUN" -eq 0 ]; then
      publish
    fi
    exit 0
  fi
  warn "版本号是最新，但本地资产不完整或校验不过，重新拉取。"
fi

say_dir
[ "$DRY_RUN" -eq 1 ] && { log "dry-run：只打印计划，不下载。"; exit 0; }

# ---------------------------------------------------------------- 准备临时目录
mkdir -p "$DIR"
# nginx 的 worker 用户要能穿过这个目录（符号链接指向它）。umask 077 的 root
# 建目录会是 700，届时线上是 403 而不是 404，很难看出是权限问题。
chmod 755 "$DIR"
STAGE="$(mktemp -d "$DIR/.incoming.XXXXXX")"
cleanup() {
  # 注意两个都用 if：set -e 下 `[ ... ] && cmd` 返回 1 会让 trap 带着非零状态退出。
  if [ -n "${STAGE:-}" ]; then rm -rf "$STAGE"; fi
  cleanup_bundle
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------- 取哈希清单
if [ -n "$SUMS" ]; then
  [ -f "$SUMS" ] || die "--sums 指向的文件不存在：$SUMS"
  cp "$SUMS" "$STAGE/SHA256SUMS"
  log "哈希清单来自 $SUMS"
elif [ -n "$FROM_DIR" ]; then
  copy_in "$FROM_DIR/SHA256SUMS" "$STAGE/SHA256SUMS"
  log "哈希清单来自 $FROM_DIR/SHA256SUMS"
else
  log "下载 SHA256SUMS…"
  curl -fL --retry 3 --connect-timeout 20 --max-time 120 \
    -o "$STAGE/SHA256SUMS" "$GH_DOWNLOAD/SHA256SUMS" \
    || die "SHA256SUMS 下载失败"
fi

# ---------------------------------------------------------------- 取资产
while IFS= read -r asset; do
  [ -n "$asset" ] || continue
  if [ -n "$FROM_DIR" ]; then
    [ -f "$FROM_DIR/$asset" ] || die "$FROM_DIR 里缺少 $asset"
    copy_in "$FROM_DIR/$asset" "$STAGE/$asset"
    log "  复制 $asset"
  else
    log "  下载 $asset …"
    curl -fL --progress-bar --retry 3 --connect-timeout 20 \
      -o "$STAGE/$asset" "$GH_DOWNLOAD/$asset" \
      || die "$asset 下载失败"
  fi
done <<EOF
$LATEST_ASSETS
EOF

# ---------------------------------------------------------------- 校验后再切
log "校验哈希…"
verify_dir "$STAGE" "$STAGE/SHA256SUMS" || die "校验未通过，已保留现有版本，未做任何切换"
require_dir "$STAGE" || die "资产不齐，未做任何切换"

if [ -z "$TAG" ]; then
  # 离线激活时的版本号，优先级：--tag → 包内 VERSION（本机 fetch 写的）→ 清单里的注释 → unknown。
  # 离线场景下推不出版本号是常态，所以 --tag / VERSION 必须有，否则 dl/VERSION 会显示 unknown，
  # 用户端第 1 步拿备用源比对版本时就失去了依据。
  TAG="$FORCED_TAG"
  if [ -z "$TAG" ] && [ -n "$FROM_DIR" ] && [ -f "$FROM_DIR/VERSION" ]; then
    TAG="$(tr -d ' \t\r\n' < "$FROM_DIR/VERSION")"
    if [ -n "$TAG" ]; then log "版本号来自 $FROM_DIR/VERSION：$TAG"; fi
  fi
  if [ -z "$TAG" ]; then
    TAG="$(sed -n 's#.*/releases/tag/\([^/?#]*\).*#\1#p' "$STAGE/SHA256SUMS" 2>/dev/null | head -1)"
  fi
  if [ -z "$TAG" ]; then
    TAG="unknown"
    warn "版本号未知，dl/VERSION 会写成 unknown；下次请带 --tag clawmessenger-vX.Y.Z"
  fi
fi
printf '%s\n' "$TAG" > "$STAGE/VERSION"

# 权限：nginx 只需要读。目录 755、文件 644，别的用户也能取。
chmod 755 "$STAGE"
find "$STAGE" -type f -exec chmod 644 {} +

# 切换：latest → prev（留一代可回滚），再 stage → latest。两步 rename，几乎无窗口。
if [ -d "$TARGET" ]; then
  if [ "$KEEP_PREV" -eq 1 ]; then
    rm -rf "$PREV"
    mv "$TARGET" "$PREV"
  else
    rm -rf "$TARGET"
  fi
fi
mv "$STAGE" "$TARGET"
STAGE=""   # 交给 trap 之前先清空，别把刚切上去的目录删了

publish

log ""
log "完成：$TAG"
while IFS= read -r asset; do
  [ -n "$asset" ] || continue
  [ -f "$TARGET/$asset" ] || continue
  size="$(wc -c < "$TARGET/$asset" | tr -d ' ')"
  printf '  %-46s %10s bytes  sha256:%s\n' "$asset" "$size" "$(SHA256_OF "$TARGET/$asset" | cut -c1-12)"
done <<EOF
$LATEST_ASSETS
EOF
log ""
log "对外地址： $WEBROOT/$LINK/  ->  https://<你的域名>/claw-web/$LINK/<asset>"
log "回滚：     mv $PREV $TARGET 反向覆盖即可（上一代保留在 $PREV）"
log "自检：     curl -sI https://<你的域名>/claw-web/$LINK/SHA256SUMS"
