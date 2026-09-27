package tasks

import (
	"fmt"
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
)

// evalCostLimit caps one guard's evaluation. The raw payload a guard sees
// is attacker-influenced and up to the webhook body cap, so the cost limit
// is what bounds a guard over it.
const evalCostLimit = 1_000_000

// envs are the CEL environments guards compile in: a task's own if sees
// the event; an action's also sees the answer.
var envs = sync.OnceValues(func() ([2]*cel.Env, error) {
	base := []cel.EnvOption{
		cel.Variable("event", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("subject", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("raw", cel.DynType),
	}
	trigger, err := cel.NewEnv(base...)
	if err != nil {
		return [2]*cel.Env{}, err
	}
	action, err := cel.NewEnv(append(base, cel.Variable("answer", cel.MapType(cel.StringType, cel.DynType)))...)
	return [2]*cel.Env{trigger, action}, err
})

// guard is a compiled if.
type guard struct {
	prg cel.Program
	src string
}

// compileGuard compiles expr in the trigger environment, or with answer in
// the action one. An empty expression is nil, always true.
func compileGuard(expr string, withAnswer bool) (*guard, error) {
	if expr == "" {
		return nil, nil
	}
	e, err := envs()
	if err != nil {
		return nil, fmt.Errorf("build CEL environment: %w", err)
	}
	env := e[0]
	if withAnswer {
		env = e[1]
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("if %q: %w", expr, iss.Err())
	}
	switch ast.OutputType().Kind() {
	case types.BoolKind, types.DynKind:
	default:
		return nil, fmt.Errorf("if %q must evaluate to a boolean, got %s", expr, ast.OutputType())
	}
	prg, err := env.Program(ast, cel.CostLimit(evalCostLimit))
	if err != nil {
		return nil, fmt.Errorf("if %q: %w", expr, err)
	}
	return &guard{prg: prg, src: expr}, nil
}

// eval reports whether the guard holds for vars; a nil guard always does.
func (g *guard) eval(vars map[string]any) (bool, error) {
	if g == nil {
		return true, nil
	}
	out, _, err := g.prg.Eval(vars)
	if err != nil {
		return false, fmt.Errorf("tasks: eval %q: %w", g.src, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("tasks: %q produced %T, want bool", g.src, out.Value())
	}
	return b, nil
}

// vars are the variables a guard sees of in, and of a when it is not nil.
func vars(in Input, a *Answer) map[string]any {
	v := map[string]any{
		"event": map[string]any{
			"forge": in.Forge, "name": in.Event, "rawEvent": in.RawEvent, "action": in.Action, "sender": in.Sender,
			"repo": map[string]any{"owner": in.Repo.Owner, "name": in.Repo.Name, "defaultBranch": in.Repo.DefaultBranch},
		},
		"subject": subjectVars(in.Subject),
		"raw":     rawOrEmpty(in.Raw),
	}
	if a != nil {
		v["answer"] = a.vars()
	}
	return v
}

// subjectVars is the subject variable; an absent subject has every key,
// with kind "", so a guard like subject.kind == "issue" is false rather
// than an error.
func subjectVars(s *Subject) map[string]any {
	if s == nil {
		s = &Subject{}
	}
	return map[string]any{
		"kind": s.Kind, "number": s.Number, "title": s.Title, keyBody: s.Body, keyState: s.State, "author": s.Author,
		"url": s.URL, keyLabels: orEmpty(s.Labels), keyAssignees: orEmpty(s.Assignees), "draft": s.Draft,
	}
}

func rawOrEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
