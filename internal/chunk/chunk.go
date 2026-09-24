// Package chunk cuts source files into declarations. It is the one place
// that speaks tree-sitter: the runner uses it for the PR-head overlay,
// definitions and callers, and the indexer will use it for the embedding
// index, so both see the same chunks.
//
// Parsing is pure Go through gotreesitter with every grammar embedded, so
// any file an operator points the service at parses without a rebuild. A
// language whose grammar carries no tags query (configuration formats,
// mostly) has no declarations and falls back to fixed windows.
package chunk

import (
	"bytes"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// Decl is one declaration in a file: a function, method, type, constant,
// or whatever the language calls a top-level thing. Lines are 1-based and
// inclusive.
type Decl struct {
	Symbol    string
	Kind      string
	Scope     string
	StartLine int
	EndLine   int
	StartByte int
	EndByte   int
	// HasError is set when the parser found syntax errors inside the
	// declaration; the text is still usable, the structure less so.
	HasError bool
}

// Lines is how many lines the declaration spans.
func (d Decl) Lines() int { return d.EndLine - d.StartLine + 1 }

// Contains reports whether line falls inside the declaration.
func (d Decl) Contains(line int) bool { return line >= d.StartLine && line <= d.EndLine }

// Limits bound one parse. Files over MaxBytes are not parsed at all;
// ParseTimeout stops a pathological parse.
type Limits struct {
	MaxBytes       int
	ParseTimeoutUs int
}

// DefaultLimits are used when a zero Limits is given.
var DefaultLimits = Limits{MaxBytes: 1 << 20, ParseTimeoutUs: 5_000_000}

// Parser cuts files. It caches one tree-sitter parser per language and
// serialises access to each, since parsers are not safe for concurrent use.
type Parser struct {
	Limits Limits

	mu      sync.Mutex
	parsers map[string]*languageParser
}

type languageParser struct {
	entry    grammars.LangEntry
	lang     *gotreesitter.Language
	parser   *gotreesitter.Parser
	outliner *gotreesitter.Outliner
}

// Language names the grammar a path would parse with, and whether that
// grammar can outline declarations. Detection is by extension and
// filename, the same table the indexer uses.
func Language(path string) (name string, declarations bool) {
	entry := grammars.DetectLanguage(path)
	if entry == nil {
		return "", false
	}
	return entry.Name, strings.TrimSpace(grammars.ResolveTagsQuery(*entry)) != ""
}

// File is a parsed file.
type File struct {
	Path     string
	Language string
	Source   []byte
	Decls    []Decl

	lang *gotreesitter.Language
	tree *gotreesitter.Tree
}

// Parse cuts src into declarations. A path with no grammar, a grammar with
// no tags query, a file over the byte limit, or a failed parse all return
// a File with no declarations and a nil error: those are the fixed-window
// cases, not failures.
func (p *Parser) Parse(path string, src []byte) *File {
	f := &File{Path: path, Source: src}
	lim := p.Limits
	if lim.MaxBytes == 0 {
		lim = DefaultLimits
	}
	if len(src) == 0 || len(src) > lim.MaxBytes || bytes.IndexByte(src, 0) >= 0 {
		return f
	}
	lp := p.language(path)
	if lp == nil {
		return f
	}
	f.Language = lp.entry.Name
	if lp.outliner == nil {
		return f
	}
	p.mu.Lock()
	tree, err := lp.parser.ParseStrict(src)
	p.mu.Unlock()
	if err != nil || tree == nil || tree.RootNode() == nil {
		return f
	}
	f.lang, f.tree = lp.lang, tree
	f.Decls = declarations(lp, tree, src)
	return f
}

func (p *Parser) language(path string) *languageParser {
	entry := grammars.DetectLanguage(path)
	if entry == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if lp, ok := p.parsers[entry.Name]; ok {
		return lp
	}
	lim := p.Limits
	if lim.ParseTimeoutUs == 0 {
		lim = DefaultLimits
	}
	lang := entry.Language()
	parser := gotreesitter.NewParser(lang)
	parser.SetTimeoutMicros(uint64(lim.ParseTimeoutUs))
	lp := &languageParser{entry: *entry, lang: lang, parser: parser}
	if q := strings.TrimSpace(grammars.ResolveTagsQuery(*entry)); q != "" {
		// A tags query that fails to compile leaves the language without
		// declarations rather than without parsing.
		lp.outliner, _ = gotreesitter.NewOutliner(lang, q)
	}
	if p.parsers == nil {
		p.parsers = map[string]*languageParser{}
	}
	p.parsers[entry.Name] = lp
	return lp
}

// declarations merges two views of the tree: the outline (named symbols
// from the grammar's tags query, nested where the language nests them) and
// the root's named children (every top-level statement, so a type or
// constant block the tags query does not cover still becomes a chunk).
func declarations(lp *languageParser, tree *gotreesitter.Tree, src []byte) []Decl {
	root := tree.RootNode()
	var out []Decl
	seen := map[[2]int]bool{}
	add := func(d Decl) {
		key := [2]int{d.StartByte, d.EndByte}
		if seen[key] || d.EndByte <= d.StartByte {
			return
		}
		seen[key] = true
		out = append(out, d)
	}
	symbols, _ := lp.outliner.OutlineTree(tree)
	var walk func(ss []gotreesitter.OutlineSymbol, owner string)
	walk = func(ss []gotreesitter.OutlineSymbol, owner string) {
		for _, s := range ss {
			scope := s.Owner
			if scope == "" {
				scope = owner
			}
			add(Decl{
				Symbol: s.Name, Kind: s.Kind, Scope: scope,
				StartLine: int(s.Range.StartPoint.Row) + 1, EndLine: int(s.Range.EndPoint.Row) + 1,
				StartByte: int(s.Range.StartByte), EndByte: int(s.Range.EndByte),
				HasError: nodeAt(root, s.Range.StartByte, s.Range.EndByte).HasError(),
			})
			walk(s.Children, s.Name)
		}
	}
	walk(symbols, "")
	for i := range root.NamedChildCount() {
		c := root.NamedChild(i)
		typ := c.Type(lp.lang)
		if strings.Contains(typ, "comment") || c.EndPoint().Row == c.StartPoint().Row && c.EndByte()-c.StartByte() < 2 {
			continue
		}
		d := Decl{
			Kind:      kindFromType(typ),
			StartLine: int(c.StartPoint().Row) + 1, EndLine: int(c.EndPoint().Row) + 1,
			StartByte: int(c.StartByte()), EndByte: int(c.EndByte()),
			HasError: c.HasError(),
		}
		d.Symbol, d.Scope = nameOf(c, lp.lang, src)
		add(d)
	}
	// Methods carry their receiver as scope when the outline gave none.
	for i := range out {
		if out[i].Scope == "" && out[i].Kind == "method" {
			if n := nodeAt(root, uint32(out[i].StartByte), uint32(out[i].EndByte)); n != nil {
				_, out[i].Scope = nameOf(n, lp.lang, src)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartByte != out[j].StartByte {
			return out[i].StartByte < out[j].StartByte
		}
		return out[i].EndByte > out[j].EndByte
	})
	return out
}

// nodeAt finds the node with exactly the given byte range, descending
// from root; nil when no node has it.
func nodeAt(root *gotreesitter.Node, start, end uint32) *gotreesitter.Node {
	n := root
	for n != nil {
		if n.StartByte() == start && n.EndByte() == end {
			return n
		}
		var next *gotreesitter.Node
		for i := range n.NamedChildCount() {
			c := n.NamedChild(i)
			if c.StartByte() <= start && c.EndByte() >= end {
				next = c
				break
			}
		}
		n = next
	}
	return nil
}

// nameOf returns a node's declared name and scope from the grammar's
// "name" and "receiver" fields, falling back to the first identifier
// inside a one-declaration block (Go's type and const groups).
func nameOf(n *gotreesitter.Node, lang *gotreesitter.Language, src []byte) (name, scope string) {
	if nn := n.ChildByFieldName("name", lang); nn != nil {
		name = strings.TrimSpace(nn.Text(src))
	}
	if r := n.ChildByFieldName("receiver", lang); r != nil {
		text := strings.Trim(strings.TrimSpace(r.Text(src)), "()")
		// "(w *Review)" -> "Review"; "(Review)" -> "Review"
		fields := strings.Fields(text)
		if len(fields) > 0 {
			scope = strings.TrimLeft(fields[len(fields)-1], "*&")
		}
	}
	if name == "" {
		// A block of specs (Go's const and type groups) takes its first
		// member's name; a bare identifier child is the name itself.
		for i := range n.NamedChildCount() {
			c := n.NamedChild(i)
			if strings.Contains(c.Type(lang), "identifier") && c.ChildCount() == 0 {
				name = c.Text(src)
			} else if strings.Contains(c.Type(lang), "spec") || strings.Contains(c.Type(lang), "declarator") {
				name, _ = nameOf(c, lang, src)
			}
			if name != "" {
				break
			}
		}
	}
	return name, scope
}

func kindFromType(typ string) string {
	switch {
	case strings.Contains(typ, "method"):
		return "method"
	case strings.Contains(typ, "function"), strings.Contains(typ, "func"):
		return "function"
	case strings.Contains(typ, "type"), strings.Contains(typ, "struct"), strings.Contains(typ, "enum"),
		strings.Contains(typ, "interface"), strings.Contains(typ, "class"):
		return "type"
	case strings.Contains(typ, "const"):
		return "constant"
	case strings.Contains(typ, "var"), strings.Contains(typ, "let"):
		return "variable"
	case strings.Contains(typ, "import"), strings.Contains(typ, "use_"), strings.Contains(typ, "package"):
		return "import"
	default:
		return "other"
	}
}

// Enclosing returns the smallest declaration that contains every line in
// lines and spans at most maxLines, or nil. Declarations are nested, so
// "smallest" prefers a method over its class.
func (f *File) Enclosing(lines []int, maxLines int) *Decl {
	var best *Decl
	for i := range f.Decls {
		d := &f.Decls[i]
		if d.Lines() > maxLines {
			continue
		}
		ok := true
		for _, l := range lines {
			if !d.Contains(l) {
				ok = false
				break
			}
		}
		if ok && (best == nil || d.Lines() < best.Lines()) {
			best = d
		}
	}
	return best
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Identifiers returns the identifiers that appear on the given lines, most
// frequent first. Only named leaf nodes whose type mentions "identifier"
// count, which excludes keywords (anonymous), strings, numbers and
// comments in every grammar that follows tree-sitter conventions.
func (f *File) Identifiers(lines []int) []string {
	if f.tree == nil || len(lines) == 0 {
		return nil
	}
	want := map[int]bool{}
	for _, l := range lines {
		want[l] = true
	}
	counts := map[string]int{}
	var walk func(n *gotreesitter.Node)
	walk = func(n *gotreesitter.Node) {
		row := int(n.StartPoint().Row) + 1
		if int(n.EndPoint().Row)+1 < minKey(want) || row > maxKey(want) {
			return
		}
		if n.ChildCount() == 0 {
			if n.IsNamed() && want[row] && strings.Contains(n.Type(f.lang), "identifier") {
				if t := n.Text(f.Source); identifier.MatchString(t) {
					counts[t]++
				}
			}
			return
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(f.tree.RootNode())
	out := make([]string, 0, len(counts))
	for id := range counts {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

func minKey(m map[int]bool) int {
	min := 0
	for k := range m {
		if min == 0 || k < min {
			min = k
		}
	}
	return min
}

func maxKey(m map[int]bool) int {
	max := 0
	for k := range m {
		if k > max {
			max = k
		}
	}
	return max
}

// Window returns the fixed-size fallback: the lines from first-radius to
// last+radius, clamped to the file. Lines are 1-based, inclusive.
func Window(src []byte, first, last, radius int) (startLine, endLine int, text string) {
	lines := strings.Split(string(src), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	startLine = max(first-radius, 1)
	endLine = min(last+radius, len(lines))
	if startLine > endLine {
		return 0, 0, ""
	}
	return startLine, endLine, strings.Join(lines[startLine-1:endLine], "\n")
}

// Text returns the source of a line range, 1-based and inclusive.
func Text(src []byte, startLine, endLine int) string {
	_, _, text := Window(src, startLine, endLine, 0)
	return text
}

// Windows cuts a file with no declarations into fixed-size pieces for the
// index: size lines each, overlapping by overlap lines so a boundary does
// not hide a match. Lines are 1-based, inclusive.
func Windows(src []byte, size, overlap int) []Decl {
	lines := strings.Count(string(src), "\n")
	if len(src) > 0 && src[len(src)-1] != '\n' {
		lines++
	}
	if lines == 0 || size <= 0 {
		return nil
	}
	if overlap >= size {
		overlap = size / 4
	}
	var out []Decl
	for start := 1; start <= lines; start += size - overlap {
		end := min(start+size-1, lines)
		out = append(out, Decl{Kind: "window", StartLine: start, EndLine: end})
		if end == lines {
			break
		}
	}
	return out
}
