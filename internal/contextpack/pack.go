// Package contextpack builds the review context the runner writes next to
// the diff: the PR-head overlay (whole declarations the diff touches),
// exact definitions of identifiers on changed lines, and callers of changed
// declarations. Everything comes from the two trees the runner fetched;
// nothing needs an index or a model key.
package contextpack

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/home-operations/kritik/internal/chunk"
)

// Stages, in the order the prompt spends its budget on them.
const (
	StageOverlay    = "overlay"
	StageDefinition = "definition"
	StageCaller     = "caller"
	// StageSimilar is added by the worker from the embedding index.
	StageSimilar = "similar"
)

// Chunk is one piece of context and the stage that contributed it.
type Chunk struct {
	Stage     string `json:"stage"`
	Path      string `json:"path"`
	Language  string `json:"language,omitempty"`
	Symbol    string `json:"symbol,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Scope     string `json:"scope,omitempty"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	// Ref is the identifier or changed symbol that pulled the chunk in.
	Ref  string `json:"ref,omitempty"`
	Text string `json:"text"`
}

// Input is what Build reads.
type Input struct {
	Head, Base *object.Tree
	Diff       string
	// Changed lists head-side paths the diff touches.
	Changed []string
	// Ignore globs (doublestar) are skipped everywhere.
	Ignore []string
}

// Options bound the work and the output.
type Options struct {
	// MaxDeclLines is the largest declaration used whole; a bigger one
	// gets a window around the change instead.
	MaxDeclLines int
	// WindowRadius is the fixed-window fallback's reach on each side.
	WindowRadius int
	// MaxIdentifiers caps stage 2 lookups per review, most frequent first.
	MaxIdentifiers int
	// DefinitionsPerIdentifier and CallersPerSymbol cap each lookup;
	// MaxDefinitions and MaxCallers cap the stages.
	DefinitionsPerIdentifier, CallersPerSymbol int
	MaxDefinitions, MaxCallers                 int
	// MaxChunkBytes truncates one chunk; MaxPackBytes stops adding chunks.
	MaxChunkBytes, MaxPackBytes int
	// MaxScanFiles and MaxScanBytes bound the head-tree walk.
	MaxScanFiles, MaxScanBytes int
	// MaxFileBytes skips files larger than this everywhere.
	MaxFileBytes int
}

// DefaultOptions fit a 24k-token budget with room for the diff.
var DefaultOptions = Options{
	MaxDeclLines: 150, WindowRadius: 20, MaxIdentifiers: 40,
	DefinitionsPerIdentifier: 2, CallersPerSymbol: 5, MaxDefinitions: 30, MaxCallers: 30,
	MaxChunkBytes: 6000, MaxPackBytes: 300_000,
	MaxScanFiles: 20_000, MaxScanBytes: 200 << 20, MaxFileBytes: 1 << 20,
}

// Stats says what Build did, for the run log.
type Stats struct {
	Overlay, Definitions, Callers int
	Identifiers, ChangedSymbols   int
	FilesScanned, FilesParsed     int
	BytesScanned                  int
	ScanTruncated                 bool
	Elapsed                       time.Duration
}

// Build runs stages 1 to 3.
func Build(ctx context.Context, in Input, opts Options) ([]Chunk, Stats, error) {
	if opts.MaxDeclLines == 0 {
		opts = DefaultOptions
	}
	start := time.Now()
	b := &builder{in: in, opts: opts, parser: &chunk.Parser{Limits: chunk.Limits{MaxBytes: opts.MaxFileBytes}}}
	if err := b.overlay(ctx); err != nil {
		return nil, b.stats, err
	}
	if err := b.scan(ctx); err != nil {
		return nil, b.stats, err
	}
	out := b.assemble()
	b.stats.Elapsed = time.Since(start)
	return out, b.stats, nil
}

type builder struct {
	in     Input
	opts   Options
	parser *chunk.Parser
	stats  Stats

	overlayChunks []Chunk
	// overlayDecls keys "path\x00symbol" of declarations the diff changed,
	// so their own text is never offered as a caller or definition.
	overlayDecls map[string]bool
	// identifiers, most frequent first, and the changed symbols to find
	// callers for.
	identifiers    []string
	changedSymbols []string
	// hits are candidate chunks per word, in scan order.
	definitions map[string][]Chunk
	callers     map[string][]Chunk
}

func (b *builder) ignored(path string) bool { return chunk.Ignored(b.in.Ignore, path) }

func (b *builder) read(tree *object.Tree, path string) []byte {
	if tree == nil {
		return nil
	}
	f, err := tree.File(path)
	if err != nil || f.Size > int64(b.opts.MaxFileBytes) {
		return nil
	}
	if bin, err := f.IsBinary(); err != nil || bin {
		return nil
	}
	s, err := f.Contents()
	if err != nil {
		return nil
	}
	return []byte(s)
}

// overlay is stage 1, and it also collects what stages 2 and 3 look for.
func (b *builder) overlay(ctx context.Context) error {
	d := parseDiff(b.in.Diff)
	b.overlayDecls = map[string]bool{}
	idCount := map[string]int{}
	seenSymbol := map[string]bool{}
	for _, path := range b.in.Changed {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if b.ignored(path) {
			continue
		}
		src := b.read(b.in.Head, path)
		if src == nil {
			continue
		}
		f := b.parser.Parse(path, src)
		added := d.added[path]
		for _, id := range f.Identifiers(added) {
			idCount[id]++
		}
		if base := b.read(b.in.Base, path); base != nil {
			bf := b.parser.Parse(path, base)
			for _, id := range bf.Identifiers(d.removed[path]) {
				idCount[id]++
			}
		}
		covered := map[int]bool{}
		for _, run := range runs(added, b.opts.WindowRadius) {
			if covered[run[0]] && covered[run[1]] {
				continue
			}
			var c Chunk
			if decl := f.Enclosing([]int{run[0], run[1]}, b.opts.MaxDeclLines); decl != nil {
				c = Chunk{
					Stage: StageOverlay, Path: path, Language: f.Language, Symbol: decl.Symbol, Kind: decl.Kind, Scope: decl.Scope,
					StartLine: decl.StartLine, EndLine: decl.EndLine, Text: chunk.Text(src, decl.StartLine, decl.EndLine),
				}
			} else {
				s, e, text := chunk.Window(src, run[0], run[1], b.opts.WindowRadius)
				if text == "" {
					continue
				}
				c = Chunk{Stage: StageOverlay, Path: path, Language: f.Language, StartLine: s, EndLine: e, Text: text}
			}
			for l := c.StartLine; l <= c.EndLine; l++ {
				covered[l] = true
			}
			if c.Symbol != "" {
				b.overlayDecls[path+"\x00"+c.Symbol] = true
				if !seenSymbol[c.Symbol] {
					seenSymbol[c.Symbol] = true
					b.changedSymbols = append(b.changedSymbols, c.Symbol)
				}
			}
			// A declaration the diff already shows whole adds nothing.
			if subset(c.StartLine, c.EndLine, d.shown[path]) {
				continue
			}
			b.overlayChunks = append(b.overlayChunks, c)
		}
	}
	for id := range idCount {
		if len(id) >= 3 && !seenSymbol[id] {
			b.identifiers = append(b.identifiers, id)
		}
	}
	sort.Slice(b.identifiers, func(i, j int) bool {
		if idCount[b.identifiers[i]] != idCount[b.identifiers[j]] {
			return idCount[b.identifiers[i]] > idCount[b.identifiers[j]]
		}
		return b.identifiers[i] < b.identifiers[j]
	})
	if len(b.identifiers) > b.opts.MaxIdentifiers {
		b.identifiers = b.identifiers[:b.opts.MaxIdentifiers]
	}
	b.stats.Overlay = len(b.overlayChunks)
	b.stats.Identifiers = len(b.identifiers)
	b.stats.ChangedSymbols = len(b.changedSymbols)
	return nil
}

func subset(start, end int, shown map[int]bool) bool {
	for l := start; l <= end; l++ {
		if !shown[l] {
			return false
		}
	}
	return true
}

var word = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// scan is stages 2 and 3 in one walk of the head tree: every parseable
// file is searched for the wanted words, and only files with a hit are
// parsed to map hits to their enclosing declarations.
func (b *builder) scan(ctx context.Context) error {
	wanted := map[string]bool{}
	for _, id := range b.identifiers {
		wanted[id] = true
	}
	for _, s := range b.changedSymbols {
		wanted[s] = true
	}
	b.definitions, b.callers = map[string][]Chunk{}, map[string][]Chunk{}
	if len(wanted) == 0 || b.in.Head == nil {
		return nil
	}
	changedSet := map[string]bool{}
	for _, s := range b.changedSymbols {
		changedSet[s] = true
	}
	iter := b.in.Head.Files()
	defer iter.Close()
	return iter.ForEach(func(f *object.File) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if b.stats.FilesScanned >= b.opts.MaxScanFiles || b.stats.BytesScanned >= b.opts.MaxScanBytes {
			b.stats.ScanTruncated = true
			return errStop
		}
		if f.Size > int64(b.opts.MaxFileBytes) || b.ignored(f.Name) {
			return nil
		}
		if _, decls := chunk.Language(f.Name); !decls {
			return nil
		}
		src := b.read(b.in.Head, f.Name)
		if src == nil {
			return nil
		}
		b.stats.FilesScanned++
		b.stats.BytesScanned += len(src)
		type hit struct {
			word string
			line int
		}
		var hits []hit
		line := 1
		last := 0
		for _, loc := range word.FindAllIndex(src, -1) {
			w := string(src[loc[0]:loc[1]])
			if !wanted[w] {
				continue
			}
			line += strings.Count(string(src[last:loc[0]]), "\n")
			last = loc[0]
			hits = append(hits, hit{w, line})
		}
		if len(hits) == 0 {
			return nil
		}
		pf := b.parser.Parse(f.Name, src)
		b.stats.FilesParsed++
		if len(pf.Decls) == 0 {
			return nil
		}
		seenDecl := map[string]bool{}
		for _, h := range hits {
			decl := pf.Enclosing([]int{h.line}, b.opts.MaxDeclLines)
			if decl == nil {
				continue
			}
			key := h.word + "\x00" + f.Name + "\x00" + decl.Symbol
			if seenDecl[key] || b.overlayDecls[f.Name+"\x00"+decl.Symbol] {
				continue
			}
			seenDecl[key] = true
			c := Chunk{
				Path: f.Name, Language: pf.Language, Symbol: decl.Symbol, Kind: decl.Kind, Scope: decl.Scope,
				StartLine: decl.StartLine, EndLine: decl.EndLine, Ref: h.word, Text: chunk.Text(src, decl.StartLine, decl.EndLine),
			}
			switch {
			case decl.Symbol == h.word && !changedSet[h.word]:
				c.Stage = StageDefinition
				b.definitions[h.word] = append(b.definitions[h.word], c)
			case changedSet[h.word] && decl.Symbol != h.word:
				c.Stage = StageCaller
				b.callers[h.word] = append(b.callers[h.word], c)
			}
		}
		return nil
	})
}

// errStop ends the tree walk early without failing it.
var errStop = fmt.Errorf("contextpack: scan budget reached")

// assemble orders and caps the stages and truncates chunks to size.
func (b *builder) assemble() []Chunk {
	var out []Chunk
	total := 0
	add := func(c Chunk) bool {
		if len(c.Text) > b.opts.MaxChunkBytes {
			c.Text = c.Text[:b.opts.MaxChunkBytes] + "\n… (truncated)"
		}
		if total+len(c.Text) > b.opts.MaxPackBytes {
			return false
		}
		total += len(c.Text)
		out = append(out, c)
		return true
	}
	for _, c := range b.overlayChunks {
		if !add(c) {
			return out
		}
	}
	n := 0
	for _, id := range b.identifiers {
		for _, c := range pick(b.definitions[id], b.opts.DefinitionsPerIdentifier) {
			if n >= b.opts.MaxDefinitions {
				break
			}
			if !add(c) {
				return out
			}
			n++
		}
	}
	b.stats.Definitions = n
	n = 0
	for _, s := range b.changedSymbols {
		for _, c := range pick(b.callers[s], b.opts.CallersPerSymbol) {
			if n >= b.opts.MaxCallers {
				break
			}
			if !add(c) {
				return out
			}
			n++
		}
	}
	b.stats.Callers = n
	return out
}

// pick keeps up to n chunks, preferring one per file before a second from
// the same file.
func pick(cs []Chunk, n int) []Chunk {
	if len(cs) <= n {
		return cs
	}
	var out []Chunk
	seen := map[string]bool{}
	for _, c := range cs {
		if !seen[c.Path] {
			seen[c.Path] = true
			out = append(out, c)
			if len(out) == n {
				return out
			}
		}
	}
	for _, c := range cs {
		if len(out) == n {
			break
		}
		if !containsChunk(out, c) {
			out = append(out, c)
		}
	}
	return out
}

func containsChunk(cs []Chunk, c Chunk) bool {
	for _, x := range cs {
		if x.Path == c.Path && x.StartLine == c.StartLine {
			return true
		}
	}
	return false
}
