//go:build integration

package webapi

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// claimSpec is a dashboard tenant with one installation per name.
func claimSpec(slug string, names ...string) map[string]any {
	ins := make([]any, 0, len(names))
	for _, n := range names {
		ins = append(ins, map[string]any{
			"name": n, "forge": "forgejo", "host": "git.example", "account": slug,
			"token": map[string]any{"value": "t"}, "webhookSecret": map[string]any{"value": "w"},
		})
	}
	return map[string]any{"slug": slug, "installations": ins}
}

// TestManageClaims covers what dashboard writes may claim: slugs and
// installation names other writes, or tenants gone before, hold.
func TestManageClaims(t *testing.T) {
	e := newManageEnv(t)
	for _, slug := range []string{"clm-a", "clm-b"} {
		status, body := e.do("operator", "POST", "/api/v1/tenants",
			CreateTenantRequest{Slug: slug, Spec: mustJSON(t, claimSpec(slug, slug+"-bot"))})
		e.expect(status, body, http.StatusCreated, "")
	}
	e.waitFor("clm-a and clm-b to merge", func(f *configfile.File) bool {
		_, a := f.Tenant("clm-a")
		_, b := f.Tenant("clm-b")
		return a && b
	})
	t.Run("concurrent writes claiming one installation name", func(t *testing.T) { testConcurrentClaims(t, e) })
	t.Run("a slug a tenant held before", func(t *testing.T) { testSlugReuse(t, e) })
}

// testConcurrentClaims races pairs of writes that each add one name: a
// single race is easily won by timing alone, so it runs a few.
func testConcurrentClaims(t *testing.T, e *manageEnv) {
	for round := range 8 {
		pair := []string{"clm-a", "clm-b"}
		shared := "clm-shared-bot"
		if round > 0 {
			r := strconv.Itoa(round)
			pair, shared = []string{"clm-a" + r, "clm-b" + r}, "clm-shared-bot"+r
			for _, slug := range pair {
				status, body := e.do("operator", "POST", "/api/v1/tenants",
					CreateTenantRequest{Slug: slug, Spec: mustJSON(t, claimSpec(slug, slug+"-bot"))})
				e.expect(status, body, http.StatusCreated, "")
			}
		}
		statuses := make([]int, 2)
		var wg sync.WaitGroup
		for i, slug := range pair {
			wg.Go(func() {
				statuses[i], _ = e.do("operator", "PUT", "/api/v1/tenants/"+slug+"/config",
					UpdateTenantRequest{Revision: 1, Spec: mustJSON(t, claimSpec(slug, slug+"-bot", shared))})
			})
		}
		wg.Wait()
		if !slices.Contains(statuses, http.StatusOK) || !slices.Contains(statuses, http.StatusUnprocessableEntity) {
			t.Fatalf("round %d: statuses = %v, want one 200 and one 422", round, statuses)
		}
		e.waitFor("the winner to merge", func(f *configfile.File) bool { _, _, ok := f.Installation(shared); return ok })
		if err := e.src.LastError(); err != nil {
			t.Fatalf("round %d: merge after the race: %v", round, err)
		}
	}
}

func testSlugReuse(t *testing.T, e *manageEnv) {
	ctx := context.Background()
	if err := e.st.ApplyConfig(ctx, e.src.Current.Get(), "claims-test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	rev := e.scalar(`SELECT revision::text FROM dashboard_tenants WHERE slug = 'clm-b'`)
	status, body := e.do("operator", "DELETE", "/api/v1/tenants/clm-b?revision="+rev, nil)
	e.expect(status, body, http.StatusNoContent, "")
	e.waitFor("clm-b to leave", func(f *configfile.File) bool { _, ok := f.Tenant("clm-b"); return !ok })
	if err := e.st.ApplyConfig(ctx, e.src.Current.Get(), "claims-test"); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	// The disabled installation stays clm-b's, whoever asks for it.
	status, body = e.do("operator", "POST", "/api/v1/tenants",
		CreateTenantRequest{Slug: "clm-c", Spec: mustJSON(t, claimSpec("clm-c", "clm-b-bot"))})
	e.expect(status, body, http.StatusConflict, CodeSlugTaken)
	if !strings.Contains(string(body), `"path":"installations[0].name"`) {
		t.Errorf("held installation = %s", body)
	}

	bID := (&configfile.Tenant{Slug: "clm-b"}).ID()
	e.signIn("clm-old-admin", "clm-old-admin", []store.Grant{{TenantID: bID, Role: store.RoleAdmin}})
	spec := mustJSON(t, claimSpec("clm-b", "clm-b-bot"))
	status, body = e.do("operator", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "clm-b", Spec: spec})
	e.expect(status, body, http.StatusConflict, CodeSlugTaken)
	if !strings.Contains(string(body), `"adoptable":true`) {
		t.Errorf("slug used before = %s, want it adoptable", body)
	}
	// A live dashboard tenant's slug is taken, not adoptable.
	status, body = e.do("operator", "POST", "/api/v1/tenants",
		CreateTenantRequest{Slug: "clm-a", Spec: mustJSON(t, claimSpec("clm-a", "clm-a-bot")), Adopt: true})
	e.expect(status, body, http.StatusConflict, CodeSlugTaken)
	if strings.Contains(string(body), "adoptable") || e.audits(AuditTenantAdopt, "clm-a") != 0 {
		t.Errorf("live dashboard slug = %s, want a plain refusal and no adopt", body)
	}
	if e.audits(AuditTenantAdopt, "clm-b") != 0 {
		t.Fatal("a refused create was audited as an adopt")
	}
	status, body = e.do("operator", "POST", "/api/v1/tenants", CreateTenantRequest{Slug: "clm-b", Spec: spec, Adopt: true})
	e.expect(status, body, http.StatusCreated, "")
	if e.audits(AuditTenantAdopt, "clm-b") != 1 {
		t.Error("tenant.adopt audit rows are wrong")
	}
	if n := e.scalar(`SELECT count(*)::text FROM memberships WHERE tenant_id = '` + bID + `'`); n != "0" {
		t.Errorf("%s memberships of the old tenant survived the adopt", n)
	}
}
