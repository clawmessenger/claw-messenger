#!/usr/bin/env bash
# restart-server.sh — 重新构建并「就地」重启 claw-messenger 的 Go 服务端。
#
# 为什么不直接 kill + ./bin/server：
#   1) 服务端不读取 .env（代码里没有 dotenv），全部配置来自进程环境变量，
#      其中就包括监听端口 PORT（代码缺省 5101）。凭记忆重敲一遍很容易把
#      端口 / 数据库地址敲错，所以这里直接从正在运行的进程
#      /proc/<pid>/environ 原样继承环境。
#   2) Linux 不允许用 go build -o 直接覆盖正在执行的二进制（ETXTBSY），
#      因此先构建到 bin/server.new，停进程后再覆盖。
#   3) 用 setsid + nohup 启动，避免终端一关服务跟着退出。
#
# 用法：
#   bash scripts/restart-server.sh                # 自动识别运行中的实例
#   bash scripts/restart-server.sh --pid 3688101  # 同机多实例时显式指定
#   bash scripts/restart-server.sh --skip-build   # 跳过重建，只重启（用现有二进制）
#   bash scripts/restart-server.sh --fg           # 前台运行（保持原来的终端方式）
#
# 可用环境变量覆盖：
#   VERSION    写入二进制的版本号，默认 0.2.0
#   LOG_FILE   后台日志文件，默认 /var/log/clawmessenger.log
#   MATCH      pgrep 匹配的服务名，默认 bin/server
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVER_DIR="$REPO_ROOT/server"
VERSION="${VERSION:-0.2.0}"
LOG_FILE="${LOG_FILE:-/var/log/clawmessenger.log}"
MATCH="${MATCH:-bin/server}"

TARGET_PID=""
SKIP_BUILD=0
FOREGROUND=0

while [ $# -gt 0 ]; do
  case "$1" in
    --pid)        TARGET_PID="${2:-}"; shift 2 ;;
    --pid=*)      TARGET_PID="${1#*=}"; shift ;;
    --skip-build) SKIP_BUILD=1; shift ;;
    --fg|--foreground) FOREGROUND=1; shift ;;
    -h|--help)    sed -n '2,22p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)            echo "未知参数：$1（-h 看用法）" >&2; exit 2 ;;
  esac
done

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[!]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------- 1. 找到正在运行的实例 ----------
if [ -z "$TARGET_PID" ]; then
  mapfile -t ALL < <(pgrep -f "$MATCH" || true)
  CAND=()
  for p in ${ALL[@]+"${ALL[@]}"}; do
    [ "$p" = "$$" ] && continue
    [ -d "/proc/$p" ] || continue
    CAND+=("$p")
  done
  case "${#CAND[@]}" in
    0) die "没找到匹配 '$MATCH' 的运行中进程。
     确认服务是否在跑：pgrep -af '$MATCH'
     或显式指定：bash scripts/restart-server.sh --pid <pid>" ;;
    1) TARGET_PID="${CAND[0]}" ;;
    *) echo "匹配到多个进程，请用 --pid 指定其一："
       ps -o pid,ppid,lstart,args -p "$(IFS=,; echo "${CAND[*]}")" 2>/dev/null || printf '%s\n' "${CAND[@]}"
       exit 1 ;;
  esac
fi

[ -d "/proc/$TARGET_PID" ] || die "pid=$TARGET_PID 不存在"
PID="$TARGET_PID"

EXE="$(readlink -f "/proc/$PID/exe" 2>/dev/null)" || die "读不到 /proc/$PID/exe（需要 root 或同属主）"
CWD="$(readlink -f "/proc/$PID/cwd" 2>/dev/null)" || die "读不到 /proc/$PID/cwd"
EXE="${EXE% (deleted)}"
[ -n "$CWD" ] || CWD="$REPO_ROOT"

info "运行中实例 pid=$PID"
info "  二进制 exe = $EXE"
info "  工作目录 cwd = $CWD"

# ---------- 2. 备份运行环境（端口等全部配置都在这里） ----------
ENV_SNAP="$(mktemp /tmp/claw-restart-env.XXXXXX)"
chmod 600 "$ENV_SNAP"
cat "/proc/$PID/environ" > "$ENV_SNAP" 2>/dev/null || true
if [ ! -s "$ENV_SNAP" ]; then
  rm -f "$ENV_SNAP"
  die "无法读取 pid=$PID 的环境变量，拒绝盲重启（会丢端口/库配置）。
     请确认当前是 root 或该进程属主，并保持进程存活后重试。"
fi

env_line() { tr '\0' '\n' < "$ENV_SNAP" | sed -n "s/^$1=//p" | head -1; }
PORT_VAL="$(env_line PORT)"
info "  端口 PORT = ${PORT_VAL:-<未设置，代码缺省 5101>}"
info "  数据库 DATABASE_URL = $(env_line DATABASE_URL | sed -E 's#(://[^:/@]*:)[^@]*@#\1****@#')"
info "  融云密钥 = $([ -n "$(env_line MULTICA_RONGCLOUD_SECRET_KEY)" ] && echo 已配置 || echo '<未设置：设备绑定/好友列表会失效>')"

# ---------- 3. 重新构建 ----------
NEW_BIN=""
if [ "$SKIP_BUILD" = 0 ]; then
  command -v go >/dev/null 2>&1 || die "未找到 go，无法重建。
    二选一：
      1) 装好 Go 后重试；
      2) 用现成的 Linux 二进制（仓库 server/bin/server-linux-new），
         复制到 $EXE 后执行：bash scripts/restart-server.sh --skip-build"
  COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  info "构建 $SERVER_DIR/bin/server.new（version=$VERSION commit=$COMMIT）"
  ( cd "$SERVER_DIR" && go build -ldflags "-X main.version=$VERSION -X main.commit=$COMMIT" -o bin/server.new ./cmd/server )
  NEW_BIN="$SERVER_DIR/bin/server.new"
  [ -s "$NEW_BIN" ] || die "构建失败：$NEW_BIN 为空"
else
  info "跳过构建（--skip-build），直接重启现有二进制"
fi

# ---------- 4. 停止旧进程 ----------
info "停止旧进程 pid=$PID ..."
kill "$PID" 2>/dev/null || true
for _ in $(seq 1 30); do
  kill -0 "$PID" 2>/dev/null || break
  sleep 1
done
if kill -0 "$PID" 2>/dev/null; then
  warn "30s 宽限期结束，强制结束"
  kill -9 "$PID" 2>/dev/null || true
  sleep 1
fi
kill -0 "$PID" 2>/dev/null && die "无法结束 pid=$PID，请手动处理"

# ---------- 5. 替换二进制 ----------
if [ -n "$NEW_BIN" ]; then
  BACKUP="${EXE}.bak.$(date +%Y%m%d-%H%M%S)"
  cp -f "$EXE" "$BACKUP" 2>/dev/null || warn "备份旧二进制失败（忽略）"
  install -m 0755 "$NEW_BIN" "$EXE" && rm -f "$NEW_BIN"
  info "已替换二进制（旧版备份：$BACKUP）"
fi

# ---------- 6. 用原环境重启 ----------
cd "$CWD" || die "无法进入工作目录：$CWD"

while IFS= read -r -d '' kv; do
  case "${kv%%=*}" in
    PWD|OLDPWD|SHLVL|_|BASH_EXECUTION_STRING) continue ;;
  esac
  export "$kv"
done < "$ENV_SNAP" || true
rm -f "$ENV_SNAP"

if [ "$FOREGROUND" = 1 ]; then
  info "前台启动（Ctrl-C 结束）：$EXE"
  exec "$EXE"
fi

info "后台启动，日志 -> $LOG_FILE"
setsid nohup "$EXE" >>"$LOG_FILE" 2>&1 </dev/null &
sleep 3

NEWPID="$(pgrep -f "$MATCH" | head -1 || true)"
if [ -z "$NEWPID" ]; then
  warn "进程没有起来，最近日志："
  tail -n 30 "$LOG_FILE" 2>/dev/null || true
  exit 1
fi
info "已启动，新 pid=$NEWPID"
info "健康检查："
curl -s -o /dev/null -w '  http://127.0.0.1:%{remote_port} => http=%{http_code}\n' \
  "http://127.0.0.1:${PORT_VAL:-5101}/healthz" 2>/dev/null || \
  warn "curl 不可用，请手动检查 http://127.0.0.1:${PORT_VAL:-5101}/healthz"
info "完成。"
