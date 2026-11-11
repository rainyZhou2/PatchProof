package ports

import (
	"context"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

type PolicyViolation struct {
	Code     string          `json:"code"`
	Severity domain.Severity `json:"severity"`
	Message  string          `json:"message"`
}

type PolicyInput struct {
	Tenant            domain.Tenant         `json:"tenant"`
	Project           domain.Project        `json:"project"`
	Policy            domain.PolicyDocument `json:"policy"`
	PolicyDigest      string                `json:"policy_digest"`
	PolicyTrusted     bool                  `json:"policy_trusted"`
	RequestedChecks   []string              `json:"requested_checks"`
	ChangedPaths      []string              `json:"changed_paths"`
	ChangedFiles      int                   `json:"changed_files"`
	RepositorySizeGiB float64               `json:"repository_size_gib"`
	Submodules        bool                  `json:"submodules"`
	Isolation         domain.IsolationTier  `json:"isolation"`
	Region            string                `json:"region"`
}

type PolicyDecision struct {
	Allowed           bool                     `json:"allowed"`
	RequiredChecks    []string                 `json:"required_checks"`
	OptionalChecks    []string                 `json:"optional_checks"`
	AllowInconclusive []string                 `json:"allow_inconclusive"`
	Isolation         domain.IsolationTier     `json:"isolation"`
	SubjectNetwork    domain.NetworkMode       `json:"subject_network"`
	ArtifactTransport domain.ArtifactTransport `json:"artifact_transport"`
	Violations        []PolicyViolation        `json:"violations,omitempty"`
	Reasons           []string                 `json:"reasons,omitempty"`
	Digest            string                   `json:"digest"`
	AllowedRegions    []string                 `json:"allowed_regions,omitempty"`
}

type PolicyEngine interface {
	Evaluate(ctx context.Context, input PolicyInput) (PolicyDecision, error)
}

type PlanRequest struct {
	Run        domain.Run
	Tenant     domain.Tenant
	Project    domain.Project
	Policy     domain.PolicyDocument
	Decision   PolicyDecision
	Budget     domain.Budget
	ShardCount int
	Now        time.Time
}

type PlanResult struct {
	Steps        []domain.Step
	Dependencies map[domain.StepID][]domain.StepID
	Claims       []domain.Claim
}

type Planner interface {
	Plan(ctx context.Context, request PlanRequest) (PlanResult, error)
}

type PlacementStrategy interface {
	Select(step domain.Step, candidates []domain.Cluster) (domain.Cluster, error)
}

type Reporter interface {
	Format() string
	ContentType() string
	Render(ctx context.Context, report domain.Report, evidence []domain.Evidence) ([]byte, error)
}

type Signer interface {
	KeyID() string
	Algorithm() string
	Sign(ctx context.Context, payload []byte) (domain.Signature, error)
}

type SignatureVerifier interface {
	Verify(ctx context.Context, payload []byte, signature domain.Signature) error
}

type Notifier interface {
	Notify(ctx context.Context, report domain.Report) error
}

type Clock interface {
	Now() time.Time
}

type AttemptTokens interface {
	Issue(ctx context.Context, ref domain.AttemptRef, ttl time.Duration) (string, time.Time, error)
	Verify(ctx context.Context, token string) (domain.AttemptRef, error)
}
