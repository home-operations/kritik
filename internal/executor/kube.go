package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/home-operations/kritik/internal/runner"
)

// runnerRole is the --role value and container name of a runner pod.
const runnerRole = "runner"

// Kube runs each spec as a Kubernetes Job in the worker's own namespace.
type Kube struct {
	Client    kubernetes.Interface
	Namespace string
	// Image is the kritik image the Job runs, normally the worker's own.
	Image string
	// ServiceAccount is the permissionless runner service account.
	ServiceAccount string
	// DatabaseSecret and DatabaseSecretKey reference the Secret holding the
	// runner role's DSN, injected as KRITIK_DATABASE_URL.
	DatabaseSecret, DatabaseSecretKey string
	// TTL is ttlSecondsAfterFinished; the run row outlives the Job.
	TTL time.Duration
	// Poll is how often the Job is checked.
	Poll   time.Duration
	Logger *slog.Logger
}

// NewKubeInCluster builds a Kube executor from the pod's service account.
func NewKubeInCluster() (kubernetes.Interface, string, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, "", fmt.Errorf("executor: in-cluster config: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("executor: kubernetes client: %w", err)
	}
	ns, err := namespaceFromServiceAccount()
	if err != nil {
		return nil, "", err
	}
	return client, ns, nil
}

// Run implements Executor: create the run's Secret and Job, wait for the
// Job, capture the pod's log tail, and report. A finished Job is left to its
// TTL, and its Secret goes with it. When ctx ends first the Job is deleted,
// pod included, and the result's error is ctx's cause.
func (k *Kube) Run(ctx context.Context, spec Spec) Result {
	if err := spec.Job.Validate(); err != nil {
		return Result{Err: fmt.Errorf("executor: %w", err)}
	}
	name := jobName(spec.RunID)
	runSpec, err := runner.EncodeSpec(spec.Job)
	if err != nil {
		return Result{Err: fmt.Errorf("executor: %w", err)}
	}
	job := k.job(spec)
	secrets := k.Client.CoreV1().Secrets(k.Namespace)
	if _, err := secrets.Create(ctx, k.secret(spec, runSpec), metav1.CreateOptions{}); err != nil {
		return Result{Err: fmt.Errorf("executor: create secret: %w", err)}
	}
	created, err := k.Client.BatchV1().Jobs(k.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		if ctx.Err() != nil {
			// The server may have persisted the Job before the call gave up.
			k.deleteJob(name)
		}
		k.deleteSecret(name)
		return Result{Err: fmt.Errorf("executor: create job: %w", err)}
	}
	res := Result{JobName: created.Name}
	if err := k.own(ctx, created); err != nil {
		k.deleteJob(created.Name)
		k.deleteSecret(name)
		res.Err = err
		return res
	}
	poll := k.Poll
	if poll <= 0 {
		poll = 3 * time.Second
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			k.cancel(ctx, &res, spec.Secrets)
			return res
		case <-t.C:
		}
		j, err := k.Client.BatchV1().Jobs(k.Namespace).Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			if ctx.Err() != nil {
				k.cancel(ctx, &res, spec.Secrets)
				return res
			}
			res.Err = fmt.Errorf("executor: get job: %w", err)
			return res
		}
		if j.Status.Succeeded > 0 || j.Status.Failed > 0 || jobFinished(j) {
			k.finish(&res, spec.Secrets)
			if j.Status.Succeeded == 0 {
				res.Err = fmt.Errorf("executor: job %s failed: %s", created.Name, res.TerminationReason)
			}
			return res
		}
	}
}

// own makes the Job the Secret's owner so garbage collection deletes the
// Secret with the Job. It is not the controller and must not block the
// Job's deletion.
func (k *Kube) own(ctx context.Context, job *batchv1.Job) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": []metav1.OwnerReference{{
		APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID,
		Controller: new(false), BlockOwnerDeletion: new(false),
	}}}})
	if err != nil {
		return fmt.Errorf("executor: encode owner reference: %w", err)
	}
	if _, err := k.Client.CoreV1().Secrets(k.Namespace).Patch(ctx, job.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("executor: own secret: %w", err)
	}
	return nil
}

// cancel deletes the Job after ctx ended, then records what the pod got to.
func (k *Kube) cancel(ctx context.Context, res *Result, secrets runner.Secrets) {
	res.Err = context.Cause(ctx)
	k.deleteJob(res.JobName)
	k.finish(res, secrets)
}

// deleteJob removes a Job and, with foreground propagation, its pod. It
// runs on its own context because the caller's has usually ended.
func (k *Kube) deleteJob(name string) {
	dctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opts := metav1.DeleteOptions{PropagationPolicy: new(metav1.DeletePropagationForeground)}
	if err := k.Client.BatchV1().Jobs(k.Namespace).Delete(dctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
		k.logger().Warn("delete runner job", "job", name, "error", err)
	}
}

// deleteSecret removes a Secret no Job owns yet.
func (k *Kube) deleteSecret(name string) {
	dctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := k.Client.CoreV1().Secrets(k.Namespace).Delete(dctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		k.logger().Warn("delete runner secret", "secret", name, "error", err)
	}
}

func (k *Kube) logger() *slog.Logger {
	if k.Logger == nil {
		return slog.Default()
	}
	return k.Logger
}

func jobFinished(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// finish fills the pod-level fields of a result from the Job's pod, with
// the run's secrets masked out of the log tail. Best effort: a missing pod
// leaves the fields empty rather than failing the run.
func (k *Kube) finish(res *Result, secrets runner.Secrets) {
	fctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pods, err := k.Client.CoreV1().Pods(k.Namespace).List(fctx, metav1.ListOptions{LabelSelector: "job-name=" + res.JobName})
	if err != nil || len(pods.Items) == 0 {
		return
	}
	pod := pods.Items[len(pods.Items)-1]
	res.PodName, res.NodeName = pod.Name, pod.Spec.NodeName
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionTrue {
			res.ScheduledAt = c.LastTransitionTime.Time
		}
	}
	if pod.Status.StartTime != nil {
		res.StartedAt = pod.Status.StartTime.Time
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			res.ExitCode = int(cs.State.Terminated.ExitCode)
			res.TerminationReason = cs.State.Terminated.Reason
			res.DeadlineExceeded = cs.State.Terminated.Reason == "DeadlineExceeded"
		}
	}
	// Reading past the kept size by the longest secret lets a secret that
	// straddles the cut be masked whole before the cut is made.
	limit := int64(LogTailBytes + max(len(secrets.GitToken), len(secrets.ModelAPIKey)))
	stream, err := k.Client.CoreV1().Pods(k.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{LimitBytes: &limit}).Stream(fctx)
	if err != nil {
		return
	}
	defer func() { _ = stream.Close() }()
	b, _ := io.ReadAll(io.LimitReader(stream, limit))
	masked := secrets.Mask(string(b))
	res.LogTail = masked[:min(len(masked), LogTailBytes)]
}

// Secret keys of a run's job-scoped Secret.
const (
	secretKeyGitToken    = "git-token"
	secretKeyModelAPIKey = "model-api-key"
	secretKeyRunSpec     = "run-spec.json"
)

// The run's job document is mounted read-only from its Secret at
// specDir/specFile.
const (
	specDir  = "/var/run/kritik"
	specFile = "spec.json"
)

func jobName(runID string) string { return "kritik-run-" + runID[:8] }

// runnerLabels are shared by a run's Job, pod and Secret. The role label is what
// a NetworkPolicy selects runner pods by.
func runnerLabels(spec Spec) map[string]string {
	l := map[string]string{
		"app.kubernetes.io/name": "kritik", "app.kubernetes.io/component": runnerRole, "kritik.home-operations.com/role": runnerRole,
	}
	for key, v := range spec.Labels {
		l["kritik.home-operations.com/"+key] = v
	}
	return l
}

// secret builds the run's job-scoped Secret: the credentials and the
// encoded job document. The model key is left out when the run has none;
// the pod reads it as an optional key.
func (k *Kube) secret(spec Spec, runSpec []byte) *corev1.Secret {
	data := map[string][]byte{secretKeyGitToken: []byte(spec.Secrets.GitToken), secretKeyRunSpec: runSpec}
	if spec.Secrets.ModelAPIKey != "" {
		data[secretKeyModelAPIKey] = []byte(spec.Secrets.ModelAPIKey)
	}
	return &corev1.Secret{
		Name: jobName(spec.RunID), Namespace: k.Namespace, Labels: runnerLabels(spec),
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

// job builds the Job for a spec. The runner gets its job document as a
// read-only file and its credentials as variables, both from the run's
// Secret, and the runner role's DSN from the database Secret; nothing else.
func (k *Kube) job(spec Spec) *batchv1.Job {
	name := jobName(spec.RunID)
	deadline := int64(spec.Deadline / time.Second)
	if deadline <= 0 {
		deadline = 900
	}
	ttl := int32(k.TTL / time.Second)
	if ttl <= 0 {
		ttl = 600
	}
	labels := runnerLabels(spec)
	annotations := map[string]string{}
	for key, v := range spec.Annotations {
		annotations["kritik.home-operations.com/"+key] = v
	}
	var backoff int32
	secretRef := func(key string, optional bool) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			Name: name, Key: key, Optional: new(optional)}}
	}
	env := []corev1.EnvVar{
		{Name: "KRITIK_RUN_SPEC_FILE", Value: specDir + "/" + specFile},
		{Name: "KRITIK_GIT_TOKEN", ValueFrom: secretRef(secretKeyGitToken, false)},
		{Name: "KRITIK_MODEL_API_KEY", ValueFrom: secretRef(secretKeyModelAPIKey, true)},
		{Name: "KRITIK_LOG_FORMAT", Value: "json"},
		{Name: "KRITIK_DATABASE_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			Name: k.DatabaseSecret, Key: k.DatabaseSecretKey}}},
	}
	container := corev1.Container{
		Name:  runnerRole,
		Image: k.Image,
		Args:  []string{"--role", runnerRole},
		Env:   env,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "scratch", MountPath: "/tmp"},
			{Name: "spec", MountPath: specDir, ReadOnly: true},
		},
	}
	if spec.Resources != nil {
		if b, err := json.Marshal(spec.Resources); err == nil {
			_ = json.Unmarshal(b, &container.Resources)
		}
	}
	return &batchv1.Job{
		Name: name, Namespace: k.Namespace, Labels: labels, Annotations: annotations,
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:           k.ServiceAccount,
					AutomountServiceAccountToken: new(false),
					RestartPolicy:                corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true), RunAsUser: new(int64(65532)), RunAsGroup: new(int64(65532)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{container},
					Volumes: []corev1.Volume{
						{Name: "scratch", EmptyDir: &corev1.EmptyDirVolumeSource{}},
						{Name: "spec", Secret: &corev1.SecretVolumeSource{
							SecretName: name, Items: []corev1.KeyToPath{{Key: secretKeyRunSpec, Path: specFile}},
						}},
					},
				},
			},
		},
	}
}
