package review

import (
	"context"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"

	"github.com/nikolalohinski/gonja/v2/builtins"
	controlstructures "github.com/nikolalohinski/gonja/v2/builtins/control_structures"
	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
	"github.com/nikolalohinski/gonja/v2/nodes"
	"github.com/nikolalohinski/gonja/v2/parser"
	"github.com/nikolalohinski/gonja/v2/tokens"
)

// Sandbox bounds for templates. A repository template is trusted to be
// written by the repository's maintainers, not to be well behaved: these
// keep a mistake in one repository's template from stalling or exhausting
// a worker that serves every tenant. gonja renders several constructs into
// private, uncapped buffers, so the bounds are enforced on what a template
// may express rather than only on what it writes.
const (
	// maxRenderDepth bounds nested control structures.
	maxRenderDepth = 64
	// maxIterations is the render-wide budget of loop iterations, charged
	// in full when a loop starts, so nested loops cannot multiply it.
	maxIterations = 20_000
	// maxRangeItems bounds one range() call.
	maxRangeItems = 10_000
	// maxLiteralItems bounds a list, tuple or dict literal.
	maxLiteralItems = 256
	// maxCallWork bounds one filter or method call, estimated as the size
	// of its input times the size of its arguments, so amplifying calls
	// such as replace or center fail before they allocate.
	maxCallWork = 16 << 20
)

// Names the sandbox adds to every template. Templates may not use
// identifiers with this prefix, so they cannot rebind them.
const (
	reservedPrefix = "__kritik_"
	addendName     = reservedPrefix + "addend"
	iterName       = reservedPrefix + "iter"
	stepName       = reservedPrefix + "step"
)

// templateName is the only name the loader knows: the template itself.
const templateName = "template"

func execute(ctx context.Context, src string, data map[string]any, limit int) (string, error) {
	if len(src) > MaxRenderBytes {
		return "", fmt.Errorf("%w: template source over %d bytes", errSandbox, MaxRenderBytes)
	}
	cfg := config.New()
	cfg.TrimBlocks, cfg.LeftStripBlocks, cfg.KeepTrailingNewline = true, true, true
	if err := checkTokens(src, cfg); err != nil {
		return "", err
	}
	g := &guard{ctx: ctx}
	env := &exec.Environment{
		Context:           exec.NewContext(g.globals()),
		Filters:           g.filters(),
		Tests:             builtins.Tests,
		ControlStructures: g.controlStructures(),
		Methods:           g.methods(),
	}
	tpl, err := exec.NewTemplate(templateName, cfg, sourceLoader(src), env)
	if err != nil {
		return "", fmt.Errorf("review: parse template: %w", err)
	}
	if err := harden(reflect.ValueOf(tpl.Root()), map[uintptr]bool{}); err != nil {
		return "", err
	}
	w := &cappedWriter{ctx: ctx, limit: limit}
	if err := tpl.Execute(w, exec.NewContext(data)); err != nil {
		if w.over {
			return "", errTooLarge
		}
		return "", fmt.Errorf("review: execute template: %w", err)
	}
	return w.b.String(), nil
}

// checkTokens rejects operators that build a large value in one step,
// before any guard can see it: * and ** (string repetition,
// exponentiation) and ~ (concatenation), and identifiers in the sandbox's
// reserved namespace.
func checkTokens(src string, cfg *config.Config) error {
	for s := tokens.LexAll(src, cfg); !s.End(); s.Next() {
		t := s.Current()
		switch {
		case t.Type == tokens.Multiply || t.Type == tokens.Power || t.Type == tokens.Tilde:
			return fmt.Errorf("%w: the %s operator is not available (line %d)", errSandbox, t.Val, t.Line)
		case t.Type == tokens.Name && strings.HasPrefix(t.Val, reservedPrefix):
			return fmt.Errorf("%w: names starting with %s are reserved (line %d)", errSandbox, reservedPrefix, t.Line)
		}
	}
	return nil
}

// harden walks the parsed template through exported fields only, bounds
// literals, and rewrites the tree so the guards see every addition and
// every loop: the operands of + pass through addendName, a loop's iterable
// through iterName, and each iteration starts with stepName. A node that
// keeps expressions in unexported fields cannot be rewritten, so any
// unexported field that is not a plain token or value fails the template.
func harden(v reflect.Value, seen map[uintptr]bool) error {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return harden(v.Elem(), seen)
	case reflect.Pointer:
		if v.IsNil() || seen[v.Pointer()] {
			return nil
		}
		seen[v.Pointer()] = true
		switch v.Interface().(type) {
		case *tokens.Token, *guard:
			return nil
		}
		if err := checkLiteral(v.Interface()); err != nil {
			return err
		}
		if err := harden(v.Elem(), seen); err != nil {
			return err
		}
		rewrite(v.Interface())
		return nil
	case reflect.Struct:
		return hardenStruct(v, seen)
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if err := harden(v.Index(i), seen); err != nil {
				return err
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if err := harden(it.Value(), seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func hardenStruct(v reflect.Value, seen map[uintptr]bool) error {
	t := v.Type()
	for i := range t.NumField() {
		f, fv := t.Field(i), v.Field(i)
		if !f.IsExported() {
			switch fv.Kind() {
			case reflect.Bool, reflect.Int, reflect.String:
				continue
			}
			if fv.IsZero() || f.Type == reflect.TypeFor[*tokens.Token]() || f.Type == reflect.TypeFor[*nodes.Data]() ||
				f.Type == reflect.TypeFor[*guard]() {
				continue
			}
			return fmt.Errorf("%w: %s is not available", errSandbox, t.Name())
		}
		if err := harden(fv, seen); err != nil {
			return err
		}
	}
	return nil
}

func checkLiteral(n any) error {
	items := 0
	switch n := n.(type) {
	case *nodes.List:
		items = len(n.Val)
	case *nodes.Tuple:
		items = len(n.Val)
	case *nodes.Dict:
		items = len(n.Pairs)
	}
	if items > maxLiteralItems {
		return fmt.Errorf("%w: literal over %d items", errSandbox, maxLiteralItems)
	}
	return nil
}

// rewrite routes the operands of + and every loop through the guards.
func rewrite(n any) {
	switch n := n.(type) {
	case *nodes.BinaryExpression:
		if n.Operator != nil && n.Operator.Token.Type == tokens.Addition {
			n.Left, n.Right = guardCall(addendName, n.Left), guardCall(addendName, n.Right)
		}
	case *controlstructures.ForControlStructure:
		n.ObjectEvaluator = guardCall(iterName, n.ObjectEvaluator)
		step := &nodes.Output{Start: n.Position(), Expression: guardCall(stepName, nil), End: n.Position()}
		n.BodyWrapper.Nodes = append([]nodes.Node{step}, n.BodyWrapper.Nodes...)
	}
}

// guardCall wraps arg, if any, in a call to the named sandbox function.
func guardCall(name string, arg nodes.Expression) *nodes.Call {
	pos := &tokens.Token{Type: tokens.Name, Val: name}
	if arg != nil && arg.Position() != nil {
		p := *arg.Position()
		pos = &tokens.Token{Type: tokens.Name, Val: name, Line: p.Line, Col: p.Col, Pos: p.Pos}
	}
	c := &nodes.Call{Location: pos, Func: &nodes.Name{Name: pos}, Kwargs: map[string]nodes.Expression{}}
	if arg != nil {
		c.Args = []nodes.Expression{arg}
	}
	return c
}

// sourceLoader serves the template's own source under templateName and
// nothing else, so include, import and extends cannot read a file even if
// they were available.
type sourceLoader string

func (l sourceLoader) Read(path string) (io.Reader, error) {
	if path != templateName {
		return nil, fmt.Errorf("review: templates cannot load %q", path)
	}
	return strings.NewReader(string(l)), nil
}

func (l sourceLoader) Resolve(path string) (string, error) {
	if path != templateName {
		return "", fmt.Errorf("review: templates cannot load %q", path)
	}
	return path, nil
}

func (sourceLoader) Inherit(from string) (loaders.Loader, error) {
	return nil, fmt.Errorf("review: templates cannot load %q", from)
}

type cappedWriter struct {
	ctx   context.Context
	b     strings.Builder
	limit int
	over  bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.b.Len()+len(p) > w.limit {
		w.over = true
		return 0, errTooLarge
	}
	return w.b.Write(p)
}

// guard is one render's sandbox state. A render runs on one goroutine, so
// its counters need no synchronisation.
type guard struct {
	ctx        context.Context
	depth      int
	iterations int
}

func (g *guard) enter() error {
	if err := g.ctx.Err(); err != nil {
		return err
	}
	if g.depth >= maxRenderDepth {
		return fmt.Errorf("%w: nesting deeper than %d", errSandbox, maxRenderDepth)
	}
	g.depth++
	return nil
}

func (g *guard) leave() { g.depth-- }

// allowedControlStructures omits every construct that renders into a
// private buffer (block-form set, filter, call, macro), that reads another
// template (extends, from, import, include), whose state the sandbox cannot
// inspect (with, block, do), or that needs gettext (trans). set is
// replaced by setStructure, which takes only the expression form.
var allowedControlStructures = []string{"autoescape", "break", "continue", "for", "if", "raw"}

func (g *guard) controlStructures() *exec.ControlStructureSet {
	set := make(map[string]parser.ControlStructureParser, len(allowedControlStructures)+1)
	for _, name := range allowedControlStructures {
		parse, ok := controlstructures.All.Get(name)
		if !ok {
			panic("review: gonja has no control structure " + name)
		}
		set[name] = g.guarded(parse)
	}
	set["set"] = g.guarded(parseSet)
	return exec.NewControlStructureSet(set)
}

func (g *guard) guarded(parse parser.ControlStructureParser) parser.ControlStructureParser {
	return func(p *parser.Parser, args *parser.Parser) (nodes.ControlStructure, error) {
		cs, err := parse(p, args)
		if err != nil {
			return nil, err
		}
		if f, ok := cs.(*controlstructures.ForControlStructure); ok && f.Recursive {
			return nil, fmt.Errorf("%w: recursive loops are not available", errSandbox)
		}
		inner, ok := cs.(exec.ControlStructure)
		if !ok {
			return cs, nil
		}
		return &guardedStructure{ControlStructure: inner, g: g}, nil
	}
}

type guardedStructure struct {
	exec.ControlStructure
	g *guard
}

func (s *guardedStructure) Execute(r *exec.Renderer, tag *nodes.ControlStructureBlock) error {
	if err := s.g.enter(); err != nil {
		return err
	}
	defer s.g.leave()
	return s.ControlStructure.Execute(r, tag)
}

// setStructure is {% set name = expression %}. The value is bounded like
// any filter result, so repeated assignment cannot grow a value past the
// output limit.
type setStructure struct {
	Location   *tokens.Token
	Name       string
	Expression nodes.Expression
}

func parseSet(p *parser.Parser, args *parser.Parser) (nodes.ControlStructure, error) {
	name := args.Match(tokens.Name)
	if name == nil || args.Match(tokens.Assign) == nil {
		return nil, fmt.Errorf("%w: only {%% set name = expression %%} is available", errSandbox)
	}
	expr, err := args.ParseExpression()
	if err != nil {
		return nil, err
	}
	if !args.End() {
		return nil, args.Error("set takes one name and one expression", args.Current())
	}
	return &setStructure{Location: p.Current(), Name: name.Val, Expression: expr}, nil
}

func (s *setStructure) Position() *tokens.Token { return s.Location }

func (s *setStructure) String() string {
	return fmt.Sprintf("set(%s, line %d)", s.Name, s.Location.Line)
}

func (s *setStructure) Execute(r *exec.Renderer, _ *nodes.ControlStructureBlock) error {
	v := r.Eval(s.Expression)
	if v.IsError() {
		return v
	}
	if err := checkValue(v); err != nil {
		return err
	}
	r.Environment.Context.Set(s.Name, v.Interface())
	return nil
}

func (g *guard) globals() map[string]any {
	out := map[string]any{
		"range":    g.rangeFn,
		addendName: addend,
		iterName:   g.iter,
		stepName:   g.step,
	}
	for _, name := range []string{"cycler", "dict", "joiner"} {
		fn, ok := builtins.GlobalFunctions.Get(name)
		if !ok {
			panic("review: gonja has no global " + name)
		}
		out[name] = fn
	}
	return out
}

// addend passes numbers to + and refuses strings and lists, whose
// concatenation grows a value in one expression.
func addend(params *exec.VarArgs) *exec.Value {
	v := params.First()
	if v.IsString() || v.IsList() {
		return exec.AsValue(fmt.Errorf("%w: + on strings or lists is not available; write the parts side by side", errSandbox))
	}
	return v
}

// iter charges a loop's whole length to the render's iteration budget
// before the loop starts.
func (g *guard) iter(params *exec.VarArgs) *exec.Value {
	v := params.First()
	if err := g.ctx.Err(); err != nil {
		return exec.AsValue(err)
	}
	n := 0
	switch {
	case v.IsString():
		n = len(v.String())
	case v.IsList(), v.IsDict():
		n = v.Len()
	}
	if g.iterations += n; g.iterations > maxIterations {
		return exec.AsValue(fmt.Errorf("%w: loops over %d iterations in total", errSandbox, maxIterations))
	}
	return v
}

// step runs at the start of every loop iteration so a loop notices the
// deadline even when its body has no other guard.
func (g *guard) step(*exec.VarArgs) *exec.Value {
	if err := g.ctx.Err(); err != nil {
		return exec.AsValue(err)
	}
	return exec.AsValue("")
}

func (g *guard) rangeFn(params *exec.VarArgs) ([]int, error) {
	if err := g.ctx.Err(); err != nil {
		return nil, err
	}
	start, stop, step := 0, 0, 1
	ints := make([]int, len(params.Args))
	for i, a := range params.Args {
		if !a.IsInteger() {
			return nil, fmt.Errorf("%w: range expects integers", errSandbox)
		}
		ints[i] = a.Integer()
	}
	switch len(ints) {
	case 1:
		stop = ints[0]
	case 2:
		start, stop = ints[0], ints[1]
	case 3:
		start, stop, step = ints[0], ints[1], ints[2]
	default:
		return nil, fmt.Errorf("%w: range expects [start, ]stop[, step]", errSandbox)
	}
	if step == 0 {
		return nil, fmt.Errorf("%w: range step cannot be 0", errSandbox)
	}
	var out []int
	for i := start; (step > 0 && i < stop) || (step < 0 && i > stop); i += step {
		if len(out) == maxRangeItems {
			return nil, fmt.Errorf("%w: range over %d items", errSandbox, maxRangeItems)
		}
		out = append(out, i)
	}
	return out, nil
}

// size is the number of bytes a value holds, counting a collection's
// string elements by length and anything else as one.
func size(v *exec.Value) int {
	if v == nil {
		return 0
	}
	if v.IsString() {
		return len(v.String())
	}
	if !v.IsList() && !v.IsDict() {
		return 1
	}
	n := 0
	walk := func(x reflect.Value) {
		for x.Kind() == reflect.Interface || x.Kind() == reflect.Pointer {
			if x.IsNil() {
				return
			}
			if ev, ok := x.Interface().(*exec.Value); ok {
				x = ev.Val
				continue
			}
			x = x.Elem()
		}
		if x.Kind() == reflect.String {
			n += x.Len()
		}
		n++
	}
	rv := reflect.ValueOf(v.Interface())
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := range rv.Len() {
			walk(rv.Index(i))
		}
	case reflect.Map:
		for it := rv.MapRange(); it.Next(); {
			walk(it.Key())
			walk(it.Value())
		}
	default:
		n = v.Len()
	}
	return n
}

// checkValue bounds a value a template keeps: by set or as a filter or
// method result.
func checkValue(v *exec.Value) error {
	if size(v) > MaxRenderBytes {
		return fmt.Errorf("%w: value over %d bytes", errSandbox, MaxRenderBytes)
	}
	if (v.IsList() || v.IsDict()) && v.Len() > maxIterations {
		return fmt.Errorf("%w: collection over %d items", errSandbox, maxIterations)
	}
	return nil
}

// filterNames are gonja's filters less random, which would make a render
// differ between runs, and format, whose width comes from the template
// string itself where the call guard cannot see it.
var filterNames = []string{
	"abs", "attr", "batch", "capitalize", "center", "default", "d", "dictsort", "e", "escape", "filesizeformat",
	"first", "float", "forceescape", "groupby", "indent", "int", "join", "items", "last", "length", "count",
	"list", "lower", "map", "max", "min", "pprint", "rejectattr", "reject", "replace", "reverse", "round",
	"safe", "selectattr", "select", "slice", "sort", "string", "striptags", "sum", "title", "tojson", "trim",
	"truncate", "unique", "upper", "urlencode", "urlize", "wordcount", "wordwrap", "xmlattr",
}

func (g *guard) filters() *exec.FilterSet {
	set := make(map[string]exec.FilterFunction, len(filterNames))
	for _, name := range filterNames {
		fn, ok := builtins.Filters.Get(name)
		if !ok {
			panic("review: gonja has no filter " + name)
		}
		set[name] = func(e *exec.Evaluator, in *exec.Value, params *exec.VarArgs) *exec.Value {
			if err := g.checkCall(in, params); err != nil {
				return exec.AsValue(err)
			}
			out := fn(e, in, params)
			if out != nil && !out.IsError() {
				if err := checkValue(out); err != nil {
					return exec.AsValue(fmt.Errorf("filter %s: %w", name, err))
				}
			}
			return out
		}
	}
	return exec.NewFilterSet(set)
}

// checkCall estimates the work of one filter or method call and refuses it
// when an amplifying call could allocate far beyond the output limit.
func (g *guard) checkCall(in *exec.Value, params *exec.VarArgs) error {
	if err := g.ctx.Err(); err != nil {
		return err
	}
	inSize := size(in)
	if inSize > maxCallWork {
		return fmt.Errorf("%w: call input too large", errSandbox)
	}
	args := 1.0
	add := func(v *exec.Value) {
		switch {
		case v == nil:
		case v.IsInteger(), v.IsFloat():
			args += math.Abs(v.Float())
		default:
			args += float64(size(v))
		}
	}
	if params != nil {
		for _, v := range params.Args {
			add(v)
		}
		for _, v := range params.KwArgs {
			add(v)
		}
	}
	if float64(inSize+1)*args > maxCallWork {
		return fmt.Errorf("%w: call too large", errSandbox)
	}
	return nil
}

var strMethodNames = []string{
	"capitalize", "capwords", "casefold", "center", "count", "endswith", "expandtabs", "find", "isalnum", "isalpha",
	"isascii", "isdecimal", "isdigit", "islower", "isnumeric", "isprintable", "isspace", "istitle", "isupper", "join",
	"ljust", "lower", "lstrip", "partition", "removeprefix", "removesuffix", "replace", "rfind", "rjust", "rpartition",
	"rsplit", "rstrip", "split", "splitlines", "startswith", "strip", "swapcase", "title", "upper", "zfill",
}

// methods keeps string methods behind the call guard and only the
// read-only dict and list methods: no template may mutate what it is given.
func (g *guard) methods() exec.Methods {
	str := make(map[string]exec.Method[string], len(strMethodNames))
	for _, name := range strMethodNames {
		fn, ok := builtins.Methods.Str.Get(name)
		if !ok {
			panic("review: gonja has no str method " + name)
		}
		str[name] = func(self string, selfValue *exec.Value, args *exec.VarArgs) (any, error) {
			if err := g.checkCall(selfValue, args); err != nil {
				return nil, err
			}
			out, err := fn(self, selfValue, args)
			if err != nil {
				return nil, err
			}
			if err := checkValue(exec.AsValue(out)); err != nil {
				return nil, fmt.Errorf("method %s: %w", name, err)
			}
			return out, nil
		}
	}
	dict := map[string]exec.Method[map[string]any]{}
	for _, name := range []string{"keys", "values", "items", "get", "copy"} {
		fn, ok := builtins.Methods.Dict.Get(name)
		if !ok {
			panic("review: gonja has no dict method " + name)
		}
		dict[name] = fn
	}
	list := map[string]exec.Method[[]any]{}
	if fn, ok := builtins.Methods.List.Get("copy"); ok {
		list["copy"] = fn
	}
	return exec.Methods{
		Bool:  builtins.Methods.Bool,
		Int:   builtins.Methods.Int,
		Float: builtins.Methods.Float,
		Str:   exec.NewMethodSet(str),
		Dict:  exec.NewMethodSet(dict),
		List:  exec.NewMethodSet(list),
	}
}
