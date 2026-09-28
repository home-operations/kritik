// Package rolemap compiles and evaluates a sign-in's role mapping: a CEL
// expression over what the identity provider says about a person, whose
// value decides what that person may do in the dashboard (ADR-0014 §2.5).
//
// The expression yields either an instance-wide role, "admin", "member" or
// "" for none, or a map from forge account ("github/<name>", or "*" for
// every account) to "member", which scopes a sign-in to those accounts.
// CEL unifies the branches of a conditional, so an expression that yields a
// string on one branch and a map on the other wraps one in dyn().
package rolemap

import (
	"fmt"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// Role is an instance-wide role a mapping may yield.
type Role string

// Roles. RoleNone is a mapping that does not place the person.
const (
	RoleNone   Role = ""
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// AllAccounts is the map key that stands for every account.
const AllAccounts = "*"

// Kind selects the variables an expression sees.
type Kind int

// Kinds of sign-in a mapping belongs to.
const (
	// OIDC sees claims, the ID token's claims with the UserInfo response
	// merged over them, and roles, the roles claim as a list of strings.
	OIDC Kind = iota
	// GitHub sees login, email, orgs (the logins of the organisations the
	// person is an active member of) and teams ("org/slug").
	GitHub
)

// evalCostLimit bounds one evaluation. The expression is the admin's and its
// inputs come from the identity provider, so this is a guard against an
// accident rather than an attack; no real mapping comes near it.
const evalCostLimit = 1_000_000

// Program is a compiled, type-checked role mapping.
type Program struct {
	prg  cel.Program
	kind Kind
	src  string
}

// Result is what a mapping decided. Accounts holds lowercased account keys,
// AllAccounts among them when the map named every account.
type Result struct {
	Role     Role
	Accounts map[string]bool
}

// Empty reports whether the mapping placed the person nowhere.
func (r Result) Empty() bool { return r.Role == RoleNone && len(r.Accounts) == 0 }

// Compile parses and type-checks expr for kind. It fails when the
// expression is not valid CEL, names a variable kind does not have, or
// cannot yield a string or a map.
func Compile(kind Kind, expr string) (*Program, error) {
	var vars []cel.EnvOption
	switch kind {
	case OIDC:
		vars = []cel.EnvOption{
			cel.Variable("claims", cel.MapType(cel.StringType, cel.DynType)),
			cel.Variable("roles", cel.ListType(cel.StringType)),
		}
	case GitHub:
		vars = []cel.EnvOption{
			cel.Variable("login", cel.StringType),
			cel.Variable("email", cel.StringType),
			cel.Variable("orgs", cel.ListType(cel.StringType)),
			cel.Variable("teams", cel.ListType(cel.StringType)),
		}
	default:
		return nil, fmt.Errorf("rolemap: unknown kind %d", kind)
	}
	env, err := cel.NewEnv(vars...)
	if err != nil {
		return nil, fmt.Errorf("rolemap: build env: %w", err)
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("rolemap: %w", iss.Err())
	}
	switch ast.OutputType().Kind() {
	case types.StringKind, types.MapKind, types.DynKind:
	default:
		return nil, fmt.Errorf("rolemap: expression must yield a role or a map of accounts to roles, got %s", ast.OutputType())
	}
	prg, err := env.Program(ast, cel.CostLimit(evalCostLimit))
	if err != nil {
		return nil, fmt.Errorf("rolemap: program: %w", err)
	}
	return &Program{prg: prg, kind: kind, src: expr}, nil
}

// Kind is the kind of sign-in the program was compiled for.
func (p *Program) Kind() Kind { return p.kind }

// Eval runs the mapping over vars, keyed by the names Compile declared for
// the program's kind. A runtime error, or a value that is not a role or a
// map of accounts to "member", is an error: the caller refuses the sign-in.
func (p *Program) Eval(vars map[string]any) (Result, error) {
	out, _, err := p.prg.Eval(vars)
	if err != nil {
		return Result{}, fmt.Errorf("rolemap: eval: %w", err)
	}
	return result(out)
}

func result(v ref.Val) (Result, error) {
	if s, ok := v.Value().(string); ok {
		switch r := Role(s); r {
		case RoleNone, RoleAdmin, RoleMember:
			return Result{Role: r}, nil
		default:
			return Result{}, fmt.Errorf("rolemap: %q is not a role: want %q, %q or %q", s, RoleAdmin, RoleMember, "")
		}
	}
	m, ok := v.(traits.Mapper)
	if !ok {
		return Result{}, fmt.Errorf("rolemap: the mapping yielded %s, want a role or a map", v.Type())
	}
	out := Result{Accounts: map[string]bool{}}
	for it := m.Iterator(); it.HasNext() == types.True; {
		k := it.Next()
		account, ok := k.Value().(string)
		if !ok {
			return Result{}, fmt.Errorf("rolemap: map key %v is not an account", k.Value())
		}
		role, ok := m.Get(k).Value().(string)
		if !ok {
			return Result{}, fmt.Errorf("rolemap: the role for %q is not a string", account)
		}
		switch Role(role) {
		case RoleMember:
			out.Accounts[strings.ToLower(account)] = true
		case RoleNone:
		default:
			return Result{}, fmt.Errorf("rolemap: account %q maps to %q; an account only takes %q", account, role, RoleMember)
		}
	}
	return out, nil
}
