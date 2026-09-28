package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/store"
)

// metaPath is the one API route served without a session.
const metaPath = "/api/v1/meta"

// slugPath points an error at the spec's slug.
var slugPath = pathDetails{Path: "slug"}

// maxBodyBytes bounds a request body; an account spec is far smaller.
const maxBodyBytes = 1 << 20

// registerManage mounts dashboard account management.
func (s *Server) registerManage(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/accounts/{slug}/config", s.handler(s.getAccountConfig))
	mux.HandleFunc("POST /api/v1/accounts", s.handler(s.createAccount))
	mux.HandleFunc("PUT /api/v1/accounts/{slug}/config", s.handler(s.updateAccount))
	mux.HandleFunc("DELETE /api/v1/accounts/{slug}", s.handler(s.deleteAccount))
}

func (s *Server) getMeta(w http.ResponseWriter, _ *http.Request) error {
	m := Meta{Version: s.version, Management: s.keyring != nil, SignIn: s.auth.Providers()}
	if s.webURL != nil {
		m.WebURL = s.webURL.String()
	}
	writeJSON(w, http.StatusOK, m)
	return nil
}

// accountIDFor is the id the account with slug has, or will have once the
// leader applies it.
func accountIDFor(slug string) string { return (&configfile.Account{Slug: slug}).ID() }

var (
	errForbidden          = errStatus(http.StatusForbidden, CodeForbidden, "this needs an admin", nil)
	errFileManaged        = errStatus(http.StatusForbidden, CodeFileManaged, "this account is declared in the configuration file", nil)
	errManagementDisabled = errStatus(http.StatusServiceUnavailable, CodeManagementDisabled,
		"dashboard accounts cannot be written: KRITIK_DASHBOARD_KEY is not set", nil)
	errDashboardSlugTaken = errStatus(http.StatusConflict, CodeSlugTaken, "a dashboard account with this slug already exists", slugPath)
	errRevisionConflict   = errStatus(http.StatusConflict, CodeRevisionConflict,
		"the account was changed by another write; reload it and try again", nil)
)

// configTarget resolves {slug} for a config route: a live account p may
// read, or, for an operator, a stored dashboard account that is not live
// (it does not validate, or has not merged yet). live is nil for the
// latter.
func (s *Server) configTarget(p *auth.Principal, slug string) (live *configfile.Account, err error) {
	t, ok := s.current.Get().Account(slug)
	switch {
	case ok && p.CanRead(t.ID()):
		return t, nil
	case !ok && p.Operator:
		return nil, nil
	default:
		return nil, errNotFound("account")
	}
}

func (s *Server) getAccountConfig(w http.ResponseWriter, r *http.Request) error {
	ctx, p, slug := r.Context(), auth.PrincipalFrom(r.Context()), r.PathValue("slug")
	live, err := s.configTarget(p, slug)
	if err != nil {
		return err
	}
	var out AccountConfig
	if live != nil && live.Origin() == configfile.OriginFile {
		out.ManagedBy, out.Policy, out.Inherited = configfile.OriginFile, fieldPolicies(false), s.inherited(live)
		if out.Spec, err = renderFileAccount(live); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}
	var d configfile.DashboardAccount
	if err := s.store.WithAccount(ctx, accountIDFor(slug), func(tx pgx.Tx) error {
		d, _, err = s.store.DashboardAccount(ctx, tx, slug)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errNotFound("account")
		}
		return err
	}
	out.ManagedBy = configfile.OriginDashboard
	out.Revision = &d.Revision
	out.Editable = s.keyring != nil && p.Operator
	out.Policy = fieldPolicies(out.Editable)
	if live == nil {
		stored, err := configfile.DecodeAccount(d)
		if err != nil {
			return err
		}
		live = &stored
	}
	out.Inherited = s.inherited(live)
	if out.Spec, err = redactSpec(d.Spec); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) error {
	p := auth.PrincipalFrom(r.Context())
	if !p.Operator {
		return errForbidden
	}
	if s.keyring == nil {
		return errManagementDisabled
	}
	var req CreateAccountRequest
	if err := readBody(r, &req); err != nil {
		return err
	}
	if req.Slug == "" || len(req.Spec) == 0 {
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, "slug and spec are required", nil)
	}
	if s.current.Get().Declares(req.Slug) {
		return errStatus(http.StatusConflict, CodeSlugTaken, "the configuration file already declares this slug", slugPath)
	}
	res, err := s.writeAccount(r.Context(), p, req.Slug, req.Spec, 0, req.Adopt)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, res)
	return nil
}

func (s *Server) updateAccount(w http.ResponseWriter, r *http.Request) error {
	p, slug := auth.PrincipalFrom(r.Context()), r.PathValue("slug")
	live, err := s.configTarget(p, slug)
	if err != nil {
		return err
	}
	switch {
	case live != nil && live.Origin() == configfile.OriginFile:
		return errFileManaged
	case live != nil && !p.Operator:
		return errForbidden
	case s.keyring == nil:
		return errManagementDisabled
	}
	var req UpdateAccountRequest
	if err := readBody(r, &req); err != nil {
		return err
	}
	if req.Revision <= 0 || len(req.Spec) == 0 {
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, "revision and spec are required", nil)
	}
	res, err := s.writeAccount(r.Context(), p, slug, req.Spec, req.Revision, false)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

// accountAudit is an account write's audit detail: which secrets were given
// a new value, never the values.
type accountAudit struct {
	Revision int64    `json:"revision"`
	Secrets  []string `json:"secretsChanged,omitempty"`
}

// writeAccount validates spec as the dashboard account slug and stores it,
// with its audit row, in one transaction. expected is the revision it
// replaces, 0 to create it; adopt lets a create re-use a slug an account
// held before.
func (s *Server) writeAccount(
	ctx context.Context, p *auth.Principal, slug string, spec json.RawMessage, expected int64, adopt bool,
) (AccountWriteResult, error) {
	tid := accountIDFor(slug)
	res := AccountWriteResult{Slug: slug}
	err := s.store.WithAccount(ctx, tid, func(tx pgx.Tx) error {
		if err := store.LockDashboardWrites(ctx, tx); err != nil {
			return err
		}
		var prev *configfile.DashboardAccount
		if expected > 0 {
			d, _, err := s.store.DashboardAccount(ctx, tx, slug)
			if errors.Is(err, store.ErrNotFound) {
				return errNotFound("account")
			}
			if err != nil {
				return err
			}
			if d.Revision != expected {
				return errRevisionConflict
			}
			prev = &d
		} else {
			_, _, err := s.store.DashboardAccount(ctx, tx, slug)
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
		dash, err := store.DashboardAccountsIn(ctx, tx)
		if err != nil {
			return err
		}
		candidate, sealed, err := s.checkSpec(slug, spec, prev, dash)
		if err != nil {
			return err
		}
		if err := s.checkTakeover(ctx, tx, tid, candidate); err != nil {
			return err
		}
		action := AuditAccountUpdate
		if expected == 0 {
			action = AuditAccountCreate
		}
		rev, err := s.store.PutDashboardAccount(ctx, tx, slug, sealed.spec, expected, p.User.ID)
		switch {
		case errors.Is(err, store.ErrDashboardConflict) && expected == 0:
			return errDashboardSlugTaken
		case errors.Is(err, store.ErrDashboardConflict):
			return errRevisionConflict
		case err != nil:
			return err
		}
		res.Revision, res.Generated = rev, sealed.generated
		return record(ctx, tx, p, &tid, action, slug, accountAudit{Revision: rev, Secrets: sealed.changed})
	})
	return res, err
}

// claimSlug refuses a create whose slug an account held before, enabled or
// not: account ids derive from slugs, so the new account would see the old
// one's reviews, findings and transcripts. adopt accepts that; only a
// refusal that adopt would overcome says so (slugTakenDetails.Adoptable), never one for an account
// the file still manages.
func (s *Server) claimSlug(ctx context.Context, tx pgx.Tx, p *auth.Principal, tid, slug string, adopt bool) error {
	held, err := store.AccountRowExists(ctx, tx, tid)
	switch {
	case err != nil:
		return err
	case !held:
		return nil
	}
	// An account the file still manages is not gone: it cannot be adopted.
	live, err := store.LiveNonDashboard(ctx, tx, tid, nil)
	switch {
	case err != nil:
		return err
	case len(live) > 0:
		return errStatus(http.StatusConflict, CodeSlugTaken, "slug is still in use by an account the configuration file manages", slugPath)
	case !adopt:
		return errStatus(http.StatusConflict, CodeSlugTaken,
			"an account used this slug before; creating it again with adopt keeps that account's review history",
			slugTakenDetails{Path: slugPath.Path, Adoptable: true})
	}
	return record(ctx, tx, p, &tid, AuditAccountAdopt, slug, struct{}{})
}

// checkSpec seals spec's secrets against the account it replaces (nil on
// create) and checks the result merges with the running file and dash, the
// dashboard accounts as the write's transaction reads them.
func (s *Server) checkSpec(
	slug string, spec json.RawMessage, prev *configfile.DashboardAccount, dash []configfile.DashboardAccount,
) (*configfile.Account, sealedSpec, error) {
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
	candidate := configfile.DashboardAccount{Slug: slug, Spec: sealed.spec, Revision: 1}
	if prev != nil {
		candidate.Revision = prev.Revision + 1
	}
	next, err := configfile.DecodeAccount(candidate)
	if err != nil {
		return nil, sealed, decodeFailure(err)
	}
	current := s.current.Get()
	if err := configfile.ValidateDashboard(current, dash, candidate, s.keyring); err != nil {
		return nil, sealed, mergeFailure(slug, &next, err, func() error { return validateWithout(current, dash, slug, s.keyring) })
	}
	return &next, sealed, nil
}

// checkTakeover refuses a write that would claim a live account or
// connection row another origin manages (one the file dropped that the
// leader has not disabled yet, say) or a connection name another
// account holds in any state, which the leader would refuse to hand over.
// The merge check already refuses anything the file still declares.
func (s *Server) checkTakeover(ctx context.Context, tx pgx.Tx, accountID string, t *configfile.Account) error {
	names := make([]string, len(t.Connections))
	for i := range t.Connections {
		names[i] = t.Connections[i].Name
	}
	taken, err := store.LiveNonDashboard(ctx, tx, accountID, names)
	if err != nil {
		return err
	}
	msg := " is still in use by an account the configuration file manages"
	if len(taken) == 0 {
		if taken, err = store.ConnectionsHeldElsewhere(ctx, tx, accountID, names); err != nil {
			return err
		}
		msg = " belongs to another account"
	}
	if len(taken) == 0 {
		return nil
	}
	path := slugPath.Path
	if taken[0] != slugPath.Path {
		for i := range t.Connections {
			if t.Connections[i].Name == taken[0] {
				path = "connections[" + strconv.Itoa(i) + "].name"
			}
		}
	}
	return errStatus(http.StatusConflict, CodeSlugTaken, path+msg, pathDetails{Path: path})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) error {
	ctx, p, slug := r.Context(), auth.PrincipalFrom(r.Context()), r.PathValue("slug")
	if !p.Operator {
		return errForbidden
	}
	if t, ok := s.current.Get().Account(slug); ok && t.Origin() == configfile.OriginFile {
		return errFileManaged
	}
	if s.keyring == nil {
		return errManagementDisabled
	}
	rev, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || rev <= 0 {
		return errBadRequest(CodeBadRequest, "revision must be the account's current revision")
	}
	tid := accountIDFor(slug)
	err = s.store.WithAccount(ctx, tid, func(tx pgx.Tx) error {
		if err := store.LockDashboardWrites(ctx, tx); err != nil {
			return err
		}
		err := s.store.DeleteDashboardAccount(ctx, tx, slug, rev)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return errNotFound("account")
		case errors.Is(err, store.ErrDashboardConflict):
			return errRevisionConflict
		case err != nil:
			return err
		}
		return record(ctx, tx, p, &tid, AuditAccountDelete, slug, accountAudit{Revision: rev})
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

// inherited is what t's fields and its repository entries' fields resolve
// to where they are left out, in the running configuration.
func (s *Server) inherited(t *configfile.Account) Inherited {
	file, none := s.current.Get(), &configfile.Account{}
	return Inherited{
		Account: repoSettings(file.Settings(none, "", "")), AccountSources: file.Sources(none, "", ""),
		Repository: repoSettings(file.Settings(t, "", "")), RepositorySources: file.Sources(t, "", ""),
	}
}
