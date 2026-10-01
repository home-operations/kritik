package worker

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/jobtimeout"
)

const timeoutConfigYAML = `
providers:
  gateway:
    type: openai
    baseUrl: https://models.example.com/v1
    apiKey: { env: TEST_SECRET }
defaults:
  models:
    review: gateway/review-model
apps:
  - name: acme-bot
    accounts: [acme]
    clientId: Iv1.x
    privateKey: { env: TEST_PEM }
    webhookSecret: { env: TEST_SECRET }
  - name: globex-bot
    accounts: [globex]
    clientId: Iv1.y
    privateKey: { env: TEST_PEM }
    webhookSecret: { env: TEST_SECRET }
repositories:
  acme/slow-agent: { agent: { timeout: 50m } }
`

func TestJobTimeouts(t *testing.T) {
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "s")
	file := configfiletest.Load(t, timeoutConfigYAML)
	current := configfile.NewCurrent(file)
	acme, _ := file.Account(configfile.ForgeGitHub, "acme")
	acmeRepo := func(name string) string { return configfile.RepositoryID(acme.ID(), name) }

	review := &Review{Current: current}
	index := &Index{Current: current}
	followUp := &FollowUp{}
	tests := []struct {
		name         string
		accountID    string
		repositoryID string
		review       time.Duration
		index        time.Duration
		followUp     time.Duration
	}{
		// The agent's 20m plus 5m of fetch headroom outlasts the runner
		// deadline, plus 15m to take the lease and 5m to publish; the index
		// has 15m + 60m to embed.
		{name: "the default agent timeout", accountID: acme.ID(), repositoryID: acmeRepo("acme/unlisted"), review: 45 * time.Minute,
			index: 75 * time.Minute, followUp: 30 * time.Minute},
		{name: "a longer agent timeout", accountID: acme.ID(), repositoryID: acmeRepo("acme/slow-agent"),
			review: 75 * time.Minute, index: 75 * time.Minute, followUp: 30 * time.Minute},
		{name: "unknown account", accountID: "missing", repositoryID: "missing", review: 35 * time.Minute, index: 75 * time.Minute, followUp: 30 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.review <= time.Minute || tt.index <= time.Minute || tt.followUp <= time.Minute {
				t.Fatal("a job timeout must outlast River's one-minute default")
			}
			got := review.Timeout(&river.Job[jobs.ReviewArgs]{Args: jobs.ReviewArgs{AccountID: tt.accountID, RepositoryID: tt.repositoryID}})
			if got != tt.review {
				t.Errorf("review timeout = %s, want %s", got, tt.review)
			}
			got = index.Timeout(&river.Job[jobs.IndexArgs]{Args: jobs.IndexArgs{AccountID: tt.accountID, RepositoryID: tt.repositoryID}})
			if got != tt.index {
				t.Errorf("index timeout = %s, want %s", got, tt.index)
			}
			got = followUp.Timeout(&river.Job[jobs.FollowUpArgs]{Args: jobs.FollowUpArgs{AccountID: tt.accountID, RepositoryID: tt.repositoryID}})
			if got != tt.followUp {
				t.Errorf("follow-up timeout = %s, want %s", got, tt.followUp)
			}
		})
	}

	// A runner deadline at configfile's max: the index job lands exactly on
	// MaxJobTimeout.
	t.Setenv("KRITIK_RUNNER_DEADLINE", jobtimeout.MaxRunnerDeadline.String())
	current = configfile.NewCurrent(configfiletest.Load(t, timeoutConfigYAML))
	review, index = &Review{Current: current}, &Index{Current: current}
	if got, want := review.Timeout(&river.Job[jobs.ReviewArgs]{Args: jobs.ReviewArgs{AccountID: acme.ID(), RepositoryID: acmeRepo("acme/unlisted")}}),
		jobtimeout.MaxRunnerDeadline+jobtimeout.LeaseWaitHeadroom+jobtimeout.PublishHeadroom; got != want {
		t.Errorf("review timeout at the max deadline = %s, want %s", got, want)
	}
	if got := index.Timeout(&river.Job[jobs.IndexArgs]{}); got != jobtimeout.MaxRunnerDeadline+jobtimeout.IndexWriteHeadroom {
		t.Errorf("index timeout at the max deadline = %s", got)
	}
}

func TestDetach(t *testing.T) {
	parent, cancelParent := context.WithCancel(t.Context())
	ctx, cancel := detach(parent)
	defer cancel()
	cancelParent()
	if ctx.Err() != nil {
		t.Fatalf("detached ctx ended with its parent: %v", ctx.Err())
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > detachTimeout {
		t.Fatalf("deadline = %v, %v; want one within %v", deadline, ok, detachTimeout)
	}
}
