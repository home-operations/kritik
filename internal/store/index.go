package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrIndexSchemaMismatch is returned when index_chunks was created for a
// different embedding model or dimension than the deployment now runs.
var ErrIndexSchemaMismatch = errors.New("store: index_chunks was built for a different embedding model")

// EnsureIndexSchema creates index_chunks at the deployment's embedding
// dimension on first use, records the model and dimension in
// index_schema, and on later starts checks they still match. A changed
// model at the same dimension, or a changed dimension, is refused unless
// reindex is set, in which case every generation is dropped and the table
// is rebuilt: each repository is then re-indexed from scratch by its next
// index job. Leader only.
func (s *Store) EnsureIndexSchema(ctx context.Context, appRole, model string, dims int, reindex bool) error {
	if s.owner == nil {
		return errors.New("store: EnsureIndexSchema needs the owner connection")
	}
	if dims <= 0 || dims > 4000 {
		return fmt.Errorf("store: embedding dimension %d is outside halfvec index limits", dims)
	}
	return pgx.BeginFunc(ctx, s.owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kritik-index-schema'))`); err != nil {
			return err
		}
		var curModel string
		var curDims int
		err := tx.QueryRow(ctx, `SELECT embed_model, embed_dims FROM index_schema WHERE id = 1`).Scan(&curModel, &curDims)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return createIndexChunks(ctx, tx, appRole, model, dims)
		case err != nil:
			return fmt.Errorf("store: read index schema: %w", err)
		case curModel == model && curDims == dims:
			return nil
		case !reindex:
			return fmt.Errorf("%w: table has %s/%d, deployment wants %s/%d (set KRITIK_REINDEX_ON_MODEL_CHANGE=true to rebuild)",
				ErrIndexSchemaMismatch, curModel, curDims, model, dims)
		}
		// Rebuild: no generation is valid for a different embedder, so
		// detach every repository and drop the vectors with the table.
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
		return createIndexChunks(ctx, tx, appRole, model, dims)
	})
}

func createIndexChunks(ctx context.Context, tx pgx.Tx, appRole, model string, dims int) error {
	app := pgx.Identifier{appRole}.Sanitize()
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS index_chunks (
			id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
			tenant_id     uuid        NOT NULL REFERENCES tenants (id),
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
		`CREATE INDEX IF NOT EXISTS index_chunks_tenant_id_idx ON index_chunks (tenant_id)`,
		`CREATE INDEX IF NOT EXISTS index_chunks_embedding_idx ON index_chunks USING hnsw (embedding halfvec_cosine_ops)`,
		`ALTER TABLE index_chunks ENABLE ROW LEVEL SECURITY`,
		`DROP POLICY IF EXISTS tenant_isolation ON index_chunks`,
		`CREATE POLICY tenant_isolation ON index_chunks
			USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
			WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)`,
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

// IndexSchema reports the model and dimension index_chunks exists for, or
// ok=false when no embedder has ever been configured.
func (s *Store) IndexSchema(ctx context.Context) (model string, dims int, ok bool, err error) {
	err = s.app.QueryRow(ctx, `SELECT embed_model, embed_dims FROM index_schema WHERE id = 1`).Scan(&model, &dims)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("store: read index schema: %w", err)
	}
	return model, dims, true, nil
}

// RepoRef names a repository and its tenant.
type RepoRef struct{ ID, TenantID string }

// RepositoriesWithoutIndex lists enabled repositories that have no active
// index generation, for the leader to enqueue onboarding index jobs.
// Owner connection: it spans every tenant.
func (s *Store) RepositoriesWithoutIndex(ctx context.Context) ([]RepoRef, error) {
	if s.owner == nil {
		return nil, errors.New("store: RepositoriesWithoutIndex needs the owner connection")
	}
	rows, err := s.owner.Query(ctx, `SELECT id, tenant_id FROM repositories WHERE enabled AND active_index_run_id IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list unindexed repositories: %w", err)
	}
	refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RepoRef, error) {
		var r RepoRef
		err := row.Scan(&r.ID, &r.TenantID)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list unindexed repositories: %w", err)
	}
	return refs, nil
}
