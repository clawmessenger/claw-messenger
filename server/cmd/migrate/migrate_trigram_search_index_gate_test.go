package main

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// trigramSearchIndexMigrations is the set that has to move together: 137
// installs pg_trgm, 138/141/142 build the fallback GIN indexes over it. 139 and
// 140 are deliberately absent — the same file range retires them through
// skipMigration, so a fresh install must not build what 455/464 immediately
// drop.
var trigramSearchIndexMigrations = []string{
	"137_search_index_pg_trgm_extension",
	"138_issue_title_trgm_index",
	"141_project_title_trgm_index",
	"142_project_description_trgm_index",
}

// TestTrigramSearchIndexMigrationsAreGated pins the registration itself, with no
// database. A gate is only consulted for a *pending* version, so a missing entry
// is invisible on every already-migrated deployment and surfaces only on a fresh
// install — the one place it cannot be noticed and repaired. That is the exact
// shape of the defect this guards: 139/140 shipped with conditions, 138/141/142
// did not, and an ungated CREATE INDEX CONCURRENTLY over an absent gin_trgm_ops
// aborts the whole run.
//
// 137 must stay out of the map. It is the migration that makes the later
// gin_trgm_ops checks resolvable, and gating it on the operator class it is
// supposed to install would be circular; its tolerance lives inside its own SQL
// as a DO/EXCEPTION block instead.
func TestTrigramSearchIndexMigrationsAreGated(t *testing.T) {
	t.Parallel()

	for _, version := range []string{
		"138_issue_title_trgm_index",
		"141_project_title_trgm_index",
		"142_project_description_trgm_index",
	} {
		if upMigrationConditions[version] == nil {
			t.Fatalf("%s has no up condition: an ungated CONCURRENTLY build aborts a fresh install without pg_trgm", version)
		}
	}
	if upMigrationConditions["137_search_index_pg_trgm_extension"] != nil {
		t.Fatal("137 must not be gated: it is the migration that installs the operator class the others are gated on")
	}
}

// TestTrigramSearchIndexesBuildOnlyWherePGTrgmExists runs 137/138/141/142's real
// SQL through the runner against a scratch schema. The gate has to hold in both
// environments: the three indexes appear where pg_trgm is installed, and where
// it is not — core Postgres, and the contrib-less self-hosted servers this gate
// exists for — every version records with its SQL skipped instead of failing the
// run. A failure there takes backend startup with it, for indexes that are only
// a search optimization: the LIKE queries they serve stay correct unaccelerated.
//
// The rollback is asserted in both environments too. The down files drop
// concurrently with IF EXISTS, so where the up direction built nothing they must
// be a no-op rather than an error.
func TestTrigramSearchIndexesBuildOnlyWherePGTrgmExists(t *testing.T) {
	t.Parallel()
	adminPool := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pgTrgmUsable := installExtensionIfAvailable(t, ctx, adminPool, "pg_trgm")

	schema := createScratchSchema(t, ctx, adminPool, "migrate_trigram_search_")
	pool := openTestPoolWithSearchPath(t, schema+", public")
	seedTrigramSearchFixture(t, ctx, pool)

	indexes := []string{
		"idx_issue_title_trgm",
		"idx_project_title_trgm",
		"idx_project_description_trgm",
	}

	options := runOptions{
		Direction:             "up",
		Files:                 realMigrationFiles(t, trigramSearchIndexMigrations, "up"),
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 hooksForDirection("up"),
		Conditions:            conditionsForDirection("up"),
	}
	if err := runMigrations(ctx, pool, options); err != nil {
		t.Fatalf("apply trigram search indexes: %v", err)
	}

	// Recorded either way: a skipped migration still advances the ledger, or
	// every later version would be blocked on a database without pg_trgm.
	for _, version := range trigramSearchIndexMigrations {
		assertMigrationVersionRecorded(t, ctx, pool, schema, version, true)
	}
	for _, indexName := range indexes {
		assertIndexExists(t, pool, schema, indexName, pgTrgmUsable)
		if pgTrgmUsable {
			assertIndexValidity(t, pool, schema, indexName, true)
		}
	}

	// Rerunning is the normal case on a real deployment: the runner is invoked
	// again after every deploy, and the ledger — not the gate — has to turn the
	// files into skips.
	if err := runMigrations(ctx, pool, options); err != nil {
		t.Fatalf("repeat trigram search indexes: %v", err)
	}
	for _, indexName := range indexes {
		assertIndexExists(t, pool, schema, indexName, pgTrgmUsable)
	}

	options.Direction = "down"
	options.Files = realMigrationFiles(t, reversed(trigramSearchIndexMigrations), "down")
	options.Hooks = hooksForDirection("down")
	options.Conditions = conditionsForDirection("down")
	if err := runMigrations(ctx, pool, options); err != nil {
		t.Fatalf("roll back trigram search indexes: %v", err)
	}
	for _, version := range trigramSearchIndexMigrations {
		assertMigrationVersionRecorded(t, ctx, pool, schema, version, false)
	}
	for _, indexName := range indexes {
		assertIndexExists(t, pool, schema, indexName, false)
	}
}

// seedTrigramSearchFixture builds the two tables 138/141/142 index, shaped like
// the ones the search handler queries so the migration SQL resolves the same
// columns it does in production instead of depending on unqualified names
// happening to match.
func seedTrigramSearchFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, statement := range []string{
		`CREATE TABLE issue (
			id BIGSERIAL PRIMARY KEY,
			title TEXT NOT NULL,
			description TEXT
		)`,
		`CREATE TABLE project (
			id BIGSERIAL PRIMARY KEY,
			title TEXT NOT NULL,
			description TEXT
		)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("apply trigram search fixture statement %q: %v", statement, err)
		}
	}
}
