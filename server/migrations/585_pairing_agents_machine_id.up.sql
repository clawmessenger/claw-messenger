-- Add machine-level agent binding support to the RongCloud integration.
--
-- Pairing sessions now carry the agents the device reported at pair time
-- (reported_agents) and the subset the web user chose to bind (bound_agents).
-- Nodes gain a machine_id so every agent node on the same device can be
-- listed and its credentials fetched by the device supervisor.

ALTER TABLE rongcloud_pairing_session
    ADD COLUMN reported_agents JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN bound_agents JSONB NOT NULL DEFAULT '[]';

ALTER TABLE rongcloud_node
    ADD COLUMN machine_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS rongcloud_node_machine_id_idx ON rongcloud_node (machine_id);
