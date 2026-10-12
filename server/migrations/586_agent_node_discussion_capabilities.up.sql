-- Give bound agent nodes the discussion capability set.
--
-- The discussion runtime picks how to run a turn from ai_type alone
-- (discussion_bridge.IsServerManagedByType, plus the IM
-- `discussion/your_turn` fallback in discussion_coordinator.go), and the Node
-- CLI honours the round / speaking_order / model parameters that dispatch
-- carries. So every bound agent node genuinely supports the four discussion
-- capabilities:
--
--     discussion_host, discussion_roundtable,
--     discussion_model_routing, discussion_role_recommendation
--
-- Nothing ever wrote them, though: the Node CLI does not send `capabilities` at
-- all (packages/xiachat-cli/src/api.ts defaults it to []), and ensureAgentNode
-- created every agent node with a hard-coded '[]'. Both pickers that gate on
-- these values — the admin 系统 → 讨论主持 AI page and the web 主持人 picker —
-- therefore listed no node at all and the pages could not be used.
--
-- The write path is fixed alongside this migration (node_capabilities.go), so
-- new binds advertise the set on their own; this backfills the rows that
-- already exist. Machine infrastructure nodes are skipped: they are internal
-- plumbing (isMachineNode) and must not look like discussion host candidates.
--
-- Idempotent: rows that already carry the whole set fall out of the WHERE.

UPDATE rongcloud_node
   SET capabilities = (
           SELECT COALESCE(jsonb_agg(DISTINCT value ORDER BY value), '[]'::jsonb)
             FROM jsonb_array_elements_text(
                      CASE
                          WHEN jsonb_typeof(capabilities) = 'array' THEN capabilities
                          ELSE '[]'::jsonb
                      END
                      || '["discussion_host","discussion_model_routing","discussion_role_recommendation","discussion_roundtable"]'::jsonb
                  ) AS value
       ),
       updated_at = now()
 WHERE ai_type IS NOT NULL
   AND ai_type <> 'ops'
   AND NOT (
           capabilities @> '["discussion_host","discussion_model_routing","discussion_role_recommendation","discussion_roundtable"]'::jsonb
       )
   AND rongcloud_user_id <> CASE
           WHEN length(machine_id) <= 33 THEN 'rc_node_' || machine_id
           ELSE 'rc_node_m_' || encode(substring(sha256(machine_id::bytea) FROM 1 FOR 12), 'hex')
       END;
