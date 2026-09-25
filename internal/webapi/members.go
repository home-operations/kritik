package webapi

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/store"
)

// Invite lifetimes, in hours.
const (
	defaultInviteTTLHours = 7 * 24
	maxInviteTTLHours     = 30 * 24
)

// removedNote is what removing a member says: only invite-granted access
// can be taken away here.
const removedNote = "Invite-granted access was removed. Access a forge grants is derived again at each sign-in, " +
	"so it lasts as long as the forge grants it."

// registerMembers mounts tenant membership and invites.
func (s *Server) registerMembers(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/tenants/{slug}/members", s.tenant(s.listMembers))
	mux.HandleFunc("POST /api/v1/tenants/{slug}/invites", s.admin(s.createInvite))
	mux.HandleFunc("DELETE /api/v1/tenants/{slug}/invites/{id}", s.admin(s.deleteInvite))
	mux.HandleFunc("PATCH /api/v1/tenants/{slug}/members/{accountId}", s.admin(s.updateMember))
	mux.HandleFunc("DELETE /api/v1/tenants/{slug}/members/{accountId}", s.admin(s.removeMember))
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	ctx := r.Context()
	out := Members{Members: []Member{}}
	err := s.read(ctx, t, func(tx pgx.Tx) error {
		members, err := store.TenantMembers(ctx, tx, t.tenant.ID())
		if err != nil {
			return err
		}
		for _, m := range members {
			out.Members = append(out.Members, member(m))
		}
		if !t.principal.CanAdmin(t.tenant.ID()) {
			return nil
		}
		invites, err := store.PendingInvites(ctx, tx, t.tenant.ID(), s.now())
		if err != nil {
			return err
		}
		out.Invites = make([]Invite, len(invites))
		for i, v := range invites {
			out.Invites[i] = invite(v)
		}
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func member(m store.Member) Member {
	out := Member{Account: account(m.Account), Role: m.Role(), Sources: make([]MemberSource, len(m.Grants))}
	for i, g := range m.Grants {
		out.Sources[i] = MemberSource{Source: g.Source, Role: g.Role}
	}
	return out
}

func account(a store.Account) Account {
	return Account{ID: a.ID, DisplayName: a.DisplayName, Email: a.Email, AvatarURL: a.AvatarURL}
}

func invite(v store.Invite) Invite {
	out := Invite{ID: v.ID, Email: v.Email, Role: v.Role, ExpiresAt: v.ExpiresAt}
	if v.CreatedBy != nil {
		a := account(*v.CreatedBy)
		out.CreatedBy = &a
	}
	return out
}

// inviteAudit is an invite write's audit detail.
type inviteAudit struct {
	Email     string     `json:"email"`
	Role      auth.Role  `json:"role,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	var req CreateInviteRequest
	if err := readBody(r, &req); err != nil {
		return err
	}
	addr, err := mail.ParseAddress(req.Email)
	if err != nil || addr.Address != strings.TrimSpace(req.Email) {
		return errBadRequest(CodeBadRequest, "email must be a plain email address")
	}
	if !req.Role.Valid() {
		return errBadRequest(CodeBadRequest, "role must be admin or member")
	}
	ttl := defaultInviteTTLHours
	if req.TTLHours != nil {
		ttl = *req.TTLHours
	}
	if ttl < 1 || ttl > maxInviteTTLHours {
		return errBadRequest(CodeBadRequest, "ttlHours must be between 1 and 720")
	}
	ctx, p, now := r.Context(), t.principal, s.now()
	v := store.Invite{
		ID: uuid.NewString(), TenantID: t.tenant.ID(), Email: addr.Address, Role: req.Role,
		ExpiresAt: now.Add(time.Duration(ttl) * time.Hour),
	}
	err = s.read(ctx, t, func(tx pgx.Tx) error {
		if err := store.CreateInvite(ctx, tx, v, p.Account.ID, now); err != nil {
			if errors.Is(err, store.ErrInviteExists) {
				return errStatus(http.StatusConflict, CodeInviteExists, "a pending invite for this email already exists", nil)
			}
			return err
		}
		tid := t.tenant.ID()
		return record(ctx, tx, p, &tid, AuditInviteCreate, v.ID, inviteAudit{Email: v.Email, Role: v.Role, ExpiresAt: &v.ExpiresAt})
	})
	if err != nil {
		return err
	}
	a := p.Account
	v.CreatedBy = &a
	writeJSON(w, http.StatusCreated, invite(v))
	return nil
}

func (s *Server) deleteInvite(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	ctx, id := r.Context(), r.PathValue("id")
	if uuid.Validate(id) != nil {
		return errNotFound("invite")
	}
	err := s.read(ctx, t, func(tx pgx.Tx) error {
		v, err := store.DeleteInvite(ctx, tx, t.tenant.ID(), id)
		if errors.Is(err, store.ErrNotFound) {
			return errNotFound("invite")
		}
		if err != nil {
			return err
		}
		tid := t.tenant.ID()
		return record(ctx, tx, t.principal, &tid, AuditInviteDelete, id, inviteAudit{Email: v.Email, Role: v.Role})
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// memberAudit is a membership write's audit detail.
type memberAudit struct {
	Role     auth.Role `json:"role,omitempty"`
	Previous auth.Role `json:"previous"`
}

func (s *Server) updateMember(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	var req UpdateMemberRequest
	if err := readBody(r, &req); err != nil {
		return err
	}
	if !req.Role.Valid() {
		return errBadRequest(CodeBadRequest, "role must be admin or member")
	}
	return s.changeInviteGrant(w, r, t, req.Role)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, t *tenantScope) error {
	return s.changeInviteGrant(w, r, t, "")
}

// changeInviteGrant sets the {accountId}'s invite-granted role to role, or
// removes it when role is "". Forge-granted access is never touched: the
// next sign-in would only derive it again.
func (s *Server) changeInviteGrant(w http.ResponseWriter, r *http.Request, t *tenantScope, role auth.Role) error {
	ctx, p, target := r.Context(), t.principal, r.PathValue("accountId")
	if uuid.Validate(target) != nil {
		return errNotFound("member")
	}
	tid := t.tenant.ID()
	err := s.read(ctx, t, func(tx pgx.Tx) error {
		grants, err := store.MemberGrants(ctx, tx, tid, target)
		if err != nil {
			return err
		}
		if len(grants) == 0 {
			return errNotFound("member")
		}
		var previous auth.Role
		for _, g := range grants {
			if g.Source == store.SourceInvite {
				previous = g.Role
			}
		}
		if previous == "" {
			return errStatus(http.StatusConflict, CodeNotInviteMember,
				"this member's access comes from the forge, which is checked again at each sign-in; change it there", nil)
		}
		if target == p.Account.ID && !p.Operator {
			others, err := store.AdminIdentities(ctx, tx, tid, target)
			if err != nil {
				return err
			}
			if leavesNoAdmin(s.current.Get().Web, grants, role, others) {
				return errStatus(http.StatusConflict, CodeLastAdmin, "the tenant would be left with no admin", nil)
			}
		}
		if role == "" {
			err = store.DeleteInviteMembership(ctx, tx, tid, target)
		} else {
			err = store.SetInviteRole(ctx, tx, tid, target, role, s.now())
		}
		if err != nil {
			return err
		}
		action := AuditMemberUpdate
		if role == "" {
			action = AuditMemberRemove
		}
		return record(ctx, tx, p, &tid, action, target, memberAudit{Role: role, Previous: previous})
	})
	if err != nil {
		return err
	}
	if role == "" {
		writeJSON(w, http.StatusOK, MemberRemoved{Note: removedNote})
		return nil
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
