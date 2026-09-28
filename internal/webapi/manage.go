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

// maxBodyBytes bounds a request body; the instance spec is far smaller.
const maxBodyBytes = 1 << 20

// registerManage mounts the instance spec and account settings routes.
func (s *Server) registerManage(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/config", s.handler(s.getInstanceConfig))
	mux.HandleFunc("PUT /api/v1/config", s.handler(s.updateInstanceConfig))
	mux.HandleFunc("GET /api/v1/accounts/{forge}/{name}/config", s.account(s.getAccountConfig))
	mux.HandleFunc("PUT /api/v1/accounts/{forge}/{name}/config", s.account(s.updateAccountConfig))
}

func (s *Server) getMeta(w http.ResponseWriter, _ *http.Request) error {
	m := Meta{Version: s.version, Management: s.keyring != nil, SignIn: s.auth.Providers()}
	if s.webURL != nil {
		m.WebURL = s.webURL.String()
	}
	writeJSON(w, http.StatusOK, m)
	return nil
}

var (
	errForbidden          = errStatus(http.StatusForbidden, CodeForbidden, "this needs an admin", nil)
	errManagementDisabled = errStatus(http.StatusServiceUnavailable, CodeManagementDisabled,
		"the configuration cannot be written: KRITIK_DASHBOARD_KEY is not set", nil)
	errRevisionConflict = errStatus(http.StatusConflict, CodeRevisionConflict,
		"the configuration was changed by another write; reload it and try again", nil)
)

func (s *Server) getInstanceConfig(w http.ResponseWriter, r *http.Request) error {
	p := auth.PrincipalFrom(r.Context())
	if !p.Operator {
		return errForbidden
	}
	stored, err := s.store.InstanceSpec(r.Context())
	if err != nil {
		return err
	}
	out := InstanceConfig{Revision: stored.Revision, Editable: s.keyring != nil}
	if out.Spec, err = redactSpec(specOrEmpty(stored.Spec)); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) updateInstanceConfig(w http.ResponseWriter, r *http.Request) error {
	p := auth.PrincipalFrom(r.Context())
	switch {
	case !p.Operator:
		return errForbidden
	case s.keyring == nil:
		return errManagementDisabled
	}
	var req UpdateConfigRequest
	if err := readBody(r, &req); err != nil {
		return err
	}
	if req.Revision < 0 || len(req.Spec) == 0 {
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, "revision and spec are required", nil)
	}
	res, err := s.writeSpec(r.Context(), p, req.Revision, "", AuditConfigUpdate, "instance",
		func(json.RawMessage) (json.RawMessage, error) { return req.Spec, nil }, specFailure)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func (s *Server) getAccountConfig(w http.ResponseWriter, r *http.Request, t *accountScope) error {
	stored, err := s.store.InstanceSpec(r.Context())
	if err != nil {
		return err
	}
	entry, _, err := accountEntry(specOrEmpty(stored.Spec), t.account)
	if err != nil {
		return err
	}
	out := AccountConfig{Revision: stored.Revision, Editable: s.keyring != nil && t.principal.Operator, Inherited: s.inherited(t.account)}
	out.Policy = fieldPolicies(out.Editable)
	if out.Spec, err = redactSpec(entry); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) updateAccountConfig(w http.ResponseWriter, r *http.Request, t *accountScope) error {
	switch {
	case !t.principal.Operator:
		return errForbidden
	case s.keyring == nil:
		return errManagementDisabled
	}
	var req UpdateConfigRequest
	if err := readBody(r, &req); err != nil {
		return err
	}
	if req.Revision < 0 || len(req.Spec) == 0 {
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, "revision and spec are required", nil)
	}
	var index int
	build := func(stored json.RawMessage) (json.RawMessage, error) {
		var next json.RawMessage
		var err error
		next, index, err = withAccountEntry(stored, t.account, req.Spec)
		return next, err
	}
	fail := func(err error, baseline func() error) error { return accountSpecFailure(err, index, baseline) }
	res, err := s.writeSpec(r.Context(), t.principal, req.Revision, t.account.ID(), AuditAccountUpdate, t.account.Slug(), build, fail)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

// configAudit is a spec write's audit detail: which secrets were given a
// new value, never the values.
type configAudit struct {
	Revision int64    `json:"revision"`
	Secrets  []string `json:"secretsChanged,omitempty"`
}

// writeSpec replaces the instance spec, while it is still at expected, with
// what build makes of the stored one, sealed and validated, and records the
// write in the audit log under accountID ("" for none) in the same
// transaction. fail turns a spec that does not merge into the API's
// answer, given a check of the stored spec alone.
func (s *Server) writeSpec(
	ctx context.Context, p *auth.Principal, expected int64, accountID string, action AuditAction, target string,
	build func(stored json.RawMessage) (json.RawMessage, error), fail func(err error, baseline func() error) error,
) (ConfigWriteResult, error) {
	var res ConfigWriteResult
	write := func(tx pgx.Tx) error {
		if err := store.LockInstanceSpec(ctx, tx); err != nil {
			return err
		}
		stored, _, err := store.InstanceSpecIn(ctx, tx)
		if err != nil {
			return err
		}
		if stored.Revision != expected {
			return errRevisionConflict
		}
		candidate, err := build(specOrEmpty(stored.Spec))
		if err != nil {
			return refusal(err)
		}
		sealed, err := sealSpec(candidate, stored.Spec, s.keyring.Seal, generateWebhookSecret)
		if err != nil {
			return refusal(err)
		}
		next := configfile.InstanceSpec{Spec: sealed.spec, Revision: stored.Revision + 1}
		if _, err := configfile.DecodeSpec(next.Spec); err != nil {
			return decodeFailure(err)
		}
		if err := configfile.ValidateSpec(s.current.Get(), stored, next, s.keyring); err != nil {
			return fail(err, func() error { return configfile.ValidateSpec(s.current.Get(), stored, stored, s.keyring) })
		}
		rev, err := s.store.PutInstanceSpec(ctx, tx, sealed.spec, expected, p.User.ID)
		if errors.Is(err, store.ErrSpecConflict) {
			return errRevisionConflict
		}
		if err != nil {
			return err
		}
		res.Revision, res.Generated = rev, sealed.generated
		var id *string
		if accountID != "" {
			id = &accountID
		}
		return record(ctx, tx, p, id, action, target, configAudit{Revision: rev, Secrets: sealed.changed})
	}
	return res, s.store.WithAccount(ctx, accountID, write)
}

// refusal is err as the API answers it: a *specError is the client's
// spec refused, a 422 at its path.
func refusal(err error) error {
	if se, ok := errors.AsType[*specError](err); ok {
		return errStatus(http.StatusUnprocessableEntity, se.errorCode(), se.Error(), pathDetails{Path: se.path})
	}
	return err
}

// specOrEmpty is spec, or an empty object for a spec never written.
func specOrEmpty(spec json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(spec)) == 0 {
		return json.RawMessage(`{}`)
	}
	return spec
}

// accountEntry is a's entry in spec, or a new one naming it when the spec
// lists none, and its index in accounts, -1 for none.
func accountEntry(spec json.RawMessage, a *configfile.Account) (json.RawMessage, int, error) {
	root, err := decodeObject(spec)
	if err != nil {
		return nil, -1, err
	}
	for i, e := range objects(root["accounts"]) {
		if entryKey(e) == a.Key() {
			out, err := json.Marshal(e)
			return out, i, err
		}
	}
	out, err := json.Marshal(map[string]any{"forge": string(a.Forge), "name": a.Name})
	return out, -1, err
}

// withAccountEntry is stored with a's entry replaced by entry, or entry
// appended when there was none, every other secret kept as stored; and the
// entry's index in accounts. entry must name a.
func withAccountEntry(stored json.RawMessage, a *configfile.Account, entry json.RawMessage) (json.RawMessage, int, error) {
	root, err := decodeObject(keepSecrets(stored))
	if err != nil {
		return nil, -1, err
	}
	e, err := decodeObject(entry)
	if err != nil {
		return nil, -1, &specError{msg: "spec must be a JSON object"}
	}
	if entryKey(e) != a.Key() {
		return nil, -1, &specError{path: "name", msg: "the spec must name the account " + a.Slug()}
	}
	list, _ := root["accounts"].([]any)
	index := -1
	for i, x := range objects(root["accounts"]) {
		if entryKey(x) == a.Key() {
			list[i], index = e, i
		}
	}
	if index < 0 {
		list, index = append(list, e), len(list)
	}
	root["accounts"] = list
	out, err := json.Marshal(root)
	return out, index, err
}

// entryKey is an account entry's key, as configfile.AccountKey spells it.
func entryKey(e map[string]any) string {
	forge, _ := e["forge"].(string)
	name, _ := e["name"].(string)
	return configfile.AccountKey(configfile.Forge(forge), name)
}

// specFailure turns a failed configfile.ValidateSpec of the instance spec
// into the API's answer: the offending key's path and the message.
func specFailure(err error, _ func() error) error {
	me, ok := errors.AsType[*configfile.MergeError](err)
	if !ok {
		return err
	}
	path, msg := splitPath(trimConfigfile(me.Err.Error()))
	return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, msg, pathDetails{Path: path})
}

// accountSpecFailure is specFailure for an account's entry at index: a path
// inside the entry is given relative to it. A failure elsewhere in the spec
// is the entry's fault when the stored spec validates without it
// (baseline); when it does not, no write can be judged until an admin
// fixes the spec, which is a 409.
func accountSpecFailure(err error, index int, baseline func() error) error {
	me, ok := errors.AsType[*configfile.MergeError](err)
	if !ok {
		return err
	}
	path, msg := splitPath(trimConfigfile(me.Err.Error()))
	prefix := "accounts[" + strconv.Itoa(index) + "]"
	if rel, ok := strings.CutPrefix(path, prefix+"."); ok {
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, strings.Replace(msg, path, rel, 1), pathDetails{Path: rel})
	}
	if path == prefix || baseline() == nil {
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, msg, pathDetails{})
	}
	return errStatus(http.StatusConflict, CodeConfigBlocked,
		"the configuration is invalid elsewhere, so this change cannot be checked; an admin must fix it first", nil)
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

// inherited is what a's fields and its repository entries' fields resolve
// to where they are left out, in the running configuration.
func (s *Server) inherited(a *configfile.Account) Inherited {
	file, none := s.current.Get(), &configfile.Account{}
	return Inherited{
		Account: repoSettings(file.Settings(none, "")), AccountSources: file.Sources(none, ""),
		Repository: repoSettings(file.Settings(a, "")), RepositorySources: file.Sources(a, ""),
	}
}
