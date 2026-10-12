-- Desired run-state for a bound agent node, so the admin 设备管理 page can
-- actually 启动/停止 a device.
--
-- Why a new table instead of a column on rongcloud_node: this is mutable
-- operator intent with its own version counter, not node identity. Keeping it
-- separate also means the admin write path and the device poll path touch the
-- same row without fighting over rongcloud_node's updated_at (which the admin
-- device list orders by and the edit dialog rewrites).
--
-- Why the server holds the desired state at all: the process that can start or
-- stop an agent is the supervisor (`clawmessenger run` with no --agent), and it
-- holds no RongCloud connection — only its per-agent children do (imlib is a
-- module-level singleton, so exactly one OS process may log in per IM user).
-- The children cannot be used as a control channel: a stopped child has no
-- process left to receive "start", and a "start" delivered over IM would never
-- be read. So the supervisor polls this table through its device credential and
-- converges the set of running children onto desired_state.
--
-- The feedback path needs no column here: the heartbeat that drives IsOnline is
-- emitted by the child process itself (packages/xiachat-cli/src/main.ts), so
-- killing a child turns the node offline within heartbeatTTL on its own. State
-- read from the admin API is therefore live, never a cached echo of intent.
--
-- desired_version increments on every operator change. It exists so a device
-- can tell "nothing moved since I last converged" from "stop was pressed
-- twice", which keeps a flapping admin click from re-killing a node that the
-- operator just started.
--
-- ON DELETE CASCADE: unbinding the node (UnbindNode) drops its control row —
-- a re-bound node starts fresh at 'running' rather than inheriting a stale
-- 'stopped' from its previous life.

CREATE TABLE rongcloud_node_control (
    node_id         uuid PRIMARY KEY REFERENCES rongcloud_node (id) ON DELETE CASCADE,
    workspace_id    uuid NOT NULL,
    desired_state   text NOT NULL CHECK (desired_state IN ('running', 'stopped')),
    desired_version bigint NOT NULL DEFAULT 1,
    -- text, not uuid: the actor is whichever admin identity drove the change and
    -- the audit log already stores admin ids as text. Keeping the same shape
    -- avoids a cast that would reject a non-uuid identity.
    changed_by      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX rongcloud_node_control_workspace_id_idx
    ON rongcloud_node_control (workspace_id);
