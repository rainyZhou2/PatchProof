package kubernetes

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/patchproof/patchproof/internal/domain"
)

const (
	specVolumeKey   = "step.json"
	tokenKey        = "token"
	scratchVolume   = "scratch"
	workspaceVolume = "workspace"
	specVolume      = "spec"
	tokenVolume     = "token"
	workerContainer = "worker"

	workerUID      = int64(65532)
	scratchMount   = "/scratch"
	workspaceMount = "/workspace"
	specMount      = "/run/spec"
	tokenMount     = "/run/secrets/attempt-token"

	expiresAtAnnotation = "patchproof.io/expires-at"
	gpuResourceName     = corev1.ResourceName("nvidia.com/gpu")
)

// buildConfigMap renders the immutable step spec config map. Attempt
// credentials are never part of the serialized spec.
func buildConfigMap(name, namespace string, labels map[string]string, spec domain.StepSpec) (*corev1.ConfigMap, error) {
	sanitized := spec
	sanitized.AttemptToken = ""
	sanitized.CallbackURL = ""
	payload, err := json.Marshal(sanitized)
	if err != nil {
		return nil, &Error{Class: domain.ErrExecutorError, Message: "encode step spec: " + err.Error()}
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-spec",
			Namespace: namespace,
			Labels:    labels,
			Annotations: map[string]string{
				"patchproof.io/spec-key": specVolumeKey,
			},
		},
		Immutable: boolPtr(true),
		Data: map[string]string{
			specVolumeKey: string(payload),
		},
	}, nil
}

// buildSecret renders the per-attempt token secret. Kubernetes has no native
// secret TTL, so the expiry is recorded as an annotation for the agent's
// garbage collector.
func buildSecret(name, namespace string, labels map[string]string, token string, expiresAt time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-token",
			Namespace: namespace,
			Labels:    labels,
			Annotations: map[string]string{
				expiresAtAnnotation: expiresAt.UTC().Format(time.RFC3339),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{tokenKey: []byte(token)},
	}
}

// buildJob renders the locked-down worker job for an attempt.
func (e *Executor) buildJob(name, namespace string, spec domain.StepSpec, labels map[string]string, runtimeClass, callbackURL string, deadlineSeconds, ttlSeconds int64) (*batchv1.Job, error) {
	requirements := spec.Requirements
	resources := resourceRequirements(requirements)

	env := []corev1.EnvVar{
		{
			Name: "PATCHPROOF_ATTEMPT_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: name + "-token"},
					Key:                  tokenKey,
				},
			},
		},
		{Name: "PATCHPROOF_CALLBACK_URL", Value: callbackURL},
		{Name: "PATCHPROOF_CLUSTER_ID", Value: string(e.cfg.ClusterID)},
		{Name: "PATCHPROOF_SPEC_FILE", Value: specMount + "/" + specVolumeKey},
	}
	if e.cfg.EngineImage != "" {
		env = append(env, corev1.EnvVar{Name: "PATCHPROOF_ENGINE_IMAGE", Value: e.cfg.EngineImage})
	}
	extraKeys := make([]string, 0, len(e.cfg.ExtraEnv))
	for key := range e.cfg.ExtraEnv {
		extraKeys = append(extraKeys, key)
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		reserved := false
		for _, existing := range env {
			if existing.Name == key {
				reserved = true
				break
			}
		}
		if reserved {
			continue
		}
		env = append(env, corev1.EnvVar{Name: key, Value: e.cfg.ExtraEnv[key]})
	}

	volumes := []corev1.Volume{
		{
			Name: scratchVolume,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: scratchSizeLimit(requirements)},
			},
		},
		{
			Name:         workspaceVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name: specVolume,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: name + "-spec"},
					Items:                []corev1.KeyToPath{{Key: specVolumeKey, Path: specVolumeKey}},
					DefaultMode:          int32Ptr(0o444),
				},
			},
		},
		{
			Name: tokenVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  name + "-token",
					Items:       []corev1.KeyToPath{{Key: tokenKey, Path: tokenKey}},
					DefaultMode: int32Ptr(0o400),
				},
			},
		},
	}

	pod := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtr(e.cfg.AutomountServiceAccountToken),
			NodeSelector:                 e.cfg.NodeSelector,
			Tolerations:                  append([]corev1.Toleration(nil), e.cfg.Tolerations...),
			EnableServiceLinks:           boolPtr(false),
			ServiceAccountName:           e.cfg.ServiceAccount,
			RuntimeClassName:             optionalString(runtimeClass),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   boolPtr(true),
				RunAsUser:      int64Ptr(workerUID),
				RunAsGroup:     int64Ptr(workerUID),
				FSGroup:        int64Ptr(workerUID),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{
				{
					Name:            workerContainer,
					Image:           e.cfg.WorkerImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Env:             env,
					Resources:       resources,
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
					VolumeMounts: []corev1.VolumeMount{
						{Name: scratchVolume, MountPath: scratchMount},
						{Name: workspaceVolume, MountPath: workspaceMount},
						{Name: specVolume, MountPath: specMount, ReadOnly: true},
						{Name: tokenVolume, MountPath: tokenMount, ReadOnly: true},
					},
				},
			},
			Volumes: volumes,
		},
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
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

func resourceRequirements(requirements domain.ResourceRequirements) corev1.ResourceRequirements {
	limits := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(requirements.CPUMilli, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(requirements.MemoryMiB<<20, resource.BinarySI),
	}
	requests := limits.DeepCopy()
	if requirements.EphemeralDiskGiB > 0 {
		disk := resource.NewQuantity(requirements.EphemeralDiskGiB<<30, resource.BinarySI)
		requests[corev1.ResourceEphemeralStorage] = *disk
		limits[corev1.ResourceEphemeralStorage] = *disk
	}
	if requirements.GPUCount > 0 {
		gpus := resource.NewQuantity(int64(requirements.GPUCount), resource.DecimalSI)
		requests[gpuResourceName] = *gpus
		limits[gpuResourceName] = *gpus
	}
	return corev1.ResourceRequirements{Requests: requests, Limits: limits}
}

func scratchSizeLimit(requirements domain.ResourceRequirements) *resource.Quantity {
	if requirements.EphemeralDiskGiB <= 0 {
		return nil
	}
	return resource.NewQuantity(requirements.EphemeralDiskGiB<<30, resource.BinarySI)
}

func boolPtr(value bool) *bool { return &value }

func int32Ptr(value int32) *int32 { return &value }

func int64Ptr(value int64) *int64 { return &value }

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return fmt.Sprintf("%s...", value[:limit])
}
