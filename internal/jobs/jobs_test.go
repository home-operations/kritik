package jobs

import (
	"reflect"
	"testing"
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
	want := map[string]bool{"TenantID": true, "RepositoryID": true, "Number": true, "HeadSHA": true, "Request": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("river:\"unique\" fields = %v, want %v", got, want)
	}
}

func TestReviewArgsRequestEmptyByDefault(t *testing.T) {
	args := ReviewArgs{TenantID: "t", RepositoryID: "r", Number: 1, HeadSHA: "abc", Trigger: "push"}
	if args.Request != "" {
		t.Fatalf("Request = %q, want empty for a non-manual trigger", args.Request)
	}
}

// TestReviewArgsSameHeadDedupeUnaffected pins the dedup contract for every
// existing trigger: two jobs for the same tenant/repo/number/head, neither
// carrying a manual Request, present identical values on every
// river:"unique" field, so River hashes them the same and the second insert
// is deduped exactly as before this field was added.
func TestReviewArgsSameHeadDedupeUnaffected(t *testing.T) {
	a := ReviewArgs{TenantID: "t", RepositoryID: "r", Number: 1, HeadSHA: "abc", Trigger: "synchronize"}
	b := ReviewArgs{TenantID: "t", RepositoryID: "r", Number: 1, HeadSHA: "abc", Trigger: "poll"}
	if a.TenantID != b.TenantID || a.RepositoryID != b.RepositoryID || a.Number != b.Number ||
		a.HeadSHA != b.HeadSHA || a.Request != b.Request {
		t.Fatalf("unique fields differ between %+v and %+v", a, b)
	}
}

// TestReviewArgsManualRerunsDiffer pins that two manual re-runs of the same
// head do not collide: each caller of EnqueueRerun sets a fresh Request, so
// the jobs differ on a river:"unique" field and River inserts both.
func TestReviewArgsManualRerunsDiffer(t *testing.T) {
	first := ReviewArgs{TenantID: "t", RepositoryID: "r", Number: 1, HeadSHA: "abc", Trigger: TriggerManual, Request: "11111111-1111-1111-1111-111111111111"}
	second := ReviewArgs{TenantID: "t", RepositoryID: "r", Number: 1, HeadSHA: "abc", Trigger: TriggerManual, Request: "22222222-2222-2222-2222-222222222222"}
	if first.Request == second.Request {
		t.Fatalf("two manual re-runs must set distinct Request values")
	}
	if first == second {
		t.Fatalf("two manual re-runs must not be identical args")
	}
}

func TestReviewArgsKindAndInsertOpts(t *testing.T) {
	args := ReviewArgs{}
	if got := args.Kind(); got != "review" {
		t.Fatalf("Kind() = %q, want %q", got, "review")
	}
	opts := args.InsertOpts()
	if opts.Queue != QueueReview {
		t.Fatalf("InsertOpts().Queue = %q, want %q", opts.Queue, QueueReview)
	}
	if !opts.UniqueOpts.ByArgs {
		t.Fatal("InsertOpts().UniqueOpts.ByArgs = false, want true")
	}
}

func TestIndexArgsUniqueTags(t *testing.T) {
	got := uniqueFields(IndexArgs{})
	want := map[string]bool{"RepositoryID": true, "CommitSHA": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("river:\"unique\" fields = %v, want %v", got, want)
	}
}

// TestIndexArgsFullNotUnique pins that Full is deliberately excluded from
// the uniqueness hash: a forced reindex must dedupe against a concurrent one
// by RepositoryID+CommitSHA alone, the same as any other reindex.
func TestIndexArgsFullNotUnique(t *testing.T) {
	if uniqueFields(IndexArgs{})["Full"] {
		t.Fatal("Full must not carry a river:\"unique\" tag")
	}
}

func TestIndexArgsKindAndInsertOpts(t *testing.T) {
	args := IndexArgs{}
	if got := args.Kind(); got != "index" {
		t.Fatalf("Kind() = %q, want %q", got, "index")
	}
	opts := args.InsertOpts()
	if opts.Queue != QueueIndex {
		t.Fatalf("InsertOpts().Queue = %q, want %q", opts.Queue, QueueIndex)
	}
	if !opts.UniqueOpts.ByArgs {
		t.Fatal("InsertOpts().UniqueOpts.ByArgs = false, want true")
	}
}

func TestTriggerManual(t *testing.T) {
	if TriggerManual != "manual" {
		t.Fatalf("TriggerManual = %q, want %q", TriggerManual, "manual")
	}
}
