package ingest

import (
	"context"
	"testing"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/webhook"
)

// TestDispatchIgnoresNonPullComments runs without a store: a comment on
// anything but a pull request must be turned away before it is touched.
func TestDispatchIgnoresNonPullComments(t *testing.T) {
	svc := NewService(nil, nil)
	tests := []struct {
		name    string
		subject *webhook.Subject
		want    string
	}{
		{"issue comment", &webhook.Subject{Kind: webhook.SubjectIssue, Number: 7}, webhook.SubjectIssue},
		{"unknown subject", &webhook.Subject{Kind: "discussion", Number: 7}, "discussion"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := webhook.Event{
				Kind: webhook.KindComment, Action: "created", Subject: tt.subject,
				Repository: &webhook.Repository{FullName: "onedr0p/home-ops"},
				Comment:    &webhook.Comment{ID: 1, Number: 7, Author: "devin", Body: "@bot-ross triage this"},
			}
			out, err := svc.Dispatch(context.Background(), Request{Event: ev})
			if err != nil || out.Status != Ignored || out.Reason != tt.want {
				t.Fatalf("out = %+v, %v", out, err)
			}
		})
	}
}

// TestDispatchLabelActionsNeverReview covers the actions GitHub sends and
// Forgejo's label changes normalize to: neither records nor reviews the pull
// request, so no store is needed.
func TestDispatchLabelActionsNeverReview(t *testing.T) {
	svc := NewService(nil, nil)
	for _, action := range []string{"labeled", "unlabeled"} {
		t.Run(action, func(t *testing.T) {
			ev := webhook.Event{
				Kind: webhook.KindPullRequest, Action: action,
				Subject:     &webhook.Subject{Kind: webhook.SubjectPull, Number: 7},
				Repository:  &webhook.Repository{FullName: "onedr0p/home-ops"},
				PullRequest: &webhook.PullRequest{Number: 7, State: "open", HeadSHA: "aaa"},
			}
			out, err := svc.Dispatch(context.Background(), Request{Event: ev})
			if err != nil || out.Status != Ignored || out.Reason != reasonAction {
				t.Fatalf("out = %+v, %v", out, err)
			}
		})
	}
}

// TestDispatchTasksNeedNoStoreWithoutACandidate: with tasks off, or no
// task that could run on the event, a delivery never reaches the store.
func TestDispatchTasksNeedNoStoreWithoutACandidate(t *testing.T) {
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "s3cret")
	off, err := configfile.Parse([]byte(configYAML))
	if err != nil {
		t.Fatal(err)
	}
	onlyReleases, err := configfile.Parse([]byte(`defaults:
  allow: { tasks: { enabled: true, repositoryTasks: false } }
  tasks: [{ name: notes, on: [{ raw: { event: release } }] }]
` + configYAML))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(nil, nil)
	for _, f := range []*configfile.File{off, onlyReleases} {
		in, tenant, _ := f.Installation("bot-ross")
		ev := webhook.Event{
			Kind: webhook.KindIssue, Action: "opened", RawEvent: "issues", Sender: "devin",
			Repository: &webhook.Repository{FullName: "onedr0p/home-ops"}, Subject: &webhook.Subject{Kind: webhook.SubjectIssue, Number: 7},
			Issue: &webhook.Issue{Number: 7},
		}
		out, err := svc.Dispatch(context.Background(), Request{File: f, Tenant: tenant, Installation: in, Event: ev})
		if err != nil || out.Status != Ignored {
			t.Fatalf("out = %+v, %v", out, err)
		}
	}
}
