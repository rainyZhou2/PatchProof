package runservice

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/telemetry"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/evidence"
)

type Limits struct {
	MaxStepCount          int
	MaxParallelSteps      int
	MaxRunWallSeconds     int
	DefaultWallSeconds    int
	DefaultCPUSeconds     float64
	DefaultFuzzCPUSeconds float64
	MaxRepositoryBytes    int64
	MaxChangedFiles       int
	APIListMaxLimit       int
}

type Config struct {
	Region string
	Clock  ports.Clock
	Limits Limits
	Logger *slog.Logger
}

type Service struct {
	store   ports.MetadataStore
	clock   ports.Clock
	region  string
	limits  Limits
	logger  *slog.Logger
	metrics *telemetry.Metrics
}

type SubmitInput struct {
	TenantID        domain.TenantID
	ProjectID       domain.ProjectID
	RequestKey      string
	BaseCommit      string
	HeadCommit      string
	PatchDigest     string
	PolicyRef       string
	PolicyDigest    string
	RequestedChecks []string
	Budget          domain.Budget
	Region          string
	CreatedBy       string
}

func New(store ports.MetadataStore, config Config, metrics *telemetry.Metrics) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("run service: metadata store is required")
	}
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Limits.APIListMaxLimit <= 0 {
		config.Limits.APIListMaxLimit = 200
	}
	return &Service{store: store, clock: config.Clock, region: config.Region, limits: config.Limits, logger: config.Logger, metrics: metrics}, nil
}

func (s *Service) Submit(ctx context.Context, input SubmitInput) (domain.Run, error) {
	tenant, err := s.store.GetTenant(ctx, input.TenantID)
	if err != nil {
		return domain.Run{}, err
	}
	if tenant.Status != domain.TenantActive {
		return domain.Run{}, &domain.PolicyError{Code: "TENANT_NOT_ACTIVE", Reason: "tenant status is " + string(tenant.Status)}
	}
	project, err := s.store.GetProject(ctx, input.TenantID, input.ProjectID)
	if err != nil {
		return domain.Run{}, err
	}
	if project.TenantID != input.TenantID {
		return domain.Run{}, &domain.NotFoundError{Resource: "project", ID: input.ProjectID.String()}
	}
	policy, digest, err := s.resolvePolicy(ctx, input, project)
	if err != nil {
		return domain.Run{}, err
	}
	checks, err := normalizeChecks(input.RequestedChecks)
	if err != nil {
		return domain.Run{}, err
	}
	budget := s.resolveBudget(input.Budget, policy)
	if err := budget.Validate(); err != nil {
		return domain.Run{}, err
	}
	if s.limits.MaxRunWallSeconds > 0 && budget.WallSeconds > int64(s.limits.MaxRunWallSeconds) {
		budget.WallSeconds = int64(s.limits.MaxRunWallSeconds)
	}
	region := input.Region
	if region == "" {
		region = tenant.HomeRegion
	}
	request := domain.RunRequest{
		TenantID:        input.TenantID,
		ProjectID:       input.ProjectID,
		RequestKey:      input.RequestKey,
		Fingerprint:     "",
		BaseCommit:      input.BaseCommit,
		HeadCommit:      input.HeadCommit,
		PatchDigest:     input.PatchDigest,
		PolicyRef:       input.PolicyRef,
		PolicyDigest:    digest,
		RequestedChecks: checks,
		Budget:          budget,
		Region:          region,
		CreatedBy:       input.CreatedBy,
	}
	fingerprint, err := fingerprintOf(request)
	if err != nil {
		return domain.Run{}, err
	}
	request.Fingerprint = fingerprint
	if err := request.Validate(); err != nil {
		return domain.Run{}, err
	}
	now := s.clock.Now()
	run := domain.Run{
		ID:              domain.NewRunID(),
		TenantID:        request.TenantID,
		ProjectID:       request.ProjectID,
		RequestKey:      request.RequestKey,
		Fingerprint:     fingerprint,
		BaseCommit:      strings.ToLower(request.BaseCommit),
		HeadCommit:      strings.ToLower(request.HeadCommit),
		PatchDigest:     request.PatchDigest,
		PolicyDigest:    digest,
		PolicyRef:       request.PolicyRef,
		RequestedChecks: checks,
		Budget:          budget,
		Status:          domain.RunCreated,
		StateVersion:    0,
		Region:          region,
		CreatedBy:       input.CreatedBy,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	payload := map[string]any{
		"run_id":           run.ID.String(),
		"project_id":       run.ProjectID.String(),
		"base_commit":      run.BaseCommit,
		"head_commit":      run.HeadCommit,
		"policy_digest":    digest,
		"requested_checks": checks,
		"budget":           budget,
		"region":           region,
	}
	event, err := domain.NewOutboxEvent(run.TenantID, domain.EventRunCreated, run.ID.String(), 0, payload, now)
	if err != nil {
		return domain.Run{}, err
	}
	created, err := s.store.CreateRun(ctx, domain.CreateRunRecord{Run: run, Events: []domain.OutboxEvent{event}})
	if err != nil {
		return domain.Run{}, err
	}
	if s.metrics != nil && created.ID == run.ID {
		s.metrics.RunRequests.WithLabelValues(region, "api").Inc()
	}
	if err := s.store.AppendAudit(ctx, domain.AuditEntry{
		ID: domain.NewEventID().String(), TenantID: run.TenantID, Actor: input.CreatedBy, Action: "run.submit",
		Resource: "run", ResourceID: created.ID.String(), Outcome: "accepted", At: now,
	}); err != nil {
		s.logger.Warn("audit append failed", slog.String("error", err.Error()))
	}
	return created, nil
}

func (s *Service) Get(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.Run, error) {
	return s.store.GetRun(ctx, tenantID, runID)
}

func (s *Service) List(ctx context.Context, filter domain.RunFilter) (domain.RunPage, error) {
	filter = filter.Normalize(s.limits.APIListMaxLimit)
	return s.store.ListRuns(ctx, filter)
}

func (s *Service) DAG(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.DAG, error) {
	return s.store.GetDAG(ctx, tenantID, runID)
}

func (s *Service) Evidence(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) ([]domain.Evidence, error) {
	return s.store.ListEvidence(ctx, tenantID, runID)
}

func (s *Service) Counterexamples(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) ([]domain.Counterexample, error) {
	return s.store.ListCounterexamples(ctx, tenantID, runID)
}

func (s *Service) Report(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.Report, error) {
	return s.store.GetReport(ctx, tenantID, runID)
}

func (s *Service) Cancel(ctx context.Context, tenantID domain.TenantID, runID domain.RunID, actor string) (domain.Run, error) {
	run, err := s.store.GetRun(ctx, tenantID, runID)
	if err != nil {
		return domain.Run{}, err
	}
	if run.Status == domain.RunCancelled || run.Status == domain.RunCancelling {
		return run, nil
	}
	if run.Status.Terminal() {
		return run, &domain.ConflictError{Resource: "run", ID: runID.String(), Reason: "run is already " + string(run.Status)}
	}
	now := s.clock.Now()
	event, err := domain.NewOutboxEvent(tenantID, domain.EventRunCancelled, runID.String(), run.StateVersion, map[string]any{
		"run_id": runID.String(), "requested_by": actor,
	}, now)
	if err != nil {
		return domain.Run{}, err
	}
	update := domain.RunUpdate{
		TenantID: tenantID, RunID: runID, ExpectedVersion: run.StateVersion,
		From: run.Status, To: domain.RunCancelling, At: now, Event: &event,
	}
	cancelled, err := s.store.UpdateRun(ctx, update)
	if err != nil {
		return domain.Run{}, err
	}
	if _, err := s.store.CancelRunSteps(ctx, tenantID, runID, now); err != nil {
		return domain.Run{}, err
	}
	_ = s.store.AppendAudit(ctx, domain.AuditEntry{
		ID: domain.NewEventID().String(), TenantID: tenantID, Actor: actor, Action: "run.cancel",
		Resource: "run", ResourceID: runID.String(), Outcome: "cancelling", At: now,
	})
	return cancelled, nil
}

func (s *Service) resolvePolicy(ctx context.Context, input SubmitInput, project domain.Project) (domain.Policy, string, error) {
	digest := input.PolicyDigest
	if digest == "" {
		digest = digestFromRef(input.PolicyRef)
	}
	if digest == "" {
		digest = project.DefaultPolicyDigest
	}
	if digest == "" {
		return domain.Policy{}, "", &domain.ValidationError{Field: "policy_ref", Reason: "no policy digest is available"}
	}
	policy, err := s.store.GetPolicy(ctx, input.TenantID, digest)
	if err != nil {
		return domain.Policy{}, "", err
	}
	if !policy.Trusted || policy.Source != domain.PolicySourceAdmin {
		return domain.Policy{}, "", &domain.PolicyError{Code: "UNTRUSTED_POLICY", Reason: "run policies must come from the trusted admin source"}
	}
	return policy, digest, nil
}

func (s *Service) resolveBudget(requested domain.Budget, policy domain.Policy) domain.Budget {
	defaults := domain.Budget{
		CPUSeconds:       s.limits.DefaultCPUSeconds,
		WallSeconds:      int64(s.limits.DefaultWallSeconds),
		FuzzCPUSeconds:   s.limits.DefaultFuzzCPUSeconds,
		MaxParallelSteps: s.limits.MaxParallelSteps,
	}
	if document, err := domain.ParsePolicyDocument(policy.Document); err == nil {
		policyBudget := document.Budget(time.Time{})
		defaults = defaults.Merge(policyBudget)
	}
	return defaults.Merge(requested)
}

func normalizeChecks(checks []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(checks))
	for _, check := range checks {
		name := domain.NormalizeCheckName(check)
		if name == "" {
			continue
		}
		if _, ok := domain.CheckStepKind(name); !ok {
			return nil, &domain.ValidationError{Field: "requested_checks", Reason: "unsupported check " + name}
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

func fingerprintOf(request domain.RunRequest) (string, error) {
	return evidence.CanonicalDigest(struct {
		ProjectID       domain.ProjectID `json:"project_id"`
		BaseCommit      string           `json:"base_commit"`
		HeadCommit      string           `json:"head_commit"`
		PatchDigest     string           `json:"patch_digest"`
		PolicyDigest    string           `json:"policy_digest"`
		RequestedChecks []string         `json:"requested_checks"`
		Budget          domain.Budget    `json:"budget"`
		Region          string           `json:"region"`
	}{
		ProjectID:       request.ProjectID,
		BaseCommit:      strings.ToLower(request.BaseCommit),
		HeadCommit:      strings.ToLower(request.HeadCommit),
		PatchDigest:     request.PatchDigest,
		PolicyDigest:    request.PolicyDigest,
		RequestedChecks: request.RequestedChecks,
		Budget:          request.Budget,
		Region:          request.Region,
	})
}

func digestFromRef(ref string) string {
	if index := strings.IndexByte(ref, '@'); index >= 0 {
		candidate := ref[index+1:]
		if strings.HasPrefix(candidate, "sha256:") {
			return candidate
		}
	}
	if strings.HasPrefix(ref, "sha256:") {
		return ref
	}
	return ""
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }
