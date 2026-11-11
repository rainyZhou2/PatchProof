package domain

import (
	"strings"
	"time"
)

type Budget struct {
	CPUSeconds       float64 `json:"cpu_seconds"`
	WallSeconds      int64   `json:"wall_seconds"`
	FuzzCPUSeconds   float64 `json:"fuzz_cpu_seconds,omitempty"`
	MaxParallelSteps int     `json:"max_parallel_steps"`
}

func (b Budget) Validate() error {
	if b.CPUSeconds < 0 || b.WallSeconds < 0 || b.FuzzCPUSeconds < 0 || b.MaxParallelSteps < 0 {
		return &ValidationError{Field: "budget", Reason: "values must not be negative"}
	}
	if b.WallSeconds > 0 && b.WallSeconds < 30 {
		return &ValidationError{Field: "budget.wall_seconds", Reason: "must be at least 30 seconds"}
	}
	return nil
}

func (b Budget) IsZero() bool {
	return b.CPUSeconds == 0 && b.WallSeconds == 0 && b.FuzzCPUSeconds == 0 && b.MaxParallelSteps == 0
}

func (b Budget) Merge(other Budget) Budget {
	out := b
	if other.CPUSeconds > 0 && (out.CPUSeconds == 0 || other.CPUSeconds < out.CPUSeconds) {
		out.CPUSeconds = other.CPUSeconds
	}
	if other.WallSeconds > 0 && (out.WallSeconds == 0 || other.WallSeconds < out.WallSeconds) {
		out.WallSeconds = other.WallSeconds
	}
	if other.FuzzCPUSeconds > 0 && (out.FuzzCPUSeconds == 0 || other.FuzzCPUSeconds < out.FuzzCPUSeconds) {
		out.FuzzCPUSeconds = other.FuzzCPUSeconds
	}
	if other.MaxParallelSteps > 0 && (out.MaxParallelSteps == 0 || other.MaxParallelSteps < out.MaxParallelSteps) {
		out.MaxParallelSteps = other.MaxParallelSteps
	}
	return out
}

type ResourceRequirements struct {
	Region            string            `json:"region,omitempty"`
	Architecture      string            `json:"architecture"`
	Language          string            `json:"language,omitempty"`
	Engine            string            `json:"engine,omitempty"`
	Isolation         IsolationTier     `json:"isolation"`
	CPUMilli          int64             `json:"cpu_milli"`
	MemoryMiB         int64             `json:"memory_mib"`
	EphemeralDiskGiB  int64             `json:"ephemeral_disk_gib"`
	Network           NetworkMode       `json:"network"`
	SubjectNetwork    NetworkMode       `json:"subject_network"`
	DeadlineSeconds   int               `json:"deadline_seconds"`
	Preemptible       bool              `json:"preemptible"`
	GPUCount          int               `json:"gpu_count"`
	RuntimeClass      string            `json:"runtime_class,omitempty"`
	ArtifactTransport ArtifactTransport `json:"artifact_transport"`
	ImageDigest       string            `json:"image_digest,omitempty"`
	RequiredFeatures  []string          `json:"required_features,omitempty"`
}

func (r ResourceRequirements) Validate() error {
	if !r.Isolation.Valid() {
		return &ValidationError{Field: "isolation", Reason: "unknown isolation tier"}
	}
	if r.CPUMilli <= 0 {
		return &ValidationError{Field: "cpu_milli", Reason: "must be positive"}
	}
	if r.MemoryMiB <= 0 {
		return &ValidationError{Field: "memory_mib", Reason: "must be positive"}
	}
	if r.EphemeralDiskGiB < 0 {
		return &ValidationError{Field: "ephemeral_disk_gib", Reason: "must not be negative"}
	}
	if !r.Network.Valid() {
		return &ValidationError{Field: "network", Reason: "must be none or restricted"}
	}
	if r.SubjectNetwork != "" && !r.SubjectNetwork.Valid() {
		return &ValidationError{Field: "subject_network", Reason: "must be none or restricted"}
	}
	if r.SubjectNetwork == NetworkRestricted && r.Network == NetworkNone {
		return &ValidationError{Field: "subject_network", Reason: "restricted subject network requires platform network access"}
	}
	if r.DeadlineSeconds <= 0 {
		return &ValidationError{Field: "deadline_seconds", Reason: "must be positive"}
	}
	if r.GPUCount < 0 {
		return &ValidationError{Field: "gpu_count", Reason: "must not be negative"}
	}
	if r.Architecture == "" {
		return &ValidationError{Field: "architecture", Reason: "must not be empty"}
	}
	if r.ArtifactTransport != "" && !r.ArtifactTransport.Valid() {
		return &ValidationError{Field: "artifact_transport", Reason: "unknown artifact transport"}
	}
	if r.ArtifactTransport == TransportBrokeredOffline && r.Network != NetworkNone {
		return &ValidationError{Field: "artifact_transport", Reason: "brokered offline IO requires network=none"}
	}
	return nil
}

func (r ResourceRequirements) Deadline() time.Duration {
	return time.Duration(r.DeadlineSeconds) * time.Second
}

type Cluster struct {
	ID                 ClusterID         `json:"cluster_id"`
	Region             string            `json:"region"`
	AvailabilityZone   string            `json:"availability_zone,omitempty"`
	ExecutorKind       string            `json:"executor_kind"`
	Endpoint           string            `json:"endpoint,omitempty"`
	Namespace          string            `json:"namespace,omitempty"`
	Architectures      []string          `json:"architectures"`
	Languages          []string          `json:"languages"`
	Engines            []string          `json:"engines"`
	IsolationTiers     []IsolationTier   `json:"isolation_tiers"`
	RuntimeClasses     []string          `json:"runtime_classes"`
	AvailableCPUMilli  int64             `json:"available_cpu_milli"`
	AvailableMemoryMiB int64             `json:"available_memory_mib"`
	QueueDepth         int               `json:"queue_depth"`
	ActiveAttempts     int               `json:"active_attempts"`
	MaxConcurrentSteps int               `json:"max_concurrent_steps"`
	CostTier           int               `json:"cost_tier"`
	CasRegion          string            `json:"cas_region"`
	Healthy            bool              `json:"healthy"`
	Maintenance        bool              `json:"maintenance"`
	Version            string            `json:"version"`
	Labels             map[string]string `json:"labels,omitempty"`
	LastHeartbeat      time.Time         `json:"last_heartbeat"`
}

func (c Cluster) Supports(r ResourceRequirements) bool {
	if !c.Healthy || c.Maintenance {
		return false
	}
	if c.MaxConcurrentSteps > 0 && c.ActiveAttempts >= c.MaxConcurrentSteps {
		return false
	}
	if c.AvailableCPUMilli < r.CPUMilli || c.AvailableMemoryMiB < r.MemoryMiB {
		return false
	}
	if !containsFold(c.Architectures, r.Architecture) {
		return false
	}
	if r.Language != "" && !containsFold(c.Languages, r.Language) {
		return false
	}
	if r.Engine != "" && !containsFold(c.Engines, r.Engine) {
		return false
	}
	if !c.supportsIsolation(r.Isolation) {
		return false
	}
	if r.RuntimeClass != "" && !containsFold(c.RuntimeClasses, r.RuntimeClass) {
		return false
	}
	if r.Region != "" && !strings.EqualFold(r.Region, c.Region) {
		return false
	}
	featureList := strings.Split(c.Labels["features"], ",")
	for _, feature := range r.RequiredFeatures {
		if !containsFold(featureList, feature) {
			return false
		}
	}
	return true
}

func (c Cluster) supportsIsolation(required IsolationTier) bool {
	for _, tier := range c.IsolationTiers {
		if tier.AtLeast(required) {
			return true
		}
	}
	return false
}

// Live reports whether the cluster has heartbeated within maxAge. A zero
// heartbeat is treated as stale so unregistered rows are never scheduled.
func (c Cluster) Live(now time.Time, maxAge time.Duration) bool {
	if maxAge <= 0 {
		return true
	}
	if c.LastHeartbeat.IsZero() {
		return false
	}
	return !now.After(c.LastHeartbeat.Add(maxAge))
}

func (c Cluster) Score(r ResourceRequirements) float64 {
	if c.MaxConcurrentSteps > 0 && c.ActiveAttempts >= c.MaxConcurrentSteps {
		return -1
	}
	cpuHeadroom := float64(c.AvailableCPUMilli-r.CPUMilli) / float64(maxInt64(c.AvailableCPUMilli, 1))
	memHeadroom := float64(c.AvailableMemoryMiB-r.MemoryMiB) / float64(maxInt64(c.AvailableMemoryMiB, 1))
	locality := 0.0
	if r.Region == "" || strings.EqualFold(r.Region, c.CasRegion) {
		locality = 1
	}
	queuePenalty := float64(c.QueueDepth) * 0.02
	costPenalty := float64(c.CostTier) * 0.05
	maintenancePenalty := 0.0
	if c.Maintenance {
		maintenancePenalty = 1
	}
	return 0.35*cpuHeadroom + 0.2*memHeadroom + 0.25*locality - queuePenalty - costPenalty - maintenancePenalty
}

func containsFold(values []string, target string) bool {
	for _, v := range values {
		if strings.EqualFold(v, target) {
			return true
		}
	}
	return false
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

type StepSpec struct {
	RunID               RunID                   `json:"run_id"`
	StepID              StepID                  `json:"step_id"`
	AttemptID           AttemptID               `json:"attempt_id"`
	TenantID            TenantID                `json:"tenant_id"`
	Generation          int64                   `json:"generation"`
	Kind                StepKind                `json:"kind"`
	Language            string                  `json:"language"`
	Engine              string                  `json:"engine"`
	PolicyDigest        string                  `json:"policy_digest"`
	InputManifestDigest string                  `json:"input_manifest_digest"`
	Inputs              []CASRef                `json:"inputs"`
	NamedInputs         map[string]InputBinding `json:"named_inputs,omitempty"`
	Requirements        ResourceRequirements    `json:"requirements"`
	Options             map[string]string       `json:"options,omitempty"`
	Deadline            time.Time               `json:"deadline"`
	AttemptToken        string                  `json:"-"`
	CallbackURL         string                  `json:"-"`
	WorkspaceDigest     string                  `json:"workspace_digest,omitempty"`
}

type InputBinding struct {
	Ref  CASRef `json:"ref"`
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
}

func (b InputBinding) Validate() error {
	if err := b.Ref.Validate(); err != nil {
		return err
	}
	switch b.Kind {
	case "file", "tar.gz", "tar.zst":
	default:
		return &ValidationError{Field: "input.kind", Reason: "must be file, tar.gz or tar.zst"}
	}
	if strings.Contains(b.Path, "..") || strings.HasPrefix(b.Path, "/") {
		return &ValidationError{Field: "input.path", Reason: "must be a relative path inside the workspace"}
	}
	return nil
}
