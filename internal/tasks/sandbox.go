package tasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
	"unicode/utf8"
)

// Sandbox bounds. A task's templates are written by a repository's
// maintainers and see attacker-influenced event data, so a render is
// bounded in output, in loop iterations and in the bytes its functions are
// handed, which together bound its time: define and template are refused,
// so a template cannot recurse, and no function amplifies its input.
const (
	// MaxTemplateBytes caps a template's source.
	MaxTemplateBytes = 64 << 10
	// maxIterations is a render's budget of loop iterations, charged in
	// full when a loop starts.
	maxIterations = 20_000
	// maxFuncBytes is a render's budget of bytes handed to functions.
	maxFuncBytes = 32 << 20
	// maxPrintfWidth bounds a printf verb's width or precision.
	maxPrintfWidth = 1024
	// maxMeasureDepth bounds how deep an argument is measured.
	maxMeasureDepth = 64
)

// errSandbox is a render the sandbox stopped.
var errSandbox = errors.New("tasks: template sandbox")

// reservedPrefix names the functions the sandbox adds; templates may not
// use it.
const (
	reservedPrefix = "__kritik_"
	iterName       = reservedPrefix + "iter"
)

// baseFuncs are the functions a template may call, plus redefinitions of
// the builtins that can amplify their input, so the guard sees them.
var baseFuncs = template.FuncMap{
	"join":      join,
	"lower":     strings.ToLower,
	"upper":     strings.ToUpper,
	"trim":      strings.TrimSpace,
	"truncate":  truncate,
	"toJSON":    toJSON,
	"default":   dflt,
	"contains":  contains,
	"hasPrefix": func(prefix, s string) bool { return strings.HasPrefix(s, prefix) },
	"quote":     func(v any) string { return strconv.Quote(fmt.Sprint(v)) },
	"printf":    printf,
	"print":     fmt.Sprint,
	"println":   fmt.Sprintln,
	"html":      template.HTMLEscaper,
	"js":        template.JSEscaper,
	"urlquery":  template.URLQueryEscaper,
}

func join(sep string, v any) (string, error) {
	switch l := v.(type) {
	case nil:
		return "", nil
	case []string:
		return strings.Join(l, sep), nil
	case []any:
		parts := make([]string, len(l))
		for i, x := range l {
			parts[i] = fmt.Sprint(x)
		}
		return strings.Join(parts, sep), nil
	}
	return "", fmt.Errorf("join: want a list, got %T", v)
}

// truncate shortens s to at most n runes.
func truncate(n int, s string) string {
	if n < 0 {
		n = 0
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

func toJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// dflt is v, or def when v is empty.
func dflt(def, v any) any {
	if v == nil {
		return def
	}
	rv := reflect.ValueOf(v)
	if rv.IsZero() {
		return def
	}
	if k := rv.Kind(); (k == reflect.Slice || k == reflect.Map) && rv.Len() == 0 {
		return def
	}
	return v
}

// contains reports whether hay, a string or a list, contains needle.
func contains(needle, hay any) (bool, error) {
	switch h := hay.(type) {
	case string:
		return strings.Contains(h, fmt.Sprint(needle)), nil
	case []string:
		return slices.Contains(h, fmt.Sprint(needle)), nil
	case []any:
		return slices.ContainsFunc(h, func(x any) bool { return fmt.Sprint(x) == fmt.Sprint(needle) }), nil
	case nil:
		return false, nil
	}
	return false, fmt.Errorf("contains: want a string or a list, got %T", hay)
}

// printfVerb captures a verb's width and precision.
var printfVerb = regexp.MustCompile(`%[-+# 0]*(\*|\d+)?(?:\.(\*|\d+)?)?`)

func printf(format string, args ...any) (string, error) {
	for _, m := range printfVerb.FindAllStringSubmatch(format, -1) {
		for _, n := range m[1:] {
			if n == "" {
				continue
			}
			if w, err := strconv.Atoi(n); err != nil || w > maxPrintfWidth {
				return "", fmt.Errorf("%w: printf width or precision %q", errSandbox, n)
			}
		}
	}
	return fmt.Sprintf(format, args...), nil
}

// parseTemplate parses src as a sandboxed template.
func parseTemplate(name, src string) (*template.Template, error) {
	if len(src) > MaxTemplateBytes {
		return nil, fmt.Errorf("template %s is %d bytes, over the %d byte limit", name, len(src), MaxTemplateBytes)
	}
	t, err := template.New(name).Option("missingkey=zero").Funcs((&renderGuard{}).funcs()).Parse(src)
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	if len(t.Templates()) > 1 {
		return nil, fmt.Errorf("template %s: define is not available", name)
	}
	if err := walk(t.Tree, t.Root); err != nil {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	return t, nil
}

// render executes t against data, writing at most limit bytes.
func render(t *template.Template, data any, limit int) (string, error) {
	c, err := t.Clone()
	if err != nil {
		return "", fmt.Errorf("tasks: render %s: %w", t.Name(), err)
	}
	c.Funcs((&renderGuard{}).funcs())
	w := &cappedWriter{limit: limit}
	if err := c.Execute(w, data); err != nil {
		return "", fmt.Errorf("tasks: render %s: %w", t.Name(), err)
	}
	return w.String(), nil
}

// walk refuses template invocations and reserved names, and routes every
// loop's value through the iteration guard.
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
		g := parse.NewIdentifier(iterName).SetTree(tree).SetPos(n.Pipe.Pos)
		n.Pipe.Cmds = append(n.Pipe.Cmds, &parse.CommandNode{NodeType: parse.NodeCommand, Pos: n.Pipe.Pos, Args: []parse.Node{g}})
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

// errTooLarge is a render past its output cap.
var errTooLarge = errors.New("tasks: rendered output exceeds its size limit")

// cappedWriter refuses to grow past limit.
type cappedWriter struct {
	limit int
	strings.Builder
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.Len()+len(p) > w.limit {
		return 0, errTooLarge
	}
	return w.Builder.Write(p)
}

// renderGuard is one render's budgets. A render runs on one goroutine.
type renderGuard struct {
	iterations int
	bytes      int
}

func (g *renderGuard) funcs() template.FuncMap {
	fm := make(template.FuncMap, len(baseFuncs)+1)
	for name, fn := range baseFuncs {
		fm[name] = g.wrap(name, reflect.ValueOf(fn))
	}
	fm[iterName] = g.iter
	return fm
}

// wrap charges fn's arguments to the render's byte budget before calling
// it. A failed check panics; text/template turns a panic in a function
// into an execution error.
func (g *renderGuard) wrap(name string, fn reflect.Value) any {
	return reflect.MakeFunc(fn.Type(), func(args []reflect.Value) []reflect.Value {
		for _, a := range args {
			g.bytes += measure(a, 0, maxFuncBytes-g.bytes)
		}
		if g.bytes > maxFuncBytes {
			panic(fmt.Errorf("%w: %s: functions were handed more than %d bytes", errSandbox, name, maxFuncBytes))
		}
		if fn.Type().IsVariadic() {
			return fn.CallSlice(args)
		}
		return fn.Call(args)
	}).Interface()
}

// iter charges a loop's iterations and passes its value through.
func (g *renderGuard) iter(v any) (any, error) {
	rv := indirect(reflect.ValueOf(v))
	n := 0
	switch rv.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map, reflect.String:
		n = rv.Len()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n = int(min(max(rv.Int(), 0), maxIterations+1))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n = int(min(rv.Uint(), maxIterations+1))
	case reflect.Func, reflect.Chan:
		return nil, fmt.Errorf("%w: cannot loop over a %s", errSandbox, rv.Kind())
	}
	g.iterations += n
	if g.iterations > maxIterations {
		return nil, fmt.Errorf("%w: loops over more than %d items per render", errSandbox, maxIterations)
	}
	return v, nil
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

// measure is roughly the bytes v holds, stopping once past limit.
func measure(v reflect.Value, depth, limit int) int {
	v = indirect(v)
	if !v.IsValid() || limit < 0 {
		return 0
	}
	if depth >= maxMeasureDepth {
		return 8
	}
	switch v.Kind() {
	case reflect.String:
		return v.Len()
	case reflect.Slice, reflect.Array:
		n := 0
		for i := 0; i < v.Len() && n <= limit; i++ {
			n += 8 + measure(v.Index(i), depth+1, limit-n)
		}
		return n
	case reflect.Map:
		n := 0
		for it := v.MapRange(); it.Next() && n <= limit; {
			n += 8 + measure(it.Key(), depth+1, limit-n) + measure(it.Value(), depth+1, limit-n)
		}
		return n
	case reflect.Struct:
		n := 0
		for i := 0; i < v.NumField() && n <= limit; i++ {
			if v.Type().Field(i).IsExported() {
				n += 8 + measure(v.Field(i), depth+1, limit-n)
			}
		}
		return n
	}
	return 8
}
