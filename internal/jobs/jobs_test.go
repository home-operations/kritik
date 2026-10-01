package jobs

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/riverqueue/river"
)

// uniqueFields returns the river:"unique" struct tag value for every field of
// v's type that carries one, keyed by field name. It mirrors, at the level a
// unit test can reach, what river's insert_opts.go hashes for ByArgs
// uniqueness: only tagged fields, nothing else.
func uniqueFields(v any) map[string]bool {
	out := make(map[string]bool)
	for f := range reflect.TypeOf(v).Fields() {
		if _, ok := f.Tag.Lookup("river"); ok {
			out[f.Name] = true
		}
	}
	return out
}

func TestReviewArgsUniqueTags(t *testing.T) {
	got := uniqueFields(ReviewArgs{})
	want := map[string]bool{"AccountID": true, "RepositoryID": true, "Number": true, "HeadSHA": true, "Request": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("river:\"unique\" fields = %v, want %v", got, want)
	}
}

// TestJobArgs pins each job's kind, queue and ByArgs uniqueness, and
// whether "request" is in the JSON River hashes: a non-manual review
// (Request left empty) must serialize with no "request" key at all, not an
// empty string, or it would perturb the hash relative to jobs enqueued
// before the field existed; a manual re-run must carry it, or two re-runs
// of the same head would collide.
func TestJobArgs(t *testing.T) {
	tests := []struct {
		name        string
		args        river.JobArgs
		wantKind    string
		wantQueue   string
		wantRequest bool
	}{
		{name: "review", args: ReviewArgs{AccountID: "t", RepositoryID: "r", Number: 1, HeadSHA: "abc", Trigger: "push"},
			wantKind: "review", wantQueue: QueueReview},
		{name: "manual review", args: ReviewArgs{AccountID: "t", RepositoryID: "r", Number: 1, HeadSHA: "abc", Trigger: TriggerManual,
			Request: "11111111-1111-1111-1111-111111111111"}, wantKind: "review", wantQueue: QueueReview, wantRequest: true},
		{name: "follow-up", args: FollowUpArgs{AccountID: "t", RepositoryID: "r", Number: 1, CommentID: 7},
			wantKind: "followup", wantQueue: QueueFollowUp},
		{name: "index", args: IndexArgs{AccountID: "t", RepositoryID: "r", Trigger: TriggerPush},
			wantKind: "index", wantQueue: QueueIndex},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.args.Kind(); got != tt.wantKind {
				t.Fatalf("Kind() = %q, want %q", got, tt.wantKind)
			}
			opts := tt.args.(river.JobArgsWithInsertOpts).InsertOpts()
			if opts.Queue != tt.wantQueue || !opts.UniqueOpts.ByArgs {
				t.Fatalf("InsertOpts() = %+v, want queue %q unique by args", opts, tt.wantQueue)
			}
			data, err := json.Marshal(tt.args)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if _, ok := m["request"]; ok != tt.wantRequest {
				t.Fatalf("json = %s, want a %q key: %v", data, "request", tt.wantRequest)
			}
		})
	}
}

func TestSettles(t *testing.T) {
	tests := []struct {
		trigger string
		want    bool
	}{
		{trigger: "synchronize", want: true},
		{trigger: "poll", want: true},
		{trigger: "opened"},
		{trigger: "reopened"},
		{trigger: "ready_for_review"},
		{trigger: TriggerManual},
		{trigger: ""},
	}
	for _, tt := range tests {
		t.Run(tt.trigger, func(t *testing.T) {
			if got := Settles(tt.trigger); got != tt.want {
				t.Fatalf("Settles(%q) = %v, want %v", tt.trigger, got, tt.want)
			}
		})
	}
}

// TestIndexArgsUniqueTags pins the key an index job is unique on: the
// repository, not the commit, so a burst of pushes is one job; and Full, so
// a forced rebuild is never folded into an update.
func TestIndexArgsUniqueTags(t *testing.T) {
	got := uniqueFields(IndexArgs{})
	want := map[string]bool{"RepositoryID": true, "Full": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("river:\"unique\" fields = %v, want %v", got, want)
	}
}

// TestIndexArgsPriorities pins that updates run before forced rebuilds and
// both before onboarding, and that every index job is retried a few times.
func TestIndexArgsPriorities(t *testing.T) {
	priority := map[string]int{}
	for _, trigger := range []string{TriggerPush, TriggerReindex, TriggerOnboard} {
		opts := IndexArgs{Trigger: trigger}.InsertOpts()
		if opts.MaxAttempts != indexAttempts {
			t.Fatalf("%s: MaxAttempts = %d, want %d", trigger, opts.MaxAttempts, indexAttempts)
		}
		priority[trigger] = opts.Priority
	}
	if priority[TriggerPush] >= priority[TriggerReindex] || priority[TriggerReindex] >= priority[TriggerOnboard] {
		t.Fatalf("priorities = %v, want push before reindex before onboard (lower runs first)", priority)
	}
}
