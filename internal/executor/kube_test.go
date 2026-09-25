package executor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/home-operations/kritik/internal/runner"
)

func spec() Spec {
	return Spec{
		RunID:       "0123456789abcdef-run",
		Labels:      map[string]string{"tenant": "onedr0p", "pr": "42", "kind": "review"},
		Annotations: map[string]string{"head-sha": "aaa"},
		Params: runner.Params{
			RunID: "0123456789abcdef-run", CloneURL: "https://x/y.git", Token: "ghs_x", Head: "aaa", Base: "bbb",
			Ignore: []string{"vendor/**", "**/*.lock"},
		},
		Deadline:  5 * time.Minute,
		Resources: map[string]any{"limits": map[string]any{"memory": "2Gi"}},
	}
}

func TestJobSpec(t *testing.T) {
	k := &Kube{Namespace: "kritik", Image: "ttl.sh/x:1h", ServiceAccount: "kritik-runner", DatabaseSecret: "kritik-postgres-runner", DatabaseSecretKey: "uri", TTL: 10 * time.Minute}
	j := k.job(spec())
	if j.Name != "kritik-run-01234567" || j.Namespace != "kritik" {
		t.Fatalf("name/namespace = %s/%s", j.Name, j.Namespace)
	}
	if *j.Spec.ActiveDeadlineSeconds != 300 || *j.Spec.TTLSecondsAfterFinished != 600 || *j.Spec.BackoffLimit != 0 {
		t.Fatalf("deadline/ttl/backoff = %d/%d/%d", *j.Spec.ActiveDeadlineSeconds, *j.Spec.TTLSecondsAfterFinished, *j.Spec.BackoffLimit)
	}
	if j.Labels["kritik.home-operations.com/tenant"] != "onedr0p" || j.Annotations["kritik.home-operations.com/head-sha"] != "aaa" ||
		j.Spec.Template.Labels["kritik.home-operations.com/role"] != "runner" {
		t.Fatalf("labels/annotations = %v %v", j.Labels, j.Annotations)
	}
	pod := j.Spec.Template.Spec
	if pod.ServiceAccountName != "kritik-runner" || *pod.AutomountServiceAccountToken || pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("pod spec = %+v", pod)
	}
	c := pod.Containers[0]
	if c.Image != "ttl.sh/x:1h" || len(c.Args) != 2 || c.Args[1] != "runner" {
		t.Fatalf("container = %+v", c)
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	// The token comes from the per-run Secret, never from the Job spec.
	if tok := env["KRITIK_GIT_TOKEN"]; tok.Value != "" || tok.ValueFrom == nil || tok.ValueFrom.SecretKeyRef.Name != j.Name || tok.ValueFrom.SecretKeyRef.Key != tokenKey {
		t.Fatalf("token env = %+v", tok)
	}
	if env["KRITIK_HEAD_SHA"].Value != "aaa" || env["KRITIK_BASE_SHA"].Value != "bbb" ||
		env["KRITIK_IGNORE"].Value != "vendor/**,**/*.lock" {
		t.Fatalf("env = %v", env)
	}
	if ref := env["KRITIK_DATABASE_URL"].ValueFrom.SecretKeyRef; ref.Name != "kritik-postgres-runner" || ref.Key != "uri" {
		t.Fatalf("db env = %+v", ref)
	}
	for _, name := range []string{"KRITIK_EMBED_API_KEY", "KRITIK_DATABASE_OWNER_URL", "OPENROUTER_API_KEY"} {
		if _, leaked := env[name]; leaked {
			t.Fatalf("%s must never reach a runner pod", name)
		}
	}
	if c.Resources.Limits.Memory().String() != "2Gi" {
		t.Fatalf("resources = %+v", c.Resources)
	}
	if !*c.SecurityContext.ReadOnlyRootFilesystem || !*pod.SecurityContext.RunAsNonRoot {
		t.Fatal("runner pod must be read-only and non-root")
	}
}

func TestKubeRunWaitsForCompletion(t *testing.T) {
	client := fake.NewSimpleClientset()
	k := &Kube{Client: client, Namespace: "kritik", Image: "img", ServiceAccount: "sa", DatabaseSecret: "s", DatabaseSecretKey: "uri", Poll: 10 * time.Millisecond}
	ctx := t.Context()
	done := make(chan Result, 1)
	go func() { done <- k.Run(ctx, spec()) }()

	// Let the Job get created, then mark it succeeded with a finished pod.
	var name string
	for i := 0; i < 100 && name == ""; i++ {
		time.Sleep(5 * time.Millisecond)
		jobs, _ := client.BatchV1().Jobs("kritik").List(ctx, metav1.ListOptions{})
		if len(jobs.Items) == 1 {
			name = jobs.Items[0].Name
		}
	}
	if name == "" {
		t.Fatal("job was not created")
	}
	_, _ = client.CoreV1().Pods("kritik").Create(ctx, &corev1.Pod{
		Name: name + "-abcde", Namespace: "kritik", Labels: map[string]string{"job-name": name},
		Spec: corev1.PodSpec{NodeName: "k8s-1"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}}}}},
	}, metav1.CreateOptions{})
	j, _ := client.BatchV1().Jobs("kritik").Get(ctx, name, metav1.GetOptions{})
	j.Status.Succeeded = 1
	_, _ = client.BatchV1().Jobs("kritik").UpdateStatus(ctx, j, metav1.UpdateOptions{})

	select {
	case res := <-done:
		if res.Err != nil || res.JobName != name || res.NodeName != "k8s-1" || res.TerminationReason != "Completed" {
			t.Fatalf("result = %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the job succeeded")
	}
	// The token Secret exists, holds the token, and is owned by the Job so
	// the TTL takes it along.
	sec, err := client.CoreV1().Secrets("kritik").Get(ctx, name, metav1.GetOptions{})
	if err != nil || sec.StringData[tokenKey] != "ghs_x" {
		t.Fatalf("token secret = %+v, %v", sec, err)
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "Job" || sec.OwnerReferences[0].Name != name {
		t.Fatalf("token secret owners = %+v", sec.OwnerReferences)
	}
}

func TestKubeRunDeletesTheJobWhenCancelled(t *testing.T) {
	client := fake.NewSimpleClientset()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	k := &Kube{
		Client: client, Namespace: "kritik", Image: "img", ServiceAccount: "sa", DatabaseSecret: "s", DatabaseSecretKey: "uri",
		Poll: 10 * time.Millisecond, Logger: logger,
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan Result, 1)
	go func() { done <- k.Run(ctx, spec()) }()
	for i := 0; i < 100; i++ {
		time.Sleep(5 * time.Millisecond)
		if jobs, _ := client.BatchV1().Jobs("kritik").List(t.Context(), metav1.ListOptions{}); len(jobs.Items) == 1 {
			break
		}
	}
	cancel()
	select {
	case res := <-done:
		if !errors.Is(res.Err, context.Canceled) {
			t.Fatalf("result = %+v; want the cancellation surfaced", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	jobs, _ := client.BatchV1().Jobs("kritik").List(t.Context(), metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Fatalf("the orphaned Job must be deleted, %d left", len(jobs.Items))
	}
}

func TestKubeRunReportsFailure(t *testing.T) {
	client := fake.NewSimpleClientset()
	k := &Kube{Client: client, Namespace: "kritik", Image: "img", ServiceAccount: "sa", DatabaseSecret: "s", DatabaseSecretKey: "uri", Poll: 10 * time.Millisecond}
	ctx := t.Context()
	done := make(chan Result, 1)
	go func() { done <- k.Run(ctx, spec()) }()
	var j *batchv1.Job
	for i := 0; i < 100 && j == nil; i++ {
		time.Sleep(5 * time.Millisecond)
		jobs, _ := client.BatchV1().Jobs("kritik").List(ctx, metav1.ListOptions{})
		if len(jobs.Items) == 1 {
			j = &jobs.Items[0]
		}
	}
	j.Status.Failed = 1
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded"}}
	_, _ = client.BatchV1().Jobs("kritik").UpdateStatus(ctx, j, metav1.UpdateOptions{})
	res := <-done
	if res.Err == nil {
		t.Fatal("a failed job must produce an error")
	}
}

var _ = context.Background
