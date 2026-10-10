#!/usr/bin/env bash
#
# retire-legacy-nodes.sh — 退役"老格式 machine id"产生的节点组，让设备重新配对后
# 拿到简短可读的节点 id。
#
# 背景：RongCloud 用户 id 是账号主键，无法改名。旧版 CLI 铸的 machine id 是
# clawmessenger-<uuid>（50 字符），超过服务端 31 字符的机器段预算，被 sha256 成
# m_<24 hex>，于是节点 id 长成 rc_node_m_e99ad19f108d9dad78ad07e3_codex。
# v0.2.3 改铸 10 位 base36（短了但不是想要的形状），v0.2.5 起改铸 10 位纯数字，
# 节点形如 hermes_1234567890。所以设备【必须重新配对】才能换 id：本脚本负责
# 删掉老节点组（DB 行 + 好友边 [+ 可选：融云账号]），重新配对由设备端执行。
#
# 特性：默认 dry-run（只打印计划，不写任何东西）；按长度扫描时自动跳过已是短 id 的组。
#
# 用法（在 claw-messenger 仓库任意位置执行）：
#   ./scripts/retire-legacy-nodes.sh                        # 预演：列出将要退役的节点组
#   ./scripts/retire-legacy-nodes.sh --commit               # 真正删除（DB + 好友边）
#   ./scripts/retire-legacy-nodes.sh --commit --purge-accounts
#                                                           # 同时注销融云账号（不可逆！）
#   ./scripts/retire-legacy-nodes.sh --max-machine-id 11    # 连 12-31 字符的 id 一起扫
#   ./scripts/retire-legacy-nodes.sh --machine-id oc5yj7nusm --commit
#                                                           # 按名字退役指定机器组（忽略长度规则）
#   ./scripts/retire-legacy-nodes.sh --owner 850509 --commit
#
# 透传给底层命令的选项（见 server/cmd/retire-legacy-nodes）：
#   --workspace <uuid>     只扫指定 workspace（默认：所有含节点的 workspace）
#   --max-machine-id <n>   机器 id 超过 n 字符即视为老格式（默认 31；传 11 可更严格）
#   --machine-id <id,..>   按名字退役这些机器组，忽略长度规则；**一旦指定就只退役这些**
#                          （长度合法的旧形状，如 v0.2.3 的 oc5yj7nusm，只能这样指名退役）
#   --owner <id[,id...]>   只清理这些 claw 账号的好友边（默认：所有 active 账号）
#   --keep-friends         不删好友边（只清 DB）
#   --purge-accounts       同时注销融云账号（不可逆，会删掉该账号的消息历史；需配合 --commit）
#   --commit               真正执行（不加则 dry-run）
#   --dsn <dsn>            直接指定数据库 DSN
#   --env-file <path>      指定 .env（默认自动探测仓库根或 server/../.env）
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
exec go run ./cmd/retire-legacy-nodes "${ARGS[@]}"
