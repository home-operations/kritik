// Package indexer cuts a repository tree into the chunks the embedding
// index stores. It runs in the runner pod, which holds the fetched trees
// and no model key; the worker embeds what it stages.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/home-operations/kritik/internal/chunk"
)

// Chunk is one staged piece of a file.
type Chunk struct {
	Path      string
	Language  string
	Symbol    string
	Kind      string
	Scope     string
	StartLine int
	EndLine   int
	Text      string
}

// Options bound the work.
type Options struct {
	// MaxDeclLines is the largest declaration stored whole; larger ones
	// are represented by their nested declarations, or windowed.
	MaxDeclLines int
	// WindowLines and WindowOverlap cut files that have no declarations.
	WindowLines, WindowOverlap int
	// MaxChunkBytes truncates one chunk's text.
	MaxChunkBytes int
	// MaxFileBytes skips larger files; MaxFiles, MaxBytes and MaxChunks
	// stop the walk.
	MaxFileBytes, MaxFiles, MaxBytes, MaxChunks int
}

// DefaultOptions suit code and configuration repositories alike.
var DefaultOptions = Options{
	MaxDeclLines: 150, WindowLines: 60, WindowOverlap: 10, MaxChunkBytes: 6000,
	MaxFileBytes: 1 << 20, MaxFiles: 50_000, MaxBytes: 500 << 20, MaxChunks: 100_000,
}

// Stats says what Build did.
type Stats struct {
	Files, Parsed, Skipped, Chunks int
	Bytes                          int
	Truncated                      bool
	Elapsed                        time.Duration
}

// Build chunks head. With base set, only the paths that differ between
// base and head are chunked, and Changed lists every differing path, the
// deleted ones included, so the worker can drop their old chunks. With
// base nil the whole tree is chunked and Changed is nil.
func Build(
	ctx context.Context, head, base *object.Tree, ignore []string, opts Options,
) (chunks []Chunk, changed []string, stats Stats, err error) {
	if opts.MaxDeclLines == 0 {
		opts = DefaultOptions
	}
	start := time.Now()
	b := &builder{opts: opts, ignore: ignore, parser: &chunk.Parser{Limits: chunk.Limits{MaxBytes: opts.MaxFileBytes}}}
	if base == nil {
		err = b.walkAll(ctx, head)
	} else {
		changed, err = b.walkChanged(ctx, head, base)
	}
	b.stats.Elapsed = time.Since(start)
	if err != nil && !errors.Is(err, errStop) {
		return nil, nil, b.stats, err
	}
	return b.chunks, changed, b.stats, nil
}

var errStop = errors.New("indexer: budget reached")

type builder struct {
	opts   Options
	ignore []string
	parser *chunk.Parser
	chunks []Chunk
	stats  Stats
}

func (b *builder) ignored(path string) bool {
	for _, g := range b.ignore {
		if ok, _ := doublestar.Match(g, path); ok {
			return true
		}
	}
	return false
}

func (b *builder) walkAll(ctx context.Context, head *object.Tree) error {
	iter := head.Files()
	defer iter.Close()
	return iter.ForEach(func(f *object.File) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return b.file(f)
	})
}

func (b *builder) walkChanged(ctx context.Context, head, base *object.Tree) ([]string, error) {
	changes, err := object.DiffTreeWithOptions(ctx, base, head, object.DefaultDiffTreeOptions)
	if err != nil {
		return nil, fmt.Errorf("indexer: diff trees: %w", err)
	}
	var changed []string
	seen := map[string]bool{}
	note := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			changed = append(changed, p)
		}
	}
	for _, c := range changes {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		note(c.From.Name)
		note(c.To.Name)
		if c.To.Name == "" {
			continue
		}
		f, err := head.File(c.To.Name)
		if err != nil {
			continue
		}
		if err := b.file(f); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

func (b *builder) file(f *object.File) error {
	if b.stats.Files >= b.opts.MaxFiles || b.stats.Bytes >= b.opts.MaxBytes || len(b.chunks) >= b.opts.MaxChunks {
		b.stats.Truncated = true
		return errStop
	}
	if f.Size > int64(b.opts.MaxFileBytes) || b.ignored(f.Name) {
		b.stats.Skipped++
		return nil
	}
	lang, _ := chunk.Language(f.Name)
	if lang == "" {
		b.stats.Skipped++
		return nil
	}
	if bin, err := f.IsBinary(); err != nil || bin {
		b.stats.Skipped++
		return nil
	}
	content, err := f.Contents()
	if err != nil || content == "" {
		b.stats.Skipped++
		return nil
	}
	src := []byte(content)
	b.stats.Files++
	b.stats.Bytes += len(src)
	pf := b.parser.Parse(f.Name, src)
	if len(pf.Decls) > 0 {
		b.stats.Parsed++
	}
	for _, d := range pieces(pf, b.opts) {
		text := chunk.Text(src, d.StartLine, d.EndLine)
		if len(text) > b.opts.MaxChunkBytes {
			text = text[:b.opts.MaxChunkBytes]
		}
		if text == "" {
			continue
		}
		b.chunks = append(b.chunks, Chunk{
			Path: f.Name, Language: pf.Language, Symbol: d.Symbol, Kind: d.Kind, Scope: d.Scope,
			StartLine: d.StartLine, EndLine: d.EndLine, Text: text,
		})
		b.stats.Chunks++
	}
	return nil
}

// pieces picks the declarations to store: the outermost ones that fit
// MaxDeclLines, descending into an oversized declaration's nested ones. A
// file with no declarations is cut into windows.
func pieces(f *chunk.File, opts Options) []chunk.Decl {
	if len(f.Decls) == 0 {
		return chunk.Windows(f.Source, opts.WindowLines, opts.WindowOverlap)
	}
	var out []chunk.Decl
	var lastEnd int
	for _, d := range f.Decls {
		if d.StartLine <= lastEnd {
			continue // nested inside a declaration already taken
		}
		if d.Lines() <= opts.MaxDeclLines {
			out = append(out, d)
			lastEnd = d.EndLine
			continue
		}
		// Too big: take its nested declarations that fit; windows for
		// the rest would mostly repeat them, so the remainder is dropped.
		nested := false
		for _, n := range f.Decls {
			if n.StartLine > d.StartLine && n.EndLine <= d.EndLine && n.Lines() <= opts.MaxDeclLines && n.StartLine > lastEnd {
				out = append(out, n)
				lastEnd = n.EndLine
				nested = true
			}
		}
		if !nested {
			for _, w := range chunk.Windows([]byte(chunk.Text(f.Source, d.StartLine, d.EndLine)), opts.WindowLines, opts.WindowOverlap) {
				out = append(out, chunk.Decl{
					Symbol: d.Symbol, Kind: d.Kind, Scope: d.Scope,
					StartLine: d.StartLine + w.StartLine - 1, EndLine: d.StartLine + w.EndLine - 1,
				})
			}
			lastEnd = d.EndLine
		}
	}
	return out
}
