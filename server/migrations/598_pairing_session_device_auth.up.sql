-- Device-code pairing: a terminal client obtains a short-lived, one-time
-- 8-char user code (printed as a verify URL); the logged-in human approves
-- the session in the browser. user_code is unique while live; device_info
-- records what the client reported at session creation.
ALTER TABLE rongcloud_pairing_session ADD COLUMN IF NOT EXISTS user_code TEXT;
ALTER TABLE rongcloud_pairing_session ADD COLUMN IF NOT EXISTS device_info JSONB NOT NULL DEFAULT '{}'::jsonb;
CREATE UNIQUE INDEX IF NOT EXISTS rongcloud_pairing_session_user_code_key
    ON rongcloud_pairing_session (user_code) WHERE user_code IS NOT NULL;
