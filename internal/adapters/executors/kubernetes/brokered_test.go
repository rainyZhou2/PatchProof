package kubernetes

import (
	"context"
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

const brokerTestImage = "registry.test/patchproof-broker:1"

func brokeredSpec() domain.StepSpec {
	spec := testSpec()
	spec.Requirements.ArtifactTransport = domain.TransportBrokeredOffline
	spec.Requirements.Network = domain.NetworkNone
	return spec
}

func brokeredExecutor(t *testing.T, mutate func(*Config)) (*Executor, *fake.Clientset) {
	t.Helper()
	return newTestExecutor(t, func(cfg *Config) {
		cfg.BrokerImage = brokerTestImage
		cfg.BrokerServiceAccount = "broker-sa"
		cfg.ServiceAccount = "worker-sa"
		cfg.BrokeredStorageClass = "fast"
		cfg.WorkspaceSizeGiB = 2
		cfg.ExtraEnv = map[string]string{
			"PATCHPROOF_S3_ENDPOINT":   "https://s3.test",
			"PATCHPROOF_S3_ACCESS_KEY": "access",
			"PATCHPROOF_CAS_DRIVER":    "s3",
			"AWS_REGION":               "region-1",
			"GOTOOLCHAIN":              "local",
		}
		if mutate != nil {
			mutate(cfg)
		}
	})
}

func startBrokered(t *testing.T, executor *Executor, spec domain.StepSpec) ports.ExecutionRef {
	t.Helper()
	return startTestStep(t, executor, spec, "brokered-token")
}

func envMap(job *batchv1.Job) map[string]corev1.EnvVar {
	env := map[string]corev1.EnvVar{}
	if len(job.Spec.Template.Spec.Containers) == 0 {
		return env
	}
	for _, variable := range job.Spec.Template.Spec.Containers[0].Env {
		env[variable.Name] = variable
	}
	return env
}

func volumeMap(job *batchv1.Job) map[string]corev1.Volume {
	volumes := map[string]corev1.Volume{}
	for _, volume := range job.Spec.Template.Spec.Volumes {
		volumes[volume.Name] = volume
	}
	return volumes
}

func getPVC(t *testing.T, client *fake.Clientset, name string) *corev1.PersistentVolumeClaim {
	t.Helper()
	claim, err := client.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pvc %s: %v", name, err)
	}
	return claim
}

func succeedJob(t *testing.T, client *fake.Clientset, name string) {
	t.Helper()
	updateJobStatus(t, client, name, func(job *batchv1.Job) {
		job.Status.Active = 0
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "Completed",
		}}
	})
}

func failJob(t *testing.T, client *fake.Clientset, name string, reason string) {
	t.Helper()
	updateJobStatus(t, client, name, func(job *batchv1.Job) {
		job.Status.Active = 0
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason,
		}}
	})
}

func assertHardened(t *testing.T, job *batchv1.Job) {
	t.Helper()
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restart policy = %s", job.Spec.Template.Spec.RestartPolicy)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("backoff limit = %v", job.Spec.BackoffLimit)
	}
	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("containers = %d", len(job.Spec.Template.Spec.Containers))
	}
	security := job.Spec.Template.Spec.Containers[0].SecurityContext
	if security == nil {
		t.Fatal("missing container security context")
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
	if security.RunAsNonRoot == nil || !*security.RunAsNonRoot || security.RunAsUser == nil || *security.RunAsUser != 65532 {
		t.Fatal("container must run as non-root 65532")
	}
	if security.Capabilities == nil || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("capabilities = %+v", security.Capabilities)
	}
	if security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("seccomp profile must be RuntimeDefault")
	}
	podSecurity := job.Spec.Template.Spec.SecurityContext
	if podSecurity == nil || podSecurity.RunAsNonRoot == nil || !*podSecurity.RunAsNonRoot || podSecurity.RunAsUser == nil || *podSecurity.RunAsUser != 65532 {
		t.Fatal("pod must run as non-root 65532")
	}
}

func TestBrokeredStartCreatesPVCAndStageIn(t *testing.T) {
	executor, client := brokeredExecutor(t, nil)
	spec := brokeredSpec()
	ref := startBrokered(t, executor, spec)
	base := ref.ExternalID

	claim := getPVC(t, client, base+"-ws")
	if claim.Labels[brokeredPhaseLabel] != brokeredWorkspaceRole {
		t.Fatalf("pvc phase label = %q", claim.Labels[brokeredPhaseLabel])
	}
	for key, want := range map[string]string{
		"app.kubernetes.io/name":   workerAppName,
		"patchproof.io/step-id":    string(spec.StepID),
		"patchproof.io/attempt-id": string(spec.AttemptID),
		"patchproof.io/generation": "3",
		"patchproof.io/tenant-id":  string(spec.TenantID),
	} {
		if claim.Labels[key] != want {
			t.Fatalf("pvc label %s = %q, want %q", key, claim.Labels[key], want)
		}
	}
	quantity := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if got := quantity.String(); got != "2Gi" {
		t.Fatalf("pvc size = %s, want 2Gi", got)
	}
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "fast" {
		t.Fatalf("storage class = %v", claim.Spec.StorageClassName)
	}
	if len(claim.Spec.AccessModes) != 1 || claim.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("access modes = %v", claim.Spec.AccessModes)
	}

	if _, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), base+"-spec", metav1.GetOptions{}); err != nil {
		t.Fatalf("spec config map: %v", err)
	}
	secret, err := client.CoreV1().Secrets(testNamespace).Get(context.Background(), base+"-token", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("token secret: %v", err)
	}
	if string(secret.Data[tokenKey]) != "brokered-token" {
		t.Fatalf("secret token = %q", string(secret.Data[tokenKey]))
	}

	job := getJob(t, client, base+"-stage-in")
	assertHardened(t, job)
	if job.Labels[brokeredPhaseLabel] != brokeredStageIn || job.Spec.Template.Labels[brokeredPhaseLabel] != brokeredStageIn {
		t.Fatalf("stage-in phase labels: job=%q pod=%q", job.Labels[brokeredPhaseLabel], job.Spec.Template.Labels[brokeredPhaseLabel])
	}
	if job.Labels["app.kubernetes.io/name"] != brokerAppName {
		t.Fatalf("broker app label = %q", job.Labels["app.kubernetes.io/name"])
	}
	if job.Spec.Template.Spec.ServiceAccountName != "broker-sa" {
		t.Fatalf("stage-in SA = %q", job.Spec.Template.Spec.ServiceAccountName)
	}
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil || !*job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("stage-in must automount the broker service account token for workload identity")
	}
	if job.Spec.Template.Spec.RuntimeClassName != nil {
		t.Fatalf("trusted stage-in must not force a sandbox runtime class: %v", *job.Spec.Template.Spec.RuntimeClassName)
	}
	if got := job.Spec.Template.Spec.Containers[0].Image; got != brokerTestImage {
		t.Fatalf("stage-in image = %q", got)
	}

	env := envMap(job)
	for key, want := range map[string]string{
		"PATCHPROOF_PHASE":       brokeredStageIn,
		"PATCHPROOF_LOCAL_ROOT":  "/work",
		"PATCHPROOF_SPEC_FILE":   "/run/spec/step.json",
		"PATCHPROOF_S3_ENDPOINT": "https://s3.test",
		"PATCHPROOF_CAS_DRIVER":  "s3",
		"AWS_REGION":             "region-1",
		"GOTOOLCHAIN":            "local",
	} {
		if env[key].Value != want {
			t.Fatalf("stage-in env %s = %q, want %q", key, env[key].Value, want)
		}
	}
	if _, ok := env["PATCHPROOF_ATTEMPT_TOKEN"]; ok {
		t.Fatal("stage-in must not receive an attempt token")
	}
	if _, ok := env["PATCHPROOF_CALLBACK_URL"]; ok {
		t.Fatal("stage-in must not receive a callback URL")
	}
	volumes := volumeMap(job)
	if _, ok := volumes[tokenVolume]; ok {
		t.Fatal("stage-in must not mount the token secret")
	}
	workspace, ok := volumes[workspaceVolume]
	if !ok || workspace.PersistentVolumeClaim == nil || workspace.PersistentVolumeClaim.ClaimName != base+"-ws" {
		t.Fatalf("workspace volume = %+v", workspace)
	}
	if _, ok := volumes[specVolume]; !ok {
		t.Fatal("stage-in must mount the spec config map")
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, mount := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		mounts[mount.Name] = mount
	}
	if mounts[workspaceVolume].MountPath != "/work" {
		t.Fatalf("workspace mount = %q", mounts[workspaceVolume].MountPath)
	}
	if !mounts[specVolume].ReadOnly {
		t.Fatal("spec mount must be read-only")
	}

	for _, suffix := range []string{brokeredExecuteSuffix, brokeredStageOutSuffix} {
		if _, err := client.BatchV1().Jobs(testNamespace).Get(context.Background(), base+suffix, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("%s must not exist yet: %v", suffix, err)
		}
	}

	// Start is idempotent and returns the existing phase.
	second := startBrokered(t, executor, spec)
	if second != ref {
		t.Fatalf("refs differ: %+v vs %+v", ref, second)
	}
	jobs, err := client.BatchV1().Jobs(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("jobs = %d, want only stage-in", len(jobs.Items))
	}
}

func TestBrokeredPhaseSequence(t *testing.T) {
	executor, client := brokeredExecutor(t, nil)
	spec := brokeredSpec()
	ref := startBrokered(t, executor, spec)
	base := ref.ExternalID

	observation, err := executor.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("Observe stage-in pending: %v", err)
	}
	if observation.State != ports.ExecutionPending {
		t.Fatalf("state = %s, want PENDING", observation.State)
	}

	succeedJob(t, client, base+"-stage-in")
	observation, err = executor.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("Observe stage-in success: %v", err)
	}
	if observation.State != ports.ExecutionRunning {
		t.Fatalf("state = %s, want RUNNING", observation.State)
	}

	execute := getJob(t, client, base+"-execute")
	assertHardened(t, execute)
	if execute.Labels[brokeredPhaseLabel] != brokeredExecute || execute.Spec.Template.Labels[brokeredPhaseLabel] != brokeredExecute {
		t.Fatalf("execute phase labels: job=%q pod=%q", execute.Labels[brokeredPhaseLabel], execute.Spec.Template.Labels[brokeredPhaseLabel])
	}
	if execute.Spec.Template.Spec.ServiceAccountName != "worker-sa" {
		t.Fatalf("execute SA = %q, want worker-sa", execute.Spec.Template.Spec.ServiceAccountName)
	}
	if execute.Spec.Template.Spec.RuntimeClassName == nil || *execute.Spec.Template.Spec.RuntimeClassName != "gvisor" {
		t.Fatalf("execute runtime class = %v", execute.Spec.Template.Spec.RuntimeClassName)
	}
	if execute.Spec.Template.Spec.AutomountServiceAccountToken == nil || *execute.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("execute must disable service account automount")
	}
	executeVolumes := volumeMap(execute)
	if _, ok := executeVolumes[tokenVolume]; ok {
		t.Fatal("execute must not mount the token secret")
	}
	workspace, ok := executeVolumes[workspaceVolume]
	if !ok || workspace.PersistentVolumeClaim == nil || workspace.PersistentVolumeClaim.ClaimName != base+"-ws" {
		t.Fatalf("execute workspace volume = %+v", workspace)
	}
	env := envMap(execute)
	for key, want := range map[string]string{
		"PATCHPROOF_PHASE":                 brokeredExecute,
		"PATCHPROOF_OFFLINE_ARTIFACT_ROOT": "/work",
		"PATCHPROOF_WORKER_WORKDIR":        "/work/workspace",
		"PATCHPROOF_SPEC_FILE":             "/run/spec/step.json",
		"TMPDIR":                           "/work",
		"GOTOOLCHAIN":                      "local",
	} {
		if env[key].Value != want {
			t.Fatalf("execute env %s = %q, want %q", key, env[key].Value, want)
		}
	}
	for _, forbidden := range []string{
		"PATCHPROOF_ATTEMPT_TOKEN", "PATCHPROOF_CALLBACK_URL", "PATCHPROOF_S3_ENDPOINT",
		"PATCHPROOF_S3_ACCESS_KEY", "PATCHPROOF_CAS_DRIVER", "AWS_REGION",
	} {
		if _, ok := env[forbidden]; ok {
			t.Fatalf("execute must not receive %s", forbidden)
		}
	}
	for _, variable := range env {
		if variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef != nil {
			t.Fatalf("execute must not reference secret %q", variable.ValueFrom.SecretKeyRef.Name)
		}
	}
	if len(execute.Spec.Template.Spec.Containers[0].Args) != 0 {
		t.Fatalf("execute args = %v, want worker entrypoint", execute.Spec.Template.Spec.Containers[0].Args)
	}

	succeedJob(t, client, base+"-execute")
	observation, err = executor.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("Observe execute success: %v", err)
	}
	if observation.State != ports.ExecutionRunning {
		t.Fatalf("state = %s, want RUNNING", observation.State)
	}

	stageOut := getJob(t, client, base+"-stage-out")
	assertHardened(t, stageOut)
	if stageOut.Labels[brokeredPhaseLabel] != brokeredStageOut {
		t.Fatalf("stage-out phase label = %q", stageOut.Labels[brokeredPhaseLabel])
	}
	if stageOut.Spec.Template.Spec.ServiceAccountName != "broker-sa" {
		t.Fatalf("stage-out SA = %q", stageOut.Spec.Template.Spec.ServiceAccountName)
	}
	if stageOut.Spec.Template.Spec.AutomountServiceAccountToken == nil || !*stageOut.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("stage-out must automount the broker service account token for workload identity")
	}
	outEnv := envMap(stageOut)
	if outEnv["PATCHPROOF_PHASE"].Value != brokeredStageOut {
		t.Fatalf("stage-out phase env = %q", outEnv["PATCHPROOF_PHASE"].Value)
	}
	if outEnv["PATCHPROOF_CALLBACK_URL"].Value != "https://controller.test/callback" {
		t.Fatalf("stage-out callback = %q", outEnv["PATCHPROOF_CALLBACK_URL"].Value)
	}
	tokenEnv, ok := outEnv["PATCHPROOF_ATTEMPT_TOKEN"]
	if !ok || tokenEnv.ValueFrom == nil || tokenEnv.ValueFrom.SecretKeyRef == nil || tokenEnv.ValueFrom.SecretKeyRef.Name != base+"-token" {
		t.Fatalf("stage-out token env = %+v", tokenEnv)
	}
	if outEnv["PATCHPROOF_S3_ENDPOINT"].Value != "https://s3.test" {
		t.Fatalf("stage-out CAS env = %q", outEnv["PATCHPROOF_S3_ENDPOINT"].Value)
	}
	if _, ok := volumeMap(stageOut)[tokenVolume]; !ok {
		t.Fatal("stage-out must mount the token secret")
	}

	succeedJob(t, client, base+"-stage-out")
	observation, err = executor.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("Observe stage-out success: %v", err)
	}
	if observation.State != ports.ExecutionCompleted || observation.Result != nil {
		t.Fatalf("observation = %+v, want COMPLETED with nil result", observation)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), base+"-ws", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("workspace PVC must be deleted: %v", err)
	}

	// Completion is idempotent: the finished stage-out job remains until TTL.
	observation, err = executor.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("Observe completed again: %v", err)
	}
	if observation.State != ports.ExecutionCompleted {
		t.Fatalf("state = %s, want COMPLETED", observation.State)
	}
}

func TestBrokeredFailureMapping(t *testing.T) {
	tests := []struct {
		name      string
		phase     string
		fail      func(*batchv1.Job)
		pod       func(*corev1.Pod)
		wantClass domain.ErrorClass
		wantText  string
	}{
		{
			name:  "stage-in image pull",
			phase: brokeredStageIn,
			fail: func(job *batchv1.Job) {
				job.Status.Active = 1
			},
			pod: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodPending
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "broker",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason: "ImagePullBackOff",
					}},
				}}
			},
			wantClass: domain.ErrImagePullFailed,
			wantText:  "ImagePullBackOff",
		},
		{
			name:  "execute oom",
			phase: brokeredExecute,
			fail: func(job *batchv1.Job) {
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
			name:  "stage-out evicted",
			phase: brokeredStageOut,
			fail: func(job *batchv1.Job) {
				job.Status.Failed = 1
				job.Status.Conditions = []batchv1.JobCondition{{
					Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
				}}
			},
			pod: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodFailed
				pod.Status.Reason = "Evicted"
				pod.Status.Message = "node was low on resource"
			},
			wantClass: domain.ErrPodEvicted,
			wantText:  "evicted",
		},
		{
			name:  "execute node lost",
			phase: brokeredExecute,
			fail: func(job *batchv1.Job) {
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
			name:  "execute deadline",
			phase: brokeredExecute,
			fail: func(job *batchv1.Job) {
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
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, client := brokeredExecutor(t, nil)
			spec := brokeredSpec()
			ref := startBrokered(t, executor, spec)
			base := ref.ExternalID

			if test.phase != brokeredStageIn {
				succeedJob(t, client, base+"-stage-in")
				if _, err := executor.Observe(context.Background(), ref); err != nil {
					t.Fatalf("advance to execute: %v", err)
				}
			}
			if test.phase == brokeredStageOut {
				succeedJob(t, client, base+"-execute")
				if _, err := executor.Observe(context.Background(), ref); err != nil {
					t.Fatalf("advance to stage-out: %v", err)
				}
			}

			jobName := base + "-" + test.phase
			updateJobStatus(t, client, jobName, test.fail)
			createPod(t, client, jobName, corev1.PodRunning, test.pod)

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

func TestBrokeredCancelDeletesAllResources(t *testing.T) {
	executor, client := brokeredExecutor(t, nil)
	spec := brokeredSpec()
	ref := startBrokered(t, executor, spec)
	base := ref.ExternalID
	succeedJob(t, client, base+"-stage-in")
	if _, err := executor.Observe(context.Background(), ref); err != nil {
		t.Fatalf("advance: %v", err)
	}

	if err := executor.Cancel(context.Background(), ref); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	for _, suffix := range []string{"", "-stage-in", "-execute", "-stage-out"} {
		if _, err := client.BatchV1().Jobs(testNamespace).Get(context.Background(), base+suffix, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("job %s still exists: %v", base+suffix, err)
		}
	}
	if _, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), base+"-spec", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("config map still exists: %v", err)
	}
	if _, err := client.CoreV1().Secrets(testNamespace).Get(context.Background(), base+"-token", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("secret still exists: %v", err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), base+"-ws", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pvc still exists: %v", err)
	}
	if err := executor.Cancel(context.Background(), ref); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
}

func TestBrokeredRequiresBrokerImage(t *testing.T) {
	executor, client := newTestExecutor(t, func(cfg *Config) {
		cfg.ServiceAccount = "worker-sa"
		cfg.BrokerImage = ""
	})
	spec := brokeredSpec()
	_, err := executor.Start(context.Background(), ports.ExecutionRequest{
		Spec: spec, AttemptToken: "token", CallbackURL: "https://controller.test/callback",
	})
	var classified *Error
	if !errors.As(err, &classified) || classified.Class != domain.ErrSandboxUnavailable {
		t.Fatalf("error = %v, want SANDBOX_UNAVAILABLE", err)
	}
	jobs, listErr := client.BatchV1().Jobs(testNamespace).List(context.Background(), metav1.ListOptions{})
	if listErr != nil {
		t.Fatalf("list jobs: %v", listErr)
	}
	if len(jobs.Items) != 0 {
		t.Fatal("no job may be created without a broker image")
	}
	if _, pvcErr := client.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), executor.resourceName(spec)+"-ws", metav1.GetOptions{}); !apierrors.IsNotFound(pvcErr) {
		t.Fatalf("pvc must not exist: %v", pvcErr)
	}
}

func TestBrokeredWorkspaceDefaults(t *testing.T) {
	executor, client := newTestExecutor(t, func(cfg *Config) {
		cfg.BrokerImage = brokerTestImage
		cfg.BrokerServiceAccount = "broker-sa"
		cfg.ServiceAccount = "worker-sa"
		cfg.BrokeredStorageClass = ""
		cfg.WorkspaceSizeGiB = 0
	})
	spec := brokeredSpec()
	ref := startBrokered(t, executor, spec)
	claim := getPVC(t, client, ref.ExternalID+"-ws")
	quantity := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if got := quantity.String(); got != "1Gi" {
		t.Fatalf("default pvc size = %s, want 1Gi", got)
	}
	if claim.Spec.StorageClassName != nil {
		t.Fatalf("storage class = %v, want nil", claim.Spec.StorageClassName)
	}
}

func TestObserveBrokeredReportsLostWithoutPhaseJob(t *testing.T) {
	executor, client := brokeredExecutor(t, nil)
	spec := brokeredSpec()
	ref := startBrokered(t, executor, spec)
	propagation := metav1.DeletePropagationBackground
	if err := client.BatchV1().Jobs(testNamespace).Delete(context.Background(), ref.ExternalID+"-stage-in", metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil {
		t.Fatalf("delete stage-in: %v", err)
	}
	observation, err := executor.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observation.State != ports.ExecutionLost || observation.ErrorClass != domain.ErrWorkerLost {
		t.Fatalf("observation = %+v, want LOST", observation)
	}
}

func TestCapabilitiesAdvertisesBrokeredAndOverrides(t *testing.T) {
	executor, _ := brokeredExecutor(t, func(cfg *Config) {
		cfg.Languages = []string{"go", "rust"}
		cfg.Engines = []string{"custom-engine"}
		cfg.ExtraFeatures = []string{"plugin", "brokered-offline-io"}
	})
	capabilities, err := executor.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(capabilities.Languages) != 2 || capabilities.Languages[1] != "rust" {
		t.Fatalf("languages = %v", capabilities.Languages)
	}
	if len(capabilities.Engines) != 1 || capabilities.Engines[0] != "custom-engine" {
		t.Fatalf("engines = %v", capabilities.Engines)
	}
	features := map[string]bool{}
	for _, feature := range capabilities.Features {
		if features[feature] {
			t.Fatalf("duplicate feature %q in %v", feature, capabilities.Features)
		}
		features[feature] = true
	}
	for _, want := range []string{"kubernetes", "callback", "plugin", "brokered-offline-io"} {
		if !features[want] {
			t.Fatalf("missing feature %q in %v", want, capabilities.Features)
		}
	}
}

func TestBrokeredStartRequiresCallbackURL(t *testing.T) {
	executor, _ := brokeredExecutor(t, func(cfg *Config) {
		cfg.CallbackURL = ""
	})
	_, err := executor.Start(context.Background(), ports.ExecutionRequest{
		Spec: brokeredSpec(), AttemptToken: "token",
	})
	var classified *Error
	if !errors.As(err, &classified) || classified.Class != domain.ErrValidationFailed {
		t.Fatalf("error = %v, want VALIDATION_FAILED", err)
	}
}

func TestBrokeredStartRequiresToken(t *testing.T) {
	executor, _ := brokeredExecutor(t, nil)
	_, err := executor.Start(context.Background(), ports.ExecutionRequest{Spec: brokeredSpec()})
	var classified *Error
	if !errors.As(err, &classified) || classified.Class != domain.ErrValidationFailed {
		t.Fatalf("error = %v, want VALIDATION_FAILED", err)
	}
}

func TestBrokeredActiveDeadlineCarriesAcrossPhases(t *testing.T) {
	executor, client := brokeredExecutor(t, nil)
	spec := brokeredSpec()
	spec.Deadline = time.Now().Add(90 * time.Second)
	ref := startBrokered(t, executor, spec)
	succeedJob(t, client, ref.ExternalID+"-stage-in")
	if _, err := executor.Observe(context.Background(), ref); err != nil {
		t.Fatalf("advance: %v", err)
	}
	execute := getJob(t, client, ref.ExternalID+"-execute")
	if execute.Spec.ActiveDeadlineSeconds == nil || *execute.Spec.ActiveDeadlineSeconds > 90 || *execute.Spec.ActiveDeadlineSeconds < 1 {
		t.Fatalf("execute deadline = %v", execute.Spec.ActiveDeadlineSeconds)
	}
}
