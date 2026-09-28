DELETE FROM usage_doc WHERE workspace_id IS NULL AND slug IN (
  'getting-started',
  'core-concepts',
  'clawmessenger-overview',
  'agent-opencode',
  'agent-openclaw',
  'agent-claude',
  'agent-codex',
  'agent-other-clis'
);
