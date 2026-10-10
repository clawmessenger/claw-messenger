#!/usr/bin/env bash
#
# backfill-friends.sh — 修补"设备绑定后好友列表里没有该设备用户"的历史数据。
#
# 背景：设备列表(远程设备管理)走 /api/claw/nodes，好友列表走融云好友关系。
# 早期版本的绑定流程没有写融云好友边，所以当时绑定的设备不会出现在好友列表。
# 新版本已在绑定时刻写入好友边；本脚本用于给【改动之前已绑定】的设备补好友。
#
# 特性：幂等（融云返回 25460「已是好友」计为成功）、自动跳过机器主节点与
# ops 运维节点、默认 dry-run（只打印计划，不写库）。
#
# 用法（在 claw-messenger 仓库任意位置执行）：
#   ./scripts/backfill-friends.sh --all-users            # 预演：所有账号
#   ./scripts/backfill-friends.sh --all-users --commit   # 真正写入：所有账号
#   ./scripts/backfill-friends.sh --user 100123,100456 --commit
#   ./scripts/backfill-friends.sh --user 100123 --workspace <uuid> --commit
#   ./scripts/backfill-friends.sh --sync-names --commit  # 把库里的节点昵称同步到融云
#
# 透传给底层命令的选项（见 server/cmd/backfill-friends）：
#   --all-users           为所有 status='active' 的 claw 用户补好友
#   --user <id[,id...]>   只处理指定用户
#   --workspace <uuid>    只处理指定 workspace（默认：所有含节点的 workspace）
#   --include-ops         连 ops 运维节点也加好友（默认跳过）
#   --both                同时写反向好友边（agent -> owner）
#   --sync-names          把每个节点的库内昵称推送到融云（/user/refresh.json），
#                         修复"改名后好友列表仍显示节点 id/旧昵称"的历史数据
#   --commit              真正写库（不加则 dry-run）
#   --dsn <dsn>           直接指定数据库 DSN
#   --env-file <path>     指定 .env（默认自动探测仓库根或 server/../.env）
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SERVER_DIR="${REPO_ROOT}/server"

# 自动探测 .env：优先仓库根，其次上一级（兼容从 server/ 运行的习惯）。
ENV_FILE=""
for candidate in "${REPO_ROOT}/.env" "${REPO_ROOT}/../.env" "${SERVER_DIR}/.env"; do
  if [[ -f "${candidate}" ]]; then
    ENV_FILE="${candidate}"
    break
  fi
done

echo "==> repo:     ${REPO_ROOT}"
echo "==> server:   ${SERVER_DIR}"
if [[ -n "${ENV_FILE}" ]]; then
  echo "==> env file: ${ENV_FILE}"
else
  echo "==> env file: (未找到 .env，将只使用当前环境变量；可用 --env-file 指定)"
fi
echo

# 组装参数：仅在没有显式传入 --env-file 时补默认值。
ARGS=("$@")
has_env_file=false
for a in "$@"; do
  [[ "${a}" == "--env-file" || "${a}" == "-env-file" ]] && has_env_file=true
done

if [[ -n "${ENV_FILE}" && "${has_env_file}" == false ]]; then
  ARGS=("--env-file" "${ENV_FILE}" "${ARGS[@]}")
fi

cd "${SERVER_DIR}"
exec go run ./cmd/backfill-friends "${ARGS[@]}"
