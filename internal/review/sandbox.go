package review

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/go-sprout/sprout"
	"github.com/go-sprout/sprout/registry/conversion"
	"github.com/go-sprout/sprout/registry/encoding"
	"github.com/go-sprout/sprout/registry/maps"
	"github.com/go-sprout/sprout/registry/numeric"
	sproutreflect "github.com/go-sprout/sprout/registry/reflect"
	"github.com/go-sprout/sprout/registry/regex"
	"github.com/go-sprout/sprout/registry/semver"
	"github.com/go-sprout/sprout/registry/slices"
	"github.com/go-sprout/sprout/registry/std"
	sproutstrings "github.com/go-sprout/sprout/registry/strings"
	sprouttime "github.com/go-sprout/sprout/registry/time"
)

// Sandbox bounds for templates. A repository template is trusted to be
// written by the repository's maintainers, not to be well behaved: these
// keep a mistake in one repository's template from stalling or exhausting
// a worker that serves every tenant. text/template has no operators, so
// every value a template builds passes through a function, and the
// bounds are enforced there and on loops.
const (
	// maxIterations is the render-wide budget of loop iterations, charged
	// in full when a loop starts, so nested loops cannot multiply it.
	maxIterations = 20_000
	// maxCallBytes bounds one function call: its arguments plus an
	// estimate of what it allocates, computed from them before the call
	// runs so amplifying functions fail before they allocate, and its
	// result.
	maxCallBytes = 4 * MaxRenderBytes
	// maxMeasureDepth bounds how deep a value is measured; anything deeper
	// counts as one node.
	maxMeasureDepth = 64
	// nodeBytes is what a slice element, map entry or struct field is
	// counted as beyond its contents, so a large shared structure is
	// refused as if it were copied.
	nodeBytes = 8
)

// Names the sandbox adds to every template. Templates may not use
// identifiers with this prefix, so they cannot call the guards themselves.
const (
	reservedPrefix = "__kritik_"
	iterName       = reservedPrefix + "iter"
	templateName   = "template"
)

// baseFuncs are the functions a template may call: sprout's registries
// less those that reach outside the render (env, filesystem, network),
// that make a render unrepeatable (random, uniqueid) or that have no use
// in a comment (checksum, crypto), and less set and unset, which mutate a
// dict in place and could make it contain itself. printf, print and
// println are redefined so the guard sees them like any other function.
var baseFuncs = func() template.FuncMap {
	h := sprout.New()
	if err := h.AddRegistries(
		std.NewRegistry(),
		sproutstrings.NewRegistry(),
		conversion.NewRegistry(),
		encoding.NewRegistry(),
		numeric.NewRegistry(),
		slices.NewRegistry(),
		maps.NewRegistry(),
		regex.NewRegistry(),
		sprouttime.NewRegistry(),
		semver.NewRegistry(),
		sproutreflect.NewRegistry(),
	); err != nil {
		panic("review: sprout registries: " + err.Error())
	}
	fm := h.Build()
	delete(fm, "set")
	delete(fm, "unset")
	fm["printf"], fm["print"], fm["println"] = fmt.Sprintf, fmt.Sprint, fmt.Sprintln
	return fm
}()

// execute parses src, hardens it and renders it against data in a
// sandbox, writing at most limit bytes.
func execute(ctx context.Context, src string, data any, limit int) (string, error) {
	g := &guard{ctx: ctx}
	t, err := template.New(templateName).Funcs(g.funcs()).Parse(src)
	if err != nil {
		return "", fmt.Errorf("review: parse template: %w", err)
	}
	if err := harden(t); err != nil {
		return "", fmt.Errorf("%w: %w", errSandbox, err)
	}
	w := &cappedWriter{ctx: ctx, limit: limit}
	if err := t.Execute(w, data); err != nil {
		return "", fmt.Errorf("review: render: %w", err)
	}
	return w.String(), nil
}

// harden rejects what the guards cannot bound and routes every loop's
// value through the iteration guard. template and define are refused:
// a template that invokes others can fan out exponentially with no loop
// and no output for the guards to see.
func harden(t *template.Template) error {
	if len(t.Templates()) > 1 {
		return errors.New("define is not available")
	}
	return walk(t.Tree, t.Root)
}

func walk(tree *parse.Tree, node parse.Node) error {
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return nil
		}
		for _, c := range n.Nodes {
			if err := walk(tree, c); err != nil {
				return err
			}
		}
	case *parse.ActionNode:
		return walk(tree, n.Pipe)
	case *parse.IfNode:
		return walkBranch(tree, &n.BranchNode)
	case *parse.WithNode:
		return walkBranch(tree, &n.BranchNode)
	case *parse.RangeNode:
		if err := walkBranch(tree, &n.BranchNode); err != nil {
			return err
		}
		guard := parse.NewIdentifier(iterName).SetTree(tree).SetPos(n.Pipe.Pos)
		n.Pipe.Cmds = append(n.Pipe.Cmds, &parse.CommandNode{NodeType: parse.NodeCommand, Pos: n.Pipe.Pos, Args: []parse.Node{guard}})
	case *parse.PipeNode:
		if n == nil {
			return nil
		}
		for _, c := range n.Cmds {
			if err := walk(tree, c); err != nil {
				return err
			}
		}
	case *parse.CommandNode:
		for _, a := range n.Args {
			if err := walk(tree, a); err != nil {
				return err
			}
		}
	case *parse.ChainNode:
		return walk(tree, n.Node)
	case *parse.IdentifierNode:
		if strings.HasPrefix(n.Ident, reservedPrefix) {
			return fmt.Errorf("%s is reserved", n.Ident)
		}
	case *parse.TemplateNode:
		return errors.New("template is not available")
	}
	return nil
}

func walkBranch(tree *parse.Tree, b *parse.BranchNode) error {
	for _, n := range []parse.Node{b.Pipe, b.List, b.ElseList} {
		if err := walk(tree, n); err != nil {
			return err
		}
	}
	return nil
}

// cappedWriter refuses to grow past limit and stops once ctx is done.
type cappedWriter struct {
	ctx   context.Context
	limit int
	strings.Builder
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.Len()+len(p) > w.limit {
		return 0, errTooLarge
	}
	return w.Builder.Write(p)
}

// guard is one render's sandbox state. A render runs on one goroutine, so
// its counters need no synchronisation.
type guard struct {
	ctx        context.Context
	iterations int
}

// funcs is the template's function map: every base function wrapped by
// the guard, plus the iteration guard harden inserts.
func (g *guard) funcs() template.FuncMap {
	fm := make(template.FuncMap, len(baseFuncs)+1)
	for name, fn := range baseFuncs {
		fm[name] = g.wrap(name, reflect.ValueOf(fn))
	}
	fm[iterName] = g.iter
	return fm
}

// wrap returns fn behind a check of the deadline, of its arguments and of
// its result. A failed check panics; text/template recovers a panic in a
// function call into an execution error.
func (g *guard) wrap(name string, fn reflect.Value) any {
	t := fn.Type()
	estimate := estimates[name]
	return reflect.MakeFunc(t, func(args []reflect.Value) []reflect.Value {
		if err := g.ctx.Err(); err != nil {
			panic(err)
		}
		size := 0
		for _, a := range args {
			size += measure(a, 0, maxCallBytes)
		}
		if estimate != nil {
			size += estimate(args)
		}
		if size > maxCallBytes {
			panic(fmt.Errorf("%w: %s: %d bytes of input and estimated allocation, at most %d", errSandbox, name, size, maxCallBytes))
		}
		var out []reflect.Value
		if t.IsVariadic() {
			out = fn.CallSlice(args)
		} else {
			out = fn.Call(args)
		}
		if n := measure(out[0], 0, maxCallBytes); n > maxCallBytes {
			panic(fmt.Errorf("%w: %s returned more than %d bytes", errSandbox, name, maxCallBytes))
		}
		return out
	}).Interface()
}

// iter is appended to every range pipeline. It charges the loop's
// iterations to the render's budget and passes the value through.
func (g *guard) iter(v any) (any, error) {
	if err := g.ctx.Err(); err != nil {
		return nil, err
	}
	n, err := iterations(reflect.ValueOf(v))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSandbox, err)
	}
	g.iterations += n
	if g.iterations > maxIterations {
		return nil, fmt.Errorf("%w: loops over more than %d items per render", errSandbox, maxIterations)
	}
	return v, nil
}

func iterations(v reflect.Value) (int, error) {
	v = indirect(v)
	switch v.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map, reflect.String:
		return v.Len(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(min(max(v.Int(), 0), maxIterations+1)), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return int(min(v.Uint(), maxIterations+1)), nil
	case reflect.Func, reflect.Chan:
		return 0, fmt.Errorf("cannot loop over a %s", v.Kind())
	}
	return 0, nil
}

func indirect(v reflect.Value) reflect.Value {
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

// measure is the bytes v holds: every string's length plus nodeBytes per
// element, entry or field. It stops once past limit, so a structure that
// shares one large value many times over is refused quickly rather than
// walked in full.
func measure(v reflect.Value, depth, limit int) int {
	v = indirect(v)
	if !v.IsValid() {
		return 0
	}
	if depth >= maxMeasureDepth {
		return nodeBytes
	}
	switch v.Kind() {
	case reflect.String:
		return v.Len()
	case reflect.Slice, reflect.Array:
		n := 0
		for i := 0; i < v.Len() && n <= limit; i++ {
			n += nodeBytes + measure(v.Index(i), depth+1, limit-n)
		}
		return n
	case reflect.Map:
		n := 0
		for it := v.MapRange(); it.Next() && n <= limit; {
			n += nodeBytes + measure(it.Key(), depth+1, limit-n) + measure(it.Value(), depth+1, limit-n)
		}
		return n
	case reflect.Struct:
		n := 0
		for i := 0; i < v.NumField() && n <= limit; i++ {
			n += nodeBytes + measure(v.Field(i), depth+1, limit-n)
		}
		return n
	}
	return 0
}

// estimates say what a function allocates beyond its arguments, for the
// functions that can build a value far larger than what they are given.
// Arguments arrive as the function's parameters, a variadic one as a
// slice.
var estimates = map[string]func(args []reflect.Value) int{
	"repeat": func(a []reflect.Value) int { return product(a[0].Int(), int64(a[1].Len())) },
	"indent": func(a []reflect.Value) int {
		return product(a[0].Int(), int64(strings.Count(a[1].String(), "\n")+1))
	},
	"nindent": func(a []reflect.Value) int {
		return product(a[0].Int(), int64(strings.Count(a[1].String(), "\n")+1))
	},
	"join": func(a []reflect.Value) int { return product(int64(a[0].Len()), int64(length(a[1]))) },
	"replace": func(a []reflect.Value) int {
		return product(int64(a[2].Len()/max(a[0].Len(), 1)+1), int64(a[1].Len()))
	},
	"regexReplaceAll":        func(a []reflect.Value) int { return product(int64(a[2].Len()+1), int64(a[1].Len())) },
	"regexReplaceAllLiteral": func(a []reflect.Value) int { return product(int64(a[2].Len()+1), int64(a[1].Len())) },
	"until":                  func(a []reflect.Value) int { return product(abs(a[0].Int()), nodeBytes) },
	"untilStep": func(a []reflect.Value) int {
		return product(span(a[0].Int(), a[1].Int(), a[2].Int()), nodeBytes)
	},
	"seq": func(a []reflect.Value) int {
		p := a[0]
		switch p.Len() {
		case 1:
			return product(span(1, p.Index(0).Int(), 1)+1, seqItemBytes)
		case 2:
			return product(span(p.Index(0).Int(), p.Index(1).Int(), 1)+1, seqItemBytes)
		case 3:
			return product(span(p.Index(0).Int(), p.Index(2).Int(), p.Index(1).Int())+1, seqItemBytes)
		}
		return 0
	},
	"printf": func(a []reflect.Value) int { return printfWidths(a[0].String()) },
}

// seqItemBytes is what one number of seq's output is counted as.
const seqItemBytes = 12

// product is x*y clamped to [0, maxCallBytes+1].
func product(x, y int64) int {
	if x <= 0 || y <= 0 {
		return 0
	}
	if x > (maxCallBytes+1)/y {
		return maxCallBytes + 1
	}
	return int(x * y)
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// span is how many steps of step lead from start towards stop, zero when
// step points the wrong way or nowhere.
func span(start, stop, step int64) int64 {
	if step == 0 || (stop-start)/step < 0 {
		return 0
	}
	return abs((stop - start) / step)
}

// length is the elements in v when it is a slice, array, map or string.
func length(v reflect.Value) int {
	v = indirect(v)
	switch v.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map, reflect.String:
		return v.Len()
	}
	return 0
}

// printfVerb captures a verb's width and precision; * takes them from an
// argument, which the estimate cannot see, so it is refused.
var printfVerb = regexp.MustCompile(`%[-+# 0]*(\*|\d+)?(?:\.(\*|\d+)?)?`)

// printfWidths is the padding a format asks for, summed over its verbs.
func printfWidths(format string) int {
	total := 0
	for _, m := range printfVerb.FindAllStringSubmatch(format, -1) {
		for _, n := range m[1:] {
			if n == "" {
				continue
			}
			w, err := strconv.Atoi(n)
			if err != nil || n == "*" {
				return maxCallBytes + 1
			}
			total += w
			if total > maxCallBytes {
				return maxCallBytes + 1
			}
		}
	}
	return total
}
