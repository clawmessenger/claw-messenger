DROP INDEX IF EXISTS rongcloud_node_machine_id_idx;

ALTER TABLE rongcloud_node
    DROP COLUMN machine_id;

ALTER TABLE rongcloud_pairing_session
    DROP COLUMN bound_agents,
    DROP COLUMN reported_agents;
