package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/configfile/configfiletest"
	"github.com/home-operations/kritik/internal/jobs"
	"github.com/home-operations/kritik/internal/jobtimeout"
)

// timeoutConfigYAMLTemplate takes globex's runner.activeDeadlineSeconds, so
// the test can drive it right up to jobtimeout.MaxRunnerDeadline without
// tripping configfile's upper-bound validation.
const timeoutConfigYAMLTemplate = `
providers:
  gateway:
    type: openai
    baseUrl: https://models.example.com/v1
    apiKey: { env: TEST_SECRET }
defaults:
  models:
    review: gateway/review-model
connections:
  - name: acme-bot
    forge: github
    accounts: [acme]
    app:
      clientId: Iv1.x
      privateKey: { env: TEST_PEM }
      webhookSecret: { env: TEST_SECRET }
  - name: globex-bot
    forge: github
    accounts: [globex]
    app:
      clientId: Iv1.y
      privateKey: { env: TEST_PEM }
      webhookSecret: { env: TEST_SECRET }
accounts:
  - forge: github
    name: acme
    repositories:
      - name: agentic
        mode: agentic
      - name: slow-agent
        mode: agentic
        agent:
          timeout: 50m
  - forge: github
    name: globex
    runner:
      activeDeadlineSeconds: %d
`

func TestJobTimeouts(t *testing.T) {
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "s")
	timeoutConfigYAML := fmt.Sprintf(timeoutConfigYAMLTemplate, int64(jobtimeout.MaxRunnerDeadline.Seconds()))
	file := configfiletest.Load(t, timeoutConfigYAML)
	current := configfile.NewCurrent(file)
	acme, _ := file.Account(configfile.ForgeGitHub, "acme")
	globex, _ := file.Account(configfile.ForgeGitHub, "globex")
	acmeRepo := func(name string) string { return configfile.RepositoryID(acme.ID(), name) }
	globexRepo := configfile.RepositoryID(globex.ID(), "globex/app")

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
		// 15m runner + 15m lease wait + 15m publish; 15m + 60m to embed.
		{name: "single mode", accountID: acme.ID(), repositoryID: acmeRepo("acme/unlisted"), review: 45 * time.Minute, index: 75 * time.Minute, followUp: 30 * time.Minute},
		// The agent's 20m plus 5m of fetch headroom outlasts the runner deadline.
		{name: "agentic mode", accountID: acme.ID(), repositoryID: acmeRepo("acme/agentic"), review: 55 * time.Minute, index: 75 * time.Minute, followUp: 30 * time.Minute},
		{name: "agentic with a longer agent timeout", accountID: acme.ID(), repositoryID: acmeRepo("acme/slow-agent"),
			review: 85 * time.Minute, index: 75 * time.Minute, followUp: 30 * time.Minute},
		// The account's runner deadline is configfile's max allowed value; index lands exactly on MaxJobTimeout.
		{name: "account runner deadline at the max", accountID: globex.ID(), repositoryID: globexRepo,
			review: jobtimeout.MaxRunnerDeadline + jobtimeout.LeaseWaitHeadroom + jobtimeout.PublishHeadroom,
			index:  jobtimeout.MaxRunnerDeadline + jobtimeout.IndexWriteHeadroom, followUp: 30 * time.Minute},
		{name: "unknown account", accountID: "missing", repositoryID: "missing", review: 45 * time.Minute, index: 75 * time.Minute, followUp: 30 * time.Minute},
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
