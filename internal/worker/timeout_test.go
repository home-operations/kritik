package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/home-operations/kritik/internal/configfile"
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
tenants:
  - slug: acme
    installations:
      - name: acme-bot
        forge: github
        account: acme
        app:
          clientId: Iv1.x
          privateKey: { env: TEST_PEM }
          webhookSecret: { env: TEST_SECRET }
    repositories:
      - name: acme/agentic
        mode: agentic
      - name: acme/slow-agent
        mode: agentic
        agent:
          timeout: 50m
  - slug: globex
    runner:
      activeDeadlineSeconds: %d
    installations:
      - name: globex-bot
        forge: github
        account: globex
        app:
          clientId: Iv1.y
          privateKey: { env: TEST_PEM }
          webhookSecret: { env: TEST_SECRET }
`

func TestJobTimeouts(t *testing.T) {
	t.Setenv("TEST_PEM", "pem")
	t.Setenv("TEST_SECRET", "s")
	timeoutConfigYAML := fmt.Sprintf(timeoutConfigYAMLTemplate, int64(jobtimeout.MaxRunnerDeadline.Seconds()))
	file, err := configfile.Parse([]byte(timeoutConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	current := configfile.NewCurrent(file)
	acme, _ := file.Tenant("acme")
	globex, _ := file.Tenant("globex")
	acmeRepo := func(name string) string { return configfile.RepositoryID(acme.Installations[0].ID(), name) }
	globexRepo := configfile.RepositoryID(globex.Installations[0].ID(), "globex/app")

	review := &Review{Current: current, Deadline: 15 * time.Minute}
	index := &Index{Current: current, Deadline: 15 * time.Minute}
	followUp := &FollowUp{}
	tests := []struct {
		name         string
		tenantID     string
		repositoryID string
		review       time.Duration
		index        time.Duration
		followUp     time.Duration
	}{
		// 15m runner + 15m lease wait + 15m publish; 15m + 60m to embed.
		{name: "single mode", tenantID: acme.ID(), repositoryID: acmeRepo("acme/unlisted"), review: 45 * time.Minute, index: 75 * time.Minute, followUp: 30 * time.Minute},
		// The agent's 20m plus 5m of fetch headroom outlasts the runner deadline.
		{name: "agentic mode", tenantID: acme.ID(), repositoryID: acmeRepo("acme/agentic"), review: 55 * time.Minute, index: 75 * time.Minute, followUp: 30 * time.Minute},
		{name: "agentic with a longer agent timeout", tenantID: acme.ID(), repositoryID: acmeRepo("acme/slow-agent"),
			review: 85 * time.Minute, index: 75 * time.Minute, followUp: 30 * time.Minute},
		// The tenant's runner deadline is configfile's max allowed value; index lands exactly on MaxJobTimeout.
		{name: "tenant runner deadline at the max", tenantID: globex.ID(), repositoryID: globexRepo,
			review: jobtimeout.MaxRunnerDeadline + jobtimeout.LeaseWaitHeadroom + jobtimeout.PublishHeadroom,
			index:  jobtimeout.MaxRunnerDeadline + jobtimeout.IndexWriteHeadroom, followUp: 30 * time.Minute},
		{name: "unknown tenant", tenantID: "missing", repositoryID: "missing", review: 45 * time.Minute, index: 75 * time.Minute, followUp: 30 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.review <= time.Minute || tt.index <= time.Minute || tt.followUp <= time.Minute {
				t.Fatal("a job timeout must outlast River's one-minute default")
			}
			got := review.Timeout(&river.Job[jobs.ReviewArgs]{Args: jobs.ReviewArgs{TenantID: tt.tenantID, RepositoryID: tt.repositoryID}})
			if got != tt.review {
				t.Errorf("review timeout = %s, want %s", got, tt.review)
			}
			got = index.Timeout(&river.Job[jobs.IndexArgs]{Args: jobs.IndexArgs{TenantID: tt.tenantID, RepositoryID: tt.repositoryID}})
			if got != tt.index {
				t.Errorf("index timeout = %s, want %s", got, tt.index)
			}
			got = followUp.Timeout(&river.Job[jobs.FollowUpArgs]{Args: jobs.FollowUpArgs{TenantID: tt.tenantID, RepositoryID: tt.repositoryID}})
			if got != tt.followUp {
				t.Errorf("follow-up timeout = %s, want %s", got, tt.followUp)
			}
		})
	}
	if RescueStuckJobsAfter <= MaxJobTimeout {
		t.Fatal("rescue must wait out the longest job timeout")
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
