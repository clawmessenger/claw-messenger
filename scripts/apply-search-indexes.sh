#!/usr/bin/env bash
#
# apply-search-indexes.sh — 给数据库补齐 pg_trgm 三元组搜索索引。
#
# 背景：SearchIssues / SearchProjects 用 LOWER(列) LIKE '%词%' 做候选集预筛，
# 依赖 gin_trgm_ops 表达式索引（迁移 137 / 138 / 141 / 142）。
# 线上那台 PostgreSQL 16.11 没装 postgresql-contrib，迁移记录虽在，三个索引
# 实际缺失 → 搜索只能全表扫（8s 超时后返回 503）。
#
# 本脚本做三件事：
#   1. 预检：psql 可用 / 能连上库 / issue 与 project 表存在 / pg_trgm 是否可用
#   2. 建扩展 + 建 3 个索引（幂等，可重复执行；已存在则跳过）
#   3. 校验索引存在性、有效性(valid/ready)
#
# ⚠ pg_trgm 是 contrib 扩展，SQL 装不了它。若预检报「pg_trgm not available」，
#   请先到【数据库主机】上执行（装完不需要重启 PostgreSQL）：
#     dnf install -y postgresql16-contrib
#     ls /usr/pgsql-16/share/extension/pg_trgm.control   # 看到文件即成功
#
# 用法（在 claw-messenger 仓库任意位置执行）：
#   ./scripts/apply-search-indexes.sh                  # 预检 + 建扩展 + 建索引 + 校验
#   ./scripts/apply-search-indexes.sh --dry-run        # 不连库，只打印将要执行的 SQL
#   ./scripts/apply-search-indexes.sh --concurrent     # 用 CONCURRENTLY（只有大表才需要）
#   ./scripts/apply-search-indexes.sh --drop           # 回滚：删掉这 3 个索引
#   ./scripts/apply-search-indexes.sh --drop --drop-extension
#   ./scripts/apply-search-indexes.sh --install-contrib   # 在数据库主机上顺手装 contrib（需 root）
#   ./scripts/apply-search-indexes.sh --dsn 'postgres://user:pass@host:5432/db'
#   ./scripts/apply-search-indexes.sh --env-file /path/to/.env
#
# 说明：默认不用 CONCURRENTLY。CREATE INDEX CONCURRENTLY 不能在事务/多语句里
# 执行，整段粘贴必失败；而这几张表是空的，普通 CREATE INDEX 瞬时完成、不阻塞。
# 只有表里已有大量数据时才加 --concurrent（脚本会把每条拆成独立执行）。
#
# 退出码：0 成功 / 1 参数错误 / 2 连接或 psql 不可用 / 3 缺少 pg_trgm 扩展 / 4 校验失败
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

DRY_RUN=false
CONCURRENT=false
DROP=false
DROP_EXTENSION=false
INSTALL_CONTRIB=false
DSN=""
ENV_FILE=""

usage() {
  sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^#\{1,\} \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)          DRY_RUN=true; shift ;;
    --concurrent)       CONCURRENT=true; shift ;;
    --drop)             DROP=true; shift ;;
    --drop-extension)   DROP_EXTENSION=true; shift ;;
    --install-contrib)  INSTALL_CONTRIB=true; shift ;;
    --dsn)              DSN="${2:-}"; shift 2 ;;
    --dsn=*)            DSN="${1#*=}"; shift ;;
    --env-file)         ENV_FILE="${2:-}"; shift 2 ;;
    --env-file=*)       ENV_FILE="${1#*=}"; shift ;;
    -h|--help)          usage; exit 0 ;;
    *) echo "未知参数：$1" >&2; echo >&2; usage >&2; exit 1 ;;
  esac
done

# ---------- 索引定义（表达式与迁移 138 / 141 / 142 逐字一致） ----------
IDX_ISSUE_TITLE="idx_issue_title_trgm"
IDX_PROJECT_TITLE="idx_project_title_trgm"
IDX_PROJECT_DESC="idx_project_description_trgm"

stmts_extension=(
  "CREATE EXTENSION IF NOT EXISTS pg_trgm;"
)
stmts_indexes=(
  "CREATE INDEX IF NOT EXISTS ${IDX_ISSUE_TITLE} ON issue USING gin (LOWER(title) gin_trgm_ops);"
  "CREATE INDEX IF NOT EXISTS ${IDX_PROJECT_TITLE} ON project USING gin (LOWER(title) gin_trgm_ops);"
  "CREATE INDEX IF NOT EXISTS ${IDX_PROJECT_DESC} ON project USING gin (LOWER(COALESCE(description, '')) gin_trgm_ops);"
)
stmts_drop=(
  "DROP INDEX IF EXISTS ${IDX_ISSUE_TITLE};"
  "DROP INDEX IF EXISTS ${IDX_PROJECT_TITLE};"
  "DROP INDEX IF EXISTS ${IDX_PROJECT_DESC};"
)

if [[ "${CONCURRENT}" == true ]]; then
  for i in "${!stmts_indexes[@]}"; do
    stmts_indexes[$i]="${stmts_indexes[$i]/CREATE INDEX IF NOT EXISTS/CREATE INDEX CONCURRENTLY IF NOT EXISTS}"
  done
  for i in "${!stmts_drop[@]}"; do
    stmts_drop[$i]="${stmts_drop[$i]/DROP INDEX IF EXISTS/DROP INDEX CONCURRENTLY IF EXISTS}"
  done
fi

print_plan() {
  echo "将要执行："
  if [[ "${DROP}" == true ]]; then
    for s in "${stmts_drop[@]}"; do echo "  ${s}"; done
    [[ "${DROP_EXTENSION}" == true ]] && echo "  DROP EXTENSION IF EXISTS pg_trgm;"
    echo "  ANALYZE issue;  ANALYZE project;"
    return
  fi
  for s in "${stmts_extension[@]}"; do echo "  ${s}"; done
  for s in "${stmts_indexes[@]}"; do echo "  ${s}"; done
  echo "  ANALYZE issue;  ANALYZE project;"
}

# ---------- --install-contrib：只在数据库主机上才有意义 ----------
if [[ "${INSTALL_CONTRIB}" == true ]]; then
  echo "==> 安装 postgresql16-contrib（仅当本机就是数据库主机时才有意义）"
  echo "    本机 hostname: $(hostname)"
  if [[ "$(id -u)" != "0" ]]; then
    echo "错误：安装系统包需要 root。" >&2; exit 1
  fi
  if command -v dnf > /dev/null 2>&1; then
    dnf install -y postgresql16-contrib
  elif command -v apt-get > /dev/null 2>&1; then
    apt-get install -y postgresql-contrib-16
  else
    echo "错误：没找到 dnf/apt-get，请手工安装 postgresql16-contrib。" >&2; exit 1
  fi
  echo
fi

# ---------- --dry-run：不连库 ----------
if [[ "${DRY_RUN}" == true ]]; then
  echo "==> 仓库根目录: ${REPO_ROOT}"
  echo "==> dry-run：不连接数据库"
  echo
  print_plan
  exit 0
fi

# ---------- 找 DSN ----------
urldecode() {
  local s="${1//+/ }"
  printf '%b' "${s//%/\\x}"
}

read_env_dsn() {
  local f="$1" line
  [[ -f "${f}" ]] || return 1
  line="$(grep -E '^[[:space:]]*(export[[:space:]]+)?DATABASE_URL=' "${f}" | tail -n 1 || true)"
  [[ -z "${line}" ]] && return 1
  line="${line#*=}"
  line="${line%\'}"; line="${line#\'}"
  line="${line%\"}"; line="${line#\"}"
  printf '%s' "${line}"
}

if [[ -z "${DSN}" ]]; then
  if [[ -z "${ENV_FILE}" ]]; then
    for candidate in "${REPO_ROOT}/.env" "${REPO_ROOT}/../.env" "${REPO_ROOT}/server/.env"; do
      if [[ -f "${candidate}" ]]; then ENV_FILE="${candidate}"; break; fi
    done
  fi
  if [[ -n "${ENV_FILE}" ]]; then
    DSN="$(read_env_dsn "${ENV_FILE}" || true)"
    echo "==> env file: ${ENV_FILE}"
  fi
fi

[[ -z "${DSN}" ]] && DSN="${DATABASE_URL:-}"

DB_HOST=""; DB_PORT="5432"; DB_NAME=""; DB_USER=""; DB_PASS=""
if [[ -n "${DSN}" ]]; then
  rest="${DSN#*://}"; rest="${rest%%\?*}"
  authority="${rest%%/*}"; path="${rest#*/}"
  [[ "${authority}" == "${rest}" ]] && path=""
  userinfo=""; hostport="${authority}"
  if [[ "${authority}" == *@* ]]; then
    userinfo="${authority%@*}"; hostport="${authority##*@}"
  fi
  if [[ -n "${userinfo}" ]]; then
    DB_USER="${userinfo%%:*}"
    [[ "${userinfo}" == *:* ]] && DB_PASS="$(urldecode "${userinfo#*:}")"
  fi
  if [[ "${hostport}" == \[* ]]; then
    DB_HOST="${hostport#\[}"; DB_HOST="${DB_HOST%%]*}"
    port_part="${hostport#*\]}"
    [[ "${port_part}" == :* && -n "${port_part#:}" ]] && DB_PORT="${port_part#:}"
  else
    DB_HOST="${hostport%%:*}"
    [[ "${hostport}" == *:* && -n "${hostport##*:}" ]] && DB_PORT="${hostport##*:}"
  fi
  [[ -n "${path}" ]] && DB_NAME="${path%%/*}"
fi

echo "==> 仓库根目录: ${REPO_ROOT}"
if [[ -n "${DB_HOST}" ]]; then
  echo "==> 目标数据库: ${DB_USER:-<默认>}@${DB_HOST}:${DB_PORT}/${DB_NAME:-<默认>}"
else
  echo "==> 目标数据库: 使用 libpq 环境变量默认值（PGHOST/PGUSER/PGDATABASE）"
fi
echo

# ---------- psql 是否可用 ----------
if ! command -v psql > /dev/null 2>&1; then
  echo "错误：找不到 psql 客户端。" >&2
  cat >&2 <<'TIP'
  安装客户端即可（版本不必与服务端一致，协议兼容）：
    应用服务器(RHEL/Rocky): dnf install -y postgresql
    Debian/Ubuntu        : apt-get install -y postgresql-client
TIP
  exit 2
fi

PSQL=(psql -X -q -v ON_ERROR_STOP=1)
[[ -n "${DB_HOST}" ]] && PSQL+=(-h "${DB_HOST}")
[[ -n "${DB_PORT}" ]] && PSQL+=(-p "${DB_PORT}")
[[ -n "${DB_USER}" ]] && PSQL+=(-U "${DB_USER}")
[[ -n "${DB_NAME}" ]] && PSQL+=(-d "${DB_NAME}")
[[ -n "${DB_PASS}" ]] && export PGPASSWORD="${DB_PASS}"

run_sql() { "${PSQL[@]}" -c "$1"; }
scalar()  { "${PSQL[@]}" -At -c "$1"; }

# ---------- 预检 ----------
echo "==> 连接数据库…"
if ! scalar "SELECT 1" > /dev/null 2>&1; then
  echo "错误：连不上数据库。请检查网络、账号密码、pg_hba.conf。" >&2
  exit 2
fi
echo "    $(scalar 'SELECT version()')"
echo "    当前用户: $(scalar 'SELECT current_user')"

tables_ok="$(scalar "SELECT (to_regclass('public.issue') IS NOT NULL AND to_regclass('public.project') IS NOT NULL)::text")"
if [[ "${tables_ok}" != "true" ]]; then
  echo "错误：public.issue / public.project 不存在。这是一个还没建表的空库。" >&2
  echo "      请先跑：cd server && go run ./cmd/migrate up" >&2
  exit 2
fi

trgm_ok="$(scalar "SELECT (EXISTS (SELECT 1 FROM pg_extension WHERE extname='pg_trgm') OR EXISTS (SELECT 1 FROM pg_available_extensions WHERE name='pg_trgm'))::text")"
if [[ "${trgm_ok}" != "true" ]]; then
  echo "错误：本服务器的 PostgreSQL 没有 pg_trgm（缺 postgresql16-contrib）。" >&2
  cat >&2 <<TIP
  SQL 装不了它，必须先在【数据库主机 $( [[ -n "${DB_HOST}" ]] && echo "${DB_HOST}" )】上以 root 执行：
      dnf install -y postgresql16-contrib
      ls /usr/pgsql-16/share/extension/pg_trgm.control   # 看到文件即成功
  装完不需要重启 PostgreSQL。
TIP
  exit 3
fi
echo "    pg_trgm 可用 ✓"
echo

# ---------- 执行 ----------
if [[ "${DROP}" == true ]]; then
  echo "==> 删除索引"
  for s in "${stmts_drop[@]}"; do
    echo "    ${s}"
    run_sql "${s}"
  done
  if [[ "${DROP_EXTENSION}" == true ]]; then
    echo "    DROP EXTENSION IF EXISTS pg_trgm;"
    run_sql "DROP EXTENSION IF EXISTS pg_trgm;"
  fi
  echo "==> 完成（索引已删除）"
  exit 0
fi

echo "==> 安装扩展"
for s in "${stmts_extension[@]}"; do
  echo "    ${s}"
  run_sql "${s}"
done

echo "==> 建立索引"
for s in "${stmts_indexes[@]}"; do
  echo "    ${s}"
  run_sql "${s}"
done

echo "==> 收集统计信息"
run_sql "ANALYZE issue;" > /dev/null
run_sql "ANALYZE project;" > /dev/null
echo "    完成"

# ---------- 校验 ----------
echo
echo "==> 校验"
result="$(scalar "
  SELECT count(*) FILTER (WHERE i.indisvalid AND i.indisready) || '/' || count(*)
  FROM pg_index i
  JOIN pg_class c ON c.oid = i.indexrelid
  WHERE c.relname IN ('${IDX_ISSUE_TITLE}','${IDX_PROJECT_TITLE}','${IDX_PROJECT_DESC}')")"
echo "    有效索引: ${result}（期望 3/3）"

scalar "
  SELECT c.relname || '  valid=' || i.indisvalid || '  ready=' || i.indisready ||
         '  size=' || pg_size_pretty(pg_relation_size(c.oid))
  FROM pg_index i
  JOIN pg_class c ON c.oid = i.indexrelid
  WHERE c.relname IN ('${IDX_ISSUE_TITLE}','${IDX_PROJECT_TITLE}','${IDX_PROJECT_DESC}')
  ORDER BY c.relname"

if [[ "${result}" != "3/3" ]]; then
  echo "错误：索引没有全部就绪，请检查上面的输出。" >&2
  exit 4
fi

echo
echo "✓ 全部完成。"
echo "  注：idx_comment_content_trgm / idx_issue_description_trgm 不需要建 ——"
echo "      它们已被迁移 455 / 464 正式退役，当前搜索代码不再读它们。"
