DROP INDEX IF EXISTS rongcloud_pairing_session_user_code_key;
ALTER TABLE rongcloud_pairing_session DROP COLUMN IF EXISTS device_info;
ALTER TABLE rongcloud_pairing_session DROP COLUMN IF EXISTS user_code;
