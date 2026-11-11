package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

const (
	brokerAppName = "patchproof-broker"

	brokeredPhaseLabel         = "patchproof.io/phase"
	brokeredCallbackAnnotation = "patchproof.io/callback-url"
	brokeredDeadlineAnnotation = "patchproof.io/deadline"

	brokeredStageIn         = "stage-in"
	brokeredExecute         = "execute"
	brokeredStageOut        = "stage-out"
	brokeredWorkspaceRole   = "workspace"
	brokeredStageInSuffix   = "-stage-in"
	brokeredExecuteSuffix   = "-execute"
	brokeredStageOutSuffix  = "-stage-out"
	brokeredWorkspaceSuffix = "-ws"

	brokeredWorkspaceMount = "/work"
	brokeredWorkerWorkDir  = "/work/workspace"
	brokeredSpecFile       = specMount + "/" + specVolumeKey
	brokeredContainer      = "broker"

	phaseEnv         = "PATCHPROOF_PHASE"
	localRootEnv     = "PATCHPROOF_LOCAL_ROOT"
	offlineRootEnv   = "PATCHPROOF_OFFLINE_ARTIFACT_ROOT"
	specFileEnv      = "PATCHPROOF_SPEC_FILE"
	workerWorkDirEnv = "PATCHPROOF_WORKER_WORKDIR"
	callbackEnv      = "PATCHPROOF_CALLBACK_URL"
	attemptTokenEnv  = "PATCHPROOF_ATTEMPT_TOKEN"
	tempDirEnv       = "TMPDIR"

	defaultWorkspaceGiB = 1
)

// executeForbiddenPrefixes are environment families that must never reach the
// offline execution sandbox. The sandbox has no network and no CAS identity.
var executeForbiddenPrefixes = []string{
	"PATCHPROOF_S3_",
	"PATCHPROOF_CAS_",
	"AWS_",
	"PATCHPROOF_ATTEMPT_TOKEN",
	"PATCHPROOF_CALLBACK_URL",
}

// brokeredReservedEnv is set by the executor on broker/worker containers and
// cannot be overridden through ExtraEnv.
var brokeredReservedEnv = map[string]struct{}{
	phaseEnv:         {},
	localRootEnv:     {},
	offlineRootEnv:   {},
	specFileEnv:      {},
	workerWorkDirEnv: {},
	callbackEnv:      {},
	attemptTokenEnv:  {},
}

// startBrokered creates the workspace PVC, the spec config map, the token
// secret, and the stage-in job. Repeated calls for the same attempt return the
// existing execution without creating a second phase.
func (e *Executor) startBrokered(ctx context.Context, spec domain.StepSpec, attemptToken string, ref ports.ExecutionRef, runtimeClass, callbackURL string, deadline time.Time) (ports.ExecutionRef, error) {
	if strings.TrimSpace(e.cfg.BrokerImage) == "" {
		return ref, &Error{Class: domain.ErrSandboxUnavailable, Message: "brokered offline IO requires a configured broker image"}
	}
	if strings.TrimSpace(callbackURL) == "" {
		return ref, &Error{Class: domain.ErrValidationFailed, Message: "brokered offline IO requires a callback URL"}
	}
	namespace, name := ref.Namespace, ref.ExternalID
	exists, err := e.brokeredPhaseExists(ctx, namespace, name)
	if err != nil {
		return ref, err
	}
	if exists {
		return ref, nil
	}

	labels := e.labels(spec)
	configMap, err := buildConfigMap(name, namespace, labels, spec)
	if err != nil {
		return ref, err
	}
	configMap.Annotations[brokeredCallbackAnnotation] = callbackURL
	configMap.Annotations[brokeredDeadlineAnnotation] = deadline.UTC().Format(time.RFC3339)
	secret := buildSecret(name, namespace, labels, attemptToken, deadline.Add(tokenTTLSlack))
	workspace := e.buildWorkspacePVC(name, namespace, labels)
	stageIn, err := e.buildPhaseJob(name, namespace, spec, labels, brokeredStageIn, runtimeClass, callbackURL,
		secondsUntil(e.cfg.Clock.Now(), deadline), ttlSeconds(e.cfg.JobTTL))
	if err != nil {
		return ref, err
	}

	if err := e.ensureConfigMap(ctx, configMap); err != nil {
		return ref, err
	}
	if err := e.ensureSecret(ctx, secret); err != nil {
		e.cleanupBrokered(ctx, namespace, name)
		return ref, err
	}
	if err := e.ensureWorkspacePVC(ctx, workspace); err != nil {
		e.cleanupBrokered(ctx, namespace, name)
		return ref, err
	}
	if err := e.ensureJob(ctx, stageIn); err != nil {
		e.cleanupBrokered(ctx, namespace, name)
		return ref, err
	}
	return ref, nil
}

// brokeredPhaseExists reports whether any phase job already exists for the
// attempt. The check makes Start idempotent even after a controller restart.
func (e *Executor) brokeredPhaseExists(ctx context.Context, namespace, name string) (bool, error) {
	for _, suffix := range []string{brokeredStageInSuffix, brokeredExecuteSuffix, brokeredStageOutSuffix} {
		_, err := e.client.BatchV1().Jobs(namespace).Get(ctx, name+suffix, metav1.GetOptions{})
		switch {
		case err == nil:
			return true, nil
		case apierrors.IsNotFound(err):
			continue
		default:
			return false, e.wrapAPIError("get phase job", err)
		}
	}
	return false, nil
}

// observeBrokered drives the stage-in -> execute -> stage-out state machine.
// The third return value reports whether the reference belongs to a brokered
// attempt; direct-mode executions fall through to the existing observer.
func (e *Executor) observeBrokered(ctx context.Context, namespace, name string) (ports.ExecutionObservation, bool, error) {
	for _, phase := range []struct {
		name   string
		suffix string
	}{
		{brokeredStageOut, brokeredStageOutSuffix},
		{brokeredExecute, brokeredExecuteSuffix},
		{brokeredStageIn, brokeredStageInSuffix},
	} {
		job, err := e.client.BatchV1().Jobs(namespace).Get(ctx, name+phase.suffix, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return ports.ExecutionObservation{}, true, e.wrapAPIError("get phase job", err)
		}
		observation, err := e.observePhaseJob(ctx, namespace, job)
		if err != nil {
			return ports.ExecutionObservation{}, true, err
		}
		switch phase.name {
		case brokeredStageIn:
			if observation.State == ports.ExecutionCompleted {
				if err := e.ensurePhaseJob(ctx, namespace, name, brokeredExecute); err != nil {
					return ports.ExecutionObservation{}, true, err
				}
				return ports.ExecutionObservation{State: ports.ExecutionRunning, Message: "stage-in complete; execute job started"}, true, nil
			}
		case brokeredExecute:
			if observation.State == ports.ExecutionCompleted {
				if err := e.ensurePhaseJob(ctx, namespace, name, brokeredStageOut); err != nil {
					return ports.ExecutionObservation{}, true, err
				}
				return ports.ExecutionObservation{State: ports.ExecutionRunning, Message: "execute complete; stage-out job started"}, true, nil
			}
		case brokeredStageOut:
			if observation.State == ports.ExecutionCompleted {
				if err := e.deleteWorkspacePVC(ctx, namespace, name); err != nil {
					return ports.ExecutionObservation{}, true, err
				}
				return ports.ExecutionObservation{State: ports.ExecutionCompleted, Message: observation.Message}, true, nil
			}
		}
		return observation, true, nil
	}

	// No phase job survived. A lingering PVC proves the attempt used brokered
	// transport, so report LOST instead of misclassifying it as a direct job.
	_, err := e.client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name+brokeredWorkspaceSuffix, metav1.GetOptions{})
	switch {
	case err == nil:
		return ports.ExecutionObservation{
			State:      ports.ExecutionLost,
			ErrorClass: domain.ErrWorkerLost,
			Message:    fmt.Sprintf("brokered attempt %s/%s has no phase job", namespace, name),
		}, true, nil
	case apierrors.IsNotFound(err):
		return ports.ExecutionObservation{}, false, nil
	default:
		return ports.ExecutionObservation{}, true, e.wrapAPIError("get workspace claim", err)
	}
}

// observePhaseJob maps one phase job to an observation using the same failure
// classification as direct mode.
func (e *Executor) observePhaseJob(ctx context.Context, namespace string, job *batchv1.Job) (ports.ExecutionObservation, error) {
	switch jobStateOf(job) {
	case jobSucceeded:
		return ports.ExecutionObservation{State: ports.ExecutionCompleted, Message: conditionMessage(job, true)}, nil
	case jobFailed, jobActive:
		pods, listErr := e.listPods(ctx, namespace, job.Name)
		if listErr != nil && jobStateOf(job) == jobFailed {
			return ports.ExecutionObservation{}, e.wrapAPIError("list job pods", listErr)
		}
		if class, message, ok := e.failureFromPods(ctx, namespace, pods); ok {
			return ports.ExecutionObservation{State: ports.ExecutionFailed, ErrorClass: class, Message: message}, nil
		}
		if jobStateOf(job) == jobFailed {
			class, message := classifyJobFailure(job, pods)
			return ports.ExecutionObservation{State: ports.ExecutionFailed, ErrorClass: class, Message: message}, nil
		}
		return ports.ExecutionObservation{State: ports.ExecutionRunning}, nil
	default:
		return ports.ExecutionObservation{State: ports.ExecutionPending}, nil
	}
}

// ensurePhaseJob creates the next phase job if it does not exist yet. The spec
// and callback metadata come from the immutable config map written at Start.
func (e *Executor) ensurePhaseJob(ctx context.Context, namespace, name, phase string) error {
	jobName := name + "-" + phase
	if _, err := e.client.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return e.wrapAPIError("get phase job", err)
	}
	spec, deadline, callbackURL, err := e.loadBrokeredMetadata(ctx, namespace, name)
	if err != nil {
		return err
	}
	runtimeClass, err := e.runtimeClassFor(spec.Requirements)
	if err != nil {
		return err
	}
	labels := e.labels(spec)
	job, err := e.buildPhaseJob(name, namespace, spec, labels, phase, runtimeClass, callbackURL,
		secondsUntil(e.cfg.Clock.Now(), deadline), ttlSeconds(e.cfg.JobTTL))
	if err != nil {
		return err
	}
	return e.ensureJob(ctx, job)
}

func (e *Executor) loadBrokeredMetadata(ctx context.Context, namespace, name string) (domain.StepSpec, time.Time, string, error) {
	configMap, err := e.client.CoreV1().ConfigMaps(namespace).Get(ctx, name+"-spec", metav1.GetOptions{})
	if err != nil {
		return domain.StepSpec{}, time.Time{}, "", e.wrapAPIError("get spec config map", err)
	}
	raw := configMap.Data[specVolumeKey]
	if strings.TrimSpace(raw) == "" {
		return domain.StepSpec{}, time.Time{}, "", &Error{Class: domain.ErrExecutorError, Message: "spec config map is missing " + specVolumeKey}
	}
	var spec domain.StepSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return domain.StepSpec{}, time.Time{}, "", &Error{Class: domain.ErrExecutorError, Message: "decode spec config map: " + err.Error()}
	}
	deadline := spec.Deadline
	if encoded := configMap.Annotations[brokeredDeadlineAnnotation]; encoded != "" {
		parsed, err := time.Parse(time.RFC3339, encoded)
		if err != nil {
			return domain.StepSpec{}, time.Time{}, "", &Error{Class: domain.ErrExecutorError, Message: "invalid deadline annotation: " + err.Error()}
		}
		deadline = parsed
	}
	if deadline.IsZero() {
		deadline = e.cfg.Clock.Now().Add(e.cfg.ActiveDeadline)
	}
	callbackURL := configMap.Annotations[brokeredCallbackAnnotation]
	if callbackURL == "" {
		callbackURL = e.cfg.CallbackURL
	}
	if callbackURL != "" && !validCallbackURL(callbackURL) {
		return domain.StepSpec{}, time.Time{}, "", &Error{Class: domain.ErrExecutorError, Message: "invalid callback URL annotation"}
	}
	return spec, deadline, callbackURL, nil
}

func (e *Executor) ensureJob(ctx context.Context, job *batchv1.Job) error {
	if _, err := e.client.BatchV1().Jobs(job.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return e.wrapAPIError("create "+job.Name+" job", err)
	}
	return nil
}

func (e *Executor) ensureWorkspacePVC(ctx context.Context, claim *corev1.PersistentVolumeClaim) error {
	if _, err := e.client.CoreV1().PersistentVolumeClaims(claim.Namespace).Create(ctx, claim, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return e.wrapAPIError("create workspace claim", err)
	}
	return nil
}

func (e *Executor) deleteWorkspacePVC(ctx context.Context, namespace, name string) error {
	if err := e.client.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name+brokeredWorkspaceSuffix, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return e.wrapAPIError("delete workspace claim", err)
	}
	return nil
}

func (e *Executor) cleanupBrokered(ctx context.Context, namespace, name string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), apiRequestTimeout)
	defer cancel()
	propagation := metav1.DeletePropagationBackground
	for _, suffix := range []string{brokeredStageInSuffix, brokeredExecuteSuffix, brokeredStageOutSuffix} {
		_ = e.client.BatchV1().Jobs(namespace).Delete(cleanupCtx, name+suffix, metav1.DeleteOptions{PropagationPolicy: &propagation})
	}
	_ = e.client.CoreV1().ConfigMaps(namespace).Delete(cleanupCtx, name+"-spec", metav1.DeleteOptions{})
	_ = e.client.CoreV1().Secrets(namespace).Delete(cleanupCtx, name+"-token", metav1.DeleteOptions{})
	_ = e.client.CoreV1().PersistentVolumeClaims(namespace).Delete(cleanupCtx, name+brokeredWorkspaceSuffix, metav1.DeleteOptions{})
}

// buildWorkspacePVC renders the per-attempt workspace claim shared by all
// three phases.
func (e *Executor) buildWorkspacePVC(name, namespace string, labels map[string]string) *corev1.PersistentVolumeClaim {
	sizeGiB := e.cfg.WorkspaceSizeGiB
	if sizeGiB < defaultWorkspaceGiB {
		sizeGiB = defaultWorkspaceGiB
	}
	quantity := resource.NewQuantity(sizeGiB<<30, resource.BinarySI)
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + brokeredWorkspaceSuffix,
			Namespace: namespace,
			Labels:    withPhaseLabel(labels, brokeredWorkspaceRole),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: optionalString(e.cfg.BrokeredStorageClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *quantity},
			},
		},
	}
}

// buildPhaseJob renders one hardened phase of the brokered flow.
func (e *Executor) buildPhaseJob(name, namespace string, spec domain.StepSpec, labels map[string]string, phase, runtimeClass, callbackURL string, deadlineSeconds, ttlSeconds int64) (*batchv1.Job, error) {
	var (
		image           string
		containerName   string
		serviceAccount  string
		args            []string
		env             []corev1.EnvVar
		volumeMounts    []corev1.VolumeMount
		volumes         []corev1.Volume
		runRuntimeClass string
	)
	phaseLabels := withPhaseLabel(labels, phase)
	workspaceVolumeMount := corev1.VolumeMount{Name: workspaceVolume, MountPath: brokeredWorkspaceMount}
	specVolumeMount := corev1.VolumeMount{Name: specVolume, MountPath: specMount, ReadOnly: true}
	specVolumeSource := corev1.Volume{
		Name: specVolume,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: name + "-spec"},
				Items:                []corev1.KeyToPath{{Key: specVolumeKey, Path: specVolumeKey}},
				DefaultMode:          int32Ptr(0o444),
			},
		},
	}
	workspaceVolumeSource := corev1.Volume{
		Name: workspaceVolume,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name + brokeredWorkspaceSuffix},
		},
	}

	switch phase {
	case brokeredStageIn, brokeredStageOut:
		image = e.cfg.BrokerImage
		containerName = brokeredContainer
		serviceAccount = firstNonEmpty(e.cfg.BrokerServiceAccount, e.cfg.ServiceAccount)
		phaseLabels["app.kubernetes.io/name"] = brokerAppName
		args = []string{
			"--phase=" + phase,
			"--spec-file=" + brokeredSpecFile,
			"--local-root=" + brokeredWorkspaceMount,
		}
		env = []corev1.EnvVar{
			{Name: phaseEnv, Value: phase},
			{Name: localRootEnv, Value: brokeredWorkspaceMount},
			{Name: specFileEnv, Value: brokeredSpecFile},
		}
		env = appendExtraEnv(env, e.cfg.ExtraEnv, func(key string) bool {
			_, reserved := brokeredReservedEnv[key]
			return !reserved
		})
		volumeMounts = []corev1.VolumeMount{workspaceVolumeMount, specVolumeMount}
		volumes = []corev1.Volume{workspaceVolumeSource, specVolumeSource}
		if phase == brokeredStageOut {
			env = append(env,
				corev1.EnvVar{Name: callbackEnv, Value: callbackURL},
				corev1.EnvVar{
					Name: attemptTokenEnv,
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: name + "-token"},
							Key:                  tokenKey,
						},
					},
				},
			)
			volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: tokenVolume, MountPath: tokenMount, ReadOnly: true})
			volumes = append(volumes, corev1.Volume{
				Name: tokenVolume,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName:  name + "-token",
						Items:       []corev1.KeyToPath{{Key: tokenKey, Path: tokenKey}},
						DefaultMode: int32Ptr(0o400),
					},
				},
			})
		}
	case brokeredExecute:
		image = e.cfg.WorkerImage
		containerName = workerContainer
		serviceAccount = e.cfg.ServiceAccount
		runRuntimeClass = runtimeClass
		env = []corev1.EnvVar{
			{Name: phaseEnv, Value: phase},
			{Name: offlineRootEnv, Value: brokeredWorkspaceMount},
			{Name: specFileEnv, Value: brokeredSpecFile},
			{Name: workerWorkDirEnv, Value: brokeredWorkerWorkDir},
			// The worker image defaults TMPDIR to /scratch, which is not
			// mounted in brokered mode. Keep toolchain scratch on the PVC.
			{Name: tempDirEnv, Value: brokeredWorkspaceMount},
			{Name: "PATCHPROOF_CLUSTER_ID", Value: string(e.cfg.ClusterID)},
		}
		if e.cfg.EngineImage != "" {
			env = append(env, corev1.EnvVar{Name: "PATCHPROOF_ENGINE_IMAGE", Value: e.cfg.EngineImage})
		}
		env = appendExtraEnv(env, e.cfg.ExtraEnv, func(key string) bool {
			if _, reserved := brokeredReservedEnv[key]; reserved {
				return false
			}
			return !forbiddenExecuteEnv(key)
		})
		volumeMounts = []corev1.VolumeMount{workspaceVolumeMount, specVolumeMount}
		volumes = []corev1.Volume{workspaceVolumeSource, specVolumeSource}
	default:
		return nil, &Error{Class: domain.ErrExecutorError, Message: fmt.Sprintf("unknown brokered phase %q", phase)}
	}

	// Trusted broker phases do not execute subject code and must not reserve
	// accelerators; only the execution sandbox inherits GPU requests.
	resourceSpec := spec.Requirements
	if phase != brokeredExecute {
		resourceSpec.GPUCount = 0
	}

	container := corev1.Container{
		Name:            containerName,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            args,
		Env:             env,
		Resources:       resourceRequirements(resourceSpec),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			Privileged:               boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			RunAsNonRoot:             boolPtr(true),
			RunAsUser:                int64Ptr(workerUID),
			RunAsGroup:               int64Ptr(workerUID),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		VolumeMounts: volumeMounts,
	}

	pod := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: phaseLabels},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtr(false),
			NodeSelector:                 e.cfg.NodeSelector,
			Tolerations:                  append([]corev1.Toleration(nil), e.cfg.Tolerations...),
			EnableServiceLinks:           boolPtr(false),
			ServiceAccountName:           serviceAccount,
			RuntimeClassName:             optionalString(runRuntimeClass),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   boolPtr(true),
				RunAsUser:      int64Ptr(workerUID),
				RunAsGroup:     int64Ptr(workerUID),
				FSGroup:        int64Ptr(workerUID),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{container},
			Volumes:    volumes,
		},
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-" + phase,
			Namespace: namespace,
			Labels:    phaseLabels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			Completions:             int32Ptr(1),
			Parallelism:             int32Ptr(1),
			ActiveDeadlineSeconds:   int64Ptr(deadlineSeconds),
			TTLSecondsAfterFinished: int32Ptr(int32(ttlSeconds)),
			Template:                pod,
		},
	}, nil
}

// appendExtraEnv appends the allowed, non-reserved ExtraEnv entries in name
// order, skipping any env name the executor already set.
func appendExtraEnv(base []corev1.EnvVar, extra map[string]string, allow func(string) bool) []corev1.EnvVar {
	existing := make(map[string]struct{}, len(base))
	for _, variable := range base {
		existing[variable.Name] = struct{}{}
	}
	keys := make([]string, 0, len(extra))
	for key := range extra {
		if _, duplicate := existing[key]; duplicate {
			continue
		}
		if !allow(key) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		base = append(base, corev1.EnvVar{Name: key, Value: extra[key]})
	}
	return base
}

func forbiddenExecuteEnv(key string) bool {
	for _, prefix := range executeForbiddenPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func withPhaseLabel(labels map[string]string, phase string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for key, value := range labels {
		out[key] = value
	}
	out[brokeredPhaseLabel] = phase
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
