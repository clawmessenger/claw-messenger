-- 圆桌成员能力补全：给派生能力集加 discussion_participant。
--
-- 为什么：旧后端把成员门槛定为
--   discussion_participant + discussion_roundtable + discussion_model_routing
--（discussion/roundtable_creation.py 的 REQUIRED_ROLE_CAPABILITIES），而 586 的
-- 派生集只有 host 视角的四项，漏了 participant。后果：库里所有 agent 节点都
-- 过不了「普通讨论成员」的能力校验 —— chatroom create / setDiscussionTeam 与
-- createRoundtable 对存量节点全部不可用。
--
-- 派生表（node_capabilities.go）同步补齐；本迁移只回填存量行。幂等：已含全集
-- 的行不落 UPDATE（与 586 同一写法）。

UPDATE rongcloud_node
   SET capabilities = (
           SELECT COALESCE(jsonb_agg(DISTINCT value ORDER BY value), '[]'::jsonb)
             FROM jsonb_array_elements_text(
                      CASE
                          WHEN jsonb_typeof(capabilities) = 'array' THEN capabilities
                          ELSE '[]'::jsonb
                      END
                      || '["discussion_host","discussion_participant","discussion_model_routing","discussion_role_recommendation","discussion_roundtable"]'::jsonb
                  ) AS value
       ),
       updated_at = now()
 WHERE ai_type IS NOT NULL
   AND ai_type <> 'ops'
   AND NOT (
       capabilities ?& ARRAY[
           'discussion_host','discussion_participant','discussion_model_routing',
           'discussion_role_recommendation','discussion_roundtable'
       ]
   );
