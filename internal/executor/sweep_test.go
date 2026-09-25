package executor

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	runA = "aaaaaaaa-0000-0000-0000-000000000000"
	runB = "bbbbbbbb-0000-0000-0000-000000000000"
	runC = "cccccccc-0000-0000-0000-000000000000"
)

// fakeRunStore hands out each tenant's pending runs and records the marks.
type fakeRunStore struct {
	pending map[string][]string
	marked  map[string][]string
	listErr error
}

func (f *fakeRunStore) RunSecretsToSweep(_ context.Context, tenantID string, settle, abandoned time.Duration, _ int) ([]string, error) {
	if settle != RunSecretSettle || abandoned != RunSecretAbandoned {
		return nil, errors.New("unexpected sweep bounds")
	}
	return f.pending[tenantID], f.listErr
}

func (f *fakeRunStore) MarkRunSecretsSwept(_ context.Context, tenantID string, ids []string) error {
	f.marked[tenantID] = append(f.marked[tenantID], ids...)
	return nil
}

func runSecret(runID string) *corev1.Secret {
	return &corev1.Secret{Name: jobName(runID), Namespace: "kritik"}
}

func secretNames(t *testing.T, client *fake.Clientset) []string {
	t.Helper()
	list, err := client.CoreV1().Secrets("kritik").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list.Items))
	for _, s := range list.Items {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	return names
}

func TestDeleteRunSecret(t *testing.T) {
	tests := []struct {
		name    string
		objects []runtime.Object
		failure error
		wantErr bool
	}{
		{name: "present", objects: []runtime.Object{runSecret(runA)}},
		{name: "already gone", objects: nil},
		{name: "api failure", objects: []runtime.Object{runSecret(runA)}, failure: apierrors.NewForbidden(corev1.Resource("secrets"), "x", errors.New("no")), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.objects...)
			if tt.failure != nil {
				client.PrependReactor("delete", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.failure
				})
			}
			k := newKube(client)
			for range 2 {
				if err := k.DeleteRunSecret(t.Context(), runA); (err != nil) != tt.wantErr {
					t.Fatalf("DeleteRunSecret = %v, wantErr %v", err, tt.wantErr)
				}
			}
			if tt.failure == nil && len(secretNames(t, client)) != 0 {
				t.Fatalf("secret left behind: %v", secretNames(t, client))
			}
		})
	}
}

func TestSweepRunSecrets(t *testing.T) {
	client := fake.NewClientset(runSecret(runA), runSecret(runC), &corev1.Secret{Name: "kritik-postgres-runner", Namespace: "kritik"})
	st := &fakeRunStore{
		pending: map[string][]string{"alpha": {runA, runB}, "beta": {runC}},
		marked:  map[string][]string{},
	}
	n, err := newKube(client).SweepRunSecrets(t.Context(), st, []string{"alpha", "beta"})
	if err != nil || n != 3 {
		t.Fatalf("SweepRunSecrets = %d, %v", n, err)
	}
	// The sweep reads no Secret: the worker's Role grants neither get nor list.
	for _, a := range client.Actions() {
		if a.GetResource().Resource == "secrets" && a.GetVerb() != "delete" {
			t.Fatalf("sweep issued %s on secrets", a.GetVerb())
		}
	}
	if want := map[string][]string{"alpha": {runA, runB}, "beta": {runC}}; !maps.EqualFunc(st.marked, want, slices.Equal) {
		t.Fatalf("marked = %v, want %v", st.marked, want)
	}
	if got, want := secretNames(t, client), []string{"kritik-postgres-runner"}; !slices.Equal(got, want) {
		t.Fatalf("left = %v, want %v", got, want)
	}
}

func TestSweepRunSecretsMarksOnlyDeleted(t *testing.T) {
	client := fake.NewClientset(runSecret(runA), runSecret(runB))
	client.PrependReactor("delete", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.DeleteAction).GetName() == jobName(runB) {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	st := &fakeRunStore{pending: map[string][]string{"alpha": {runA, runB}}, marked: map[string][]string{}}
	n, err := newKube(client).SweepRunSecrets(t.Context(), st, []string{"alpha"})
	if err == nil || n != 1 {
		t.Fatalf("SweepRunSecrets = %d, %v; want 1 and an error", n, err)
	}
	if got := st.marked["alpha"]; !slices.Equal(got, []string{runA}) {
		t.Fatalf("marked = %v, want only the deleted run", got)
	}
}
