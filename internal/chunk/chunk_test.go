package chunk

import (
	"strings"
	"testing"
)

const goSource = `package demo

import "fmt"

// Widget is a thing.
type Widget struct {
	Name string
}

const (
	Small = 1
	Large = 2
)

func (w *Widget) Describe(prefix string) string {
	return fmt.Sprintf("%s %s", prefix, w.Name)
}

func Build(name string) *Widget {
	w := &Widget{Name: name}
	fmt.Println(w.Describe("built"))
	return w
}
`

func TestParseGoDeclarations(t *testing.T) {
	p := &Parser{}
	f := p.Parse("demo.go", []byte(goSource))
	if f.Language != "go" {
		t.Fatalf("language = %q", f.Language)
	}
	byName := map[string]Decl{}
	for _, d := range f.Decls {
		byName[d.Symbol] = d
	}
	tests := []struct {
		symbol, kind, scope string
		start, end          int
	}{
		{"Widget", "type", "", 6, 8},
		{"Describe", "method", "Widget", 15, 17},
		{"Build", "function", "", 19, 23},
	}
	for _, tt := range tests {
		d, ok := byName[tt.symbol]
		if !ok {
			t.Fatalf("no declaration %q in %+v", tt.symbol, f.Decls)
		}
		if d.Kind != tt.kind || d.Scope != tt.scope || d.StartLine != tt.start || d.EndLine != tt.end {
			t.Errorf("%s = %+v, want kind=%s scope=%s L%d-%d", tt.symbol, d, tt.kind, tt.scope, tt.start, tt.end)
		}
	}
	if _, ok := byName["Small"]; !ok {
		// The const block is one declaration; its first name is the symbol.
		t.Errorf("const block not chunked: %+v", f.Decls)
	}
	if d := f.Enclosing([]int{21}, 100); d == nil || d.Symbol != "Build" {
		t.Fatalf("Enclosing(21) = %+v", d)
	}
	if d := f.Enclosing([]int{21}, 3); d != nil {
		t.Fatalf("Enclosing with a 3-line cap should be nil, got %+v", d)
	}
	if d := f.Enclosing([]int{15, 22}, 100); d != nil {
		t.Fatalf("no declaration spans lines 15 and 22, got %+v", d)
	}
	ids := f.Identifiers([]int{20, 21})
	want := []string{"Describe", "Name", "Println", "Widget", "fmt", "name", "w"}
	for _, id := range want {
		if !contains(ids, id) {
			t.Errorf("identifiers %v missing %q", ids, id)
		}
	}
	if contains(ids, "built") || contains(ids, "return") {
		t.Errorf("identifiers %v should exclude string contents and keywords", ids)
	}
	if ids[0] != "w" {
		t.Errorf("most frequent identifier should come first, got %v", ids)
	}
}

func TestParseFallsBackForConfigAndUnknown(t *testing.T) {
	p := &Parser{}
	yaml := p.Parse("values.yaml", []byte("a: 1\nb:\n  c: 2\n"))
	if yaml.Language != "yaml" || len(yaml.Decls) != 0 {
		t.Fatalf("yaml = lang %q decls %d; want no declarations", yaml.Language, len(yaml.Decls))
	}
	unknown := p.Parse("noext", []byte("hello"))
	if unknown.Language != "" || len(unknown.Decls) != 0 || unknown.Identifiers([]int{1}) != nil {
		t.Fatalf("unknown = %+v", unknown)
	}
	binary := p.Parse("x.go", []byte("package x\x00"))
	if len(binary.Decls) != 0 {
		t.Fatal("a file with a NUL byte must not be parsed")
	}
	name, decls := Language("main.rs")
	if name != "rust" || !decls {
		t.Fatalf("Language(main.rs) = %q %v", name, decls)
	}
	if name, decls := Language("Dockerfile"); name != "dockerfile" || decls {
		t.Fatalf("Language(Dockerfile) = %q %v; want a grammar without declarations", name, decls)
	}
}

func TestWindow(t *testing.T) {
	src := []byte("l1\nl2\nl3\nl4\nl5\n")
	s, e, text := Window(src, 3, 3, 1)
	if s != 2 || e != 4 || text != "l2\nl3\nl4" {
		t.Fatalf("Window = %d %d %q", s, e, text)
	}
	s, e, text = Window(src, 1, 5, 10)
	if s != 1 || e != 5 || !strings.HasSuffix(text, "l5") {
		t.Fatalf("clamped Window = %d %d %q", s, e, text)
	}
	if Text(src, 4, 5) != "l4\nl5" {
		t.Fatal("Text")
	}
}

func TestWindows(t *testing.T) {
	src := []byte(strings.Repeat("x\n", 130))
	w := Windows(src, 60, 10)
	if len(w) != 3 || w[0] != (Decl{Kind: "window", StartLine: 1, EndLine: 60}) || w[1].StartLine != 51 || w[2].StartLine != 101 || w[2].EndLine != 130 {
		t.Fatalf("Windows = %+v", w)
	}
	if got := Windows([]byte("one"), 60, 10); len(got) != 1 || got[0].EndLine != 1 {
		t.Fatalf("Windows(one line) = %+v", got)
	}
	if Windows(nil, 60, 10) != nil {
		t.Fatal("empty file must give no windows")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
