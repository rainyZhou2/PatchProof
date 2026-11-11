package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

const testNamespace = "pp-exec"

func testSpec() domain.StepSpec {
	return domain.StepSpec{
		RunID:      domain.NewRunID(),
		StepID:     domain.NewStepID(),
		AttemptID:  domain.NewAttemptID(),
		TenantID:   domain.NewTenantID(),
		Generation: 3,
		Kind:       domain.StepKindCompile,
		Language:   "go",
		Engine:     "go-analyzer",
		Deadline:   time.Now().Add(10 * time.Minute),
		Requirements: domain.ResourceRequirements{
			Architecture:     "amd64",
			Isolation:        domain.IsolationGVisor,
			CPUMilli:         500,
			MemoryMiB:        256,
			EphemeralDiskGiB: 2,
			Network:          domain.NetworkNone,
			DeadlineSeconds:  600,
			RuntimeClass:     "gvisor",
		},
	}
}

func newTestExecutor(t *testing.T, mutate func(*Config)) (*Executor, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset()
	cfg := Config{
		ClusterID:        "cluster-a",
		Region:           "region-1",
		Namespace:        testNamespace,
		WorkerImage:      "registry.test/patchproof-worker:1",
		EngineImage:      "registry.test/patchproof-engine:1",
		RuntimeClass:     "gvisor",
		JobTTL:           10 * time.Minute,
		ActiveDeadline:   5 * time.Minute,
		PollInterval:     10 * time.Millisecond,
		MaxConcurrent:    8,
		SecretNamePrefix: "pp",
		CallbackURL:      "https://controller.test/default-callback",
		Client:           client,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	executor, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return executor, client
}

func startTestStep(t *testing.T, executor *Executor, spec domain.StepSpec, token string) ports.ExecutionRef {
	t.Helper()
	ref, err := executor.Start(context.Background(), ports.ExecutionRequest{
		Spec:         spec,
		AttemptToken: token,
		CallbackURL:  "https://controller.test/callback",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return ref
}

func getJob(t *testing.T, client *fake.Clientset, name string) *batchv1.Job {
	t.Helper()
	job, err := client.BatchV1().Jobs(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job %s: %v", name, err)
	}
	return job
}

func updateJobStatus(t *testing.T, client *fake.Clientset, name string, mutate func(*batchv1.Job)) {
	t.Helper()
	job := getJob(t, client, name)
	mutate(job)
	if _, err := client.BatchV1().Jobs(testNamespace).UpdateStatus(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update job status: %v", err)
	}
}

func createPod(t *testing.T, client *fake.Clientset, jobName string, phase corev1.PodPhase, mutate func(*corev1.Pod)) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-abcde",
			Namespace: testNamespace,
			Labels:    map[string]string{"job-name": jobName},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	if mutate != nil {
		mutate(pod)
	}
	created, err := client.CoreV1().Pods(testNamespace).Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}
	if _, err := client.CoreV1().Pods(testNamespace).UpdateStatus(context.Background(), created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	return created
}

func TestStartCreatesHardenedJob(t *testing.T) {
	executor, client := newTestExecutor(t, nil)
	spec := testSpec()
	ref := startTestStep(t, executor, spec, "attempt-token-abc")

	expectedName := "pp-" + string(spec.StepID)[:8] + "-" + string(spec.AttemptID)[:8]
	if ref.ExternalID != expectedName {
		t.Fatalf("external id = %q, want %q", ref.ExternalID, expectedName)
	}
	if ref.ClusterID != "cluster-a" || ref.Namespace != testNamespace || ref.Executor != executorName {
		t.Fatalf("unexpected ref: %+v", ref)
	}

	job := getJob(t, client, expectedName)
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("backoff limit = %v, want 0", job.Spec.BackoffLimit)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds <= 0 {
		t.Fatalf("active deadline = %v", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 600 {
		t.Fatalf("ttl = %v, want 600", job.Spec.TTLSecondsAfterFinished)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restart policy = %s", job.Spec.Template.Spec.RestartPolicy)
	}
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("service account token must not be automounted")
	}
	if job.Spec.Template.Spec.RuntimeClassName == nil || *job.Spec.Template.Spec.RuntimeClassName != "gvisor" {
		t.Fatalf("runtime class = %v", job.Spec.Template.Spec.RuntimeClassName)
	}

	labels := job.Labels
	for key, want := range map[string]string{
		"app.kubernetes.io/name":   workerAppName,
		"patchproof.io/run-id":     string(spec.RunID),
		"patchproof.io/step-id":    string(spec.StepID),
		"patchproof.io/attempt-id": string(spec.AttemptID),
		"patchproof.io/generation": "3",
		"patchproof.io/tenant-id":  string(spec.TenantID),
	} {
		if labels[key] != want {
			t.Fatalf("label %s = %q, want %q", key, labels[key], want)
		}
	}

	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("containers = %d", len(job.Spec.Template.Spec.Containers))
	}
	container := job.Spec.Template.Spec.Containers[0]
	security := container.SecurityContext
	if security == nil {
		t.Fatal("container security context missing")
	}
	if security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation {
		t.Fatal("allowPrivilegeEscalation must be false")
	}
	if security.Privileged == nil || *security.Privileged {
		t.Fatal("privileged must be false")
	}
	if security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem {
		t.Fatal("readOnlyRootFilesystem must be true")
	}
	if security.RunAsNonRoot == nil || !*security.RunAsNonRoot {
		t.Fatal("runAsNonRoot must be true")
	}
	if security.Capabilities == nil || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("capabilities = %+v", security.Capabilities)
	}
	if security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("seccomp profile must be RuntimeDefault")
	}
	podSecurity := job.Spec.Template.Spec.SecurityContext
	if podSecurity == nil || podSecurity.RunAsNonRoot == nil || !*podSecurity.RunAsNonRoot {
		t.Fatal("pod security context must set runAsNonRoot")
	}

	if err := checkSecurityInvariants(job); err != nil {
		t.Fatalf("security invariants: %v", err)
	}

	if got := container.Resources.Requests.Cpu().String(); got != "500m" {
		t.Fatalf("cpu request = %s", got)
	}
	if got := container.Resources.Limits.Memory().String(); got != "256Mi" {
		t.Fatalf("memory limit = %s", got)
	}
	if got := container.Resources.Requests.StorageEphemeral().String(); got != "2Gi" {
		t.Fatalf("ephemeral storage request = %s", got)
	}

	mounts := map[string]corev1.VolumeMount{}
	for _, mount := range container.VolumeMounts {
		mounts[mount.Name] = mount
	}
	if !mounts[specVolume].ReadOnly || !mounts[tokenVolume].ReadOnly {
		t.Fatal("spec and token mounts must be read-only")
	}

	env := map[string]corev1.EnvVar{}
	for _, variable := range container.Env {
		env[variable.Name] = variable
	}
	tokenEnv, ok := env["PATCHPROOF_ATTEMPT_TOKEN"]
	if !ok || tokenEnv.ValueFrom == nil || tokenEnv.ValueFrom.SecretKeyRef == nil {
		t.Fatal("attempt token must come from a secret reference")
	}
	if tokenEnv.ValueFrom.SecretKeyRef.Name != expectedName+"-token" || tokenEnv.ValueFrom.SecretKeyRef.Key != tokenKey {
		t.Fatalf("token secret ref = %+v", tokenEnv.ValueFrom.SecretKeyRef)
	}
	if env["PATCHPROOF_CALLBACK_URL"].Value != "https://controller.test/callback" {
		t.Fatalf("callback url = %q", env["PATCHPROOF_CALLBACK_URL"].Value)
	}
	if env["PATCHPROOF_CLUSTER_ID"].Value != "cluster-a" {
		t.Fatalf("cluster id = %q", env["PATCHPROOF_CLUSTER_ID"].Value)
	}
	if env["PATCHPROOF_SPEC_FILE"].Value != "/run/spec/step.json" {
		t.Fatalf("spec file = %q", env["PATCHPROOF_SPEC_FILE"].Value)
	}

	secret, err := client.CoreV1().Secrets(testNamespace).Get(context.Background(), expectedName+"-token", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if string(secret.Data[tokenKey]) != "attempt-token-abc" {
		t.Fatalf("secret token = %q", string(secret.Data[tokenKey]))
	}
	if secret.Annotations[expiresAtAnnotation] == "" {
		t.Fatal("secret must carry an expiry annotation")
	}

	configMap, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), expectedName+"-spec", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get config map: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(configMap.Data[specVolumeKey]), &decoded); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	for _, forbidden := range []string{"attempt_token", "callback_url"} {
		if _, ok := decoded[forbidden]; ok {
			t.Fatalf("serialized spec must not contain %q", forbidden)
		}
	}
	if configMap.Immutable == nil || !*configMap.Immutable {
		t.Fatal("spec config map must be immutable")
	}
}

func checkSecurityInvariants(job *batchv1.Job) error {
	if job.Spec.Template.Spec.HostNetwork || job.Spec.Template.Spec.HostPID || job.Spec.Template.Spec.HostIPC {
		return errors.New("host namespaces must not be used")
	}
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.HostPath != nil {
			return errors.New("hostPath volumes are forbidden")
		}
		if volume.EmptyDir == nil && volume.ConfigMap == nil && volume.Secret == nil {
			return errors.New("unexpected volume source")
		}
	}
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.SecurityContext == nil || container.SecurityContext.Privileged == nil || *container.SecurityContext.Privileged {
			return errors.New("privileged containers are forbidden")
		}
	}
	return nil
}

func TestStartIsIdempotent(t *testing.T) {
	executor, client := newTestExecutor(t, nil)
	spec := testSpec()
	first := startTestStep(t, executor, spec, "token-a")
	second := startTestStep(t, executor, spec, "token-a")
	if first != second {
		t.Fatalf("refs differ: %+v vs %+v", first, second)
	}
	jobs, err := client.BatchV1().Jobs(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs.Items))
	}
	configMaps, err := client.CoreV1().ConfigMaps(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list config maps: %v", err)
	}
	if len(configMaps.Items) != 1 {
		t.Fatalf("config maps = %d, want 1", len(configMaps.Items))
	}
}

func TestStartRejectsMissingRuntimeClass(t *testing.T) {
	executor, client := newTestExecutor(t, func(cfg *Config) {
		cfg.RuntimeClass = ""
	})
	spec := testSpec()
	spec.Requirements.RuntimeClass = ""
	_, err := executor.Start(context.Background(), ports.ExecutionRequest{Spec: spec, AttemptToken: "token"})
	if err == nil {
		t.Fatal("expected sandbox unavailable")
	}
	var classified *Error
	if !errors.As(err, &classified) || classified.Class != domain.ErrSandboxUnavailable {
		t.Fatalf("error = %v, want sandbox unavailable", err)
	}
	jobs, listErr := client.BatchV1().Jobs(testNamespace).List(context.Background(), metav1.ListOptions{})
	if listErr != nil {
		t.Fatalf("list jobs: %v", listErr)
	}
	if len(jobs.Items) != 0 {
		t.Fatal("no job may be created for an unavailable sandbox")
	}
}

func TestRuntimeClassOnlyWhenConfigured(t *testing.T) {
	executor, client := newTestExecutor(t, func(cfg *Config) {
		cfg.RuntimeClass = ""
	})
	spec := testSpec()
	spec.Requirements.Isolation = domain.IsolationContainer
	spec.Requirements.RuntimeClass = ""
	startTestStep(t, executor, spec, "token")
	job := getJob(t, client, executor.resourceName(spec))
	if job.Spec.Template.Spec.RuntimeClassName != nil {
		t.Fatalf("runtime class = %q, want unset", *job.Spec.Template.Spec.RuntimeClassName)
	}
}

func TestObserveStates(t *testing.T) {
	executor, client := newTestExecutor(t, nil)
	spec := testSpec()
	spec.Requirements.Isolation = domain.IsolationContainer
	spec.Requirements.RuntimeClass = ""
	ref := startTestStep(t, executor, spec, "token")

	observation, err := executor.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("Observe pending: %v", err)
	}
	if observation.State != ports.ExecutionPending {
		t.Fatalf("state = %s, want PENDING", observation.State)
	}

	updateJobStatus(t, client, ref.ExternalID, func(job *batchv1.Job) { job.Status.Active = 1 })
	observation, err = executor.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("Observe running: %v", err)
	}
	if observation.State != ports.ExecutionRunning {
		t.Fatalf("state = %s, want RUNNING", observation.State)
	}

	updateJobStatus(t, client, ref.ExternalID, func(job *batchv1.Job) {
		job.Status.Active = 0
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{{
			Type:   batchv1.JobComplete,
			Status: corev1.ConditionTrue,
			Reason: "Completed",
		}}
	})
	observation, err = executor.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("Observe complete: %v", err)
	}
	if observation.State != ports.ExecutionCompleted || observation.Result != nil {
		t.Fatalf("observation = %+v, want COMPLETED without result", observation)
	}

	missing := ports.ExecutionRef{ExternalID: "pp-00000000-00000000", Namespace: testNamespace}
	observation, err = executor.Observe(context.Background(), missing)
	if err != nil {
		t.Fatalf("Observe missing: %v", err)
	}
	if observation.State != ports.ExecutionLost || observation.ErrorClass != domain.ErrWorkerLost {
		t.Fatalf("observation = %+v, want LOST", observation)
	}
}

func TestObserveFailureMapping(t *testing.T) {
	tests := []struct {
		name      string
		jobUpdate func(*batchv1.Job)
		pod       func(*corev1.Pod)
		wantClass domain.ErrorClass
		wantText  string
	}{
		{
			name: "oom",
			jobUpdate: func(job *batchv1.Job) {
				job.Status.Failed = 1
				job.Status.Conditions = []batchv1.JobCondition{{
					Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
				}}
			},
			pod: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodFailed
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "worker",
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						Reason: "OOMKilled", ExitCode: 137,
					}},
				}}
			},
			wantClass: domain.ErrExecutorError,
			wantText:  "OOMKilled",
		},
		{
			name: "evicted",
			jobUpdate: func(job *batchv1.Job) {
				job.Status.Failed = 1
				job.Status.Conditions = []batchv1.JobCondition{{
					Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
				}}
			},
			pod: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodFailed
				pod.Status.Reason = "Evicted"
				pod.Status.Message = "The node was low on resource: memory."
			},
			wantClass: domain.ErrPodEvicted,
			wantText:  "evicted",
		},
		{
			name: "node lost",
			jobUpdate: func(job *batchv1.Job) {
				job.Status.Failed = 1
				job.Status.Conditions = []batchv1.JobCondition{{
					Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
				}}
			},
			pod: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodFailed
				pod.Status.Reason = "NodeLost"
			},
			wantClass: domain.ErrNodeLost,
			wantText:  "node lost",
		},
		{
			name:      "image pull",
			jobUpdate: func(job *batchv1.Job) { job.Status.Active = 1 },
			pod: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodPending
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "worker",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason: "ImagePullBackOff",
					}},
				}}
			},
			wantClass: domain.ErrImagePullFailed,
			wantText:  "ImagePullBackOff",
		},
		{
			name: "deadline with running container",
			jobUpdate: func(job *batchv1.Job) {
				job.Status.Failed = 1
				job.Status.Conditions = []batchv1.JobCondition{{
					Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded",
					Message: "Job was active longer than specified deadline",
				}}
			},
			pod: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodFailed
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  "worker",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}}
			},
			wantClass: domain.ErrTimeout,
			wantText:  "DeadlineExceeded",
		},
		{
			name: "stuck initializing",
			jobUpdate: func(job *batchv1.Job) {
				job.Status.Failed = 1
				job.Status.Conditions = []batchv1.JobCondition{{
					Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded",
					Message: "Job was active longer than specified deadline",
				}}
			},
			pod: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodPending
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  "worker",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
				}}
			},
			wantClass: domain.ErrExecutorError,
			wantText:  "initializing",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, client := newTestExecutor(t, nil)
			spec := testSpec()
			ref := startTestStep(t, executor, spec, "token")
			updateJobStatus(t, client, ref.ExternalID, test.jobUpdate)
			createPod(t, client, ref.ExternalID, corev1.PodRunning, test.pod)

			observation, err := executor.Observe(context.Background(), ref)
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if observation.State != ports.ExecutionFailed {
				t.Fatalf("state = %s, want FAILED", observation.State)
			}
			if observation.ErrorClass != test.wantClass {
				t.Fatalf("class = %s, want %s (message %q)", observation.ErrorClass, test.wantClass, observation.Message)
			}
			if !strings.Contains(strings.ToLower(observation.Message), strings.ToLower(test.wantText)) {
				t.Fatalf("message = %q, want it to contain %q", observation.Message, test.wantText)
			}
		})
	}
}

func TestObservePodEventMessage(t *testing.T) {
	executor, client := newTestExecutor(t, nil)
	spec := testSpec()
	ref := startTestStep(t, executor, spec, "token")
	updateJobStatus(t, client, ref.ExternalID, func(job *batchv1.Job) {
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
		}}
	})
	pod := createPod(t, client, ref.ExternalID, corev1.PodFailed, func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "worker",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "OOMKilled", ExitCode: 137,
			}},
		}}
	})
	_, err := client.CoreV1().Events(testNamespace).Create(context.Background(), &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name + ".event", Namespace: testNamespace},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Namespace: testNamespace, Name: pod.Name,
		},
		Reason:  "Killing",
		Message: "Container worker exceeded its memory limit.",
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}

	observation, err := executor.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !strings.Contains(observation.Message, "exceeded its memory limit") {
		t.Fatalf("message = %q, want event message", observation.Message)
	}
}

func TestCancel(t *testing.T) {
	executor, client := newTestExecutor(t, nil)
	spec := testSpec()
	ref := startTestStep(t, executor, spec, "token")

	if err := executor.Cancel(context.Background(), ref); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := client.BatchV1().Jobs(testNamespace).Get(context.Background(), ref.ExternalID, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("job still exists: %v", err)
	}
	if _, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), ref.ExternalID+"-spec", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("config map still exists: %v", err)
	}
	if _, err := client.CoreV1().Secrets(testNamespace).Get(context.Background(), ref.ExternalID+"-token", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("secret still exists: %v", err)
	}
	if err := executor.Cancel(context.Background(), ref); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
}

func TestCancelRejectsForeignNamespace(t *testing.T) {
	executor, _ := newTestExecutor(t, nil)
	err := executor.Cancel(context.Background(), ports.ExecutionRef{ExternalID: "pp-00000000-00000000", Namespace: "other"})
	if err == nil {
		t.Fatal("expected namespace rejection")
	}
}

func TestCapabilities(t *testing.T) {
	executor, _ := newTestExecutor(t, nil)
	capabilities, err := executor.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if capabilities.ExecutorKind != executorKind || capabilities.Region != "region-1" {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if capabilities.MaxConcurrent != 8 {
		t.Fatalf("max concurrent = %d", capabilities.MaxConcurrent)
	}
	tiers := map[domain.IsolationTier]bool{}
	for _, tier := range capabilities.IsolationTiers {
		tiers[tier] = true
	}
	for _, want := range []domain.IsolationTier{domain.IsolationTrustedLocal, domain.IsolationContainer, domain.IsolationGVisor, domain.IsolationVM} {
		if !tiers[want] {
			t.Fatalf("missing isolation tier %s in %v", want, capabilities.IsolationTiers)
		}
	}
	if len(capabilities.RuntimeClasses) != 1 || capabilities.RuntimeClasses[0] != "gvisor" {
		t.Fatalf("runtime classes = %v", capabilities.RuntimeClasses)
	}
}

func TestNewRequiresWorkerImage(t *testing.T) {
	if _, err := New(Config{WorkerImage: ""}); err == nil {
		t.Fatal("expected error for missing worker image")
	}
}
