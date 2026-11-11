// Package kubernetes executes steps as hardened Kubernetes Jobs. The
// controller-owned retry loop is authoritative: jobs run with backoffLimit 0
// and no restart policy so an attempt never silently outlives its lease.
package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/clock"
	"github.com/patchproof/patchproof/internal/ports"
)

const (
	executorName  = "kubernetes"
	workerAppName = "patchproof-worker"
	executorKind  = "kubernetes"

	defaultNamespace      = "default"
	defaultJobTTL         = 30 * time.Minute
	defaultActiveDeadline = 20 * time.Minute
	defaultPollInterval   = 2 * time.Second
	defaultMaxConcurrent  = 64
	apiRequestTimeout     = 30 * time.Second
	tokenTTLSlack         = 5 * time.Minute
	maxMessageLength      = 512
)

var (
	supportedLanguages = []string{"go"}
	supportedEngines   = []string{"go-analyzer", "go-differential", "go-fuzz", "z3-bounded", "semgrep"}
)

// Config configures the Kubernetes executor.
type Config struct {
	// ClusterID identifies the cluster that owns the created jobs.
	ClusterID domain.ClusterID
	// Region is reported in capabilities.
	Region string
	// Namespace is the only namespace the executor creates jobs in.
	Namespace string
	// Kubeconfig points at a kubeconfig file; empty selects in-cluster config.
	Kubeconfig string
	// ServiceAccount is the workload identity used by worker pods.
	ServiceAccount string
	// WorkerImage is the image running the worker entrypoint.
	WorkerImage string
	// BrokerImage is the image running the trusted stage-in/stage-out broker.
	// It is required for brokered-offline-io attempts.
	BrokerImage string
	// BrokerServiceAccount is the workload identity used by broker pods. It
	// falls back to ServiceAccount when empty.
	BrokerServiceAccount string
	// BrokeredStorageClass is the storage class of the per-attempt workspace
	// PVC. Empty uses the cluster default.
	BrokeredStorageClass string
	// WorkspaceSizeGiB is the workspace PVC size in GiB. Values below one
	// select the one GiB default.
	WorkspaceSizeGiB int64
	// EngineImage is passed to the worker through PATCHPROOF_ENGINE_IMAGE.
	EngineImage string
	// RuntimeClass enables the untrusted isolation tiers when set.
	RuntimeClass string
	// JobTTL is the ttlSecondsAfterFinished applied to finished jobs.
	JobTTL time.Duration
	// ActiveDeadline is used when a step spec does not carry its own deadline.
	ActiveDeadline time.Duration
	// PollInterval is the advisory cadence for callers polling Observe.
	PollInterval time.Duration
	// MaxConcurrent is advertised to the scheduler.
	MaxConcurrent int
	// CallbackURL is the default worker callback endpoint.
	CallbackURL string
	// SecretNamePrefix prefixes derived resource names and defaults to "pp".
	SecretNamePrefix string
	// Clock supplies the current time and defaults to the system clock.
	Clock ports.Clock
	// NodeSelector restricts worker pods to the configured execution pool.
	NodeSelector map[string]string
	// Tolerations allow worker pods onto tainted execution node pools.
	Tolerations []corev1.Toleration
	// AutomountServiceAccountToken exposes the pod service account token to
	// the worker. Enable only with a tightly scoped workload-identity role.
	AutomountServiceAccountToken bool
	// Languages and Engines override the advertised capability lists. Empty
	// values keep the bundled Go defaults. Operators add external plugin
	// engines and languages here.
	Languages []string
	Engines   []string
	// ExtraFeatures adds capability features to the advertised cluster.
	ExtraFeatures []string
	// ExtraEnv adds non-secret worker environment variables (sorted by name).
	// Reserved keys already set by the executor cannot be overridden.
	ExtraEnv map[string]string
	// Client overrides the clientset. It exists for tests and embedded use;
	// production callers leave it nil.
	Client kubeclient.Interface
}

// Error reports a classified executor failure.
type Error struct {
	Class   domain.ErrorClass
	Message string
}

func (e *Error) Error() string { return e.Message }

// ErrorClass returns the domain classification of the failure.
func (e *Error) ErrorClass() domain.ErrorClass { return e.Class }

var _ ports.Executor = (*Executor)(nil)

// Executor creates and observes hardened Kubernetes Jobs.
type Executor struct {
	cfg    Config
	client kubeclient.Interface
}

// New builds an executor. It never contacts the API server; connectivity is
// discovered lazily by Start, Observe, and Cancel.
func New(cfg Config) (*Executor, error) {
	if strings.TrimSpace(cfg.WorkerImage) == "" {
		return nil, errors.New("kubernetes executor: worker image is required")
	}
	if cfg.Namespace == "" {
		cfg.Namespace = defaultNamespace
	}
	if problems := validation.IsDNS1123Label(cfg.Namespace); len(problems) > 0 {
		return nil, fmt.Errorf("kubernetes executor: invalid namespace %q: %s", cfg.Namespace, strings.Join(problems, "; "))
	}
	if cfg.RuntimeClass != "" {
		if problems := validation.IsDNS1123Subdomain(cfg.RuntimeClass); len(problems) > 0 {
			return nil, fmt.Errorf("kubernetes executor: invalid runtime class %q: %s", cfg.RuntimeClass, strings.Join(problems, "; "))
		}
	}
	if cfg.ServiceAccount != "" {
		if problems := validation.IsDNS1123Subdomain(cfg.ServiceAccount); len(problems) > 0 {
			return nil, fmt.Errorf("kubernetes executor: invalid service account %q: %s", cfg.ServiceAccount, strings.Join(problems, "; "))
		}
	}
	if cfg.BrokerServiceAccount != "" {
		if problems := validation.IsDNS1123Subdomain(cfg.BrokerServiceAccount); len(problems) > 0 {
			return nil, fmt.Errorf("kubernetes executor: invalid broker service account %q: %s", cfg.BrokerServiceAccount, strings.Join(problems, "; "))
		}
	}
	if cfg.BrokeredStorageClass != "" {
		if problems := validation.IsDNS1123Subdomain(cfg.BrokeredStorageClass); len(problems) > 0 {
			return nil, fmt.Errorf("kubernetes executor: invalid brokered storage class %q: %s", cfg.BrokeredStorageClass, strings.Join(problems, "; "))
		}
	}
	if cfg.WorkspaceSizeGiB < 0 {
		return nil, errors.New("kubernetes executor: workspace size must not be negative")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System{}
	}
	if cfg.JobTTL <= 0 {
		cfg.JobTTL = defaultJobTTL
	}
	if cfg.JobTTL < time.Second {
		return nil, errors.New("kubernetes executor: job TTL must be at least one second")
	}
	if cfg.ActiveDeadline < 0 {
		return nil, errors.New("kubernetes executor: active deadline must not be negative")
	}
	if cfg.ActiveDeadline == 0 {
		cfg.ActiveDeadline = defaultActiveDeadline
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = defaultMaxConcurrent
	}
	if cfg.SecretNamePrefix == "" {
		cfg.SecretNamePrefix = "pp"
	}
	if cfg.CallbackURL != "" && !validCallbackURL(cfg.CallbackURL) {
		return nil, fmt.Errorf("kubernetes executor: callback URL %q must be an absolute HTTP(S) URL", cfg.CallbackURL)
	}
	if cfg.ClusterID == "" {
		cfg.ClusterID = domain.ClusterID(executorName)
	}

	client := cfg.Client
	if client == nil {
		restConfig, err := restConfigFor(cfg.Kubeconfig)
		if err != nil {
			return nil, err
		}
		client, err = kubeclient.NewForConfig(restConfig)
		if err != nil {
			return nil, fmt.Errorf("kubernetes executor: build client: %w", err)
		}
	}
	return &Executor{cfg: cfg, client: client}, nil
}

func restConfigFor(kubeconfig string) (*rest.Config, error) {
	var (
		cfg *rest.Config
		err error
	)
	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("kubernetes executor: in-cluster configuration: %w", err)
		}
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("kubernetes executor: load kubeconfig %s: %w", kubeconfig, err)
		}
	}
	cfg.QPS = 20
	cfg.Burst = 40
	cfg.Timeout = apiRequestTimeout
	cfg.UserAgent = "patchproof-kubernetes-executor/" + executorKind
	return cfg, nil
}

// Capabilities reports the isolation tiers, runtime classes, and engines this
// executor can serve.
func (e *Executor) Capabilities(ctx context.Context) (ports.ExecutorCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return ports.ExecutorCapabilities{}, err
	}
	isolations := []domain.IsolationTier{domain.IsolationTrustedLocal, domain.IsolationContainer}
	var runtimeClasses []string
	if e.cfg.RuntimeClass != "" {
		isolations = append(isolations, domain.IsolationGVisor, domain.IsolationVM)
		runtimeClasses = []string{e.cfg.RuntimeClass}
	}
	languages := append([]string(nil), supportedLanguages...)
	if len(e.cfg.Languages) > 0 {
		languages = append([]string(nil), e.cfg.Languages...)
	}
	engines := append([]string(nil), supportedEngines...)
	if len(e.cfg.Engines) > 0 {
		engines = append([]string(nil), e.cfg.Engines...)
	}
	features := []string{"kubernetes", "callback"}
	features = append(features, e.cfg.ExtraFeatures...)
	if e.cfg.BrokerImage != "" {
		features = append(features, "brokered-offline-io")
	}
	return ports.ExecutorCapabilities{
		Name:           executorName,
		ExecutorKind:   executorKind,
		Region:         e.cfg.Region,
		Architectures:  []string{"amd64", "arm64"},
		Languages:      languages,
		Engines:        engines,
		IsolationTiers: isolations,
		RuntimeClasses: runtimeClasses,
		Features:       dedupeStrings(features),
		MaxConcurrent:  e.cfg.MaxConcurrent,
	}, nil
}

// Start creates the spec config map, the token secret, and the worker job. It
// is idempotent: an existing job with the deterministic attempt name is
// returned unchanged.
func (e *Executor) Start(ctx context.Context, request ports.ExecutionRequest) (ports.ExecutionRef, error) {
	spec := request.Spec
	if err := validateSpec(spec); err != nil {
		return ports.ExecutionRef{}, &Error{Class: domain.ErrValidationFailed, Message: "invalid step spec: " + err.Error()}
	}
	if request.AttemptToken == "" {
		return ports.ExecutionRef{}, &Error{Class: domain.ErrValidationFailed, Message: "attempt token is required"}
	}
	if err := validateResourceBounds(spec.Requirements); err != nil {
		return ports.ExecutionRef{}, &Error{Class: domain.ErrValidationFailed, Message: err.Error()}
	}
	runtimeClass, err := e.runtimeClassFor(spec.Requirements)
	if err != nil {
		return ports.ExecutionRef{}, err
	}
	callbackURL := request.CallbackURL
	if callbackURL == "" {
		callbackURL = e.cfg.CallbackURL
	}
	if callbackURL != "" && !validCallbackURL(callbackURL) {
		return ports.ExecutionRef{}, &Error{Class: domain.ErrValidationFailed, Message: "callback URL must be an absolute HTTP(S) URL"}
	}
	deadline, err := e.deadline(spec)
	if err != nil {
		return ports.ExecutionRef{}, err
	}

	namespace := e.cfg.Namespace
	name := e.resourceName(spec)
	ref := ports.ExecutionRef{
		Executor:   executorName,
		ClusterID:  e.cfg.ClusterID,
		Namespace:  namespace,
		ExternalID: name,
	}

	if spec.Requirements.ArtifactTransport == domain.TransportBrokeredOffline {
		return e.startBrokered(ctx, spec, request.AttemptToken, ref, runtimeClass, callbackURL, deadline)
	}

	if _, err := e.client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		return ref, nil
	} else if !apierrors.IsNotFound(err) {
		return ports.ExecutionRef{}, e.wrapAPIError("get job", err)
	}

	labels := e.labels(spec)
	configMap, err := buildConfigMap(name, namespace, labels, spec)
	if err != nil {
		return ports.ExecutionRef{}, err
	}
	secret := buildSecret(name, namespace, labels, request.AttemptToken, deadline.Add(tokenTTLSlack))
	job, err := e.buildJob(name, namespace, spec, labels, runtimeClass, callbackURL, secondsUntil(e.cfg.Clock.Now(), deadline), ttlSeconds(e.cfg.JobTTL))
	if err != nil {
		return ports.ExecutionRef{}, err
	}

	if err := e.ensureConfigMap(ctx, configMap); err != nil {
		return ports.ExecutionRef{}, err
	}
	if err := e.ensureSecret(ctx, secret); err != nil {
		e.cleanupResources(ctx, namespace, name)
		return ports.ExecutionRef{}, err
	}
	if _, err := e.client.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if _, getErr := e.client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{}); getErr == nil {
				return ref, nil
			}
			return ports.ExecutionRef{}, e.wrapAPIError("get existing job", err)
		}
		e.cleanupResources(ctx, namespace, name)
		return ports.ExecutionRef{}, e.wrapAPIError("create job", err)
	}
	return ref, nil
}

// Observe maps the job and its pods to an execution observation. Terminal
// results arrive through the worker callback; Result is always nil here.
func (e *Executor) Observe(ctx context.Context, ref ports.ExecutionRef) (ports.ExecutionObservation, error) {
	namespace, name, err := e.target(ref)
	if err != nil {
		return ports.ExecutionObservation{}, err
	}
	if observation, handled, err := e.observeBrokered(ctx, namespace, name); handled || err != nil {
		return observation, err
	}
	job, err := e.client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ports.ExecutionObservation{
			State:      ports.ExecutionLost,
			ErrorClass: domain.ErrWorkerLost,
			Message:    fmt.Sprintf("job %s/%s no longer exists", namespace, name),
		}, nil
	}
	if err != nil {
		return ports.ExecutionObservation{}, e.wrapAPIError("get job", err)
	}

	switch jobStateOf(job) {
	case jobSucceeded:
		return ports.ExecutionObservation{State: ports.ExecutionCompleted, Message: conditionMessage(job, true)}, nil
	case jobFailed, jobActive:
		pods, listErr := e.listPods(ctx, namespace, name)
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

// Cancel deletes the job, spec config map, and token secret. Missing objects
// are treated as already cancelled. Brokered attempts also remove the three
// phase jobs and the workspace PVC.
func (e *Executor) Cancel(ctx context.Context, ref ports.ExecutionRef) error {
	namespace, name, err := e.target(ref)
	if err != nil {
		return err
	}
	propagation := metav1.DeletePropagationBackground
	var errs []error
	for _, jobName := range []string{name, name + "-stage-in", name + "-execute", name + "-stage-out"} {
		if delErr := e.client.BatchV1().Jobs(namespace).Delete(ctx, jobName, metav1.DeleteOptions{PropagationPolicy: &propagation}); delErr != nil && !apierrors.IsNotFound(delErr) {
			errs = append(errs, e.wrapAPIError("delete job "+jobName, delErr))
		}
	}
	if delErr := e.client.CoreV1().ConfigMaps(namespace).Delete(ctx, name+"-spec", metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
		errs = append(errs, e.wrapAPIError("delete spec config map", delErr))
	}
	if delErr := e.client.CoreV1().Secrets(namespace).Delete(ctx, name+"-token", metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
		errs = append(errs, e.wrapAPIError("delete token secret", delErr))
	}
	if delErr := e.client.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name+"-ws", metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
		errs = append(errs, e.wrapAPIError("delete workspace claim", delErr))
	}
	return errors.Join(errs...)
}

func (e *Executor) ensureConfigMap(ctx context.Context, configMap *corev1.ConfigMap) error {
	if _, err := e.client.CoreV1().ConfigMaps(configMap.Namespace).Create(ctx, configMap, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return e.wrapAPIError("create spec config map", err)
	}
	return nil
}

func (e *Executor) ensureSecret(ctx context.Context, secret *corev1.Secret) error {
	if _, err := e.client.CoreV1().Secrets(secret.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return e.wrapAPIError("create token secret", err)
	}
	return nil
}

func (e *Executor) cleanupResources(ctx context.Context, namespace, name string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), apiRequestTimeout)
	defer cancel()
	_ = e.client.CoreV1().ConfigMaps(namespace).Delete(cleanupCtx, name+"-spec", metav1.DeleteOptions{})
	_ = e.client.CoreV1().Secrets(namespace).Delete(cleanupCtx, name+"-token", metav1.DeleteOptions{})
}

func (e *Executor) listPods(ctx context.Context, namespace, name string) ([]corev1.Pod, error) {
	pods, err := e.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func (e *Executor) target(ref ports.ExecutionRef) (string, string, error) {
	name := strings.TrimSpace(ref.ExternalID)
	if name == "" {
		return "", "", &Error{Class: domain.ErrValidationFailed, Message: "execution reference has no external id"}
	}
	if problems := validation.IsDNS1123Subdomain(name); len(problems) > 0 {
		return "", "", &Error{Class: domain.ErrValidationFailed, Message: "invalid execution id: " + strings.Join(problems, "; ")}
	}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = e.cfg.Namespace
	}
	if namespace != e.cfg.Namespace {
		return "", "", &Error{Class: domain.ErrExecutorError, Message: fmt.Sprintf("namespace %q is not served by this executor", namespace)}
	}
	return namespace, name, nil
}

func (e *Executor) runtimeClassFor(requirements domain.ResourceRequirements) (string, error) {
	var required string
	switch requirements.Isolation {
	case domain.IsolationTrustedLocal, domain.IsolationContainer:
	case domain.IsolationGVisor, domain.IsolationVM:
		if e.cfg.RuntimeClass == "" {
			return "", &Error{
				Class:   domain.ErrSandboxUnavailable,
				Message: fmt.Sprintf("isolation %q requires a configured runtime class", requirements.Isolation),
			}
		}
		required = e.cfg.RuntimeClass
	default:
		return "", &Error{Class: domain.ErrValidationFailed, Message: fmt.Sprintf("unknown isolation tier %q", requirements.Isolation)}
	}
	if requirements.RuntimeClass != "" {
		if e.cfg.RuntimeClass == "" || !strings.EqualFold(requirements.RuntimeClass, e.cfg.RuntimeClass) {
			return "", &Error{
				Class:   domain.ErrSandboxUnavailable,
				Message: fmt.Sprintf("runtime class %q is not configured", requirements.RuntimeClass),
			}
		}
		required = e.cfg.RuntimeClass
	}
	return required, nil
}

func (e *Executor) deadline(spec domain.StepSpec) (time.Time, error) {
	now := e.cfg.Clock.Now()
	deadline := spec.Deadline
	if deadline.IsZero() {
		deadline = now.Add(e.cfg.ActiveDeadline)
	}
	if !deadline.After(now) {
		return time.Time{}, &Error{Class: domain.ErrTimeout, Message: "step deadline has already passed"}
	}
	return deadline, nil
}

func (e *Executor) wrapAPIError(action string, err error) error {
	class := domain.ErrExecutorError
	if apierrors.IsForbidden(err) {
		if strings.Contains(strings.ToLower(err.Error()), "exceeded quota") {
			class = domain.ErrTenantQuotaExceeded
		} else {
			class = domain.ErrAuthzDenied
		}
	}
	return &Error{Class: class, Message: action + ": " + err.Error()}
}

func validateSpec(spec domain.StepSpec) error {
	if err := spec.RunID.Validate(); err != nil {
		return err
	}
	if err := spec.StepID.Validate(); err != nil {
		return err
	}
	if err := spec.AttemptID.Validate(); err != nil {
		return err
	}
	if err := spec.TenantID.Validate(); err != nil {
		return err
	}
	if spec.Generation < 0 {
		return domain.Validationf("generation", "must not be negative")
	}
	return spec.Requirements.Validate()
}

func validateResourceBounds(requirements domain.ResourceRequirements) error {
	switch {
	case requirements.CPUMilli > 1_000_000:
		return domain.Validationf("cpu_milli", "must not exceed 1000000")
	case requirements.MemoryMiB > 1<<20:
		return domain.Validationf("memory_mib", "must not exceed 1048576")
	case requirements.EphemeralDiskGiB > 100_000:
		return domain.Validationf("ephemeral_disk_gib", "must not exceed 100000")
	case requirements.GPUCount > 64:
		return domain.Validationf("gpu_count", "must not exceed 64")
	}
	return nil
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func validCallbackURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Host != ""
}

func secondsUntil(now, deadline time.Time) int64 {
	seconds := int64(deadline.Sub(now) / time.Second)
	if deadline.Sub(now)%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		return 1
	}
	return seconds
}

func ttlSeconds(ttl time.Duration) int64 {
	seconds := int64(ttl / time.Second)
	if seconds < 1 {
		return 1
	}
	if seconds > 1<<31-1 {
		return 1<<31 - 1
	}
	return seconds
}
