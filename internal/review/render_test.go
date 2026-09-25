package review

import (
	"context"
	"strings"
	"testing"
	"time"
)

func sampleData() RenderData {
	res := Result{
		Summary: Summary{Take: "Solid change with one real bug.", Praise: []string{"Clear tests"}},
		Findings: []Finding{
			{Path: "main.go", Line: 11, Severity: SeverityBlocking, Title: "nil map write", Explanation: "m is nil here.", SuggestedFix: "m = map[string]int{}"},
			{Path: "README.md", Line: 2, Severity: SeverityNit, Title: "typo", Explanation: "the the"},
		},
	}
	return RenderData{Number: 42, HeadSHA: "0123456789abcdef", Model: "vendor/model-x", Result: res, Counts: res.Counts(),
		Notes: []string{"1 file(s) were omitted from the diff to fit the context budget"}}
}

func TestRenderSummaryDefault(t *testing.T) {
	body, notes := RenderSummary(t.Context(), Templates{}, sampleData())
	if len(notes) != 0 {
		t.Fatalf("notes = %v", notes)
	}
	first, _, _ := strings.Cut(body, "\n")
	if first != Marker(42) {
		t.Fatalf("first line = %q", first)
	}
	for _, want := range []string{
		"### kritik review",
		"- **Blocking:** 1", "- **Important:** 0", "- **Nit:** 1",
		"Solid change with one real bug.",
		"- Clear tests",
		"- **[blocking]** `main.go:11` nil map write",
		"- **[nit]** `README.md:2` typo",
		"_1 file(s) were omitted from the diff to fit the context budget._",
		"Reviewed `0123456` by kritik with vendor/model-x. Reviews never block a merge.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Index(body, "Blocking:") > strings.Index(body, "Solid change") || strings.Index(body, "Solid change") > strings.Index(body, "`main.go:11`") {
		t.Fatalf("sections out of order:\n%s", body)
	}

	empty := sampleData()
	empty.Result.Findings, empty.Counts, empty.Notes, empty.Result.Summary.Praise = nil, Counts{}, nil, nil
	empty.Incremental, empty.PriorHeadSHA = true, "fedcba9876543210"
	body, _ = RenderSummary(t.Context(), Templates{}, empty)
	if !strings.Contains(body, "Nothing worth flagging") || strings.Contains(body, "_1 file") || !strings.Contains(body, "`fedcba9`") {
		t.Fatalf("empty body:\n%s", body)
	}
}

func TestRenderSummaryCustom(t *testing.T) {
	longLoop := `{% for a in range(10000) %}{% for b in range(10000) %}{% if a %}{% endif %}{% endfor %}{% endfor %}`
	tests := []struct {
		name     string
		template string
		want     []string
		note     string // substring of the fallback note; empty means the template is used
	}{
		{
			name:     "fields render",
			template: "#{{ number }} {{ head_sha }} {{ model }} {{ summary.take }} {{ summary.praise|join(',') }} {{ counts.blocking }}/{{ counts.important }}/{{ counts.nit }} {{ incremental }}\n{% for f in findings %}{{ f.severity }} {{ f.path }}:{{ f.line }} {{ f.title }} {{ f.explanation }} {{ f.suggested_fix }};{% endfor %}{{ notes|length }}",
			want:     []string{"#42 0123456789abcdef vendor/model-x Solid change with one real bug. Clear tests 1/0/1 False", "blocking main.go:11 nil map write m is nil here. m = map[string]int{};", "nit README.md:2 typo the the ;1"},
		},
		{name: "include resolves nothing", template: `{% include "secrets.txt" %}`, note: "summary template"},
		{name: "import resolves nothing", template: `{% import "x.j2" as x %}`, note: "summary template"},
		{name: "extends resolves nothing", template: `{% extends "/etc/passwd" %}`, note: "summary template"},
		{name: "syntax error", template: `{% for %}`, note: "summary template"},
		{name: "recursive macro", template: `{% macro f(n) %}{{ f(n) }}{% endmacro %}{{ f(1) }}`, note: "summary template"},
		{name: "recursive loop", template: `{% for x in [1] recursive %}{{ loop([1]) }}{% endfor %}`, note: "summary template"},
		{name: "string repetition", template: `{{ "x" * 1000000000000 }}`, note: "summary template"},
		{name: "oversized filter width", template: `{{ "x"|center(1000000000000) }}`, note: "summary template"},
		{name: "oversized output", template: `{% for i in range(10000) %}{{ "0123456789" }}{% endfor %}`, note: "64 KiB"},
		{name: "deadline", template: longLoop, note: "time"},
		{name: "mutating a list", template: `{% set l = [] %}{{ l.append(1) }}`, note: "summary template"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.name == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
			}
			body, notes := RenderSummary(ctx, Templates{Summary: tt.template}, sampleData())
			if !strings.HasPrefix(body, Marker(42)+"\n") {
				t.Fatalf("marker missing:\n%s", body)
			}
			if tt.note == "" {
				if len(notes) != 0 {
					t.Fatalf("notes = %v", notes)
				}
				for _, w := range tt.want {
					if !strings.Contains(body, w) {
						t.Fatalf("missing %q in:\n%s", w, body)
					}
				}
				return
			}
			if len(notes) != 1 || !strings.Contains(notes[0], tt.note) {
				t.Fatalf("notes = %v, want one containing %q", notes, tt.note)
			}
			if !strings.Contains(body, "### kritik review") || !strings.Contains(body, notes[0]) {
				t.Fatalf("fallback body should be the default and carry the note:\n%s", body)
			}
			if len(body) > MaxRenderBytes {
				t.Fatalf("body is %d bytes", len(body))
			}
		})
	}
}

func TestRenderSummaryMarkerCannotBeRemoved(t *testing.T) {
	body, notes := RenderSummary(t.Context(), Templates{Summary: "{# nothing #}"}, sampleData())
	if len(notes) != 0 || body != Marker(42)+"\n" {
		t.Fatalf("body = %q, notes = %v", body, notes)
	}
}

func TestRenderInline(t *testing.T) {
	f := sampleData().Result.Findings[0]
	body, notes := RenderInline(t.Context(), Templates{}, f)
	if len(notes) != 0 {
		t.Fatalf("notes = %v", notes)
	}
	for _, want := range []string{"**[blocking]** **nil map write**", "m is nil here.", "m = map[string]int{}"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	noFix := f
	noFix.SuggestedFix = ""
	if body, _ := RenderInline(t.Context(), Templates{}, noFix); strings.Contains(body, "Suggested fix") {
		t.Fatalf("no fix should render no fix section:\n%s", body)
	}

	body, notes = RenderInline(t.Context(), Templates{Inline: "{{ severity }}|{{ path }}:{{ line }}|{{ title }}|{{ explanation }}|{{ suggested_fix }}"}, f)
	if len(notes) != 0 || body != "blocking|main.go:11|nil map write|m is nil here.|m = map[string]int{}" {
		t.Fatalf("custom inline = %q, notes %v", body, notes)
	}
	body, notes = RenderInline(t.Context(), Templates{Inline: "{% include 'x' %}"}, f)
	if len(notes) != 1 || !strings.Contains(body, "**nil map write**") {
		t.Fatalf("fallback inline = %q, notes %v", body, notes)
	}
}
