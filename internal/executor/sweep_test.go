package executor

import (
	"errors"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestSweepSecrets(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-time.Hour))
	fresh := metav1.NewTime(time.Now().Add(-time.Minute))
	runnerLabel := map[string]string{"kritik.home-operations.com/role": "runner"}
	owned := []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "kritik-run-owned", UID: "u1"}}
	secret := func(name string, created metav1.Time, labels map[string]string, owners []metav1.OwnerReference) *corev1.Secret {
		return &corev1.Secret{
			Name: name, Namespace: "kritik", CreationTimestamp: created, Labels: labels, OwnerReferences: owners,
		}
	}
	client := fake.NewSimpleClientset(
		secret("kritik-run-orphan", old, runnerLabel, nil),
		secret("kritik-run-owned", old, runnerLabel, owned),
		secret("kritik-run-fresh", fresh, runnerLabel, nil),
		secret("kritik-postgres-runner", old, nil, nil),
	)
	n, err := newKube(client).SweepSecrets(t.Context(), OrphanSecretAge)
	if err != nil || n != 1 {
		t.Fatalf("SweepSecrets = %d, %v", n, err)
	}
	list, err := client.CoreV1().Secrets("kritik").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	left := make([]string, 0, len(list.Items))
	for _, s := range list.Items {
		left = append(left, s.Name)
	}
	slices.Sort(left)
	if want := []string{"kritik-postgres-runner", "kritik-run-fresh", "kritik-run-owned"}; !slices.Equal(left, want) {
		t.Fatalf("left = %v, want %v", left, want)
	}
}

func TestSweepSecretsToleratesALostRace(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-time.Hour))
	client := fake.NewSimpleClientset(&corev1.Secret{
		Name: "kritik-run-raced", Namespace: "kritik", CreationTimestamp: old,
		Labels: map[string]string{"kritik.home-operations.com/role": "runner"},
	})
	// The owner reference lands between the list and the delete.
	client.PrependReactor("delete", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "kritik-run-raced", errors.New("precondition failed"))
	})
	if n, err := newKube(client).SweepSecrets(t.Context(), OrphanSecretAge); err != nil || n != 0 {
		t.Fatalf("SweepSecrets = %d, %v", n, err)
	}
}
