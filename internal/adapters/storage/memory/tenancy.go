package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// CreateTenant inserts a tenant and rejects duplicate IDs.
func (s *Store) CreateTenant(ctx context.Context, tenant domain.Tenant) error {
	if err := tenant.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	if _, exists := s.tenants[tenant.ID]; exists {
		return &domain.ConflictError{Resource: "tenant", ID: tenant.ID.String(), Reason: "already exists"}
	}
	stored := cloneTenant(tenant)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC()
	} else {
		stored.CreatedAt = stored.CreatedAt.UTC()
	}
	if stored.UpdatedAt.IsZero() {
		stored.UpdatedAt = stored.CreatedAt
	} else {
		stored.UpdatedAt = stored.UpdatedAt.UTC()
	}
	s.tenants[stored.ID] = stored
	return nil
}

// GetTenant returns a tenant by ID.
func (s *Store) GetTenant(ctx context.Context, tenantID domain.TenantID) (domain.Tenant, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Tenant{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return domain.Tenant{}, err
	}
	tenant, ok := s.tenants[tenantID]
	if !ok {
		return domain.Tenant{}, &domain.NotFoundError{Resource: "tenant", ID: tenantID.String()}
	}
	return cloneTenant(tenant), nil
}

// UpdateTenantEpoch advances the active writer epoch using compare-and-swap.
func (s *Store) UpdateTenantEpoch(ctx context.Context, tenantID domain.TenantID, expectedEpoch, newEpoch int64, homeRegion string) (domain.Tenant, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Tenant{}, err
	}
	if expectedEpoch < 0 || newEpoch < 0 {
		return domain.Tenant{}, &domain.ValidationError{Field: "tenant_epoch", Reason: "must not be negative"}
	}
	if err := domain.RequireNonEmpty("home_region", homeRegion); err != nil {
		return domain.Tenant{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return domain.Tenant{}, err
	}
	tenant, ok := s.tenants[tenantID]
	if !ok {
		return domain.Tenant{}, &domain.NotFoundError{Resource: "tenant", ID: tenantID.String()}
	}
	if tenant.ActiveWriterEpoch != expectedEpoch {
		return domain.Tenant{}, &domain.VersionConflictError{
			Resource: "tenant " + tenantID.String(),
			Expected: expectedEpoch,
			Actual:   tenant.ActiveWriterEpoch,
		}
	}
	tenant.ActiveWriterEpoch = newEpoch
	tenant.HomeRegion = homeRegion
	tenant.UpdatedAt = time.Now().UTC()
	if err := tenant.Validate(); err != nil {
		return domain.Tenant{}, err
	}
	s.tenants[tenantID] = cloneTenant(tenant)
	return cloneTenant(tenant), nil
}

// CreateProject inserts a project and rejects duplicate IDs.
func (s *Store) CreateProject(ctx context.Context, project domain.Project) error {
	if err := project.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	key := projectKey{tenantID: project.TenantID, projectID: project.ID}
	if _, exists := s.projects[key]; exists {
		return &domain.ConflictError{Resource: "project", ID: project.ID.String(), Reason: "already exists"}
	}
	stored := cloneProject(project)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC()
	} else {
		stored.CreatedAt = stored.CreatedAt.UTC()
	}
	if stored.UpdatedAt.IsZero() {
		stored.UpdatedAt = stored.CreatedAt
	} else {
		stored.UpdatedAt = stored.UpdatedAt.UTC()
	}
	s.projects[key] = stored
	return nil
}

// GetProject returns a project scoped to its tenant.
func (s *Store) GetProject(ctx context.Context, tenantID domain.TenantID, projectID domain.ProjectID) (domain.Project, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Project{}, err
	}
	if projectID.String() == "" {
		return domain.Project{}, &domain.ValidationError{Field: "project_id", Reason: "must not be empty"}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return domain.Project{}, err
	}
	project, ok := s.projects[projectKey{tenantID: tenantID, projectID: projectID}]
	if !ok {
		return domain.Project{}, &domain.NotFoundError{Resource: "project", ID: projectID.String()}
	}
	return cloneProject(project), nil
}

// PutPolicy upserts a policy keyed by tenant and digest.
func (s *Store) PutPolicy(ctx context.Context, policy domain.Policy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	stored := clonePolicy(policy)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC()
	} else {
		stored.CreatedAt = stored.CreatedAt.UTC()
	}
	s.policies[policyKey{tenantID: stored.TenantID, digest: stored.Digest}] = stored
	return nil
}

// GetPolicy returns a policy by tenant and digest.
func (s *Store) GetPolicy(ctx context.Context, tenantID domain.TenantID, digest string) (domain.Policy, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Policy{}, err
	}
	if err := domain.RequireNonEmpty("policy_digest", digest); err != nil {
		return domain.Policy{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return domain.Policy{}, err
	}
	policy, ok := s.policies[policyKey{tenantID: tenantID, digest: digest}]
	if !ok {
		return domain.Policy{}, &domain.NotFoundError{Resource: "policy", ID: fmt.Sprintf("%s/%s", tenantID, digest)}
	}
	return clonePolicy(policy), nil
}
