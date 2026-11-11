package admin

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

type Config struct {
	Region string
	Clock  ports.Clock
	Logger *slog.Logger
}

type Service struct {
	store  ports.MetadataStore
	clock  ports.Clock
	logger *slog.Logger
	region string
}

type TenantInput struct {
	HomeRegion   string
	SecurityTier domain.SecurityTier
	DisplayName  string
	Quota        domain.TenantQuota
}

type ProjectInput struct {
	Provider            string
	RepositoryURI       string
	DefaultBranch       string
	DefaultPolicyDigest string
	AllowedRegions      []string
	Classification      string
}

func New(store ports.MetadataStore, config Config) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("admin service: metadata store is required")
	}
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Region == "" {
		config.Region = "region-a"
	}
	return &Service{store: store, clock: config.Clock, logger: config.Logger, region: config.Region}, nil
}

func (s *Service) CreateTenant(ctx context.Context, actor string, input TenantInput) (domain.Tenant, error) {
	if input.HomeRegion == "" {
		input.HomeRegion = s.region
	}
	if input.SecurityTier == "" {
		input.SecurityTier = domain.SecurityTierStandard
	}
	if !input.SecurityTier.Valid() {
		return domain.Tenant{}, &domain.ValidationError{Field: "security_tier", Reason: "unknown security tier"}
	}
	quota := input.Quota
	if quota == (domain.TenantQuota{}) {
		quota = domain.TenantQuota{
			MaxConcurrentRuns: 20, MaxConcurrentSteps: 64,
			CPUCoreSecondsDay: 50000, FuzzSecondsDay: 20000,
			StorageBytes: 500 << 30, APIRPS: 20,
		}
	}
	now := s.clock.Now()
	tenant := domain.Tenant{
		ID: domain.NewTenantID(), HomeRegion: input.HomeRegion, SecurityTier: input.SecurityTier,
		Status: domain.TenantActive, ActiveWriterEpoch: 1, Quota: quota,
		DisplayName: input.DisplayName, CreatedAt: now, UpdatedAt: now,
	}
	if err := tenant.Validate(); err != nil {
		return domain.Tenant{}, err
	}
	if err := s.store.CreateTenant(ctx, tenant); err != nil {
		return domain.Tenant{}, err
	}
	_ = s.store.AppendAudit(ctx, domain.AuditEntry{
		ID: domain.NewEventID().String(), TenantID: tenant.ID, Actor: actor, Action: "tenant.create",
		Resource: "tenant", ResourceID: tenant.ID.String(), Outcome: "created", At: now,
	})
	return tenant, nil
}

func (s *Service) GetTenant(ctx context.Context, tenantID domain.TenantID) (domain.Tenant, error) {
	return s.store.GetTenant(ctx, tenantID)
}

func (s *Service) CreateProject(ctx context.Context, tenantID domain.TenantID, actor string, input ProjectInput) (domain.Project, error) {
	if _, err := s.store.GetTenant(ctx, tenantID); err != nil {
		return domain.Project{}, err
	}
	if input.DefaultBranch == "" {
		input.DefaultBranch = "main"
	}
	if input.Classification == "" {
		input.Classification = "standard"
	}
	if input.DefaultPolicyDigest != "" {
		if _, err := s.store.GetPolicy(ctx, tenantID, input.DefaultPolicyDigest); err != nil {
			return domain.Project{}, &domain.ValidationError{Field: "default_policy_digest", Reason: "policy does not exist"}
		}
	}
	now := s.clock.Now()
	project := domain.Project{
		ID: domain.NewProjectID(), TenantID: tenantID, Provider: input.Provider,
		RepositoryURI: input.RepositoryURI, DefaultBranch: input.DefaultBranch,
		DefaultPolicyDigest: input.DefaultPolicyDigest, AllowedRegions: input.AllowedRegions,
		Classification: input.Classification, CreatedAt: now, UpdatedAt: now,
	}
	if err := project.Validate(); err != nil {
		return domain.Project{}, err
	}
	if err := s.store.CreateProject(ctx, project); err != nil {
		return domain.Project{}, err
	}
	_ = s.store.AppendAudit(ctx, domain.AuditEntry{
		ID: domain.NewEventID().String(), TenantID: tenantID, Actor: actor, Action: "project.create",
		Resource: "project", ResourceID: project.ID.String(), Outcome: "created", At: now,
	})
	return project, nil
}

func (s *Service) GetProject(ctx context.Context, tenantID domain.TenantID, projectID domain.ProjectID) (domain.Project, error) {
	return s.store.GetProject(ctx, tenantID, projectID)
}

func (s *Service) PutPolicy(ctx context.Context, tenantID domain.TenantID, projectID domain.ProjectID, actor string, raw []byte) (domain.Policy, error) {
	if projectID != "" {
		if _, err := s.store.GetProject(ctx, tenantID, projectID); err != nil {
			return domain.Policy{}, err
		}
	}
	document, err := domain.ParsePolicyDocument(raw)
	if err != nil {
		return domain.Policy{}, err
	}
	digest, err := document.Digest()
	if err != nil {
		return domain.Policy{}, err
	}
	policy := domain.Policy{
		TenantID: tenantID, ProjectID: projectID, Digest: digest, Version: "v1",
		Source: domain.PolicySourceAdmin, Trusted: true, Document: raw, CreatedBy: actor, CreatedAt: s.clock.Now(),
	}
	if err := policy.Validate(); err != nil {
		return domain.Policy{}, err
	}
	if err := s.store.PutPolicy(ctx, policy); err != nil {
		return domain.Policy{}, err
	}
	_ = s.store.AppendAudit(ctx, domain.AuditEntry{
		ID: domain.NewEventID().String(), TenantID: tenantID, Actor: actor, Action: "policy.put",
		Resource: "policy", ResourceID: digest, Outcome: "created", At: s.clock.Now(),
	})
	return policy, nil
}

type PolicyValidation struct {
	Digest         string   `json:"digest"`
	Name           string   `json:"name"`
	RequiredChecks []string `json:"required_checks"`
	OptionalChecks []string `json:"optional_checks"`
	Contracts      int      `json:"contracts"`
	IsolationTier  string   `json:"isolation_tier"`
	SubjectNetwork string   `json:"subject_network"`
}

func (s *Service) ValidatePolicy(raw []byte) (PolicyValidation, error) {
	document, err := domain.ParsePolicyDocument(raw)
	if err != nil {
		return PolicyValidation{}, err
	}
	digest, err := document.Digest()
	if err != nil {
		return PolicyValidation{}, err
	}
	return PolicyValidation{
		Digest: digest, Name: document.Metadata.Name, RequiredChecks: document.RequiredChecks(),
		OptionalChecks: document.Spec.Verification.Optional, Contracts: len(document.Spec.Verification.Contracts),
		IsolationTier: string(document.Spec.Execution.IsolationTier), SubjectNetwork: string(document.Spec.Execution.SubjectNetwork),
	}, nil
}

func (s *Service) ListClusters(ctx context.Context, region string) ([]domain.Cluster, error) {
	return s.store.ListClusters(ctx, region)
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }
