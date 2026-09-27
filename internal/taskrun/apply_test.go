package taskrun

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/home-operations/kritik/internal/forge"
	"github.com/home-operations/kritik/internal/tasks"
)

// fakeForge records the writes a plan makes, in order, failing those named
// in fail. Anything else it is asked panics through the nil embedded
// Client.
type fakeForge struct {
	forge.Client
	calls    []string
	fail     map[string]bool
	comments map[int64]string
	found    int64
	review   []forge.InlineComment
}

func (f *fakeForge) do(call string) error {
	f.calls = append(f.calls, call)
	if f.fail[strings.Fields(call)[0]] {
		return errors.New("boom")
	}
	return nil
}

func (f *fakeForge) AddLabels(_ context.Context, _, _ string, _ int, ls []string) error {
	return f.do("AddLabels " + strings.Join(ls, ","))
}

func (f *fakeForge) RemoveLabel(_ context.Context, _, _ string, _ int, l string) error {
	return f.do("RemoveLabel " + l)
}

func (f *fakeForge) AddAssignees(_ context.Context, _, _ string, _ int, ls []string) error {
	return f.do("AddAssignees " + strings.Join(ls, ","))
}

func (f *fakeForge) RequestReviewers(_ context.Context, _, _ string, _ int, ls []string) error {
	return f.do("RequestReviewers " + strings.Join(ls, ","))
}

func (f *fakeForge) SetState(_ context.Context, _, _ string, _ int, open bool) error {
	if open {
		return f.do("SetState open")
	}
	return f.do("SetState closed")
}

func (f *fakeForge) LineRanges() bool { return true }

func (f *fakeForge) CreateReview(_ context.Context, _, _ string, _ int, head string, cs []forge.InlineComment) error {
	f.review = cs
	return f.do("CreateReview " + head)
}

func (f *fakeForge) FindComment(_ context.Context, _, _ string, _ int, login, marker string) (int64, error) {
	if login != "kritik[bot]" {
		return 0, errors.New("wrong login")
	}
	f.calls = append(f.calls, "FindComment "+marker)
	return f.found, nil
}

func (f *fakeForge) CreateComment(_ context.Context, _, _ string, _ int, body string) (int64, error) {
	if err := f.do("CreateComment"); err != nil {
		return 0, err
	}
	if f.comments == nil {
		f.comments = map[int64]string{}
	}
	id := int64(100 + len(f.comments))
	f.comments[id] = body
	return id, nil
}

func (f *fakeForge) UpdateComment(_ context.Context, _, _ string, id int64, body string) error {
	if err := f.do("UpdateComment"); err != nil {
		return err
	}
	f.comments[id] = body
	return nil
}

func TestApply(t *testing.T) {
	plan := tasks.Plan{
		AddLabels: []string{"bug", "area/api"}, RemoveLabels: []string{"needs-triage", "stale"}, Assignees: []string{"alice"},
		Reviewers: []string{"bob"}, State: tasks.StateClose, Inline: []tasks.Inline{{Path: "a.go", Line: 3, EndLine: 4, Body: "x"}},
	}
	order := []string{
		"AddLabels bug,area/api", "RemoveLabel needs-triage", "RemoveLabel stale", "AddAssignees alice", "RequestReviewers bob",
		"SetState closed", "CreateReview head",
	}
	tests := []struct {
		name        string
		fail        map[string]bool
		wantApplied tasks.Applied
		wantDropped []string
		failed      bool
	}{
		{"every write lands", nil, plan.Applied(), nil, false},
		{"a refused write is recorded and the rest still made", map[string]bool{"RemoveLabel": true, "SetState": true},
			tasks.Applied{AddLabels: plan.AddLabels, Assignees: plan.Assignees, Reviewers: plan.Reviewers, Inline: plan.Inline},
			[]string{"labels.remove needs-triage", "labels.remove stale", "state close"}, false},
		{"everything refused", map[string]bool{
			"AddLabels": true, "RemoveLabel": true, "AddAssignees": true, "RequestReviewers": true, "SetState": true, "CreateReview": true,
		}, tasks.Applied{}, []string{
			"labels.add bug, area/api", "labels.remove needs-triage", "labels.remove stale", "assign alice", "reviewers bob",
			"state close", "inline 1 comment(s)",
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeForge{fail: tt.fail}
			res := Apply(context.Background(), f, Target{Owner: "acme", Repo: "w", Number: 7, HeadSHA: "head"}, plan)
			if !reflect.DeepEqual(f.calls, order) {
				t.Fatalf("calls = %q, want %q", f.calls, order)
			}
			if !reflect.DeepEqual(res.Applied, tt.wantApplied) {
				t.Fatalf("applied = %+v, want %+v", res.Applied, tt.wantApplied)
			}
			var dropped []string
			for _, d := range res.Dropped {
				if !strings.HasPrefix(d.Reason, "forge: ") {
					t.Fatalf("drop reason = %q", d.Reason)
				}
				dropped = append(dropped, d.Action+" "+d.Value)
			}
			if !reflect.DeepEqual(dropped, tt.wantDropped) {
				t.Fatalf("dropped = %q, want %q", dropped, tt.wantDropped)
			}
			if res.Failed() != tt.failed {
				t.Fatalf("Failed = %v, want %v", res.Failed(), tt.failed)
			}
		})
	}
	f := &fakeForge{}
	Apply(context.Background(), f, Target{HeadSHA: "head"}, tasks.Plan{Inline: plan.Inline})
	if want := []forge.InlineComment{{Path: "a.go", StartLine: 3, Line: 4, Body: "x"}}; !reflect.DeepEqual(f.review, want) {
		t.Fatalf("review = %+v, want %+v", f.review, want)
	}
	if res := Apply(context.Background(), &fakeForge{}, Target{}, tasks.Plan{}); res.Attempted != 0 || res.Failed() {
		t.Fatalf("an empty plan = %+v", res)
	}
}

func TestPost(t *testing.T) {
	marker := tasks.StickyMarker("triage")
	tests := []struct {
		name  string
		mode  string
		found int64
		calls []string
		id    int64
	}{
		{"sticky, first run creates", tasks.CommentSticky, 0, []string{"FindComment " + marker, "CreateComment"}, 100},
		{"sticky, later run updates", tasks.CommentSticky, 42, []string{"FindComment " + marker, "UpdateComment"}, 42},
		{"append always creates", tasks.CommentAppend, 42, []string{"CreateComment"}, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeForge{found: tt.found, comments: map[int64]string{}}
			id, err := Post(context.Background(), f, Target{Number: 7}, "triage", "kritik[bot]", tt.mode, "report")
			if err != nil || id != tt.id || !reflect.DeepEqual(f.calls, tt.calls) {
				t.Fatalf("Post = %d, %v; calls %q", id, err, f.calls)
			}
			body := f.comments[id]
			if sticky := strings.HasSuffix(body, marker); sticky != (tt.mode == tasks.CommentSticky) || !strings.HasPrefix(body, "report") {
				t.Fatalf("body = %q", body)
			}
		})
	}
	f := &fakeForge{fail: map[string]bool{"CreateComment": true}}
	if _, err := Post(context.Background(), f, Target{}, "triage", "kritik[bot]", tasks.CommentAppend, "x"); err == nil {
		t.Fatal("a refused comment must be an error")
	}
}

func TestAnchor(t *testing.T) {
	anchors := map[string]map[int]bool{"a.go": {3: true, 4: true, 5: true}, "b.go": {10: true}}
	in := []tasks.Inline{
		{Path: "a.go", Line: 3, Body: "one line"},
		{Path: "a.go", Line: 4, EndLine: 5, Body: "range"},
		{Path: "a.go", Line: 5, EndLine: 6, Body: "range off the diff"},
		{Path: "b.go", Line: 11, Body: "off the diff"},
		{Path: "c.go", Line: 1, Body: "file not in the diff"},
	}
	kept, dropped := Anchor(in, anchors)
	if !reflect.DeepEqual(kept, in[:2]) {
		t.Fatalf("kept = %+v", kept)
	}
	values := make([]string, 0, len(dropped))
	for _, d := range dropped {
		if d.Action != "inline" {
			t.Fatalf("drop = %+v", d)
		}
		values = append(values, d.Value)
	}
	if want := []string{"a.go:5-6", "b.go:11", "c.go:1"}; !reflect.DeepEqual(values, want) {
		t.Fatalf("dropped = %q, want %q", values, want)
	}

	pl := tasks.Plan{Inline: in[:1]}
	DropInline(&pl, "no head")
	if pl.Inline != nil || len(pl.Dropped) != 1 || pl.Dropped[0].Reason != "no head" {
		t.Fatalf("DropInline = %+v", pl)
	}
}
