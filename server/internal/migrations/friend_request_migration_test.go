package migrations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 603 introduces im_friend_request, the only record of "someone asked
// to add this AI node as a friend" between the request and the owner's decision.
//
// The invariant this file exists for is the agreement between one index and one
// statement:
//
//	603_im_friend_request.up.sql:
//	  CREATE UNIQUE INDEX uq_im_friend_request_pending
//	      ON im_friend_request (from_user_id, device_node_id) WHERE status = 0;
//
//	internal/integrations/rongcloud/friend_request.go (FriendRequestStore.Create):
//	  INSERT INTO im_friend_request (from_user_id, to_user_id, device_node_id, message)
//	  VALUES ($1, $2, $3, $4)
//	  ON CONFLICT (from_user_id, device_node_id) WHERE status = 0
//	  DO UPDATE SET ...
//
// Postgres infers the arbiter index from the ON CONFLICT clause, and only a
// partial index whose predicate matches the conflict target counts. Once the two
// drift apart the *first* tap on 发送申请 already fails with 42P10 ("there is no
// unique or exclusion constraint matching the ON CONFLICT specification"), which
// the handler wraps into a 500 — the user cannot send the request at all. This
// test runs the real migration file against a real Postgres, so that drift shows
// up here instead of in production.

// friendRequestUpsertSQL mirrors FriendRequestStore.Create (friend_request.go)
// statement for statement; only the RETURNING list is trimmed to the columns
// asserted below. The conflict target is the part that has to keep matching the
// partial unique index, so it is spelled out rather than generated.
const friendRequestUpsertSQL = `
	INSERT INTO im_friend_request (from_user_id, to_user_id, device_node_id, message)
	VALUES ($1, $2, $3, $4)
	ON CONFLICT (from_user_id, device_node_id) WHERE status = 0
	DO UPDATE SET message = EXCLUDED.message,
	              to_user_id = EXCLUDED.to_user_id,
	              updated_at = now()
	RETURNING id::text, message, status`

// friendRequestMigrationSandbox is a pool whose every connection resolves
// unqualified names inside one throwaway schema, so the migration runs against
// nothing but what it creates itself.
type friendRequestMigrationSandbox struct {
	pool   *pgxpool.Pool
	schema string
}

func newFriendRequestMigrationSandbox(t *testing.T, schema string) *friendRequestMigrationSandbox {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	// Every pooled connection, not just the first, must land in the sandbox: the
	// migration and the assertions below all use unqualified table names.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	dropSchema := func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	}
	dropSchema()
	t.Cleanup(func() {
		dropSchema()
		pool.Close()
	})
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create isolated migration schema: %v", err)
	}
	return &friendRequestMigrationSandbox{pool: pool, schema: schema}
}

// columns lists the columns the migration actually created, sorted by name.
func (s *friendRequestMigrationSandbox) columns(t *testing.T, ctx context.Context) []string {
	t.Helper()
	rows, err := s.pool.Query(ctx, `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'im_friend_request'
		ORDER BY column_name
	`, s.schema)
	if err != nil {
		t.Fatalf("read im_friend_request columns: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan im_friend_request column: %v", err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate im_friend_request columns: %v", err)
	}
	return columns
}

// relationExists resolves a table or index name inside the sandbox schema.
// to_regclass returns NULL rather than raising for a missing name, which is
// exactly the "did the down migration remove it" question.
func (s *friendRequestMigrationSandbox) relationExists(t *testing.T, ctx context.Context, name string) bool {
	t.Helper()
	var exists bool
	if err := s.pool.QueryRow(ctx, `
		SELECT to_regclass(quote_ident($1) || '.' || quote_ident($2)) IS NOT NULL
	`, s.schema, name).Scan(&exists); err != nil {
		t.Fatalf("resolve relation %s.%s: %v", s.schema, name, err)
	}
	return exists
}

// pendingIndexPredicate describes the arbiter index the way Postgres will see it
// when it tries to infer it. It is reported inside failure messages, not
// asserted on: pg_get_expr's exact formatting is a server detail, while the
// behaviour asserted below is the contract.
func (s *friendRequestMigrationSandbox) pendingIndexPredicate(t *testing.T, ctx context.Context) string {
	t.Helper()
	var predicate *string
	err := s.pool.QueryRow(ctx, `
		SELECT pg_get_expr(indpred, indrelid)
		FROM pg_index
		WHERE indexrelid = to_regclass(quote_ident($1) || '.uq_im_friend_request_pending')
	`, s.schema).Scan(&predicate)
	if errors.Is(err, pgx.ErrNoRows) {
		return "<uq_im_friend_request_pending does not exist>"
	}
	if err != nil {
		return fmt.Sprintf("<cannot read uq_im_friend_request_pending: %v>", err)
	}
	if predicate == nil {
		return "<uq_im_friend_request_pending is not a partial index>"
	}
	return *predicate
}

type friendRequestUpsertResult struct {
	ID      string
	Message string
	Status  int16
}

// upsertFriendRequest sends the store's Create statement and hands the error
// back unexamined, so each caller can say which invariant it broke.
func (s *friendRequestMigrationSandbox) upsertFriendRequest(
	t *testing.T, ctx context.Context, from, to, device, message string,
) (friendRequestUpsertResult, error) {
	t.Helper()
	var row friendRequestUpsertResult
	err := s.pool.QueryRow(ctx, friendRequestUpsertSQL, from, to, device, message).
		Scan(&row.ID, &row.Message, &row.Status)
	return row, err
}

// assertRelationsGone checks that the table and the three indexes 603 creates
// are all gone. Indexes live in the same namespace as tables, so to_regclass
// answers for both.
func (s *friendRequestMigrationSandbox) assertRelationsGone(t *testing.T, ctx context.Context, step string) {
	t.Helper()
	for _, relation := range []string{
		"im_friend_request",
		"uq_im_friend_request_pending",
		"idx_im_friend_request_to_user",
		"idx_im_friend_request_from_user",
	} {
		if s.relationExists(t, ctx, relation) {
			t.Errorf("<%s> still resolves after %s; 603_im_friend_request.down.sql must drop it", relation, step)
		}
	}
}

// describeFriendRequestUpsertError names the drifted invariant behind the two
// failure modes this test exists to catch.
func describeFriendRequestUpsertError(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42P10":
			return "42P10 (there is no unique or exclusion constraint matching the ON CONFLICT specification) — " +
				"the predicate of uq_im_friend_request_pending in 603_im_friend_request.up.sql no longer matches " +
				"the ON CONFLICT (from_user_id, device_node_id) WHERE status = 0 clause of FriendRequestStore.Create"
		case "23505":
			return "23505 (unique_violation) — the second pending row was inserted instead of updating the existing one"
		}
		return fmt.Sprintf("%s: %s", pgErr.Code, pgErr.Message)
	}
	return err.Error()
}

func TestFriendRequestMigration(t *testing.T) {
	ctx := context.Background()
	s := newFriendRequestMigrationSandbox(t, "friend_request_migration_test")

	// Step 1: the migration itself, exactly as the deploy path runs it.
	upSQL := readMigrationFile(t, "603_im_friend_request.up.sql")
	if _, err := s.pool.Exec(ctx, upSQL); err != nil {
		// The id DEFAULT calls gen_random_uuid(), built in from PG13 and
		// otherwise provided by pgcrypto. Installing an extension usually needs
		// privileges a hosted test database does not grant, so an old server is a
		// skip rather than a failure.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42883" && strings.Contains(pgErr.Message, "gen_random_uuid") {
			t.Skipf("sandbox Postgres has no gen_random_uuid() (needs pgcrypto or PG13+): %v", err)
		}
		t.Fatalf("apply 603_im_friend_request.up.sql: %v", err)
	}

	t.Run("up_creates_the_table_with_every_contract_column", func(t *testing.T) {
		// Every column the store reads or writes (friendRequestColumns plus the
		// decided_at / decided_by that Decide fills in) must be there, or the
		// first request fails at runtime instead of at deploy time.
		want := []string{
			"id", "from_user_id", "to_user_id", "device_node_id", "message",
			"status", "decided_at", "decided_by", "created_at", "updated_at",
		}
		got := s.columns(t, ctx)
		var missing []string
		for _, column := range want {
			if !slices.Contains(got, column) {
				missing = append(missing, column)
			}
		}
		if len(missing) > 0 {
			t.Fatalf("im_friend_request is missing %v; columns present: %v", missing, got)
		}
	})

	t.Run("status_check_rejects_an_unknown_value", func(t *testing.T) {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO im_friend_request (from_user_id, to_user_id, device_node_id, status)
			VALUES ('requester-check', 'owner-check', 'node-check', 9)
		`); !isCheckViolation(err) {
			t.Fatalf("status = 9 gave %v, want a check violation; 603 must keep CHECK (status IN (0, 1, 2)) "+
				"because the store only ever writes 0, 1 and 2", err)
		}
		// The three legal values must all be accepted — Decide writes 1 and 2, so
		// a CHECK that only admitted 0 would break approval instead of the request.
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO im_friend_request (from_user_id, to_user_id, device_node_id, status)
			VALUES ('requester-check', 'owner-check', 'node-check-0', 0),
			       ('requester-check', 'owner-check', 'node-check-1', 1),
			       ('requester-check', 'owner-check', 'node-check-2', 2)
		`); err != nil {
			t.Fatalf("status 0/1/2 were rejected: %v", err)
		}
		// Leave the table empty so the next steps can assert absolute row counts.
		if _, err := s.pool.Exec(ctx, `DELETE FROM im_friend_request WHERE from_user_id = 'requester-check'`); err != nil {
			t.Fatalf("clean up the check rows: %v", err)
		}
	})

	t.Run("the_same_pending_request_is_upserted_not_duplicated", func(t *testing.T) {
		const (
			from   = "requester-upsert"
			owner  = "owner-upsert"
			device = "node-upsert"
		)

		first, err := s.upsertFriendRequest(t, ctx, from, owner, device, "第一次申请")
		if err != nil {
			t.Fatalf("the first request must insert cleanly, got %s (uq_im_friend_request_pending predicate: %s)",
				describeFriendRequestUpsertError(err), s.pendingIndexPredicate(t, ctx))
		}
		if !s.relationExists(t, ctx, "im_friend_request") {
			t.Fatal("im_friend_request vanished after the first insert")
		}

		// The second tap on 发送申请: this is the statement whose conflict target
		// has to infer the partial unique index.
		second, err := s.upsertFriendRequest(t, ctx, from, owner, device, "第二次申请")
		if err != nil {
			t.Fatalf("the second request must go through ON CONFLICT (from_user_id, device_node_id) WHERE status = 0 "+
				"and update the pending row, got %s (uq_im_friend_request_pending predicate: %s) — the partial index "+
				"predicate and the ON CONFLICT clause have drifted",
				describeFriendRequestUpsertError(err), s.pendingIndexPredicate(t, ctx))
		}
		if second.ID != first.ID {
			t.Errorf("the second request produced row %s instead of updating %s; DO UPDATE must keep the row identity",
				second.ID, first.ID)
		}
		if second.Message != "第二次申请" {
			t.Errorf("message = %q after the second request, want %q; DO UPDATE must refresh the remark",
				second.Message, "第二次申请")
		}
		if second.Status != 0 {
			t.Errorf("status = %d after the second request, want 0; re-sending must not leave the pending state",
				second.Status)
		}
		if n := countRows(t, ctx, s.pool, `SELECT count(*) FROM im_friend_request`); n != 1 {
			t.Errorf("im_friend_request holds %d row(s) after the same request twice, want 1; zombie pending rows "+
				"would pile up in the owner's 新的朋友 list", n)
		}
	})

	t.Run("a_decided_request_does_not_block_a_new_pending_one", func(t *testing.T) {
		const (
			from   = "requester-history"
			owner  = "owner-history"
			device = "node-history"
		)
		first, err := s.upsertFriendRequest(t, ctx, from, owner, device, "第一次申请")
		if err != nil {
			t.Fatalf("seed a pending request: %s", describeFriendRequestUpsertError(err))
		}
		// Approve it the way FriendRequestStore.Decide does.
		if _, err := s.pool.Exec(ctx, `
			UPDATE im_friend_request
			SET status = 1, decided_at = now(), decided_by = $2, updated_at = now()
			WHERE id = $1::uuid AND status = 0
		`, first.ID, owner); err != nil {
			t.Fatalf("approve the seeded request: %v", err)
		}

		second, err := s.upsertFriendRequest(t, ctx, from, owner, device, "再次申请")
		if err != nil {
			t.Fatalf("a new pending request after an approved one must succeed, got %s; the partial index may only "+
				"constrain status = 0, or the requester can never ask twice",
				describeFriendRequestUpsertError(err))
		}
		if second.ID == first.ID {
			t.Errorf("the new request reused the already approved row %s; the decision history must stay intact",
				first.ID)
		}
		if n := countRows(t, ctx, s.pool,
			`SELECT count(*) FROM im_friend_request WHERE from_user_id = $1 AND device_node_id = $2`,
			from, device); n != 2 {
			t.Errorf("%d row(s) for one requester and one device, want 2 (one approved, one pending); "+
				"the partial index must scope the uniqueness to status = 0", n)
		}
	})

	t.Run("down_rolls_back_and_can_be_replayed", func(t *testing.T) {
		applyMigrationFile(t, ctx, s.pool, "603_im_friend_request.down.sql")
		s.assertRelationsGone(t, ctx, "the first 603 down")

		// The down file uses DROP ... IF EXISTS throughout, so a deploy that
		// retries the rollback must not fail on the second pass.
		applyMigrationFile(t, ctx, s.pool, "603_im_friend_request.down.sql")
		s.assertRelationsGone(t, ctx, "a replayed 603 down")
	})
}
