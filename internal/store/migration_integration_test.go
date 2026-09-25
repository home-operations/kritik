//go:build integration

package store

import (
	"context"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/review"
)

// TestContractMigrationKeepsFindings replays the migrations before 0009 into
// a scratch schema, seeds findings in the old shape, then applies 0009 and
// checks every row survived with its severity mapped and its body moved.
func TestContractMigrationKeepsFindings(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	tx, err := s.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `CREATE SCHEMA contract_migration; SET LOCAL search_path TO contract_migration, public`); err != nil {
		t.Fatal(err)
	}
	names, _ := fs.Glob(migrationFS, "migrations/*.sql")
	sort.Strings(names)
	apply := func(name string) {
		t.Helper()
		sql, err := migrationFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var contract string
	for _, name := range names {
		if strings.HasPrefix(name, "migrations/0009_") {
			contract = name
			break
		}
		apply(name)
	}
	if contract == "" {
		t.Fatal("no 0009 migration")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tenants (id, slug, managed_by) VALUES ('00000000-0000-0000-0000-000000000001', 'acme', 'file');
		INSERT INTO installations (id, tenant_id, name, forge, account, credential_kind, managed_by)
			VALUES ('00000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-000000000001', 'acme-bot', 'forgejo', 'acme', 'token', 'file');
		INSERT INTO repositories (id, tenant_id, installation_id, name, managed_by)
			VALUES ('00000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000002', 'acme/widgets', 'file');
		INSERT INTO pull_requests (id, tenant_id, repository_id, number, head_sha)
			VALUES ('00000000-0000-0000-0000-000000000004', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000003', 1, 'abc');
		INSERT INTO reviews (id, tenant_id, pull_request_id, head_sha, status)
			VALUES ('00000000-0000-0000-0000-000000000005', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000004', 'abc', 'completed');
		INSERT INTO findings (tenant_id, review_id, path, line, severity, title, body) VALUES
			('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000005', 'a.go', 1, 'error', ' E ', 'body e'),
			('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000005', 'a.go', 2, 'warning', 'w', 'body w'),
			('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000005', 'a.go', 3, 'info', 'i', 'body i')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	apply(contract)

	rows, err := tx.Query(ctx, `SELECT title, severity, explanation, suggested_fix, fingerprint FROM findings ORDER BY line`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var title, severity, explanation, fix, fingerprint string
		if err := rows.Scan(&title, &severity, &explanation, &fix, &fingerprint); err != nil {
			t.Fatal(err)
		}
		got = append(got, strings.TrimSpace(strings.ToLower(title))+":"+severity+":"+explanation+":"+fix+":"+fingerprint)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"e:blocking:body e::" + review.Fingerprint(review.Finding{Path: "a.go", Title: "e"}),
		"w:important:body w::" + review.Fingerprint(review.Finding{Path: "a.go", Title: "w"}),
		"i:nit:body i::" + review.Fingerprint(review.Finding{Path: "a.go", Title: "i"}),
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("findings = %v, want %v", got, want)
	}

	var mode, scope string
	if err := tx.QueryRow(ctx, `SELECT mode, scope FROM reviews`).Scan(&mode, &scope); err != nil || mode != "single" || scope != "full" {
		t.Fatalf("review defaults: mode=%q scope=%q err=%v", mode, scope, err)
	}
	for _, bad := range []string{
		`UPDATE findings SET severity = 'error'`,
		`UPDATE reviews SET mode = 'other'`,
		`UPDATE reviews SET scope = 'partial'`,
	} {
		if _, err := tx.Exec(ctx, `SAVEPOINT s`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, bad); err == nil {
			t.Fatalf("%q should violate a CHECK", bad)
		}
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT s`); err != nil {
			t.Fatal(err)
		}
	}
}
