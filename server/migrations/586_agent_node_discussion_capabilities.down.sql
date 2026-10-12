-- Remove the discussion capability set added by the up migration, leaving any
-- capability a device reported untouched.

UPDATE rongcloud_node
   SET capabilities = (
           SELECT COALESCE(jsonb_agg(value ORDER BY value), '[]'::jsonb)
             FROM jsonb_array_elements_text(capabilities) AS value
            WHERE value NOT IN (
                      'discussion_host',
                      'discussion_model_routing',
                      'discussion_role_recommendation',
                      'discussion_roundtable'
                  )
       ),
       updated_at = now()
 WHERE ai_type IS NOT NULL
   AND ai_type <> 'ops'
   AND jsonb_typeof(capabilities) = 'array'
   AND capabilities @> '["discussion_host"]'::jsonb;
