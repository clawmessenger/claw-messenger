#!/usr/bin/env bash
#
# 体检：每个 workspace 是否都有 owner/admin。
#
# 为什么需要它：设备绑定链路的 POST /api/ai/register 会先解析节点归属
# （internal/integrations/rongcloud/node_service.go 的
# resolveWorkspaceAttribution），查的是
#
#     SELECT user_id FROM member WHERE workspace_id = $1 AND role IN ('owner','admin')
#
# 查不到就直接返回 "workspace ... has no manager to own the node"，而 handler
# 把它翻成 500 "failed to register node" —— 这条路径一行日志都不打，
# CLI 只看到 500、网页只看到「等待设备连接」，很难查。
#
# 正常走 POST /api/workspaces 建 workspace 会在同一事务里写 owner，不会缺；
# 但 seed 脚本或手工 \copy 搬库会绕开那层，于是留下没有 owner 的 workspace。
#
# 用法：
#   ./scripts/ensure-workspace-owner.sh            # 只读体检，有缺失则 exit 1
#   ./scripts/ensure-workspace-owner.sh --fix      # 缺的补一条 owner（幂等）
#
# 连接串来源：$DATABASE_URL → ./.env → ../.env → 脚本所在仓库根的 .env

set -uo pipefail

FIX=0
case "${1:-}" in
  --fix) FIX=1 ;;
  -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
  "") ;;
  *) echo "unknown argument: $1 (use --fix)"; exit 2 ;;
esac

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

DSN="${DATABASE_URL:-}"
if [ -z "$DSN" ]; then
  for f in ./.env ../.env "$SCRIPT_DIR/../.env" "$SCRIPT_DIR/../../.env"; do
    if [ -f "$f" ]; then
      DSN=$(sed -n 's/^DATABASE_URL=//p' "$f" | head -1)
      [ -n "$DSN" ] && { echo "dsn: from $f"; break; }
    fi
  done
fi
if [ -z "$DSN" ]; then
  echo "!! 找不到 DATABASE_URL（环境变量或 .env 都没有）"
  exit 1
fi

if ! psql "$DSN" -Atqc 'select 1' >/dev/null 2>&1; then
  echo "!! 数据库连不上，检查 DATABASE_URL"
  exit 1
fi

TAB=$(printf '\t')
missing=0
total=0

while IFS="$TAB" read -r wid slug mgr; do
  [ -z "$wid" ] && continue
  total=$((total + 1))
  if [ "${mgr:-0}" != "0" ]; then
    printf '[ok] %-28s owner/admin=%s\n' "$slug" "$mgr"
    continue
  fi

  missing=$((missing + 1))
  printf '[!]  %-28s 没有 owner/admin —— 设备绑定会 500\n' "$slug"

  if [ "$FIX" != "1" ]; then
    continue
  fi

  owner=$(psql "$DSN" -Atqc 'select id from "user" order by created_at limit 1' 2>/dev/null)
  if [ -z "$owner" ]; then
    echo "     !! \"user\" 表为空，无法指定 owner —— 先建一个用户"
    continue
  fi

  if out=$(psql "$DSN" -v ON_ERROR_STOP=1 -Atqc "
      insert into member (workspace_id, user_id, role)
      values ('$wid'::uuid, '$owner'::uuid, 'owner')
      on conflict (workspace_id, user_id) do update set role = 'owner'
      returning 'owner=' || role" 2>&1); then
    echo "     已补：workspace=$slug owner=$owner -> $out"
    missing=$((missing - 1))
  else
    echo "     !! 补失败：$out"
  fi
done < <(psql "$DSN" -Atqc "
  select w.id, w.slug,
         (select count(*) from member m
           where m.workspace_id = w.id and m.role in ('owner','admin'))
  from workspace w
  order by w.created_at")

echo
if [ "$total" = "0" ]; then
  echo "没有 workspace —— 先把库初始化出来"
  exit 1
fi
if [ "$missing" -gt 0 ]; then
  echo "共 $total 个 workspace，其中 $missing 个缺 owner/admin。加 --fix 自动补（用最早创建的 \"user\" 当 owner）。"
  exit 1
fi
echo "共 $total 个 workspace，全部有 owner/admin。设备绑定不会因归属缺失而 500。"
exit 0
