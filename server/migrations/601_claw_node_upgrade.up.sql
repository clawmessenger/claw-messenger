-- Device (exe) upgrade intent + progress, one row per MACHINE.
--
-- Why a machine-keyed table instead of a per-node one: the thing being upgraded
-- is the clawmessenger.exe install (%LOCALAPPDATA%\clawmessenger\clawmessenger.exe,
-- see packages/xiachat-cli/src/install.ts), which is a property of the machine,
-- not of an individual agent node. The admin device list shows one row per agent
-- node, so several rows share one machine_id — keying by machine_id means a
-- batch request downloads an 88MB asset once per machine instead of once per
-- node, and every row for that machine renders the same version/progress.
--
-- Why the version and the progress share a row: the page needs "which exe is on
-- that machine" and "how far along is the upgrade" in the same render. Two
-- tables would be two queries and one more chance to show a version from before
-- the upgrade next to a status from after it.
--
-- Who writes what:
--   * admin   -> target_version / status='pending' / desired_generation+1
--   * device  -> reported_version / reported_platform / reported_generation /
--                status transitions (downloading -> ... -> succeeded|failed)
-- desired_generation is the same anti-flapping device the run-state table uses
-- (rongcloud_node_control.desired_version, migrations/587): every request bumps
-- it, and a device report is only applied when it carries the current value, so
-- a slow report from an earlier request can never overwrite a newer one.
--
-- A row also exists for machines that were never upgraded: the supervisor
-- reports its exe version on the existing 10s control poll, which keeps the
-- version column fresh without adding a second timer or a heartbeat field.
--
-- node_id is the machine's infrastructure node (the one holding the device
-- credential) when we know it; it is informational (the admin list groups by
-- machine_id) and NULLable because an admin can request an upgrade before that
-- machine has ever polled.
--
-- ON DELETE SET NULL (not CASCADE): unbinding the infrastructure node
-- (ClawAdminDeleteNode) must not erase the machine's version record — the
-- machine is still out there and will report again on its next poll. The row is
-- overwritten in place by machine_id, so a re-bound machine simply takes over
-- the existing row.

CREATE TABLE rongcloud_node_upgrade (
    machine_id          text PRIMARY KEY,
    node_id             uuid REFERENCES rongcloud_node (id) ON DELETE SET NULL,
    workspace_id        uuid,
    -- Latest version the machine reported for its install. Empty = never
    -- reported (the UI renders 未知).
    reported_version    text NOT NULL DEFAULT '',
    -- "<platform>-<arch>" as reported by the device, e.g. "win32-x64". Used to
    -- refuse an upgrade on a platform whose restart path this release does not
    -- implement yet, and to disable the button in the UI.
    reported_platform   text NOT NULL DEFAULT '',
    reported_at         timestamptz,
    -- Highest generation this machine has acknowledged. GREATEST()-ed on write
    -- so an out-of-order poll cannot roll it back.
    reported_generation bigint NOT NULL DEFAULT 0,
    target_version      text NOT NULL DEFAULT '',
    -- Bumped by every admin request. The device acts once per generation.
    desired_generation  bigint NOT NULL DEFAULT 0,
    -- '' = no upgrade has ever been requested for this machine. The CHECK keeps
    -- the vocabulary closed so a typo in one writer cannot render as a stuck
    -- "downloading" forever; the explicit '' arm is what lets a version-only
    -- row exist.
    status              text NOT NULL DEFAULT '' CHECK (status IN (
                            '', 'pending', 'waiting_idle', 'downloading',
                            'verifying', 'restarting', 'succeeded', 'failed',
                            'cancelled')),
    error_code          text NOT NULL DEFAULT '',
    error_detail        text NOT NULL DEFAULT '',
    -- 'force' lets the device terminate an in-flight agent turn instead of
    -- waiting for idle (see the supervisor's upgrade branch).
    force               boolean NOT NULL DEFAULT false,
    -- Batch wave gate: one batch request writes every machine as 'pending' but
    -- a machine only becomes actionable once no lower-numbered wave in the same
    -- batch is still active (upgradeWaveOpen in claw_device_upgrade.go). Batch size is a
    -- constant there, not a column, so the policy lives on the server.
    batch_id            text NOT NULL DEFAULT '',
    wave                integer NOT NULL DEFAULT 0,
    requested_by        text NOT NULL DEFAULT '',
    requested_at        timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- The admin list reads every row in one go (the table is one row per machine,
-- so it is the size of the device fleet), but the wave gate and the progress
-- endpoint filter by batch.
CREATE INDEX rongcloud_node_upgrade_batch_idx
    ON rongcloud_node_upgrade (batch_id, wave);

CREATE INDEX rongcloud_node_upgrade_status_idx
    ON rongcloud_node_upgrade (status);
