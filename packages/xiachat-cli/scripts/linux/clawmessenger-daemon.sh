#!/usr/bin/env bash
#
# clawmessenger 后台守护工具（Linux / macOS）
#
# 为什么需要它：
#   `clawmessenger run`（不带 --agent 就是 supervisor 模式）只有前台模式，
#   进程一退，它拉起的所有 agent 子进程一起消失 -> 智能体全部下线。
#   CLI 里没有 --daemon / service 之类的参数，所以只能在 OS 层把它放到后台。
#   另外 supervisor 只处理 SIGINT / SIGTERM，**不处理 SIGHUP** —— 所以关掉终端
#   它就下线，必须靠 systemd（或 setsid 脱离会话）托管。
#
# 动作（与 Windows 版 clawmessenger-daemon.ps1 同名同义）：
#   start             后台启动 supervisor（装了 systemd 服务时走 systemctl，否则 setsid + nohup）
#   stop              停止 supervisor（supervisor 会先把它拉起的 agent 一起优雅停掉）
#   restart           停掉再后台起
#   status            看有没有在跑、PID、起了几个 agent 子进程、自启有没有配
#   logs              看最新日志（装了 systemd 服务时读 journal，否则读日志文件）
#   run               前台阻塞运行（给 systemd / 其它进程管理器内部调用，不建议手动用）
#   install-service   装 systemd 服务（开机自启 + 崩溃自动重试）；
#                     root 写系统级 unit，非 root 写 --user unit 并尝试 enable-linger
#   uninstall         摘掉 systemd 服务（不删可执行文件、不删日志）
#
# 用法：
#   ./clawmessenger-daemon.sh start
#   ./clawmessenger-daemon.sh status
#
# 可选参数（跟在动作后面）：
#   --exe <路径>       显式指定 clawmessenger（默认从 PATH / ~/.local/bin 找）
#   --model <名字>     透传给 `clawmessenger run --model <名字>`
#   --workdir <路径>   supervisor 的工作目录，也是 agent 轮次的 cwd（默认 = $HOME）
#   --name <服务名>    systemd 服务名，默认 clawmessenger
#
# 注意：必须在**同一个 Linux 用户**下运行。凭据在 $HOME/.clawmessenger ，
# 换成别的用户（例如 pair 用的是普通用户、却用 root 跑服务）会找不到凭据、
# 或者注册成另一批节点。
#
# 平台说明：Linux 上 install-service 用 systemd（root -> 系统级 unit，非 root -> --user unit）。
# macOS 没有 systemd，只支持 start / stop / restart / status / logs / run
# （靠 nohup + disown 脱离终端；macOS 无 setsid，脚本会自动退化）。
#
set -euo pipefail

SERVICE_NAME="clawmessenger"
ACTION="${1:-status}"
if [ "$#" -gt 0 ]; then shift; fi

EXE=""
MODEL=""
WORKDIR=""

need_value() {
  [ "$#" -ge 2 ] || { echo "error: $1 需要一个值" >&2; exit 2; }
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --exe)      need_value "$@"; EXE="$2";     shift 2 ;;
    --model)    need_value "$@"; MODEL="$2";   shift 2 ;;
    --workdir)  need_value "$@"; WORKDIR="$2"; shift 2 ;;
    --name)     need_value "$@"; SERVICE_NAME="$2"; shift 2 ;;
    -h|--help)  ACTION="help"; shift ;;
    *)          echo "error: 未知参数 $1（用 --help 看用法）" >&2; exit 2 ;;
  esac
done

# ---------------------------------------------------------------- 常量与工具

STATE_DIR="${CLAWMESSENGER_HOME:-$HOME/.clawmessenger}"
LOG_DIR="${STATE_DIR}/logs"
PID_FILE="${STATE_DIR}/supervisor.pid"

SYSTEM_UNIT="/etc/systemd/system/${SERVICE_NAME}.service"
USER_UNIT="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/${SERVICE_NAME}.service"

if [ -t 1 ]; then
  C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_OFF=$'\033[0m'
else
  C_OK=''; C_WARN=''; C_ERR=''; C_OFF=''
fi
ok()   { printf '%s%s%s\n' "$C_OK"   "$1" "$C_OFF"; }
warn() { printf '%s%s%s\n' "$C_WARN" "$1" "$C_OFF"; }
err2() { printf '%s%s%s\n' "$C_ERR"  "$1" "$C_OFF"; }
die()  { err2 "$1"; exit 1; }

# 透传给 run 的 --model。刻意用未加引号的变量拼串（model 名字不含空格），
# 这样不需要 bash 4.4 才支持的「空数组展开」写法。
MODEL_ARG=""
if [ -n "$MODEL" ]; then MODEL_ARG="--model ${MODEL}"; fi
# shellcheck disable=SC2086

usage() {
  cat <<'USAGE'
clawmessenger 后台守护工具（Linux / macOS）

用法：
  clawmessenger-daemon.sh <动作> [--exe <路径>] [--model <名字>] [--workdir <路径>] [--name <服务名>]

动作：
  start             后台启动 supervisor
  stop              停止 supervisor（含它拉起的 agent 子进程）
  restart           停掉再后台起
  status            运行状态、PID、agent 子进程、自启配置、日志位置
  logs              看最新日志（systemd 服务读 journal，否则读日志文件）
  run               前台阻塞运行（给 systemd / 进程管理器内部调用）
  install-service   安装 systemd 服务（开机自启 + 崩溃自动重启）
  uninstall         移除 systemd 服务（不删 exe、不删日志）

状态文件：
  $HOME/.clawmessenger/supervisor.pid     supervisor 的 PID
  $HOME/.clawmessenger/logs/              supervisor 日志（非 systemd 模式下）
USAGE
}

# ---------------------------------------------------------------- 路径解析

resolve_exe() {
  if [ -n "$EXE" ]; then
    [ -x "$EXE" ] || die "--exe 指定的文件不存在或不可执行: $EXE"
    printf '%s\n' "$EXE"
    return 0
  fi
  local p
  p="$(type -P clawmessenger 2>/dev/null || true)"
  if [ -n "$p" ] && [ -x "$p" ]; then printf '%s\n' "$p"; return 0; fi
  for p in "$HOME/.local/bin/clawmessenger" "/usr/local/bin/clawmessenger" "/usr/bin/clawmessenger"; do
    if [ -x "$p" ]; then printf '%s\n' "$p"; return 0; fi
  done
  die "找不到 clawmessenger。请先在这台设备上执行 clawmessenger pair，或用 --exe <路径> 指定。"
}

resolve_workdir() {
  local exe="$1"
  if [ -n "$WORKDIR" ]; then
    [ -d "$WORKDIR" ] || die "--workdir 指定的目录不存在: $WORKDIR"
    printf '%s\n' "$WORKDIR"
    return 0
  fi
  # 默认用家目录：agent 轮次会以它为 cwd，放在 ~/.local/bin 里不合适
  printf '%s\n' "$HOME"
}

# ---------------------------------------------------------------- 进程探测

cmdline_of() {
  local pid="$1"
  if [ -r "/proc/${pid}/cmdline" ]; then
    tr '\0' ' ' < "/proc/${pid}/cmdline" 2>/dev/null || true
  else
    ps -p "$pid" -o command= 2>/dev/null || true
  fi
}

pid_alive() { kill -0 "$1" 2>/dev/null; }

# supervisor 的 argv 是 `clawmessenger run`；它拉起的 agent 子进程是
# `clawmessenger run --agent <名字>`。两者只能靠命令行特征区分。
is_supervisor_cmd() {
  local c="$1"
  case "$c" in *clawmessenger-daemon*) return 1 ;; esac
  case "$c" in *clawmessenger*)        ;; *) return 1 ;; esac
  case " ${c} " in *" run "*)          ;; *) return 1 ;; esac
  case "$c" in *--agent*)              return 1 ;; esac
  return 0
}

scan_supervisor() {
  [ -d /proc ] || return 1
  local d pid c
  for d in /proc/[0-9]*; do
    pid="${d#/proc/}"
    c="$(cmdline_of "$pid")"
    [ -n "$c" ] || continue
    if is_supervisor_cmd "$c"; then printf '%s\n' "$pid"; return 0; fi
  done
  return 1
}

# pid 文件优先（必须是活着的、且命令行对得上，避免 PID 被回收后误判）；
# 没有 pid 文件时（历史上手动启动过）回退到按命令行特征扫描。
find_supervisor() {
  if [ -f "$PID_FILE" ]; then
    local pid
    pid="$(tr -dc '0-9' < "$PID_FILE" 2>/dev/null || true)"
    if [ -n "$pid" ] && pid_alive "$pid" && is_supervisor_cmd "$(cmdline_of "$pid")"; then
      printf '%s\n' "$pid"
      return 0
    fi
  fi
  scan_supervisor
}

# 每个 agent 子进程输出一行：`<pid> <cmdline>`
list_agent_children() {
  [ -d /proc ] || return 0
  local d pid c
  for d in /proc/[0-9]*; do
    pid="${d#/proc/}"
    c="$(cmdline_of "$pid")"
    [ -n "$c" ] || continue
    case "$c" in *clawmessenger*) ;; *) continue ;; esac
    case "$c" in *--agent*)       ;; *) continue ;; esac
    printf '%s %s\n' "$pid" "$c"
  done
}

# ---------------------------------------------------------------- systemd

unit_installed() { [ -f "$SYSTEM_UNIT" ] || [ -f "$USER_UNIT" ]; }

# 装了用户级 unit 就用 `systemctl --user`，否则用系统级。
sctl() {
  if [ -f "$USER_UNIT" ] && [ ! -f "$SYSTEM_UNIT" ]; then
    systemctl --user "$@"
  else
    systemctl "$@"
  fi
}

sctl_user_scope() { [ -f "$USER_UNIT" ] && [ ! -f "$SYSTEM_UNIT" ]; }

# ---------------------------------------------------------------- 动作实现

do_start() {
  local exe; exe="$(resolve_exe)"

  if unit_installed && command -v systemctl >/dev/null 2>&1; then
    echo "检测到 systemd 服务 ${SERVICE_NAME}.service，交给 systemctl 启动。"
    sctl start "$SERVICE_NAME"
    sctl --no-pager --full status "$SERVICE_NAME" 2>/dev/null | head -n 12 || true
    return 0
  fi

  local running; running="$(find_supervisor || true)"
  if [ -n "$running" ]; then
    warn "检测到已有 supervisor 在运行（PID ${running}），不重复启动 —— 两个 supervisor 会重复拉起同一批 agent。"
    echo "  想改成后台常驻模式：先 stop，再 start。"
    return 0
  fi

  mkdir -p "$LOG_DIR"
  local stamp out err wd
  stamp="$(date +%Y%m%d-%H%M%S)"
  out="${LOG_DIR}/supervisor-${stamp}.out.log"
  err="${LOG_DIR}/supervisor-${stamp}.err.log"
  wd="$(resolve_workdir "$exe")"

  # setsid 把它放进新会话（脱离控制终端，关终端不再牵连），
  # nohup 让 SIGHUP 被忽略 —— supervisor 自己不处理 SIGHUP，缺了这层关窗即掉线。
  # macOS 没有 setsid，退化成 nohup + disown（一样能活过终端退出）。
  ( cd "$wd" || exit 1
    if command -v setsid >/dev/null 2>&1; then
      # shellcheck disable=SC2086
      setsid nohup "$exe" run $MODEL_ARG >>"$out" 2>>"$err" </dev/null &
    else
      # shellcheck disable=SC2086
      nohup "$exe" run $MODEL_ARG >>"$out" 2>>"$err" </dev/null &
      disown 2>/dev/null || true
    fi )

  sleep 3
  local pid; pid="$(find_supervisor || true)"
  if [ -z "$pid" ]; then
    err2 "启动后没有检测到 supervisor 进程。日志："
    echo "  ${out}"
    if [ -f "$err" ]; then tail -n 20 "$err" | sed 's/^/  /'; fi
    return 1
  fi

  printf '%s\n' "$pid" > "$PID_FILE"
  ok "已后台启动：supervisor PID ${pid}"
  echo "  工作目录 : ${wd}"
  echo "  日志     : ${out}"
  echo "  查看状态 : $0 status ；停止：$0 stop"
  echo "  提示：心跳/上线要几秒到十几秒，稍后在管理端刷新设备列表。"
}

do_stop() {
  if unit_installed && command -v systemctl >/dev/null 2>&1; then
    sctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
  fi

  local pid; pid="$(find_supervisor || true)"
  if [ -z "$pid" ]; then
    rm -f "$PID_FILE"
    warn '没有在运行的 clawmessenger supervisor。'
    return 0
  fi

  local kids; kids="$(list_agent_children || true)"
  local kid_count=0
  if [ -n "$kids" ]; then kid_count="$(printf '%s\n' "$kids" | grep -c . || true)"; fi

  echo "正在停止 supervisor PID ${pid}（连带 ${kid_count} 个 agent 子进程）..."
  # 与 Windows 不同，Linux 有真正的 POSIX 信号：发 SIGTERM 让 supervisor
  # 走优雅关闭（它会先给自己拉起的 agent 发 SIGTERM 再退出）。
  kill -TERM "$pid" 2>/dev/null || true

  local i
  for i in $(seq 1 30); do
    pid_alive "$pid" || break
    sleep 0.5
  done
  if pid_alive "$pid"; then
    warn '优雅退出超时（约 15 秒），改用 SIGKILL。'
    kill -KILL "$pid" 2>/dev/null || true
    sleep 1
  fi
  rm -f "$PID_FILE"

  # 兜底：正常情况下 supervisor 已经停掉子进程；若它是被 SIGKILL 的，
  # 子进程可能变成孤儿，这里再扫一遍收尾。
  local leftovers="" cpid
  leftovers="$(list_agent_children 2>/dev/null | awk '{print $1}' || true)"
  for cpid in $leftovers; do
    kill -TERM "$cpid" 2>/dev/null || true
  done

  if pid_alive "$pid"; then
    err2 "停止失败，PID ${pid} 仍然存在。可以再执行一次。"
  else
    ok '已停止。子进程心跳停下后，管理端大约 90 秒后显示离线。'
  fi
}

do_restart() {
  do_stop
  sleep 2
  do_start
}

do_status() {
  local exe=""; exe="$(resolve_exe 2>/dev/null || true)"
  if [ -n "$exe" ]; then
    echo "exe      : ${exe}"
  else
    warn 'exe      : 未找到 clawmessenger'
  fi

  echo '自启     :'
  local found=0
  if [ -f "$SYSTEM_UNIT" ]; then
    echo "  systemd 系统服务「${SERVICE_NAME}」（${SYSTEM_UNIT}）"
    found=1
  fi
  if [ -f "$USER_UNIT" ]; then
    echo "  systemd 用户服务「${SERVICE_NAME}」（${USER_UNIT}）"
    found=1
  fi
  if [ "$found" -eq 0 ]; then
    echo "  未配置（用 $0 install-service 配置开机自启）"
  fi

  if unit_installed && command -v systemctl >/dev/null 2>&1; then
    local act en
    act="$(sctl is-active "$SERVICE_NAME" 2>/dev/null || true)"
    en="$(sctl is-enabled "$SERVICE_NAME" 2>/dev/null || true)"
    echo "服务状态 : ${act:-unknown} / 开机自启 ${en:-unknown}"
  fi

  local pid; pid="$(find_supervisor || true)"
  if [ -z "$pid" ]; then
    warn '运行状态 : 未在后台运行（智能体应为离线）'
    return 0
  fi

  local up=""
  if ! unit_installed; then
    up="$(ps -o etime= -p "$pid" 2>/dev/null | tr -d ' ' || true)"
  fi
  ok "运行状态 : 后台运行中（PID ${pid}${up:+，已运行 ${up}}）"

  if [ ! -f "$PID_FILE" ] && ! unit_installed; then
    warn '  注意：没有 pid 文件，说明它可能是在终端里直接前台启动的（关掉那个终端就会掉线）。'
    warn "  想让它真正脱离终端常驻：先 stop，再 start。"
  fi

  local kids; kids="$(list_agent_children || true)"
  local cnt=0
  if [ -n "$kids" ]; then cnt="$(printf '%s\n' "$kids" | grep -c . || true)"; fi
  echo "agent 子进程 : ${cnt} 个"
  if [ -n "$kids" ]; then printf '%s\n' "$kids" | sed 's/^/  - PID /'; fi
  echo "日志目录 : ${LOG_DIR}"
}

do_logs() {
  if unit_installed && command -v journalctl >/dev/null 2>&1; then
    echo "（systemd 服务日志，最近 40 行；持续跟踪用 journalctl -f）"
    if sctl_user_scope; then
      journalctl --user -u "$SERVICE_NAME" -n 40 --no-pager || true
    else
      journalctl -u "$SERVICE_NAME" -n 40 --no-pager || true
    fi
    return 0
  fi
  if [ ! -d "$LOG_DIR" ]; then
    warn "还没有日志目录：${LOG_DIR}"
    return 0
  fi
  local file
  file="$(ls -1t "${LOG_DIR}"/supervisor-*.log 2>/dev/null | head -n 1 || true)"
  if [ -z "$file" ]; then
    warn '没有找到 supervisor 日志。'
    return 0
  fi
  echo "最新日志：${file}"
  tail -n 40 "$file"
}

# 前台阻塞运行：给 systemd / 其它进程管理器内部调用。
# 用 exec 替换掉本 shell，PID 不变 —— 于是 pid 文件里记的就是 supervisor 自己的 PID。
do_run() {
  local exe; exe="$(resolve_exe)"
  mkdir -p "$LOG_DIR"
  local wd; wd="$(resolve_workdir "$exe")"
  cd "$wd" || die "无法进入工作目录: ${wd}"
  printf '%s\n' "$$" > "$PID_FILE"
  # shellcheck disable=SC2086
  exec "$exe" run $MODEL_ARG
}

do_install_service() {
  command -v systemctl >/dev/null 2>&1 || die '这台机器上没有 systemd（找不到 systemctl）。请直接用 start 动作后台启动，或用其它进程管理器托管。'

  local exe wd unit_path scope wanted need_sudo
  exe="$(resolve_exe)"
  wd="$(resolve_workdir "$exe")"
  if [ "$(id -u)" -eq 0 ]; then
    scope='system'; unit_path="$SYSTEM_UNIT"; wanted='multi-user.target'; need_sudo=''
  else
    scope='user';   unit_path="$USER_UNIT";   wanted='default.target';   need_sudo='1'
  fi
  mkdir -p "$(dirname "$unit_path")"

  # 把「当前这个 shell 的 PATH」写进 unit：agent CLI（opencode / codex / claude 等）
  # 往往装在 nvm 之类不在 systemd 默认 PATH 里的位置，不写死它们会找不到 -> 子进程被跳过。
  #
  # 引号策略（踩过坑，别改回去）：systemd 对各个指令的去引号处理**不一致** ——
  # Environment= / ExecStart= 会去引号，而 WorkingDirectory= **不去**，值被当成字面量路径。
  # 所以 `WorkingDirectory="${wd}"` 里的 `"/root"` 不是绝对路径，会走到
  # path_simplify_and_warn(PATH_CHECK_ABSOLUTE|PATH_CHECK_FATAL) 返回 -ENOEXEC
  # （src/core/load-fragment.c，systemd v240 与 v257 逐字相同），unit 被记成 UNIT_BAD_SETTING，
  # `systemctl start` 只报一句 "Unit ... has a bad unit file setting."。
  # 结论：没有空白就一律裸写，有空白才加引号；WorkingDirectory= 永远裸写。
  q_ws() { case "$1" in *[[:space:]]*) printf '"%s"' "$1" ;; *) printf '%s' "$1" ;; esac; }
  cat > "$unit_path" <<EOF
[Unit]
Description=clawmessenger agent supervisor (RongCloud IM)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=HOME=$(q_ws "$HOME")
Environment=PATH=$(q_ws "$PATH")
WorkingDirectory=${wd}
ExecStart=$(q_ws "$exe") run${MODEL_ARG:+ ${MODEL_ARG}}
Restart=always
RestartSec=5
KillMode=control-group
TimeoutStopSec=20
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=${wanted}
EOF

  # 先让 systemd 自己校验一遍（零副作用）：unit 不合法就到此为止，
  # 既不停已跑着的 supervisor、也不 enable，免得装自启反而把在线设备搞下线。
  if command -v systemd-analyze >/dev/null 2>&1; then
    if ! verify_out="$(systemd-analyze verify "$unit_path" 2>&1)"; then
      warn 'systemd 拒绝了这个 unit（systemd-analyze verify）：'
      printf '%s\n' "$verify_out" | sed 's/^/  /'
      warn "unit 正文：${unit_path}"
      sed 's/^/  /' "$unit_path"
      warn '没有停掉任何东西、也没有 enable —— 设备仍然在线。改掉上面那行错误再试，或 ./clawmessenger-daemon.sh uninstall 摘掉。'
      return 1
    fi
  fi

  if [ "$scope" = 'system' ]; then
    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME" >/dev/null
  else
    systemctl --user daemon-reload
    systemctl --user enable "$SERVICE_NAME" >/dev/null
  fi

  ok "已写入 systemd ${scope} 服务：${unit_path}"
  echo "  执行     : ${exe} run${MODEL_ARG:+ ${MODEL_ARG}}"
  echo "  工作目录 : ${wd}"

  # 切换：如果之前是手动前台/后台启动的，先收掉，避免两份 supervisor 重复拉起同一批 agent。
  do_stop >/dev/null 2>&1 || true
  if [ "$scope" = 'system' ]; then
    systemctl start "$SERVICE_NAME"
    echo '  管理     : systemctl {start|stop|restart|status} '"${SERVICE_NAME}"
    echo '  日志     : journalctl -u '"${SERVICE_NAME}"' -f'
  else
    systemctl --user start "$SERVICE_NAME"
    loginctl enable-linger "$(id -un)" >/dev/null 2>&1 \
      || warn "  提示：未能开启 linger（注销后用户服务可能被停）。可手动执行：sudo loginctl enable-linger $(id -un)"
    echo '  管理     : systemctl --user {start|stop|restart|status} '"${SERVICE_NAME}"
    echo '  日志     : journalctl --user -u '"${SERVICE_NAME}"' -f'
  fi
  echo "  取消     : $0 uninstall"
  echo '  注意     : unit 里的 PATH 取自本次执行时的环境；以后换了 agent CLI 的安装位置，请重新执行安装。'
}

do_uninstall() {
  local removed=''
  if [ -f "$SYSTEM_UNIT" ]; then
    systemctl disable --now "$SERVICE_NAME" >/dev/null 2>&1 || true
    rm -f "$SYSTEM_UNIT"
    systemctl daemon-reload >/dev/null 2>&1 || true
    removed="systemd 系统服务「${SERVICE_NAME}」"
  fi
  if [ -f "$USER_UNIT" ]; then
    systemctl --user disable --now "$SERVICE_NAME" >/dev/null 2>&1 || true
    rm -f "$USER_UNIT"
    systemctl --user daemon-reload >/dev/null 2>&1 || true
    if [ -n "$removed" ]; then
      removed="${removed}、systemd 用户服务「${SERVICE_NAME}」"
    else
      removed="systemd 用户服务「${SERVICE_NAME}」"
    fi
  fi
  if [ -z "$removed" ]; then
    warn '没有发现已配置的 systemd 服务。'
  else
    ok "已移除：${removed}"
  fi
  echo "（正在后台跑的进程没动；要停请执行 $0 stop）"
}

# ---------------------------------------------------------------- 入口

case "$ACTION" in
  start)           do_start ;;
  stop)            do_stop ;;
  restart)         do_restart ;;
  status)          do_status ;;
  logs)            do_logs ;;
  run)             do_run ;;
  install-service) do_install_service ;;
  uninstall)       do_uninstall ;;
  help|-h|--help)  usage ;;
  *)
    err2 "error: 未知动作 '${ACTION}'"
    echo
    usage
    exit 2
    ;;
esac
