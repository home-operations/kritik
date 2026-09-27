package worker

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/tasks"
)

// contextForge serves an issue, its thread, files by path and a search.
type contextForge struct {
	forge.Client
	files    map[string]string
	found    []forge.Issue
	searched []string
}

func (f *contextForge) Issue(_ context.Context, _, _ string, number int) (forge.Issue, error) {
	return forge.Issue{Number: number, Title: "It crashes", State: "open"}, nil
}

func (f *contextForge) ListConversation(context.Context, string, string, int) ([]forge.Comment, error) {
	return []forge.Comment{{Author: "devin", Body: "trace"}}, nil
}

func (f *contextForge) FileAt(_ context.Context, _, _, ref, path string) ([]byte, error) {
	if path == "huge.md" {
		return nil, forge.ErrFileTooLarge
	}
	if s, ok := f.files[path]; ok && ref == "c0ffee" {
		return []byte(s), nil
	}
	return nil, fmt.Errorf("context forge: %s: %w", path, fs.ErrNotExist)
}

func (f *contextForge) SearchIssues(_ context.Context, _, _, query string, _ int) ([]forge.Issue, error) {
	f.searched = append(f.searched, query)
	return f.found, nil
}

func contextRunner(t *testing.T, doc string, f *contextForge) *taskRunner {
	t.Helper()
	ts := taskDefs(t, doc)
	p, err := tasks.Prepare(&ts[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	return &taskRunner{
		w: &Task{}, client: f, task: &ts[0], prepared: p, owner: "acme", name: "widgets",
		args: jobs.TaskArgs{ConfigSHA: "c0ffee"}, ev: store.TaskEvent{SubjectNumber: 7, Event: "issue", Action: "opened"},
		repo:   taskRepo{name: "acme/widgets", defaultBranch: "main"},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestPromptDataContext(t *testing.T) {
	f := &contextForge{
		files: map[string]string{"CONTRIBUTING.md": "Be kind.", "big.md": strings.Repeat("x", taskSourceBytes+10)},
		found: []forge.Issue{{Number: 7, Title: "self"}, {Number: 3, Title: "Crash on boot", State: "open", IsPull: true, URL: "u"}},
	}
	tests := []struct {
		name      string
		doc       string
		want      map[string]any
		wantNotes []string
		wantLeft  int
	}{
		{
			name: "files by path in single mode, globs noted",
			doc: `
- name: triage
  mode: single
  on: [{issue: []}]
  context:
    files: [{path: CONTRIBUTING.md}, {path: missing.md}, {path: huge.md}, {glob: "docs/*.md", max: 2}]
`,
			want: map[string]any{"files": []contextFile{{Path: "CONTRIBUTING.md", Content: "Be kind."}}},
			wantNotes: []string{
				"context files missing.md left out: not found", "context files huge.md left out: too large",
				"context files docs/*.md left out: a glob is gathered in agentic mode only",
			},
			wantLeft: taskContextBytes - len("Be kind."),
		},
		{
			name: "globs are the runner's in agentic mode",
			doc: `
- name: triage
  on: [{issue: []}]
  context: {files: [{glob: "docs/*.md"}]}
`,
			want:     map[string]any{},
			wantLeft: taskContextBytes,
		},
		{
			name: "a file over the source bound is cut",
			doc: `
- name: triage
  mode: single
  on: [{issue: []}]
  context: {files: [{path: big.md}]}
`,
			want:      map[string]any{"files": []contextFile{{Path: "big.md", Content: strings.Repeat("x", taskSourceBytes)}}},
			wantNotes: []string{fmt.Sprintf("context files big.md cut to %d bytes", taskSourceBytes)},
			wantLeft:  taskContextBytes - taskSourceBytes,
		},
		{
			name: "related issues without the subject, search noted without an index",
			doc: `
- name: triage
  mode: single
  on: [{issue: []}]
  context:
    related: [{name: dupes, query: "is:open {{ .Subject.Title }}", k: 5}]
    search: [{name: code, query: "{{ .Subject.Title }}"}]
`,
			want:      map[string]any{"dupes": []relatedIssue{{Number: 3, Title: "Crash on boot", State: "open", Pull: true, URL: "u"}}},
			wantNotes: []string{"context code left out: repository indexing is off"},
			wantLeft:  taskContextBytes - len(`[{"number":3,"title":"Crash on boot","state":"open","isPull":true,"url":"u"}]`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := contextRunner(t, tt.doc, f)
			d, err := r.promptData(t.Context())
			if err != nil {
				t.Fatalf("promptData: %v", err)
			}
			if !reflect.DeepEqual(d.Context, tt.want) {
				t.Fatalf("context = %#v\nwant %#v", d.Context, tt.want)
			}
			if !slices.Equal(r.notes, tt.wantNotes) || r.contextLeft != tt.wantLeft {
				t.Fatalf("notes = %q, left %d; want %q, %d", r.notes, r.contextLeft, tt.wantNotes, tt.wantLeft)
			}
		})
	}
	if !slices.Contains(f.searched, "is:open It crashes") {
		t.Fatalf("searched = %q", f.searched)
	}
}
