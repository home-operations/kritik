package review

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/exec"
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
		{name: "deadline", template: "{{ number }}", note: "time"},
		{name: "loop iteration budget", template: `{% for a in range(10000) %}{% for b in range(10000) %}{% endfor %}{% endfor %}`, note: "summary template"},
		{name: "numbers still add", template: `{% for f in findings %}{{ loop.index0 + 1 }}{% endfor %}{% set n = counts.blocking + counts.nit %}={{ n }}`,
			want: []string{"12=2"}},
		{name: "string +", template: `{{ summary.take + "x" }}`, note: "summary template"},
		{name: "list +", template: `{{ findings + findings }}`, note: "summary template"},
		{name: "concatenation ~", template: `{{ model ~ model }}`, note: "summary template"},
		{name: "block set", template: `{% set x %}a{% endset %}{{ x }}`, note: "summary template"},
		{name: "filter block", template: `{% filter upper %}a{% endfilter %}`, note: "summary template"},
		{name: "call block", template: `{% call f() %}a{% endcall %}`, note: "summary template"},
		{name: "with block", template: `{% with a = 1 %}{{ a }}{% endwith %}`, note: "summary template"},
		{name: "reserved names", template: `{{ __kritik_step() }}`, note: "summary template"},
		{name: "large literal", template: "{{ [" + strings.Repeat("1,", 300) + "1] }}", note: "summary template"},
		{name: "mutating a list", template: `{% set l = [] %}{{ l.append(1) }}`, note: "summary template"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.name == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 0)
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

// TestRenderMemoryIsBounded renders templates that try to build large
// values in gonja's private buffers or by repeated growth. Each must fall
// back quickly, and the render must allocate little on the way.
func TestRenderMemoryIsBounded(t *testing.T) {
	const maxAlloc = 64 << 20
	s60k := `{% set s = "a"|center(60000) %}`
	tests := map[string]string{
		"block set of a loop":               s60k + `{% set x %}{% for i in range(10000) %}{{ s }}{% endfor %}{% endset %}`,
		"block set of a nested loop":        s60k + `{% set x %}{% for i in range(10000) %}{% for j in range(10000) %}{{ s }}{% endfor %}{% endfor %}{% endset %}`,
		"chained concatenation":             `{% set a = "aaaaaaaaaaaaaaaa" %}` + strings.Repeat(`{% set a = a ~ a %}`, 12) + `{{ a|length }}`,
		"chained addition":                  `{% set a = "aaaaaaaaaaaaaaaa" %}` + strings.Repeat(`{% set a = a + a %}`, 12) + `{{ a|length }}`,
		"nested loops over a string":        s60k + `{% for c in s %}{% for d in s %}{% endfor %}{% endfor %}`,
		"chained growth by filter":          `{% set b = "a"|center(40000) %}` + strings.Repeat(`{% set b = b|replace(" ", "  ") %}`, 12),
		"join of repeated value":            s60k + "{{ [" + strings.Repeat("s,", 200) + "s]|join }}",
		"slice with a huge count":           `{{ ([1]|slice(8300000, fill_with=1))|length }}`,
		"batch with a huge count":           `{{ ([1]|batch(8300000, fill_with=1))|length }}`,
		"tojson with a huge indent":         `{{ findings|tojson(indent=5500000) }}`,
		"indent with a huge width":          `{{ summary.take|indent(60000) }}`,
		"map of an amplifier":               `{{ range(10000)|map("center", 60000)|list|length }}`,
		"join method with a long separator": s60k + `{{ s.join(range(10000)|map("string")|list) }}`,
		"wordwrap of many short words":      `{% set s = "x"|center(30000)|replace(" ", "a ") %}{{ s|wordwrap(65536)|length }}`,
		"wordwrap within its width":         `{% set s = "x"|center(30000)|replace(" ", "a ") %}{{ s|wordwrap(1000)|length }}`,
		"tojson of long model text":         `{{ findings|tojson(indent=16) }}{{ findings|tojson(indent=16) }}`,
	}
	longData := sampleData()
	longData.Result.Findings[0].Explanation = strings.Repeat("model text ", 3000)
	for name, src := range tests {
		t.Run(name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			started := time.Now()
			body, notes := RenderSummary(t.Context(), Templates{Summary: src}, longData)
			elapsed := time.Since(started)
			runtime.ReadMemStats(&after)
			if len(notes) != 1 || !strings.Contains(body, notes[0]) {
				t.Fatalf("want a fallback with a note, got notes %v", notes)
			}
			if alloc := after.TotalAlloc - before.TotalAlloc; alloc > maxAlloc {
				t.Fatalf("render allocated %d MiB", alloc>>20)
			}
			if elapsed > time.Second {
				t.Fatalf("render took %s", elapsed)
			}
		})
	}
}

func TestMeasureRecursesIntoData(t *testing.T) {
	d := sampleData()
	d.Result.Findings[0].Explanation = strings.Repeat("x", 50_000)
	m := measureValue(exec.AsValue(summaryContext(d)[keyFindings]))
	if m.bytes < 50_000 || m.nodes < 14 {
		t.Fatalf("measure = %+v, want the nested explanation counted", m)
	}
}

func TestLoopsAndAdditionsAreAllGuarded(t *testing.T) {
	tests := []struct {
		name, src, want string
		fails           bool
	}{
		{name: "loops and additions render", src: `{% for i in range(3) %}{% for j in range(2) %}{{ i + j }}{% endfor %}{% endfor %}{{ 1 + 2 }}`, want: "0112233"},
		{name: "an addition the rewrite cannot see fails", src: `{{ +1 }}`, fails: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := execute(t.Context(), tt.src, map[string]any{}, MaxRenderBytes)
			if tt.fails {
				if err == nil {
					t.Fatalf("rendered %q, want an error", out)
				}
				return
			}
			if err != nil || out != tt.want {
				t.Fatalf("out = %q, err = %v", out, err)
			}
		})
	}
	counts, err := checkTokens(`{% for a in b %}{{ a + 1 + 2 }}{% endfor %}{% raw %}{% for x in y %}{{ 1 + 1 }}{% endraw %}{{ "for + x" }}`, config.New())
	if err != nil || counts != (guardedCounts{loops: 1, additions: 2}) {
		t.Fatalf("counts = %+v, err = %v", counts, err)
	}
}

// TestLoopStopsAtTheDeadline runs a loop that is within every budget but
// slow, under a deadline that expires part way through: the render itself,
// not only the caller, must stop.
func TestLoopStopsAtTheDeadline(t *testing.T) {
	src := `{% set s = "a"|center(30000) %}{% for i in range(10000) %}{% set t = s|replace(" ", "  ") %}{% endfor %}done`
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	out, err := execute(ctx, src, map[string]any{}, MaxRenderBytes)
	elapsed := time.Since(started)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("out = %q, err = %v after %s; want the deadline to stop the loop", out, err, elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("the loop ran %s past a 30ms deadline", elapsed)
	}

	g := &guard{ctx: ctx}
	if v := g.step(nil); !v.IsError() {
		t.Fatal("step must fail once the deadline has passed")
	}
}

// TestCallLimitsMatchTheDocs pins the documented per-call limits: the
// largest documented count or width is accepted, one more is refused.
func TestCallLimitsMatchTheDocs(t *testing.T) {
	tests := []struct {
		src  string
		want string
		fail bool
	}{
		{src: `{{ ([1]|slice(10000))|length }}`, want: "10000"},
		{src: `{{ ([1]|slice(10001))|length }}`, fail: true},
		{src: `{{ ([1]|batch(10000, fill_with=1))|first|length }}`, want: "10000"},
		{src: `{{ ([1]|batch(10001, fill_with=1))|length }}`, fail: true},
		{src: `{{ "a b c"|wordwrap(1000) }}`, want: "a b c"},
		{src: `{{ "a b c"|wordwrap(1001) }}`, fail: true},
		{src: `{{ "a"|indent(16) }}`, want: "a"},
		{src: `{{ "a"|indent(17) }}`, fail: true},
		{src: `{{ "a"|center(65536)|length }}`, want: "65536"},
		{src: `{{ "a"|center(65537)|length }}`, fail: true},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			out, err := execute(t.Context(), tt.src, map[string]any{}, MaxRenderBytes)
			if tt.fail != (err != nil) || (!tt.fail && out != tt.want) {
				t.Fatalf("out = %q, err = %v", out, err)
			}
		})
	}
}
