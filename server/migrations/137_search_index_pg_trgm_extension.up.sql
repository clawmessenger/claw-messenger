-- Install pg_trgm for the fallback trigram search indexes added in the next
-- migrations. Keep this separate from CREATE INDEX CONCURRENTLY files:
-- Postgres rejects concurrent index builds in a multi-statement migration.
--
-- The extension itself is optional. pg_trgm ships with contrib rather than
-- core Postgres, so a self-hosted server built without it has nothing to
-- install, and an unprotected CREATE EXTENSION would abort the whole run here.
-- Skip gracefully instead: the indexes in 138/141/142 are gated on
-- gin_trgm_ops and record a no-op in exactly this environment. Mirrors the
-- pattern of migration 032 (pg_bigm) and 076 (pg_cron).
DO $$
BEGIN
    CREATE EXTENSION IF NOT EXISTS pg_trgm;
EXCEPTION WHEN OTHERS THEN
    RAISE NOTICE 'pg_trgm not available; skipping trigram search indexes.';
END
$$;
