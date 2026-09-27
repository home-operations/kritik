package ingest

import (
	"context"
	"testing"

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
