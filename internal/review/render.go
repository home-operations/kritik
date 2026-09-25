package review

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/nikolalohinski/gonja/v2/builtins"
	controlstructures "github.com/nikolalohinski/gonja/v2/builtins/control_structures"
	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
	"github.com/nikolalohinski/gonja/v2/nodes"
	"github.com/nikolalohinski/gonja/v2/parser"
	"github.com/nikolalohinski/gonja/v2/tokens"
)

// MaxRenderBytes bounds a rendered comment, marker included. GitHub rejects
// comment bodies over 65,536 characters, and a character is at least a byte.
const MaxRenderBytes = 64 << 10

// Sandbox bounds for repository templates. A template is trusted to be
// written by the repository's maintainers, not to be well behaved: these
// keep a mistake in one repository's template from stalling or exhausting
// a worker that serves every tenant.
const (
	// maxRenderDepth bounds nested macro calls and control structures, so
	// unbounded recursion fails instead of overflowing the goroutine stack,
	// which the runtime cannot recover from.
	maxRenderDepth = 64
	// maxRangeItems bounds one range() call.
	maxRangeItems = 10_000
	// maxCallWork bounds one filter or method call, estimated as the size
	// of its input times the size of its arguments, so amplifying calls
	// such as replace or center fail before they allocate.
	maxCallWork = 16 << 20
)

// renderTimeout bounds one render, parse included.
var renderTimeout = 2 * time.Second

//go:embed templates/summary.md.j2
var defaultSummary string

//go:embed templates/inline.md.j2
var defaultInline string

// Templates are repository-supplied template sources. An empty field means
// the embedded default.
type Templates struct {
	Summary, Inline string
}

// RenderData is what the summary template sees.
type RenderData struct {
	Number       int
	HeadSHA      string
	Model        string
	Result       Result
	Counts       Counts
	Notes        []string
	Incremental  bool
	PriorHeadSHA string
}

var (
	errTooLarge = errors.New("review: rendered output exceeds the size limit")
	errSandbox  = errors.New("review: template exceeded a sandbox limit")
)

// RenderSummary renders the sticky comment. kritik's marker always leads,
// whatever the template does, so sticky discovery cannot be defeated by a
// template. When the repository's template fails, the default is used and
// the returned note, also shown in the comment, says why.
func RenderSummary(ctx context.Context, t Templates, d RenderData) (body string, notes []string) {
	marker := Marker(d.Number) + "\n"
	limit := MaxRenderBytes - len(marker)
	if t.Summary != "" {
		out, err := render(ctx, t.Summary, summaryContext(d), limit)
		if err == nil {
			return marker + out, nil
		}
		note := fallbackNote("summary", err)
		notes = append(notes, note)
		d.Notes = append(slices.Clone(d.Notes), note)
	}
	out, err := render(context.WithoutCancel(ctx), defaultSummary, summaryContext(d), limit)
	if err != nil {
		// The default renders data kritik bounds itself; failing here is a
		// bug, but the comment must still carry the marker and the take.
		out = truncateUTF8(fmt.Sprintf("### kritik review\n\n%s\n", d.Result.Summary.Take), limit)
	}
	return marker + out, notes
}

// RenderInline renders one finding as an inline review comment, falling
// back to the default template as RenderSummary does.
func RenderInline(ctx context.Context, t Templates, f Finding) (string, []string) {
	var notes []string
	if t.Inline != "" {
		out, err := render(ctx, t.Inline, findingContext(f), MaxRenderBytes)
		if err == nil {
			return out, nil
		}
		notes = append(notes, fallbackNote("inline", err))
	}
	out, err := render(context.WithoutCancel(ctx), defaultInline, findingContext(f), MaxRenderBytes)
	if err != nil {
		out = truncateUTF8(fmt.Sprintf("**[%s]** **%s**\n\n%s\n", f.Severity, f.Title, f.Explanation), MaxRenderBytes)
	}
	return out, notes
}

func fallbackNote(which string, err error) string {
	why := "failed to render"
	switch {
	case errors.Is(err, errTooLarge):
		why = fmt.Sprintf("produced more than %d KiB", MaxRenderBytes>>10)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		why = "ran out of time"
	}
	return fmt.Sprintf("The repository's %s template %s, so kritik's default was used", which, why)
}

func summaryContext(d RenderData) map[string]any {
	praise := make([]any, len(d.Result.Summary.Praise))
	for i, p := range d.Result.Summary.Praise {
		praise[i] = p
	}
	findings := make([]any, len(d.Result.Findings))
	for i, f := range d.Result.Findings {
		findings[i] = findingContext(f)
	}
	notes := make([]any, len(d.Notes))
	for i, n := range d.Notes {
		notes[i] = n
	}
	return map[string]any{
		"number":    d.Number,
		"head_sha":  d.HeadSHA,
		"model":     d.Model,
		keySummary:  map[string]any{keyTake: d.Result.Summary.Take, keyPraise: praise},
		keyFindings: findings,
		"counts": map[string]any{
			"blocking": d.Counts.Blocking, "important": d.Counts.Important, "nit": d.Counts.Nit,
		},
		"notes":          notes,
		"incremental":    d.Incremental,
		"prior_head_sha": d.PriorHeadSHA,
	}
}

func findingContext(f Finding) map[string]any {
	return map[string]any{
		keyPath:         f.Path,
		keyLine:         f.Line,
		keySeverity:     string(f.Severity),
		keyTitle:        f.Title,
		keyExplanation:  f.Explanation,
		keySuggestedFix: f.SuggestedFix,
	}
}

// render parses and executes src in a sandbox, bounded by ctx and
// renderTimeout. A render that outlives its deadline is abandoned; the
// guards make it stop at its next control structure, call or write.
func render(ctx context.Context, src string, data map[string]any, limit int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, renderTimeout)
	defer cancel()
	type result struct {
		out string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- result{err: fmt.Errorf("%w: panic: %v", errSandbox, r)}
			}
		}()
		out, err := execute(ctx, src, data, limit)
		ch <- result{out, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil && ctx.Err() != nil {
			return "", fmt.Errorf("review: render: %w", ctx.Err())
		}
		return r.out, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("review: render: %w", ctx.Err())
	}
}

// templateName is the only name the loader knows: the template itself.
const templateName = "template"

func execute(ctx context.Context, src string, data map[string]any, limit int) (string, error) {
	if len(src) > MaxRenderBytes {
		return "", fmt.Errorf("%w: template source over %d bytes", errSandbox, MaxRenderBytes)
	}
	cfg := config.New()
	cfg.TrimBlocks, cfg.LeftStripBlocks, cfg.KeepTrailingNewline = true, true, true
	if err := checkOperators(src, cfg); err != nil {
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
	w := &cappedWriter{ctx: ctx, limit: limit}
	if err := tpl.Execute(w, exec.NewContext(data)); err != nil {
		if w.over {
			return "", errTooLarge
		}
		return "", fmt.Errorf("review: execute template: %w", err)
	}
	return w.b.String(), nil
}

// checkOperators rejects * and **: string repetition and exponentiation
// allocate in one step, before any guard can see the result.
func checkOperators(src string, cfg *config.Config) error {
	for s := tokens.LexAll(src, cfg); !s.End(); s.Next() {
		if t := s.Current().Type; t == tokens.Multiply || t == tokens.Power {
			return fmt.Errorf("%w: the * and ** operators are not available (line %d)", errSandbox, s.Current().Line)
		}
	}
	return nil
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

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// guard is one render's sandbox state. A render runs on one goroutine, so
// depth needs no synchronisation.
type guard struct {
	ctx   context.Context
	depth int
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

// allowedControlStructures omits everything that reads another template
// (extends, from, import, include), do, whose only use is calling a
// mutating method, and trans, which needs gettext.
var allowedControlStructures = []string{
	"autoescape", "block", "break", "call", "continue", "filter", "for", "if", "macro", "raw", "set", "with",
}

func (g *guard) controlStructures() *exec.ControlStructureSet {
	set := make(map[string]parser.ControlStructureParser, len(allowedControlStructures))
	for _, name := range allowedControlStructures {
		parse, ok := controlstructures.All.Get(name)
		if !ok {
			panic("review: gonja has no control structure " + name)
		}
		set[name] = func(p *parser.Parser, args *parser.Parser) (nodes.ControlStructure, error) {
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
	return exec.NewControlStructureSet(set)
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
	if err := s.ControlStructure.Execute(r, tag); err != nil {
		return err
	}
	if m, ok := s.ControlStructure.(*controlstructures.MacroControlStructure); ok {
		if v, ok := r.Environment.Context.Get(m.Name); ok {
			if fn, ok := v.(exec.Macro); ok {
				r.Environment.Context.Set(m.Name, s.g.macro(fn))
			}
		}
	}
	return nil
}

func (g *guard) macro(fn exec.Macro) exec.Macro {
	return func(params *exec.VarArgs) *exec.Value {
		if err := g.enter(); err != nil {
			return exec.AsValue(err)
		}
		defer g.leave()
		for _, v := range params.Args {
			if v.IsString() && v.Len() > MaxRenderBytes {
				return exec.AsValue(fmt.Errorf("%w: macro argument over %d bytes", errSandbox, MaxRenderBytes))
			}
		}
		return fn(params)
	}
}

func (g *guard) globals() map[string]any {
	out := map[string]any{"range": g.rangeFn}
	for _, name := range []string{"cycler", "dict", "joiner"} {
		fn, ok := builtins.GlobalFunctions.Get(name)
		if !ok {
			panic("review: gonja has no global " + name)
		}
		out[name] = fn
	}
	return out
}

func (g *guard) rangeFn(params *exec.VarArgs) ([]int, error) {
	if err := g.ctx.Err(); err != nil {
		return nil, err
	}
	start, stop, step := 0, 0, 1
	ints := make([]int, len(params.Args))
	for i, a := range params.Args {
		if !a.IsInteger() {
			return nil, errors.New("range expects integers")
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
		return nil, errors.New("range expects [start, ]stop[, step]")
	}
	if step == 0 {
		return nil, errors.New("range step cannot be 0")
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
			if out != nil && out.IsString() && out.Len() > MaxRenderBytes {
				return exec.AsValue(fmt.Errorf("%w: filter %s produced over %d bytes", errSandbox, name, MaxRenderBytes))
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
	size := 0
	if in != nil && (in.IsString() || in.IsList()) {
		size = in.Len()
	}
	args := 1.0
	add := func(v *exec.Value) {
		switch {
		case v == nil:
		case v.IsString():
			args += float64(v.Len())
		case v.IsInteger(), v.IsFloat():
			args += math.Abs(v.Float())
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
	if float64(size+1)*args > maxCallWork {
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
// read-only dict and list methods: no template may mutate what it is given
// or build state that grows across loop iterations.
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
			return fn(self, selfValue, args)
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
