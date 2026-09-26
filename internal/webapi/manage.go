package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// metaPath is the one API route served without a session.
const metaPath = "/api/v1/meta"

// slugPath points an error at the spec's slug.
var slugPath = pathDetails{Path: "slug"}

// maxBodyBytes bounds a request body; a tenant spec is far smaller.
const maxBodyBytes = 1 << 20

// registerManage mounts dashboard tenant management.
func (s *Server) registerManage(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/tenants/{slug}/config", s.handler(s.getTenantConfig))
	mux.HandleFunc("POST /api/v1/tenants", s.handler(s.createTenant))
	mux.HandleFunc("PUT /api/v1/tenants/{slug}/config", s.handler(s.updateTenant))
	mux.HandleFunc("DELETE /api/v1/tenants/{slug}", s.handler(s.deleteTenant))
}

func (s *Server) getMeta(w http.ResponseWriter, _ *http.Request) error {
	m := Meta{Version: s.version, Management: s.keyring != nil, SignIn: s.auth.Providers()}
	if s.webURL != nil {
		m.WebURL = s.webURL.String()
	}
	writeJSON(w, http.StatusOK, m)
	return nil
}

// tenantIDFor is the id the tenant with slug has, or will have once the
// leader applies it.
func tenantIDFor(slug string) string { return (&configfile.Tenant{Slug: slug}).ID() }

var (
	errForbidden          = errStatus(http.StatusForbidden, CodeForbidden, "this needs a tenant admin", nil)
	errOperatorOnly       = errStatus(http.StatusForbidden, CodeForbidden, "this needs an instance operator", nil)
	errFileManaged        = errStatus(http.StatusForbidden, CodeFileManaged, "this tenant is declared in the configuration file", nil)
	errManagementDisabled = errStatus(http.StatusServiceUnavailable, CodeManagementDisabled,
		"dashboard tenants cannot be written: KRITIK_DASHBOARD_KEY is not set", nil)
	errDashboardSlugTaken = errStatus(http.StatusConflict, CodeSlugTaken, "a dashboard tenant with this slug already exists", slugPath)
	errRevisionConflict   = errStatus(http.StatusConflict, CodeRevisionConflict,
		"the tenant was changed by another write; reload it and try again", nil)
)

// configTarget resolves {slug} for a config route: a live tenant p may
// read, or, for an operator, a stored dashboard tenant that is not live
// (it does not validate, or has not merged yet). live is nil for the
// latter.
func (s *Server) configTarget(p *auth.Principal, slug string) (live *configfile.Tenant, err error) {
	t, ok := s.current.Get().Tenant(slug)
	switch {
	case ok && p.CanRead(t.ID()):
		return t, nil
	case !ok && p.Operator:
		return nil, nil
	default:
		return nil, errNotFound("tenant")
	}
}

func (s *Server) getTenantConfig(w http.ResponseWriter, r *http.Request) error {
	ctx, p, slug := r.Context(), auth.PrincipalFrom(r.Context()), r.PathValue("slug")
	live, err := s.configTarget(p, slug)
	if err != nil {
		return err
	}
	out := TenantConfig{OperatorOnlyFields: operatorOnlyFields}
	if live != nil && live.Origin() == configfile.OriginFile {
		out.ManagedBy = configfile.OriginFile
		if out.Spec, err = renderFileTenant(live); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}
	var d configfile.DashboardTenant
	if err := s.store.WithTenant(ctx, tenantIDFor(slug), func(tx pgx.Tx) error {
		d, _, err = s.store.DashboardTenant(ctx, tx, slug)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errNotFound("tenant")
		}
		return err
	}
	out.ManagedBy = configfile.OriginDashboard
	out.Revision = &d.Revision
	out.Editable = s.keyring != nil && p.CanAdmin(tenantIDFor(slug))
	if out.Spec, err = redactSpec(d.Spec); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) error {
	p := auth.PrincipalFrom(r.Context())
	if !p.Operator {
		return errOperatorOnly
	}
	if s.keyring == nil {
		return errManagementDisabled
	}
	var req CreateTenantRequest
	if err := readBody(r, &req); err != nil {
		return err
	}
	if req.Slug == "" || len(req.Spec) == 0 {
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, "slug and spec are required", nil)
	}
	if t, ok := s.current.Get().Tenant(req.Slug); ok && t.Origin() == configfile.OriginFile {
		return errStatus(http.StatusConflict, CodeSlugTaken, "the configuration file already declares this slug", slugPath)
	}
	res, err := s.writeTenant(r.Context(), p, req.Slug, req.Spec, 0, req.Adopt)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, res)
	return nil
}

func (s *Server) updateTenant(w http.ResponseWriter, r *http.Request) error {
	p, slug := auth.PrincipalFrom(r.Context()), r.PathValue("slug")
	live, err := s.configTarget(p, slug)
	if err != nil {
		return err
	}
	switch {
	case live != nil && live.Origin() == configfile.OriginFile:
		return errFileManaged
	case live != nil && !p.CanAdmin(live.ID()):
		return errForbidden
	case s.keyring == nil:
		return errManagementDisabled
	}
	var req UpdateTenantRequest
	if err := readBody(r, &req); err != nil {
		return err
	}
	if req.Revision <= 0 || len(req.Spec) == 0 {
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, "revision and spec are required", nil)
	}
	res, err := s.writeTenant(r.Context(), p, slug, req.Spec, req.Revision, false)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

// tenantAudit is a tenant write's audit detail: which secrets were given
// a new value, never the values.
type tenantAudit struct {
	Revision int64    `json:"revision"`
	Secrets  []string `json:"secretsChanged,omitempty"`
}

// writeTenant validates spec as the dashboard tenant slug and stores it,
// with its audit row, in one transaction. expected is the revision it
// replaces, 0 to create it; adopt lets a create re-use a slug a tenant
// held before.
func (s *Server) writeTenant(
	ctx context.Context, p *auth.Principal, slug string, spec json.RawMessage, expected int64, adopt bool,
) (TenantWriteResult, error) {
	tid := tenantIDFor(slug)
	res := TenantWriteResult{Slug: slug}
	err := s.store.WithTenant(ctx, tid, func(tx pgx.Tx) error {
		if err := store.LockDashboardWrites(ctx, tx); err != nil {
			return err
		}
		var prev *configfile.DashboardTenant
		if expected > 0 {
			d, _, err := s.store.DashboardTenant(ctx, tx, slug)
			if errors.Is(err, store.ErrNotFound) {
				return errNotFound("tenant")
			}
			if err != nil {
				return err
			}
			if d.Revision != expected {
				return errRevisionConflict
			}
			prev = &d
		} else {
			_, _, err := s.store.DashboardTenant(ctx, tx, slug)
			switch {
			case err == nil:
				return errDashboardSlugTaken
			case !errors.Is(err, store.ErrNotFound):
				return err
			}
			if err := s.claimSlug(ctx, tx, p, tid, slug, adopt); err != nil {
				return err
			}
		}
		dash, err := store.DashboardTenantsIn(ctx, tx)
		if err != nil {
			return err
		}
		candidate, sealed, err := s.checkSpec(p, slug, spec, prev, dash)
		if err != nil {
			return err
		}
		if err := s.checkTakeover(ctx, tx, tid, candidate); err != nil {
			return err
		}
		action := AuditTenantUpdate
		if expected == 0 {
			action = AuditTenantCreate
		}
		rev, err := s.store.PutDashboardTenant(ctx, tx, slug, sealed.spec, expected, p.Account.ID)
		switch {
		case errors.Is(err, store.ErrDashboardConflict) && expected == 0:
			return errDashboardSlugTaken
		case errors.Is(err, store.ErrDashboardConflict):
			return errRevisionConflict
		case err != nil:
			return err
		}
		res.Revision, res.Generated = rev, sealed.generated
		return record(ctx, tx, p, &tid, action, slug, tenantAudit{Revision: rev, Secrets: sealed.changed})
	})
	return res, err
}

// claimSlug refuses a create whose slug a tenant held before, enabled or
// not: tenant ids derive from slugs, so the new tenant would see the old
// one's reviews, findings and transcripts. adopt accepts that, clearing
// the old tenant's members and invites; only a refusal that adopt would
// overcome says so (slugTakenDetails.Adoptable), never one for a tenant
// the file still manages.
func (s *Server) claimSlug(ctx context.Context, tx pgx.Tx, p *auth.Principal, tid, slug string, adopt bool) error {
	held, err := store.TenantRowExists(ctx, tx, tid)
	switch {
	case err != nil:
		return err
	case !held:
		return nil
	}
	// A tenant the file still manages is not gone: it cannot be adopted.
	live, err := store.LiveNonDashboard(ctx, tx, tid, nil)
	switch {
	case err != nil:
		return err
	case len(live) > 0:
		return errStatus(http.StatusConflict, CodeSlugTaken, "slug is still in use by a tenant the configuration file manages", slugPath)
	case !adopt:
		return errStatus(http.StatusConflict, CodeSlugTaken,
			"a tenant used this slug before; creating it again with adopt keeps that tenant's review history",
			slugTakenDetails{Path: slugPath.Path, Adoptable: true})
	}
	if err := store.DeleteTenantAccess(ctx, tx, tid); err != nil {
		return err
	}
	return record(ctx, tx, p, &tid, AuditTenantAdopt, slug, struct{}{})
}

// checkSpec seals spec's secrets against the tenant it replaces (nil on
// create), holds a non-operator to the fields it may change, and checks
// the result merges with the running file and dash, the dashboard tenants
// as the write's transaction reads them.
func (s *Server) checkSpec(
	p *auth.Principal, slug string, spec json.RawMessage, prev *configfile.DashboardTenant, dash []configfile.DashboardTenant,
) (*configfile.Tenant, sealedSpec, error) {
	var stored json.RawMessage
	if prev != nil {
		stored = prev.Spec
	}
	sealed, err := sealSpec(spec, stored, s.keyring.Seal, generateWebhookSecret)
	if se, ok := errors.AsType[*specError](err); ok {
		return nil, sealed, errStatus(http.StatusUnprocessableEntity, se.errorCode(), se.Error(), pathDetails{Path: se.path})
	}
	if err != nil {
		return nil, sealed, err
	}
	candidate := configfile.DashboardTenant{Slug: slug, Spec: sealed.spec, Revision: 1}
	if prev != nil {
		candidate.Revision = prev.Revision + 1
	}
	next, err := configfile.DecodeTenant(candidate)
	if err != nil {
		return nil, sealed, decodeFailure(err)
	}
	if err := checkDashboardHosts(&next); err != nil {
		return nil, sealed, err
	}
	if prev != nil && !p.Operator {
		old, err := configfile.DecodeTenant(*prev)
		if err != nil {
			return nil, sealed, err
		}
		if path := operatorOnlyChange(&old, &next); path != "" {
			return nil, sealed, errStatus(http.StatusUnprocessableEntity, CodeOperatorOnly,
				path+" can only be changed by an instance operator", pathDetails{Path: path})
		}
	}
	current := s.current.Get()
	if err := configfile.ValidateDashboard(current, dash, candidate, s.keyring); err != nil {
		return nil, sealed, mergeFailure(slug, &next, err, func() error { return validateWithout(current, dash, slug, s.keyring) })
	}
	return &next, sealed, nil
}

// checkDashboardHosts refuses a dashboard installation on a plain-http
// forge: its credentials would cross the network in the clear.
func checkDashboardHosts(t *configfile.Tenant) error {
	for i := range t.Installations {
		if h := strings.ToLower(strings.TrimSpace(t.Installations[i].Host)); strings.HasPrefix(h, "http://") {
			path := "installations[" + strconv.Itoa(i) + "].host"
			return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec,
				path+": a dashboard installation must reach its forge over https", pathDetails{Path: path})
		}
	}
	return nil
}

// checkTakeover refuses a write that would claim a live tenant or
// installation row another origin manages (one the file dropped that the
// leader has not disabled yet, say) or an installation name another
// tenant holds in any state, which the leader would refuse to hand over.
// The merge already refuses anything the running file still declares.
func (s *Server) checkTakeover(ctx context.Context, tx pgx.Tx, tenantID string, t *configfile.Tenant) error {
	names := make([]string, len(t.Installations))
	for i := range t.Installations {
		names[i] = t.Installations[i].Name
	}
	taken, err := store.LiveNonDashboard(ctx, tx, tenantID, names)
	if err != nil {
		return err
	}
	msg := " is still in use by a tenant the configuration file manages"
	if len(taken) == 0 {
		if taken, err = store.InstallationsHeldElsewhere(ctx, tx, tenantID, names); err != nil {
			return err
		}
		msg = " belongs to another tenant"
	}
	if len(taken) == 0 {
		return nil
	}
	path := slugPath.Path
	if taken[0] != slugPath.Path {
		for i := range t.Installations {
			if t.Installations[i].Name == taken[0] {
				path = "installations[" + strconv.Itoa(i) + "].name"
			}
		}
	}
	return errStatus(http.StatusConflict, CodeSlugTaken, path+msg, pathDetails{Path: path})
}

func (s *Server) deleteTenant(w http.ResponseWriter, r *http.Request) error {
	ctx, p, slug := r.Context(), auth.PrincipalFrom(r.Context()), r.PathValue("slug")
	if !p.Operator {
		return errOperatorOnly
	}
	if t, ok := s.current.Get().Tenant(slug); ok && t.Origin() == configfile.OriginFile {
		return errFileManaged
	}
	if s.keyring == nil {
		return errManagementDisabled
	}
	rev, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || rev <= 0 {
		return errBadRequest(CodeBadRequest, "revision must be the tenant's current revision")
	}
	tid := tenantIDFor(slug)
	err = s.store.WithTenant(ctx, tid, func(tx pgx.Tx) error {
		if err := store.LockDashboardWrites(ctx, tx); err != nil {
			return err
		}
		err := s.store.DeleteDashboardTenant(ctx, tx, slug, rev)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return errNotFound("tenant")
		case errors.Is(err, store.ErrDashboardConflict):
			return errRevisionConflict
		case err != nil:
			return err
		}
		if err := store.DeleteTenantAccess(ctx, tx, tid); err != nil {
			return err
		}
		return record(ctx, tx, p, &tid, AuditTenantDelete, slug, tenantAudit{Revision: rev})
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// readBody strictly decodes one JSON document into v.
func readBody(r *http.Request, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err != nil {
		return errBadRequest(CodeBadRequest, "request body is too large or unreadable")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errBadRequest(CodeBadRequest, "request body is not valid JSON: "+err.Error())
	}
	if dec.More() {
		return errBadRequest(CodeBadRequest, "request body must hold one JSON document")
	}
	return nil
}
