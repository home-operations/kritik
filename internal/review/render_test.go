package review

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func sampleData() RenderData {
	res := Result{
		Summary: Summary{Take: "Solid change with one real bug.", Praise: []string{"Clear tests"}},
		Findings: []Finding{
			{Path: "main.go", Line: 11, Severity: SeverityBlocking, Title: "nil map write", Explanation: "m is nil here.", SuggestedFix: "m = map[string]int{}",
				URL: "https://forge.example/o/r/blob/0123456789abcdef/main.go#L11"},
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
		"**2 findings** · 1 blocking · 0 important · 1 nit",
		"Solid change with one real bug.",
		"- Clear tests",
		"- **[blocking]** [`main.go:11`](https://forge.example/o/r/blob/0123456789abcdef/main.go#L11) nil map write",
		"- **[nit]** `README.md:2` typo",
		"_1 file(s) were omitted from the diff to fit the context budget._",
		"Reviewed `0123456` by kritik with vendor/model-x. Reviews never block a merge.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "\n\n\n") {
		t.Fatalf("blank lines doubled:\n%s", body)
	}
	if strings.Index(body, "2 findings") > strings.Index(body, "Solid change") || strings.Index(body, "Solid change") > strings.Index(body, "`main.go:11`") {
		t.Fatalf("sections out of order:\n%s", body)
	}

	empty := sampleData()
	empty.Result.Findings, empty.Counts, empty.Notes, empty.Result.Summary.Praise = nil, Counts{}, nil, nil
	empty.Incremental, empty.PriorHeadSHA = true, "fedcba9876543210"
	body, _ = RenderSummary(t.Context(), Templates{}, empty)
	if !strings.Contains(body, "Nothing worth flagging") || strings.Contains(body, "_1 file") || !strings.Contains(body, "`fedcba9`") ||
		strings.Contains(body, "0 findings") || strings.Contains(body, "\n\n\n") {
		t.Fatalf("empty body:\n%s", body)
	}
}

func TestRenderSummarySources(t *testing.T) {
	d := sampleData()
	body, _ := RenderSummary(t.Context(), Templates{}, d)
	if strings.Contains(body, "Sources consulted") {
		t.Fatalf("a review that fetched nothing lists sources:\n%s", body)
	}
	d.Sources = []string{"https://api.github.com/repos/a/b/releases/tags/v2", "https://github.com/a/b/compare/v1...v2"}
	body, _ = RenderSummary(t.Context(), Templates{}, d)
	want := "<details>\n<summary>Sources consulted</summary>\n\n" +
		"- <https://api.github.com/repos/a/b/releases/tags/v2>\n- <https://github.com/a/b/compare/v1...v2>\n\n</details>\n"
	if !strings.Contains(body, want) || strings.Contains(body, "\n\n\n") {
		t.Fatalf("body:\n%s", body)
	}
	if strings.Index(body, "`README.md:2`") > strings.Index(body, "Sources consulted") ||
		strings.Index(body, "Sources consulted") > strings.Index(body, "_1 file") {
		t.Fatalf("sections out of order:\n%s", body)
	}
}

func TestRenderSummaryIncomplete(t *testing.T) {
	d := RenderData{Number: 7, HeadSHA: "0123456789abcdef", Model: "vendor/model-x", Incomplete: "agent stopped: max_steps",
		Notes: []string{"a note"}}
	body, notes := RenderSummary(t.Context(), Templates{}, d)
	if len(notes) != 0 {
		t.Fatalf("notes = %v", notes)
	}
	for _, want := range []string{
		Marker(7), "### kritik review", "**Review incomplete for `0123456`:** agent stopped: max_steps.", "_a note._",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"Nothing worth flagging", "blocking ·"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("an incomplete review must not claim %q:\n%s", unwanted, body)
		}
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
			template: "#{{ .Number }} {{ .HeadSHA }} {{ .Model }} {{ .Result.Summary.Take }} {{ join \",\" .Result.Summary.Praise }} {{ .Counts.Blocking }}/{{ .Counts.Important }}/{{ .Counts.Nit }} {{ .Incremental }}\n{{ range .Result.Findings }}{{ .Severity }} {{ .Path }}:{{ .Line }} {{ .Title }} {{ .Explanation }} {{ .SuggestedFix }};{{ end }}{{ len .Notes }}",
			want:     []string{"#42 0123456789abcdef vendor/model-x Solid change with one real bug. Clear tests 1/0/1 false", "blocking main.go:11 nil map write m is nil here. m = map[string]int{};", "nit README.md:2 typo the the ;1"},
		},
		{name: "sprout functions", template: `{{ trunc 7 .HeadSHA }} {{ .Model | toUpper }} {{ printf "%03d" .Number }} {{ range $i, $f := .Result.Findings }}{{ add $i 1 }}.{{ end }}`, want: []string{"0123456 VENDOR/MODEL-X 042 1.2."}},
		{name: "template resolves nothing", template: `{{ template "secrets" }}`, note: "summary template"},
		{name: "define is refused", template: `{{ define "x" }}a{{ end }}{{ template "x" }}`, note: "summary template"},
		{name: "block is refused", template: `{{ block "x" . }}a{{ end }}`, note: "summary template"},
		{name: "env is not defined", template: `{{ env "HOME" }}`, note: "summary template"},
		{name: "syntax error", template: `{{ range }}`, note: "summary template"},
		{name: "unknown field", template: `{{ .Secret }}`, note: "summary template"},
		{name: "string repetition", template: `{{ repeat 1000000000000 "x" }}`, note: "summary template"},
		{name: "oversized printf width", template: `{{ printf "%01000000000d" 1 }}`, note: "summary template"},
		{name: "printf width from an argument", template: `{{ printf "%*d" 10 1 }}`, note: "summary template"},
		{name: "oversized output", template: `{{ range until 10000 }}0123456789{{ end }}`, note: "64 KiB"},
		{name: "deadline", template: "{{ .Number }}", note: "time"},
		{name: "loop iteration budget", template: `{{ range until 10000 }}{{ range until 10000 }}{{ end }}{{ end }}`, note: "summary template"},
		{name: "integer range", template: `{{ range 1000000000 }}{{ end }}`, note: "summary template"},
		{name: "reserved names", template: `{{ __kritik_iter 1 }}`, note: "summary template"},
		{name: "mutating a dict", template: `{{ $d := dict "a" 1 }}{{ set $d "b" $d }}`, note: "summary template"},
		{name: "random is not defined", template: `{{ randAlpha 8 }}`, note: "summary template"},
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
	body, notes := RenderSummary(t.Context(), Templates{Summary: "{{/* nothing */}}"}, sampleData())
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
	for _, unwanted := range []string{"```suggestion", "<details>"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("no replacement or agent prompt should render %q:\n%s", unwanted, body)
		}
	}
	rich := f
	rich.SuggestedFix, rich.Replacement, rich.AgentPrompt = "", "m := map[string]int{}\nm[k] = v", "In main.go replace lines 11-12 so `m` is made before `m[k] = v`; use ``x``."
	body, _ = RenderInline(t.Context(), Templates{}, rich)
	if !strings.Contains(body, "```suggestion\nm := map[string]int{}\nm[k] = v\n```") ||
		!strings.Contains(body, "<details>\n<summary>Prompt for a coding agent</summary>\n\n```\nIn main.go replace") ||
		!strings.HasSuffix(body, "use ``x``.\n```\n\n</details>\n") {
		t.Fatalf("rich inline:\n%s", body)
	}

	body, notes = RenderInline(t.Context(), Templates{Inline: "{{ .Severity }}|{{ .Path }}:{{ .Line }}|{{ .Title }}|{{ .Explanation }}|{{ .SuggestedFix }}"}, f)
	if len(notes) != 0 || body != "blocking|main.go:11|nil map write|m is nil here.|m = map[string]int{}" {
		t.Fatalf("custom inline = %q, notes %v", body, notes)
	}
	body, notes = RenderInline(t.Context(), Templates{Inline: `{{ template "x" }}`}, f)
	if len(notes) != 1 || !strings.Contains(body, "**nil map write**") {
		t.Fatalf("fallback inline = %q, notes %v", body, notes)
	}
}

// TestRenderMemoryIsBounded renders templates that try to build large
// values by amplification or by repeated growth. Each must fall back
// quickly, and the render must allocate little on the way.
func TestRenderMemoryIsBounded(t *testing.T) {
	const maxAlloc = 64 << 20
	s60k := `{{ $s := repeat 60000 "a" }}`
	tests := map[string]string{
		"chained cat":                     `{{ $a := repeat 16 "a" }}` + strings.Repeat(`{{ $a = cat $a $a }}`, 30) + `{{ len $a }}`,
		"chained replace":                 `{{ $b := repeat 40000 "a" }}` + strings.Repeat(`{{ $b = replace "a" "aa" $b }}`, 12) + `{{ len $b }}`,
		"chained list":                    `{{ $l := list 1 }}` + strings.Repeat(`{{ $l = concat $l $l }}`, 30) + `{{ len $l }}`,
		"shared structure":                s60k + `{{ $a := list $s $s $s $s $s $s $s $s $s $s }}{{ $b := list $a $a $a $a $a $a $a $a $a $a }}{{ $c := list $b $b $b $b $b $b $b $b $b $b }}{{ toJSON $c }}`,
		"join with a long separator":      s60k + `{{ join $s (until 10000) }}`,
		"repeat of a long value":          s60k + `{{ repeat 10000 $s }}`,
		"indent with a huge width":        `{{ $s := repeat 30000 "a\n" }}{{ indent 65536 $s }}`,
		"nindent with a huge width":       `{{ $s := repeat 30000 "a\n" }}{{ nindent 65536 $s }}`,
		"regex replace of every position": s60k + `{{ regexReplaceAll "" $s $s }}`,
		"seq of many numbers":             `{{ seq 1 100000000 }}`,
		"until of many numbers":           `{{ until 100000000 }}`,
		"untilStep of many numbers":       `{{ untilStep 0 100000000 1 }}`,
		"printf with many wide verbs":     `{{ printf (repeat 5000 "%09999d") 1 }}`,
		"printf of a huge width":          `{{ printf "%0900000d" 1 }}`,
		"toPrettyJSON of long model text": `{{ toPrettyJSON .Result.Findings }}{{ toPrettyJSON .Result.Findings }}{{ toPrettyJSON .Result.Findings }}`,
		"nested loops over a long string": s60k + `{{ range until 10000 }}{{ range until 10000 }}{{ end }}{{ end }}`,
		"deep copy of a shared structure": s60k + `{{ $a := list $s $s $s $s $s $s $s $s $s $s }}{{ $b := list $a $a $a $a $a $a $a $a $a $a }}{{ len (deepCopy (list $b $b $b $b $b $b $b $b $b $b)) }}`,
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
	if n := measure(reflect.ValueOf(d), 0, maxCallBytes); n < 50_000 {
		t.Fatalf("measure = %d, want the nested explanation counted", n)
	}
	shared := strings.Repeat("x", 1000)
	list := []any{shared, shared, shared, shared, shared, shared, shared, shared, shared, shared}
	nested := []any{list, list, list, list, list, list, list, list, list, list}
	if n := measure(reflect.ValueOf([]any{nested, nested, nested, nested, nested, nested, nested, nested, nested, nested}), 0, maxCallBytes); n <= maxCallBytes {
		t.Fatalf("measure = %d, want a shared value counted at every reference", n)
	}
}

// TestLoopsAreAllGuarded checks that every range, wherever it sits, is
// charged to the iteration budget.
func TestLoopsAreAllGuarded(t *testing.T) {
	tests := []struct {
		name, src, want string
		fails           bool
	}{
		{name: "nested loops render", src: `{{ range until 3 }}{{ range until 2 }}x{{ end }}{{ end }}`, want: "xxxxxx"},
		{name: "loop over a pipeline", src: `{{ range until 3 | reverse }}{{ . }}{{ end }}`, want: "210"},
		{name: "loop with variables", src: `{{ range $i, $v := list "a" "b" }}{{ $i }}{{ $v }}{{ end }}`, want: "0a1b"},
		{name: "loop with else", src: `{{ range list }}x{{ else }}none{{ end }}`, want: "none"},
		{name: "loop in an if", src: `{{ if true }}{{ range until 30000 }}{{ end }}{{ end }}`, fails: true},
		{name: "loop in an else", src: `{{ if false }}{{ else }}{{ range until 30000 }}{{ end }}{{ end }}`, fails: true},
		{name: "loop in a with", src: `{{ with .Result }}{{ range until 30000 }}{{ end }}{{ end }}`, fails: true},
		{name: "loop in a loop", src: `{{ range until 2 }}{{ range until 15000 }}{{ end }}{{ end }}`, fails: true},
		{name: "loop over a dict", src: `{{ range $k, $v := dict "a" 1 }}{{ $k }}{{ $v }}{{ end }}`, want: "a1"},
		{name: "loop over a channel-like value", src: `{{ range .Result }}{{ end }}`, fails: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := execute(t.Context(), tt.src, sampleData(), MaxRenderBytes)
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
}

// TestLoopStopsAtTheDeadline runs a loop that is within every budget but
// slow, under a deadline that expires part way through: the render itself,
// not only the caller, must stop.
func TestLoopStopsAtTheDeadline(t *testing.T) {
	src := `{{ $s := repeat 30000 "a " }}{{ range until 10000 }}{{ $t := replace " " "  " $s }}{{ end }}done`
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	out, err := execute(ctx, src, sampleData(), MaxRenderBytes)
	elapsed := time.Since(started)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("out = %q, err = %v after %s; want the deadline to stop the loop", out, err, elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("the loop ran %s past a 30ms deadline", elapsed)
	}
}

// TestCallLimitsMatchTheDocs pins the documented limits: the largest
// documented size is accepted, one more is refused.
func TestCallLimitsMatchTheDocs(t *testing.T) {
	tests := []struct {
		src  string
		want string
		fail bool
	}{
		{src: `{{ range until 20000 }}{{ end }}ok`, want: "ok"},
		{src: `{{ range until 20001 }}{{ end }}ok`, fail: true},
		{src: `{{ len (until 32768) }}`, want: "32768"},
		{src: `{{ len (until 32769) }}`, fail: true},
		{src: `{{ len (repeat 262100 "a") }}`, want: "262100"},
		{src: `{{ len (repeat 262144 "a") }}`, fail: true},
		{src: `{{ len (printf "%0262100d" 1) }}`, want: "262100"},
		{src: `{{ len (printf "%0262144d" 1) }}`, fail: true},
		{src: `{{ $a := repeat 131000 "a" }}{{ $b := repeat 131000 "b" }}{{ len (cat $a $b) }}`, want: "262001"},
		{src: `{{ $a := repeat 131072 "a" }}{{ $b := repeat 131073 "b" }}{{ len (cat $a $b) }}`, fail: true},
		{src: `{{ $a := repeat 65000 "a" }}{{ len (join $a (list 1 2 3)) }}`, want: "130003"},
		{src: `{{ $a := repeat 65000 "a" }}{{ len (join $a (list 1 2 3 4)) }}`, fail: true},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			out, err := execute(t.Context(), tt.src, sampleData(), MaxRenderBytes)
			if tt.fail != (err != nil) || (!tt.fail && out != tt.want) {
				t.Fatalf("out = %q, err = %v", out, err)
			}
		})
	}
}
