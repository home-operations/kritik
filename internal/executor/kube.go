package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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

// Run implements Executor: create the Job, wait for it, capture the pod's
// log tail, delete nothing (the TTL does), and report.
func (k *Kube) Run(ctx context.Context, spec Spec) Result {
	job := k.job(spec)
	created, err := k.Client.BatchV1().Jobs(k.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return Result{Err: fmt.Errorf("executor: create job: %w", err)}
	}
	res := Result{JobName: created.Name}
	poll := k.Poll
	if poll <= 0 {
		poll = 3 * time.Second
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			res.Err = ctx.Err()
			k.finish(&res)
			return res
		case <-t.C:
		}
		j, err := k.Client.BatchV1().Jobs(k.Namespace).Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			res.Err = fmt.Errorf("executor: get job: %w", err)
			return res
		}
		if j.Status.Succeeded > 0 || j.Status.Failed > 0 || jobFinished(j) {
			k.finish(&res)
			if j.Status.Succeeded == 0 {
				res.Err = fmt.Errorf("executor: job %s failed: %s", created.Name, res.TerminationReason)
			}
			return res
		}
	}
}

func jobFinished(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// finish fills the pod-level fields of a result from the Job's pod. Best
// effort: a missing pod leaves the fields empty rather than failing the run.
func (k *Kube) finish(res *Result) {
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
	limit := int64(LogTailBytes)
	stream, err := k.Client.CoreV1().Pods(k.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{LimitBytes: &limit}).Stream(fctx)
	if err != nil {
		return
	}
	defer func() { _ = stream.Close() }()
	b, _ := io.ReadAll(io.LimitReader(stream, LogTailBytes))
	res.LogTail = string(b)
}

// job builds the Job for a spec. The runner gets the git token as a plain
// variable (it is short-lived and scoped to one repository) and the runner
// role's DSN from the Secret; nothing else.
func (k *Kube) job(spec Spec) *batchv1.Job {
	name := "kritik-run-" + spec.RunID[:8]
	deadline := int64(spec.Deadline / time.Second)
	if deadline <= 0 {
		deadline = 900
	}
	ttl := int32(k.TTL / time.Second)
	if ttl <= 0 {
		ttl = 600
	}
	// The role label is what a NetworkPolicy selects runner pods by.
	labels := map[string]string{
		"app.kubernetes.io/name": "kritik", "app.kubernetes.io/component": runnerRole, "kritik.home-operations.com/role": runnerRole,
	}
	for key, v := range spec.Labels {
		labels["kritik.home-operations.com/"+key] = v
	}
	annotations := map[string]string{}
	for key, v := range spec.Annotations {
		annotations["kritik.home-operations.com/"+key] = v
	}
	var backoff int32
	env := []corev1.EnvVar{
		{Name: "KRITIK_RUN_KIND", Value: runKind(spec.Params.Kind)},
		{Name: "KRITIK_RUN_ID", Value: spec.Params.RunID},
		{Name: "KRITIK_CLONE_URL", Value: spec.Params.CloneURL},
		{Name: "KRITIK_GIT_TOKEN", Value: spec.Params.Token},
		{Name: "KRITIK_HEAD_SHA", Value: spec.Params.Head},
		{Name: "KRITIK_BASE_SHA", Value: spec.Params.Base},
		{Name: "KRITIK_IGNORE", Value: strings.Join(spec.Params.Ignore, ",")},
		{Name: "KRITIK_LOG_FORMAT", Value: "json"},
		{Name: "KRITIK_DATABASE_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: k.DatabaseSecret}, Key: k.DatabaseSecretKey}}},
	}
	container := corev1.Container{
		Name:  runnerRole,
		Image: k.Image,
		Args:  []string{"--role", runnerRole},
		Env:   env,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			ReadOnlyRootFilesystem:   ptr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "scratch", MountPath: "/tmp"}},
	}
	if spec.Resources != nil {
		if b, err := json.Marshal(spec.Resources); err == nil {
			_ = json.Unmarshal(b, &container.Resources)
		}
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.Namespace, Labels: labels, Annotations: annotations},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:           k.ServiceAccount,
					AutomountServiceAccountToken: ptr(false),
					RestartPolicy:                corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(65532)), RunAsGroup: ptr(int64(65532)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{container},
					Volumes:    []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				},
			},
		},
	}
}

func ptr[T any](v T) *T { return &v }

func runKind(kind string) string {
	if kind == "" {
		return "review"
	}
	return kind
}
