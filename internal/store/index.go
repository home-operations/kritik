package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
)

// EnsureIndexSchema makes index_chunks match the instance's embedder: it
// creates the table at dims on first use and records model and dims in
// index_schema. When either differs from what is recorded, every
// generation is dropped with the table and it is rebuilt, so each
// repository is indexed afresh by the onboarding that follows; rebuilt
// reports that. The dashboard asks the admin to confirm such a change
// before it is saved. Leader only.
func (s *Store) EnsureIndexSchema(ctx context.Context, appRole, model string, dims int) (rebuilt bool, err error) {
	if s.owner == nil {
		return false, errors.New("store: EnsureIndexSchema needs the owner connection")
	}
	if dims <= 0 || dims > configfile.MaxEmbedDims {
		return false, fmt.Errorf("store: embedding dimension %d is outside the index limit of %d", dims, configfile.MaxEmbedDims)
	}
	err = pgx.BeginFunc(ctx, s.owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kritik-index-schema'))`); err != nil {
			return fmt.Errorf("store: lock index schema: %w", err)
		}
		curModel, curDims, ok, err := IndexSchemaIn(ctx, tx)
		switch {
		case err != nil:
			return err
		case !ok:
			return createIndexChunks(ctx, tx, appRole, model, dims)
		case curModel == model && curDims == dims:
			return nil
		}
		// No generation is valid for a different embedder, so detach every
		// repository and drop the vectors with the table.
		stmts := []string{
			`UPDATE repositories SET active_index_run_id = NULL`,
			`UPDATE index_runs SET status = 'superseded', finished_at = coalesce(finished_at, now()) WHERE status IN ('running', 'completed')`,
			`DROP TABLE IF EXISTS index_chunks`,
			`DELETE FROM index_schema WHERE id = 1`,
		}
		for _, stmt := range stmts {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("store: rebuild index schema: %w", err)
			}
		}
		rebuilt = true
		return createIndexChunks(ctx, tx, appRole, model, dims)
	})
	return rebuilt && err == nil, err
}

// IndexSchemaIn is the embedding model and dimension index_chunks was built
// for, read in tx; ok is false when it was never built.
func IndexSchemaIn(ctx context.Context, tx pgx.Tx) (model string, dims int, ok bool, err error) {
	err = tx.QueryRow(ctx, `SELECT embed_model, embed_dims FROM index_schema WHERE id = 1`).Scan(&model, &dims)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("store: read index schema: %w", err)
	}
	return model, dims, true, nil
}

func createIndexChunks(ctx context.Context, tx pgx.Tx, appRole, model string, dims int) error {
	app := pgx.Identifier{appRole}.Sanitize()
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS index_chunks (
			id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
			account_id     uuid        NOT NULL REFERENCES accounts (id),
			repository_id uuid        NOT NULL REFERENCES repositories (id),
			index_run_id  uuid        NOT NULL REFERENCES index_runs (id) ON DELETE CASCADE,
			path          text        NOT NULL,
			start_line    int         NOT NULL,
			end_line      int         NOT NULL,
			language      text        NOT NULL DEFAULT '',
			symbol        text        NOT NULL DEFAULT '',
			kind          text        NOT NULL DEFAULT '',
			scope         text        NOT NULL DEFAULT '',
			text          text        NOT NULL,
			embedding     halfvec(%d) NOT NULL,
			created_at    timestamptz NOT NULL DEFAULT now()
		)`, dims),
		`CREATE INDEX IF NOT EXISTS index_chunks_run_path_idx ON index_chunks (index_run_id, path)`,
		`CREATE INDEX IF NOT EXISTS index_chunks_account_id_idx ON index_chunks (account_id)`,
		// VectorChord's access method: it partitions and quantises rather
		// than building a graph, so it builds fast and answers a filtered
		// query in full. Unpartitioned, since the table stays far below the
		// size at which VectorChord recommends lists.
		`CREATE INDEX IF NOT EXISTS index_chunks_embedding_idx ON index_chunks USING vchordrq (embedding halfvec_cosine_ops)`,
		`ALTER TABLE index_chunks ENABLE ROW LEVEL SECURITY`,
		`DROP POLICY IF EXISTS account_isolation ON index_chunks`,
		`CREATE POLICY account_isolation ON index_chunks
			USING      (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)
			WITH CHECK (account_id = NULLIF(current_setting('app.account_id', true), '')::uuid)`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON index_chunks TO ` + app,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store: create index_chunks: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO index_schema (id, embed_model, embed_dims) VALUES (1, $1, $2)`, model, dims); err != nil {
		return fmt.Errorf("store: record index schema: %w", err)
	}
	return nil
}

// SweepDisabledIndexes drops the index of every repository disabled for
// longer than grace and returns how many it dropped. The repository is left
// with no active generation, which is marked superseded, so enabling it
// again onboards a fresh index. It runs on the owner connection, which
// row-level security does not restrict. Leader only.
func (s *Store) SweepDisabledIndexes(ctx context.Context, grace time.Duration) (int64, error) {
	if s.owner == nil {
		return 0, errors.New("store: SweepDisabledIndexes needs the owner connection")
	}
	var runs []string
	err := pgx.BeginFunc(ctx, s.owner, func(tx pgx.Tx) error {
		// Locked, so a repository enabled again meanwhile is either left out
		// or waits until its index is gone, never swept halfway.
		rows, err := tx.Query(ctx, `SELECT active_index_run_id::text FROM repositories
			WHERE active_index_run_id IS NOT NULL AND (NOT enabled AND disabled_at < now() - make_interval(secs => $1)
				OR NOT coalesce(turned_on, true) AND turned_at < now() - make_interval(secs => $1))
			FOR UPDATE`, grace.Seconds())
		if err != nil {
			return err
		}
		if runs, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(runs) == 0 {
			return err
		}
		for _, stmt := range []string{
			`UPDATE repositories SET active_index_run_id = NULL, updated_at = now() WHERE active_index_run_id = ANY($1::uuid[])`,
			`UPDATE index_runs SET status = 'superseded', finished_at = coalesce(finished_at, now()) WHERE id = ANY($1::uuid[])`,
			`DELETE FROM index_chunks WHERE index_run_id = ANY($1::uuid[])`,
		} {
			if _, err := tx.Exec(ctx, stmt, runs); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: sweep disabled indexes: %w", err)
	}
	return int64(len(runs)), nil
}

// RepoRef names a repository and its account.
type RepoRef struct{ ID, AccountID string }

// liveIndexJob is an index job of a repository still queued or running,
// in River's own table: the states its unique key spans.
const liveIndexJob = `j.kind = 'index' AND j.state IN ('available', 'pending', 'running', 'scheduled', 'retryable')`

// OnboardingInFlight counts onboarding index jobs queued or running.
// Owner connection: it spans every account.
func (s *Store) OnboardingInFlight(ctx context.Context) (int, error) {
	if s.owner == nil {
		return 0, errors.New("store: OnboardingInFlight needs the owner connection")
	}
	var n int
	if err := s.owner.QueryRow(ctx, `SELECT count(*) FROM river_job j WHERE `+liveIndexJob+` AND j.args->>'trigger' = 'onboard'`).
		Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count onboarding jobs: %w", err)
	}
	return n, nil
}

// OnboardCandidates lists up to limit enabled repositories, of those
// enabled also admits by account ID, full name and traits, with no active index
// generation, no index job queued or running, and no onboarding job
// that finished within retryAfter without building an index: one that
// failed or was skipped would fail or be skipped again. Accounts take turns,
// and within an account the repositories whose pull requests moved last come
// first, as the ones a review is likeliest to need soon. Owner connection:
// it spans every account.
func (s *Store) OnboardCandidates(
	ctx context.Context, limit int, retryAfter time.Duration, enabled func(accountID, fullName string, t configfile.RepoTraits) bool,
) ([]RepoRef, error) {
	if s.owner == nil {
		return nil, errors.New("store: OnboardCandidates needs the owner connection")
	}
	rows, err := s.owner.Query(ctx, `WITH candidates AS (
			SELECT r.id, r.account_id, r.name, r.archived, r.fork, r.turned_on, r.created_at,
				(SELECT max(p.updated_at) FROM pull_requests p WHERE p.repository_id = r.id) AS active
			FROM repositories r
			WHERE r.enabled AND r.active_index_run_id IS NULL
			  AND NOT EXISTS (SELECT 1 FROM river_job j WHERE `+liveIndexJob+` AND j.args->>'repository_id' = r.id::text)
			  AND NOT EXISTS (SELECT 1 FROM river_job j WHERE j.kind = 'index' AND j.args->>'repository_id' = r.id::text
			                  AND j.args->>'trigger' = 'onboard' AND j.finalized_at > now() - make_interval(secs => $1)
			                  AND NOT EXISTS (SELECT 1 FROM index_runs ir WHERE ir.repository_id = r.id
			                                  AND ir.status IN ('completed', 'superseded') AND ir.created_at >= j.created_at))
		)
		SELECT id, account_id, name, archived, fork, turned_on FROM (
			SELECT c.*, row_number() OVER (PARTITION BY account_id ORDER BY active DESC NULLS LAST, created_at, id) AS turn FROM candidates c
		) ranked
		ORDER BY turn, active DESC NULLS LAST, created_at, id`, retryAfter.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: list onboarding candidates: %w", err)
	}
	defer rows.Close()
	var refs []RepoRef
	for len(refs) < limit && rows.Next() {
		var r RepoRef
		var name string
		var t configfile.RepoTraits
		if err := rows.Scan(&r.ID, &r.AccountID, &name, &t.Archived, &t.Fork, &t.TurnedOn); err != nil {
			return nil, fmt.Errorf("store: list onboarding candidates: %w", err)
		}
		if enabled(r.AccountID, name, t) {
			refs = append(refs, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list onboarding candidates: %w", err)
	}
	return refs, nil
}
