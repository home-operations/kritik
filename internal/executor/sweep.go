package executor

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OrphanSecretAge is how old a run Secret no Job owns must be before it is
// swept: far longer than the moment between creating a run's Secret and
// making its Job the owner.
const OrphanSecretAge = 15 * time.Minute

// SweepSecrets deletes run Secrets that no Job owns and that are older than
// age. A worker that dies between creating a run's Secret and setting its
// owner reference leaves one behind, credentials included, that garbage
// collection will never remove. It returns how many it deleted.
func (k *Kube) SweepSecrets(ctx context.Context, age time.Duration) (int, error) {
	secrets := k.Client.CoreV1().Secrets(k.Namespace)
	list, err := secrets.List(ctx, metav1.ListOptions{LabelSelector: "kritik.home-operations.com/role=" + runnerRole})
	if err != nil {
		return 0, fmt.Errorf("executor: list run secrets: %w", err)
	}
	cutoff := time.Now().Add(-age)
	deleted := 0
	for i := range list.Items {
		s := &list.Items[i]
		if len(s.OwnerReferences) > 0 || !s.CreationTimestamp.Time.Before(cutoff) {
			continue
		}
		// The preconditions keep a Secret an owner was set on since the
		// list from being deleted under its Job.
		err := secrets.Delete(ctx, s.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &s.UID, ResourceVersion: &s.ResourceVersion},
		})
		switch {
		case err == nil:
			deleted++
		case apierrors.IsNotFound(err) || apierrors.IsConflict(err):
		default:
			return deleted, fmt.Errorf("executor: delete orphaned secret %s: %w", s.Name, err)
		}
	}
	return deleted, nil
}

// RunSecretSweeper sweeps orphaned run Secrets now and then every interval
// until ctx ends. A failed sweep is logged and tried again next time.
func (k *Kube) RunSecretSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		n, err := k.SweepSecrets(ctx, OrphanSecretAge)
		switch {
		case err != nil && ctx.Err() == nil:
			k.logger().Warn("orphaned runner secrets not swept", "error", err)
		case n > 0:
			k.logger().Info("orphaned runner secrets deleted", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
