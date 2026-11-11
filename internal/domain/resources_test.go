package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
)

func validCluster() domain.Cluster {
	return domain.Cluster{
		ID:                 domain.ClusterID("cluster-a"),
		Region:             "us-east-1",
		ExecutorKind:       "kubernetes",
		Architectures:      []string{"amd64"},
		Languages:          []string{"go"},
		Engines:            []string{"go-build", "go-analyzer"},
		IsolationTiers:     []domain.IsolationTier{domain.IsolationContainer, domain.IsolationGVisor},
		RuntimeClasses:     []string{"standard"},
		AvailableCPUMilli:  8000,
		AvailableMemoryMiB: 16384,
		MaxConcurrentSteps: 100,
		CostTier:           1,
		CasRegion:          "us-east-1",
		Healthy:            true,
		Labels:             map[string]string{"features": "gpu,ssd"},
		LastHeartbeat:      oldTime,
	}
}

func TestResourceRequirementsValidate(t *testing.T) {
	base := testRequirements()
	if err := base.Validate(); err != nil {
		t.Fatalf("valid requirements rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.ResourceRequirements)
		field  string
	}{
		{"unknown isolation", func(r *domain.ResourceRequirements) { r.Isolation = domain.IsolationTier("bogus") }, "isolation"},
		{"zero cpu", func(r *domain.ResourceRequirements) { r.CPUMilli = 0 }, "cpu_milli"},
		{"negative cpu", func(r *domain.ResourceRequirements) { r.CPUMilli = -1 }, "cpu_milli"},
		{"zero memory", func(r *domain.ResourceRequirements) { r.MemoryMiB = 0 }, "memory_mib"},
		{"negative disk", func(r *domain.ResourceRequirements) { r.EphemeralDiskGiB = -1 }, "ephemeral_disk_gib"},
		{"invalid network", func(r *domain.ResourceRequirements) { r.Network = domain.NetworkMode("open") }, "network"},
		{"invalid subject network", func(r *domain.ResourceRequirements) { r.SubjectNetwork = domain.NetworkMode("open") }, "subject_network"},
		{"subject restricted without platform network", func(r *domain.ResourceRequirements) {
			r.SubjectNetwork = domain.NetworkRestricted
			r.Network = domain.NetworkNone
		}, "subject_network"},
		{"zero deadline", func(r *domain.ResourceRequirements) { r.DeadlineSeconds = 0 }, "deadline_seconds"},
		{"negative gpu", func(r *domain.ResourceRequirements) { r.GPUCount = -1 }, "gpu_count"},
		{"empty architecture", func(r *domain.ResourceRequirements) { r.Architecture = "" }, "architecture"},
		{"invalid artifact transport", func(r *domain.ResourceRequirements) { r.ArtifactTransport = domain.ArtifactTransport("ftp") }, "artifact_transport"},
		{"brokered offline with restricted network", func(r *domain.ResourceRequirements) {
			r.ArtifactTransport = domain.TransportBrokeredOffline
			r.Network = domain.NetworkRestricted
		}, "artifact_transport"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mutate(&req)
			err := req.Validate()
			var validationErr *domain.ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %v, want *ValidationError", err)
			}
			if validationErr.Field != tc.field {
				t.Fatalf("field = %q, want %q (%v)", validationErr.Field, tc.field, err)
			}
		})
	}

	validVariants := []domain.ResourceRequirements{}
	withRestrictedNetwork := base
	withRestrictedNetwork.Network = domain.NetworkRestricted
	withRestrictedNetwork.SubjectNetwork = domain.NetworkRestricted
	validVariants = append(validVariants, withRestrictedNetwork)
	withBrokered := base
	withBrokered.ArtifactTransport = domain.TransportBrokeredOffline
	validVariants = append(validVariants, withBrokered)
	withFeatures := base
	withFeatures.RequiredFeatures = []string{"gpu"}
	validVariants = append(validVariants, withFeatures)
	for _, req := range validVariants {
		if err := req.Validate(); err != nil {
			t.Errorf("variant rejected: %v (%+v)", err, req)
		}
	}
}

func TestClusterSupports(t *testing.T) {
	baseReq := testRequirements()
	cluster := validCluster()
	if !cluster.Supports(baseReq) {
		t.Fatal("healthy matching cluster not supported")
	}

	tests := []struct {
		name   string
		mutate func(*domain.Cluster, *domain.ResourceRequirements)
		want   bool
	}{
		{"unhealthy", func(c *domain.Cluster, _ *domain.ResourceRequirements) { c.Healthy = false }, false},
		{"maintenance", func(c *domain.Cluster, _ *domain.ResourceRequirements) { c.Maintenance = true }, false},
		{"at max concurrency", func(c *domain.Cluster, _ *domain.ResourceRequirements) { c.ActiveAttempts = c.MaxConcurrentSteps }, false},
		{"insufficient cpu", func(c *domain.Cluster, _ *domain.ResourceRequirements) { c.AvailableCPUMilli = 999 }, false},
		{"insufficient memory", func(c *domain.Cluster, _ *domain.ResourceRequirements) { c.AvailableMemoryMiB = 2047 }, false},
		{"architecture mismatch", func(c *domain.Cluster, _ *domain.ResourceRequirements) { c.Architectures = []string{"arm64"} }, false},
		{"architecture case insensitive", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			c.Architectures = []string{"AMD64"}
			r.Architecture = "amd64"
		}, true},
		{"language mismatch", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			c.Languages = []string{"rust"}
			r.Language = "go"
		}, false},
		{"empty language ignored", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			c.Languages = nil
			r.Language = ""
		}, true},
		{"engine mismatch", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			c.Engines = []string{"go-build"}
			r.Engine = "bounded-smt"
		}, false},
		{"empty engine ignored", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			c.Engines = nil
			r.Engine = ""
		}, true},
		{"isolation tier too weak", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			c.IsolationTiers = []domain.IsolationTier{domain.IsolationContainer}
			r.Isolation = domain.IsolationGVisor
		}, false},
		{"isolation stronger tier satisfies weaker request", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			c.IsolationTiers = []domain.IsolationTier{domain.IsolationVM}
			r.Isolation = domain.IsolationContainer
		}, true},
		{"runtime class mismatch", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			c.RuntimeClasses = []string{"gvisor"}
			r.RuntimeClass = "standard"
		}, false},
		{"empty runtime class ignored", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			c.RuntimeClasses = nil
			r.RuntimeClass = ""
		}, true},
		{"region mismatch", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			c.Region = "eu-west-1"
			r.Region = "us-east-1"
		}, false},
		{"region case insensitive", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			c.Region = "US-EAST-1"
			r.Region = "us-east-1"
		}, true},
		{"feature present", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			r.RequiredFeatures = []string{"GPU"}
		}, true},
		{"feature missing", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			r.RequiredFeatures = []string{"gpu", "fpga"}
		}, false},
		{"no features on cluster", func(c *domain.Cluster, r *domain.ResourceRequirements) {
			c.Labels = nil
			r.RequiredFeatures = []string{"gpu"}
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validCluster()
			r := baseReq
			tc.mutate(&c, &r)
			if got := c.Supports(r); got != tc.want {
				t.Fatalf("Supports = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClusterScoreOrdering(t *testing.T) {
	requirements := domain.ResourceRequirements{
		Region:          "us-east-1",
		Architecture:    "amd64",
		Isolation:       domain.IsolationContainer,
		CPUMilli:        2000,
		MemoryMiB:       4096,
		Network:         domain.NetworkNone,
		DeadlineSeconds: 60,
	}
	generous := validCluster()
	generous.AvailableCPUMilli = 16000
	generous.AvailableMemoryMiB = 32768
	generous.QueueDepth = 0
	generous.CostTier = 0
	generous.CasRegion = "us-east-1"

	cramped := validCluster()
	cramped.AvailableCPUMilli = 2500
	cramped.AvailableMemoryMiB = 5000
	cramped.QueueDepth = 25
	cramped.CostTier = 4
	cramped.CasRegion = "eu-west-1"

	if generous.Score(requirements) <= cramped.Score(requirements) {
		t.Fatalf("generous score %.3f should exceed cramped score %.3f", generous.Score(requirements), cramped.Score(requirements))
	}

	saturated := cramped
	saturated.MaxConcurrentSteps = 5
	saturated.ActiveAttempts = 5
	if got := saturated.Score(requirements); got != -1 {
		t.Fatalf("saturated cluster score = %v, want -1", got)
	}

	// Same cluster, locality only: a matching CAS region must score higher.
	local := validCluster()
	local.CasRegion = "us-east-1"
	remote := validCluster()
	remote.CasRegion = "eu-west-1"
	if local.Score(requirements) <= remote.Score(requirements) {
		t.Fatalf("local score %.3f should exceed remote score %.3f", local.Score(requirements), remote.Score(requirements))
	}
}

func TestBudgetValidateAndMerge(t *testing.T) {
	if err := (domain.Budget{}).Validate(); err != nil {
		t.Fatalf("zero budget rejected: %v", err)
	}
	if !(domain.Budget{}).IsZero() {
		t.Fatal("zero budget should be zero")
	}
	if err := (domain.Budget{WallSeconds: 10}).Validate(); err == nil {
		t.Fatal("sub-30s wall budget must be rejected")
	}
	if err := (domain.Budget{CPUSeconds: -1}).Validate(); err == nil {
		t.Fatal("negative cpu budget must be rejected")
	}
	merged := domain.Budget{CPUSeconds: 100, WallSeconds: 600}.Merge(domain.Budget{CPUSeconds: 50, WallSeconds: 900, MaxParallelSteps: 4})
	if merged.CPUSeconds != 50 || merged.WallSeconds != 600 || merged.MaxParallelSteps != 4 {
		t.Fatalf("merged budget = %+v", merged)
	}
}

func TestInputBindingValidate(t *testing.T) {
	ref := domain.NewCASRef(digestHexA, 10)
	valid := domain.InputBinding{Ref: ref, Kind: "file", Path: "src/main.go"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid binding rejected: %v", err)
	}
	for _, kind := range []string{"file", "tar.gz", "tar.zst"} {
		binding := valid
		binding.Kind = kind
		if err := binding.Validate(); err != nil {
			t.Errorf("kind %s rejected: %v", kind, err)
		}
	}
	for _, path := range []string{"../escape", "/absolute", "a/../../b"} {
		binding := valid
		binding.Path = path
		if err := binding.Validate(); err == nil {
			t.Errorf("path %q should be rejected", path)
		}
	}
	badKind := valid
	badKind.Kind = "zip"
	if err := badKind.Validate(); err == nil {
		t.Error("unsupported kind should be rejected")
	}
	badRef := valid
	badRef.Ref = domain.CASRef{Algorithm: "sha256", Digest: strings.Repeat("Z", 64)}
	if err := badRef.Validate(); err == nil {
		t.Error("invalid CAS ref should be rejected")
	}
}
