package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/home-operations/kritik/internal/runner"
)

const (
	headSHA = "0123456789abcdef0123456789abcdef01234567"
	baseSHA = "89abcdef0123456789abcdef0123456789abcdef"
)

func spec() Spec {
	return Spec{
		RunID:       "0123456789abcdef-run",
		Labels:      map[string]string{"tenant": "acme", "pr": "42", "kind": "review"},
		Annotations: map[string]string{"head-sha": headSHA},
		Job: runner.Spec{
			Version: runner.SpecVersion, Kind: runner.KindReview, RunID: "0123456789abcdef-run",
			CloneURL: "https://forge.example.com/acme/widgets.git", Head: headSHA, Base: baseSHA,
			Ignore: []string{"vendor/**", "**/*.lock"},
		},
		Secrets:   runner.Secrets{GitToken: "ghs_secret_token", ModelAPIKey: "sk-model-key"},
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
	if j.Labels["kritik.home-operations.com/tenant"] != "acme" || j.Annotations["kritik.home-operations.com/head-sha"] != headSHA ||
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
	checkRunnerEnv(t, c.Env)
	checkSpecMount(t, pod, c)
	if c.Resources.Limits.Memory().String() != "2Gi" {
		t.Fatalf("resources = %+v", c.Resources)
	}
	if !*c.SecurityContext.ReadOnlyRootFilesystem || !*pod.SecurityContext.RunAsNonRoot {
		t.Fatal("runner pod must be read-only and non-root")
	}
}

// checkRunnerEnv asserts the runner gets its job document, its credentials
// only through secret references, and nothing it must never see.
func checkRunnerEnv(t *testing.T, vars []corev1.EnvVar) {
	t.Helper()
	env := map[string]corev1.EnvVar{}
	for _, e := range vars {
		env[e.Name] = e
	}
	if e, ok := env["KRITIK_RUN_SPEC_FILE"]; !ok || e.Value != "/var/run/kritik/spec.json" {
		t.Fatalf("KRITIK_RUN_SPEC_FILE = %+v", e)
	}
	if _, ok := env["KRITIK_RUN_SPEC"]; ok {
		t.Fatal("the job document must not travel as a variable")
	}
	for _, e := range vars {
		if strings.Contains(e.Value, "ghs_secret_token") || strings.Contains(e.Value, "sk-model-key") {
			t.Fatalf("%s carries a secret in plain text", e.Name)
		}
	}
	for name, want := range map[string]struct {
		key      string
		optional bool
	}{"KRITIK_GIT_TOKEN": {"git-token", false}, "KRITIK_MODEL_API_KEY": {"model-api-key", true}} {
		ref := env[name].ValueFrom
		if ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != "kritik-run-01234567" || ref.SecretKeyRef.Key != want.key ||
			(ref.SecretKeyRef.Optional != nil && *ref.SecretKeyRef.Optional) != want.optional {
			t.Fatalf("%s = %+v", name, env[name])
		}
	}
	for _, name := range []string{"KRITIK_RUN_KIND", "KRITIK_RUN_ID", "KRITIK_CLONE_URL", "KRITIK_HEAD_SHA", "KRITIK_BASE_SHA", "KRITIK_IGNORE"} {
		if _, ok := env[name]; ok {
			t.Fatalf("%s is replaced by the job document", name)
		}
	}
	if ref := env["KRITIK_DATABASE_URL"].ValueFrom.SecretKeyRef; ref.Name != "kritik-postgres-runner" || ref.Key != "uri" {
		t.Fatalf("db env = %+v", ref)
	}
	for _, name := range []string{"KRITIK_EMBED_API_KEY", "KRITIK_DATABASE_OWNER_URL", "OPENROUTER_API_KEY"} {
		if _, leaked := env[name]; leaked {
			t.Fatalf("%s must never reach a runner pod", name)
		}
	}
}

// checkSpecMount asserts the job document is mounted read-only from the
// run's Secret, and only that key of it.
func checkSpecMount(t *testing.T, pod corev1.PodSpec, c corev1.Container) {
	t.Helper()
	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.Name == "spec" {
			mounted = m.MountPath == "/var/run/kritik" && m.ReadOnly
		}
	}
	var vol *corev1.SecretVolumeSource
	for _, v := range pod.Volumes {
		if v.Name == "spec" {
			vol = v.Secret
		}
	}
	if !mounted || vol == nil || vol.SecretName != "kritik-run-01234567" || len(vol.Items) != 1 ||
		vol.Items[0].Key != "run-spec.json" || vol.Items[0].Path != "spec.json" {
		t.Fatalf("spec mount = %v, volume = %+v", mounted, vol)
	}
}

func TestKubeRunRejectsOversizedSpecBeforeCreatingAnything(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := spec()
	s.Job.RepoFiles = []string{strings.Repeat("<", runner.MaxSpecBytes)}
	res := newKube(client).Run(t.Context(), s)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "byte limit") {
		t.Fatalf("err = %v", res.Err)
	}
	if n := len(client.Actions()); n != 0 {
		t.Fatalf("%d API calls for a spec that cannot be delivered", n)
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
}

// TestKubeRunDeletesTheJobWhenCancelled checks the Job is actually gone
// after a plain cancellation, not only that a delete was sent.
func TestKubeRunDeletesTheJobWhenCancelled(t *testing.T) {
	client := fake.NewSimpleClientset()
	k := newKube(client)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan Result, 1)
	go func() { done <- k.Run(ctx, spec()) }()
	waitJob(t, client)
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

func newKube(client *fake.Clientset) *Kube {
	return &Kube{Client: client, Namespace: "kritik", Image: "img", ServiceAccount: "sa", DatabaseSecret: "s", DatabaseSecretKey: "uri", Poll: 10 * time.Millisecond}
}

// waitJob returns the Job once Run has created it.
func waitJob(t *testing.T, client *fake.Clientset) *batchv1.Job {
	t.Helper()
	for range 400 {
		jobs, _ := client.BatchV1().Jobs("kritik").List(t.Context(), metav1.ListOptions{})
		if len(jobs.Items) == 1 {
			return &jobs.Items[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job was not created")
	return nil
}

func TestKubeRunSecretLifecycle(t *testing.T) {
	client := fake.NewSimpleClientset()
	k := newKube(client)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan Result, 1)
	go func() { done <- k.Run(ctx, spec()) }()
	j := waitJob(t, client)

	var sec *corev1.Secret
	for range 400 {
		s, err := client.CoreV1().Secrets("kritik").Get(t.Context(), j.Name, metav1.GetOptions{})
		if err == nil && len(s.OwnerReferences) == 1 {
			sec = s
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sec == nil {
		t.Fatal("secret was not given an owner reference")
	}
	if string(sec.Data["git-token"]) != "ghs_secret_token" || string(sec.Data["model-api-key"]) != "sk-model-key" {
		t.Fatalf("secret data = %v", sec.Data)
	}
	got, err := runner.DecodeSpec(sec.Data["run-spec.json"])
	if err != nil || got.Head != headSHA || got.Base != baseSHA || strings.Join(got.Ignore, ",") != "vendor/**,**/*.lock" {
		t.Fatalf("run spec = %+v, %v", got, err)
	}
	if sec.Labels["kritik.home-operations.com/role"] != "runner" || sec.Labels["kritik.home-operations.com/tenant"] != "acme" {
		t.Fatalf("secret labels = %v", sec.Labels)
	}
	ref := sec.OwnerReferences[0]
	if ref.Kind != "Job" || ref.APIVersion != "batch/v1" || ref.Name != j.Name ||
		ref.Controller == nil || *ref.Controller || ref.BlockOwnerDeletion == nil || *ref.BlockOwnerDeletion {
		t.Fatalf("owner reference = %+v", ref)
	}

	var order []string
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" || a.GetVerb() == "patch" {
			order = append(order, a.GetVerb()+" "+a.GetResource().Resource)
		}
	}
	if strings.Join(order, ",") != "create secrets,create jobs,patch secrets" {
		t.Fatalf("order = %v", order)
	}
	cancel()
	<-done
}

func TestKubeRunDeletesSecretWhenJobCreateFails(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("quota exceeded")
	})
	res := newKube(client).Run(t.Context(), spec())
	if res.Err == nil || !strings.Contains(res.Err.Error(), "quota exceeded") {
		t.Fatalf("err = %v", res.Err)
	}
	secrets, _ := client.CoreV1().Secrets("kritik").List(t.Context(), metav1.ListOptions{})
	if len(secrets.Items) != 0 {
		t.Fatalf("secret left behind: %v", secrets.Items)
	}
}

func TestKubeRunCancelDeletesJobInForeground(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "log" {
			return true, &runtime.Unknown{Raw: []byte("cloning with ghs_secret_token and sk-model-key\n")}, nil
		}
		return false, nil, nil
	})
	k := newKube(client)
	cause := errors.New("superseded")
	ctx, cancel := context.WithCancelCause(t.Context())
	done := make(chan Result, 1)
	go func() { done <- k.Run(ctx, spec()) }()
	j := waitJob(t, client)
	_, _ = client.CoreV1().Pods("kritik").Create(t.Context(), &corev1.Pod{
		Name: j.Name + "-abcde", Namespace: "kritik", Labels: map[string]string{"job-name": j.Name},
	}, metav1.CreateOptions{})
	cancel(cause)

	var res Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if !errors.Is(res.Err, cause) {
		t.Fatalf("err = %v, want the cancellation cause", res.Err)
	}
	var deleted bool
	for _, a := range client.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok && a.GetResource().Resource == "jobs" && d.GetName() == j.Name {
			p := d.GetDeleteOptions().PropagationPolicy
			deleted = p != nil && *p == metav1.DeletePropagationForeground
		}
	}
	if !deleted {
		t.Fatal("the Job must be deleted with foreground propagation")
	}
	if strings.Contains(res.LogTail, "ghs_secret_token") || strings.Contains(res.LogTail, "sk-model-key") || !strings.Contains(res.LogTail, "***") {
		t.Fatalf("log tail not masked: %q", res.LogTail)
	}
}

func TestKubeRunRejectsInvalidSpecBeforeCreatingAnything(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := spec()
	s.Job.Head = "not-a-sha"
	res := newKube(client).Run(t.Context(), s)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "head") {
		t.Fatalf("err = %v", res.Err)
	}
	if actions := client.Actions(); len(actions) != 0 {
		t.Fatalf("an invalid spec must create nothing, got %v", actions)
	}
}

func TestKubeRunDeletesJobWhenCreateFailsOnCancel(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(t.Context())
	client.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		// The server may have persisted the Job before the caller gave up.
		cancel()
		return true, nil, context.Canceled
	})
	res := newKube(client).Run(ctx, spec())
	if res.Err == nil {
		t.Fatal("a failed create must produce an error")
	}
	var jobDeleted, secretDeleted bool
	for _, a := range client.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok && d.GetName() == "kritik-run-01234567" {
			switch a.GetResource().Resource {
			case "jobs":
				jobDeleted = true
			case "secrets":
				secretDeleted = true
			}
		}
	}
	if !jobDeleted || !secretDeleted {
		t.Fatalf("job deleted = %v, secret deleted = %v", jobDeleted, secretDeleted)
	}
}

func TestKubeRunCleansUpWhenOwnerPatchFails(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("patch", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("conflict")
	})
	res := newKube(client).Run(t.Context(), spec())
	if res.Err == nil || !strings.Contains(res.Err.Error(), "conflict") {
		t.Fatalf("err = %v", res.Err)
	}
	jobs, _ := client.BatchV1().Jobs("kritik").List(t.Context(), metav1.ListOptions{})
	secrets, _ := client.CoreV1().Secrets("kritik").List(t.Context(), metav1.ListOptions{})
	if len(jobs.Items) != 0 || len(secrets.Items) != 0 {
		t.Fatalf("left behind: %d jobs, %d secrets", len(jobs.Items), len(secrets.Items))
	}
}

func TestFinishKeepsTheTailOfTheLog(t *testing.T) {
	const token = "ghs_secret_token"
	secrets := runner.Secrets{GitToken: token, ModelAPIKey: "sk-model-key"}
	// A secret straddles the point LogTailBytes from the end.
	straddling := "HEAD " + strings.Repeat("x", 1000) + token + strings.Repeat("y", LogTailBytes-8) + " END\n"
	tests := []struct {
		name  string
		logs  string
		check func(t *testing.T, tail string)
	}{
		{name: "the end is kept and a straddling secret masked", logs: straddling, check: func(t *testing.T, tail string) {
			if len(tail) > LogTailBytes || !strings.HasSuffix(tail, " END\n") || strings.Contains(tail, "HEAD") ||
				strings.Contains(tail, "ghs_") || strings.Contains(tail, "_token") {
				t.Fatalf("tail len %d starts %q ends %q", len(tail), tail[:20], tail[len(tail)-20:])
			}
		}},
		{name: "a short log is kept whole", logs: "fetched\ndone\n", check: func(t *testing.T, tail string) {
			if tail != "fetched\ndone\n" {
				t.Fatalf("tail = %q", tail)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			var opts *corev1.PodLogOptions
			client.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if a.GetSubresource() != "log" {
					return false, nil, nil
				}
				opts, _ = a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
				return true, &runtime.Unknown{Raw: []byte(tt.logs)}, nil
			})
			_, _ = client.CoreV1().Pods("kritik").Create(t.Context(), &corev1.Pod{
				Name: "kritik-run-01234567-abcde", Namespace: "kritik", Labels: map[string]string{"job-name": "kritik-run-01234567"},
			}, metav1.CreateOptions{})
			res := Result{JobName: "kritik-run-01234567"}
			newKube(client).finish(&res, secrets)
			if opts == nil || opts.TailLines == nil || *opts.TailLines != logTailLines || opts.LimitBytes == nil || *opts.LimitBytes != logReadBytes {
				t.Fatalf("log options = %+v", opts)
			}
			tt.check(t, res.LogTail)
		})
	}
}

func TestLogTailDropsALineCutAtTheCeiling(t *testing.T) {
	const token = "ghs_secret_token"
	// The read stopped inside the secret on its last line.
	log := "first\nsecond\nclone with ghs_secr"
	got := logTail(log, true, runner.Secrets{GitToken: token})
	if got != "first\nsecond\n" {
		t.Fatalf("tail = %q", got)
	}
	if got := logTail(log, false, runner.Secrets{GitToken: token}); got != log {
		t.Fatalf("a read under the ceiling is kept whole, got %q", got)
	}
}

func TestTailKeepsValidUTF8(t *testing.T) {
	got := tail("ab€cd", 4)
	if got != "cd" || !utf8.ValidString(got) {
		t.Fatalf("tail = %q", got)
	}
}
