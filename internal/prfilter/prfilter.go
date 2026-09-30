// Package prfilter compiles and evaluates a CEL boolean expression that decides
// which pull requests kritik reviews (the `filter` setting in the configuration file).
//
// The expression sees a single variable, pr — a map of the PR's fields. The
// caller supplies that map (ingest builds it from the webhook's pull request,
// the worker from the stored row), so this package stays decoupled from the
// forge model and is testable with plain maps. An
// expression must evaluate to a boolean; the program is type-checked once at
// Compile so a malformed filter fails fast at startup rather than per request.
//
// CEL is the right tool here: it's a safe, bounded, non-Turing-complete
// expression language (no I/O, no unbounded loops), so an admin-supplied
// predicate can't hang or escape — and the home-ops/Kubernetes audience already
// knows it from admission policies.
package prfilter

import (
	"fmt"
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
)

// Program is a compiled, type-checked PR-filter expression.
type Program struct {
	prg cel.Program
	src string
}

// evalCostLimit caps the CEL cost of a single filter evaluation. The expression
// is admin-supplied (trusted) and its attacker-influenced inputs (pr.title,
// pr.labels) are forge-bounded, so this is defense-in-depth against a pathological
// admin's expression rather than a likely attack — a ceiling no reasonable PR
// predicate approaches, while still bounding an accidental blow-up (CEL has no
// loops, so a finite cost is guaranteed to exist).
const evalCostLimit = 1_000_000

// env is the one CEL environment every filter compiles in: building one
// loads the standard library, so it is shared, which cel.Env permits once
// built. pr is a string-keyed map of dynamic values (the caller fills it
// from the PR); field access is therefore statically dyn, see the bool/dyn
// check in Compile.
var env = sync.OnceValues(func() (*cel.Env, error) {
	return cel.NewEnv(cel.Variable("pr", cel.MapType(cel.StringType, cel.DynType)))
})

// Compile parses and type-checks expr and returns a runnable Program. It fails
// when the expression is syntactically invalid, references unknown
// variables/functions, or cannot produce a boolean.
func Compile(expr string) (*Program, error) {
	env, err := env()
	if err != nil {
		return nil, fmt.Errorf("prfilter: build env: %w", err)
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("prfilter: %w", iss.Err())
	}
	// The filter is a yes/no decision. Field access on the dyn-valued pr map is
	// statically dyn (only known boolean at runtime), so accept bool or dyn here;
	// a literal of the wrong type (e.g. 42, "x") is statically typed and rejected.
	// Eval enforces an actual boolean result.
	switch ast.OutputType().Kind() {
	case types.BoolKind, types.DynKind:
	default:
		return nil, fmt.Errorf("prfilter: expression must evaluate to a boolean, got %s", ast.OutputType())
	}
	prg, err := env.Program(ast, cel.CostLimit(evalCostLimit))
	if err != nil {
		return nil, fmt.Errorf("prfilter: program: %w", err)
	}
	return &Program{prg: prg, src: expr}, nil
}

// Eval runs the expression against the given pr field map and reports whether
// the PR is allowed. A runtime error or a non-boolean result is returned as an
// error (the caller decides the fail-safe; kritik skips the PR and logs).
func (p *Program) Eval(pr map[string]any) (bool, error) {
	out, _, err := p.prg.Eval(map[string]any{"pr": pr})
	if err != nil {
		return false, fmt.Errorf("prfilter: eval %q: %w", p.src, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("prfilter: %q produced %T, want bool", p.src, out.Value())
	}
	return b, nil
}

// Source returns the original expression text (for logs and diagnostics).
func (p *Program) Source() string { return p.src }
