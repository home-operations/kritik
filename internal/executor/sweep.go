package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/home-operations/kritik/internal/jobtimeout"
)

const (
	// RunSecretSettle is how old a run must be before its Secret is swept:
	// far longer than the moment between creating a run's Secret and
	// making its Job the owner.
	RunSecretSettle = 15 * time.Minute
	// RunSecretAbandoned is how long a run with no recorded end is left
	// before its Secret is swept. Past it River has cancelled the job that
	// ran it, and any Job a dead worker left has passed its deadline.
	RunSecretAbandoned = jobtimeout.MaxJobTimeout
	// sweepBatch bounds the runs one tenant's sweep visits per pass.
	sweepBatch = 200
)

// RunSecretStore finds the runs whose Secret the sweep should delete and
// records the ones it did.
type RunSecretStore interface {
	RunSecretsToSweep(ctx context.Context, tenantID string, settle, abandoned time.Duration, limit int) ([]string, error)
	MarkRunSecretsSwept(ctx context.Context, tenantID string, runIDs []string) error
}

// DeleteRunSecret deletes a run's job-scoped Secret by name. A Secret that
// is already gone, with its Job or by an earlier sweep, counts as deleted.
func (k *Kube) DeleteRunSecret(ctx context.Context, runID string) error {
	name := jobName(runID)
	err := k.Client.CoreV1().Secrets(k.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("executor: delete run secret %s: %w", name, err)
	}
	return nil
}

// SweepRunSecrets deletes, by name, the Secret of every run the store says
// no longer needs one, for each tenant, and marks those runs swept. A
// worker that dies between creating a run's Secret and setting its owner
// reference leaves one behind, credentials included, that garbage
// collection will never remove; the sweep never lists or reads a Secret to
// find it. It returns how many runs it marked.
func (k *Kube) SweepRunSecrets(ctx context.Context, st RunSecretStore, tenantIDs []string) (int, error) {
	swept := 0
	var errs []error
	for _, tenantID := range tenantIDs {
		ids, err := st.RunSecretsToSweep(ctx, tenantID, RunSecretSettle, RunSecretAbandoned, sweepBatch)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		done := make([]string, 0, len(ids))
		for _, id := range ids {
			if err := k.DeleteRunSecret(ctx, id); err != nil {
				errs = append(errs, err)
				continue
			}
			done = append(done, id)
		}
		if err := st.MarkRunSecretsSwept(ctx, tenantID, done); err != nil {
			errs = append(errs, err)
			continue
		}
		swept += len(done)
	}
	return swept, errors.Join(errs...)
}

// RunSecretSweeper sweeps run Secrets now and then every interval until
// ctx ends, for the tenants tenantIDs returns at each pass. A failed sweep
// is logged and tried again next time.
func (k *Kube) RunSecretSweeper(ctx context.Context, st RunSecretStore, tenantIDs func() []string, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		n, err := k.SweepRunSecrets(ctx, st, tenantIDs())
		switch {
		case err != nil && ctx.Err() == nil:
			k.logger().Warn("runner secrets not swept", "error", err)
		case n > 0:
			k.logger().Info("runner secrets swept", "runs", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
