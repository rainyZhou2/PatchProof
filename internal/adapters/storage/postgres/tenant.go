package postgres

import (
	"context"

	"github.com/patchproof/patchproof/internal/domain"
)

// CreateTenant inserts a tenant row.
func (s *Store) CreateTenant(ctx context.Context, tenant domain.Tenant) error {
	if err := tenant.Validate(); err != nil {
		return err
	}
	quota, err := encodeJSON(tenant.Quota)
	if err != nil {
		return err
	}
	tenant.CreatedAt = normalizeTime(tenant.CreatedAt)
	if tenant.UpdatedAt.IsZero() {
		tenant.UpdatedAt = tenant.CreatedAt
	} else {
		tenant.UpdatedAt = tenant.UpdatedAt.UTC()
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO tenants (
		tenant_id, home_region, security_tier, status, active_writer_epoch, quota,
		display_name, created_at, updated_at
	) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)`,
		tenant.ID.String(), tenant.HomeRegion, string(tenant.SecurityTier), string(tenant.Status),
		tenant.ActiveWriterEpoch, quota, tenant.DisplayName, tenant.CreatedAt, tenant.UpdatedAt,
	)
	if err != nil {
		return mapError(err, "tenant", tenant.ID.String())
	}
	return nil
}

// GetTenant loads a tenant by id.
func (s *Store) GetTenant(ctx context.Context, tenantID domain.TenantID) (domain.Tenant, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Tenant{}, err
	}
	tenant, err := scanTenant(s.pool.QueryRow(ctx,
		"SELECT "+tenantColumns+" FROM tenants WHERE tenant_id = $1::uuid", tenantID.String()))
	if err != nil {
		return domain.Tenant{}, mapError(err, "tenant", tenantID.String())
	}
	return tenant, nil
}

// UpdateTenantEpoch advances the tenant writer epoch and optionally moves the
// tenant home region using an optimistic epoch check.
func (s *Store) UpdateTenantEpoch(ctx context.Context, tenantID domain.TenantID, expectedEpoch, newEpoch int64, homeRegion string) (domain.Tenant, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Tenant{}, err
	}
	if err := domain.RequireNonEmpty("home_region", homeRegion); err != nil {
		return domain.Tenant{}, err
	}
	if expectedEpoch < 0 || newEpoch < 0 {
		return domain.Tenant{}, &domain.ValidationError{Field: "tenant_epoch", Reason: "must not be negative"}
	}
	tenant, err := scanTenant(s.pool.QueryRow(ctx, `UPDATE tenants SET
		active_writer_epoch = $3, home_region = $4, updated_at = now()
		WHERE tenant_id = $1::uuid AND active_writer_epoch = $2
		RETURNING `+tenantColumns, tenantID.String(), expectedEpoch, newEpoch, homeRegion))
	if err == nil {
		return tenant, nil
	}
	current, loadErr := scanTenant(s.pool.QueryRow(ctx,
		"SELECT "+tenantColumns+" FROM tenants WHERE tenant_id = $1::uuid", tenantID.String()))
	if loadErr != nil {
		return domain.Tenant{}, mapError(loadErr, "tenant", tenantID.String())
	}
	return domain.Tenant{}, &domain.VersionConflictError{
		Resource: "tenant", Expected: expectedEpoch, Actual: current.ActiveWriterEpoch,
	}
}

// CreateProject inserts a project row.
func (s *Store) CreateProject(ctx context.Context, project domain.Project) error {
	if err := project.Validate(); err != nil {
		return err
	}
	regions, err := encodeJSON(project.AllowedRegions)
	if err != nil {
		return err
	}
	project.CreatedAt = normalizeTime(project.CreatedAt)
	if project.UpdatedAt.IsZero() {
		project.UpdatedAt = project.CreatedAt
	} else {
		project.UpdatedAt = project.UpdatedAt.UTC()
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO projects (
		project_id, tenant_id, provider, repository_uri, default_branch,
		default_policy_digest, allowed_regions, classification, created_at, updated_at
	) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10)`,
		project.ID.String(), project.TenantID.String(), project.Provider, project.RepositoryURI,
		project.DefaultBranch, project.DefaultPolicyDigest, regions, project.Classification,
		project.CreatedAt, project.UpdatedAt,
	)
	if err != nil {
		return mapError(err, "project", project.ID.String())
	}
	return nil
}

// GetProject loads a project scoped to its tenant.
func (s *Store) GetProject(ctx context.Context, tenantID domain.TenantID, projectID domain.ProjectID) (domain.Project, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Project{}, err
	}
	project, err := scanProject(s.pool.QueryRow(ctx,
		"SELECT "+projectColumns+" FROM projects WHERE tenant_id = $1::uuid AND project_id = $2::uuid",
		tenantID.String(), projectID.String()))
	if err != nil {
		return domain.Project{}, mapError(err, "project", projectID.String())
	}
	return project, nil
}

// PutPolicy inserts or replaces a policy document keyed by tenant and digest.
func (s *Store) PutPolicy(ctx context.Context, policy domain.Policy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	policy.CreatedAt = normalizeTime(policy.CreatedAt)
	var projectID any
	if policy.ProjectID != "" {
		projectID = policy.ProjectID.String()
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO policies (
		tenant_id, digest, project_id, version, source, trusted, document, created_by, created_at
	) VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6, $7, $8, $9)
	ON CONFLICT (tenant_id, digest) DO UPDATE SET
		project_id = EXCLUDED.project_id,
		version = EXCLUDED.version,
		source = EXCLUDED.source,
		trusted = EXCLUDED.trusted,
		document = EXCLUDED.document,
		created_by = EXCLUDED.created_by`,
		policy.TenantID.String(), policy.Digest, projectID, policy.Version, string(policy.Source),
		policy.Trusted, policy.Document, policy.CreatedBy, policy.CreatedAt,
	)
	if err != nil {
		return mapError(err, "policy", policy.Digest)
	}
	return nil
}

// GetPolicy loads a policy document by tenant and digest.
func (s *Store) GetPolicy(ctx context.Context, tenantID domain.TenantID, digest string) (domain.Policy, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Policy{}, err
	}
	if err := domain.RequireNonEmpty("policy_digest", digest); err != nil {
		return domain.Policy{}, err
	}
	policy, err := scanPolicy(s.pool.QueryRow(ctx,
		"SELECT "+policyColumns+" FROM policies WHERE tenant_id = $1::uuid AND digest = $2",
		tenantID.String(), digest))
	if err != nil {
		return domain.Policy{}, mapError(err, "policy", digest)
	}
	return policy, nil
}
