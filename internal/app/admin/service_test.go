package admin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/adapters/storage/memory"
	"github.com/patchproof/patchproof/internal/domain"
)

var adminTestNow = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

const adminPolicyYAML = `apiVersion: patchproof.io/v1
kind: VerificationPolicy
metadata:
  name: baseline
  labels:
    owner: platform
spec:
  verification:
    required:
      - compile
    optional:
      - regression-test
    contracts:
      - id: invariants
        symbol: ParseConfig
        language: go
        kind: fix-invariant
        domainRef: contracts/invariants.yaml
        required: true
        minimumResult: TESTED_WITHIN_SCOPE
  execution:
    isolationTier: isolated-container
    defaultCpuMilli: 1000
    defaultMemoryMiB: 1024
    maxStepWallSeconds: 600
    maxRunWallSeconds: 1800
    subjectNetwork: none
    artifactTransport: direct
  budgets:
    totalCpuSeconds: 300
    fuzzCpuSeconds: 50
    maxParallelSteps: 4
  protectedPaths:
    - secrets/**
`

type adminClock struct{ at time.Time }

func (c adminClock) Now() time.Time { return c.at }

// auditStore records audit entries while delegating everything else to the
// in-memory store, which does not expose a read API for its audit log.
type auditStore struct {
	*memory.Store
	mu      sync.Mutex
	entries []domain.AuditEntry
}

func (s *auditStore) AppendAudit(ctx context.Context, entry domain.AuditEntry) error {
	s.mu.Lock()
	s.entries = append(s.entries, entry)
	s.mu.Unlock()
	return s.Store.AppendAudit(ctx, entry)
}

func (s *auditStore) recorded() []domain.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.AuditEntry(nil), s.entries...)
}

type adminFixture struct {
	store   *auditStore
	service *Service
	tenant  domain.Tenant
	policy  domain.Policy
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	store := &auditStore{Store: memory.New()}
	service, err := New(store, Config{Region: "region-test", Clock: adminClock{at: adminTestNow}})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	fixture := &adminFixture{store: store, service: service}
	tenant, err := service.CreateTenant(context.Background(), "alice", TenantInput{DisplayName: "Acme"})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	fixture.tenant = tenant
	policy, err := service.PutPolicy(context.Background(), tenant.ID, "", "alice", []byte(adminPolicyYAML))
	if err != nil {
		t.Fatalf("put policy: %v", err)
	}
	fixture.policy = policy
	return fixture
}

func TestCreateTenantDefaultsAndAudit(t *testing.T) {
	fixture := newAdminFixture(t)
	// The fixture already created a tenant; create a second one to assert the
	// defaults in isolation.
	tenant, err := fixture.service.CreateTenant(context.Background(), "bob", TenantInput{DisplayName: "Beta"})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if tenant.HomeRegion != "region-test" {
		t.Fatalf("home region = %q, want region-test", tenant.HomeRegion)
	}
	if tenant.SecurityTier != domain.SecurityTierStandard {
		t.Fatalf("security tier = %q, want %q", tenant.SecurityTier, domain.SecurityTierStandard)
	}
	if tenant.Status != domain.TenantActive {
		t.Fatalf("status = %q, want ACTIVE", tenant.Status)
	}
	if tenant.ActiveWriterEpoch != 1 {
		t.Fatalf("active writer epoch = %d, want 1", tenant.ActiveWriterEpoch)
	}
	wantQuota := domain.TenantQuota{
		MaxConcurrentRuns: 20, MaxConcurrentSteps: 64,
		CPUCoreSecondsDay: 50000, FuzzSecondsDay: 20000,
		StorageBytes: 500 << 30, APIRPS: 20,
	}
	if tenant.Quota != wantQuota {
		t.Fatalf("quota = %+v, want %+v", tenant.Quota, wantQuota)
	}
	if !tenant.CreatedAt.Equal(adminTestNow) || !tenant.UpdatedAt.Equal(adminTestNow) {
		t.Fatalf("timestamps = %v/%v, want %v", tenant.CreatedAt, tenant.UpdatedAt, adminTestNow)
	}
	stored, err := fixture.service.GetTenant(context.Background(), tenant.ID)
	if err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	if stored.DisplayName != "Beta" {
		t.Fatalf("display name = %q, want Beta", stored.DisplayName)
	}

	var created *domain.AuditEntry
	for _, entry := range fixture.store.recorded() {
		if entry.Action == "tenant.create" && entry.ResourceID == tenant.ID.String() {
			candidate := entry
			created = &candidate
		}
	}
	if created == nil {
		t.Fatalf("no tenant.create audit entry recorded; entries = %+v", fixture.store.recorded())
	}
	if created.Actor != "bob" || created.Outcome != "created" || created.Resource != "tenant" {
		t.Fatalf("audit entry = %+v", created)
	}
	if !created.At.Equal(adminTestNow) {
		t.Fatalf("audit timestamp = %v, want %v", created.At, adminTestNow)
	}
}

func TestCreateTenantValidation(t *testing.T) {
	fixture := newAdminFixture(t)
	ctx := context.Background()

	_, err := fixture.service.CreateTenant(ctx, "alice", TenantInput{SecurityTier: domain.SecurityTier("platinum")})
	var validation *domain.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("unknown tier error = %v, want *domain.ValidationError", err)
	}
	if validation.Field != "security_tier" {
		t.Fatalf("error field = %q, want security_tier", validation.Field)
	}

	_, err = fixture.service.CreateTenant(ctx, "alice", TenantInput{
		Quota: domain.TenantQuota{MaxConcurrentRuns: -1},
	})
	if !errors.As(err, &validation) {
		t.Fatalf("negative quota error = %v, want *domain.ValidationError", err)
	}
	if validation.Field != "quota" {
		t.Fatalf("error field = %q, want quota", validation.Field)
	}

	tenant, err := fixture.service.CreateTenant(ctx, "alice", TenantInput{
		HomeRegion: "region-other", SecurityTier: domain.SecurityTierElevated,
	})
	if err != nil {
		t.Fatalf("create elevated tenant: %v", err)
	}
	if tenant.HomeRegion != "region-other" || tenant.SecurityTier != domain.SecurityTierElevated {
		t.Fatalf("tenant = %+v", tenant)
	}
}

func TestGetTenantNotFound(t *testing.T) {
	fixture := newAdminFixture(t)
	_, err := fixture.service.GetTenant(context.Background(), domain.NewTenantID())
	var notFound *domain.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("error = %v, want *domain.NotFoundError", err)
	}
	if notFound.Resource != "tenant" {
		t.Fatalf("resource = %q, want tenant", notFound.Resource)
	}
}

func TestCreateProject(t *testing.T) {
	ctx := context.Background()
	fixture := newAdminFixture(t)

	project, err := fixture.service.CreateProject(ctx, fixture.tenant.ID, "alice", ProjectInput{
		Provider:      "git",
		RepositoryURI: "https://example.com/acme/app.git",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if project.DefaultBranch != "main" {
		t.Fatalf("default branch = %q, want main", project.DefaultBranch)
	}
	if project.Classification != "standard" {
		t.Fatalf("classification = %q, want standard", project.Classification)
	}
	if project.TenantID != fixture.tenant.ID {
		t.Fatalf("tenant = %q, want %q", project.TenantID, fixture.tenant.ID)
	}
	if project.ID.String() == "" {
		t.Fatal("project ID is empty")
	}
	if !project.CreatedAt.Equal(adminTestNow) || !project.UpdatedAt.Equal(adminTestNow) {
		t.Fatalf("timestamps = %v/%v, want %v", project.CreatedAt, project.UpdatedAt, adminTestNow)
	}
	stored, err := fixture.service.GetProject(ctx, fixture.tenant.ID, project.ID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if stored.RepositoryURI != project.RepositoryURI {
		t.Fatalf("stored URI = %q, want %q", stored.RepositoryURI, project.RepositoryURI)
	}
	var audited bool
	for _, entry := range fixture.store.recorded() {
		if entry.Action == "project.create" && entry.ResourceID == project.ID.String() {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("no project.create audit entry recorded; entries = %+v", fixture.store.recorded())
	}

	// Tenant must already exist.
	_, err = fixture.service.CreateProject(ctx, domain.NewTenantID(), "alice", ProjectInput{
		Provider: "git", RepositoryURI: "https://example.com/acme/app.git",
	})
	var notFound *domain.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("missing tenant error = %v, want *domain.NotFoundError", err)
	}

	// Default policy digest must resolve to a stored policy.
	_, err = fixture.service.CreateProject(ctx, fixture.tenant.ID, "alice", ProjectInput{
		Provider: "git", RepositoryURI: "https://example.com/acme/app.git",
		DefaultPolicyDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	})
	var validation *domain.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("unknown policy error = %v, want *domain.ValidationError", err)
	}
	if validation.Field != "default_policy_digest" {
		t.Fatalf("error field = %q, want default_policy_digest", validation.Field)
	}

	// A stored policy digest is accepted.
	withPolicy, err := fixture.service.CreateProject(ctx, fixture.tenant.ID, "alice", ProjectInput{
		Provider: "git", RepositoryURI: "https://example.com/acme/app.git",
		DefaultPolicyDigest: fixture.policy.Digest,
	})
	if err != nil {
		t.Fatalf("create project with policy: %v", err)
	}
	if withPolicy.DefaultPolicyDigest != fixture.policy.Digest {
		t.Fatalf("policy digest = %q, want %q", withPolicy.DefaultPolicyDigest, fixture.policy.Digest)
	}

	// File repositories are only allowed for trusted-local projects.
	_, err = fixture.service.CreateProject(ctx, fixture.tenant.ID, "alice", ProjectInput{
		Provider: "git", RepositoryURI: "file:///tmp/patchproof-repo",
	})
	if !errors.As(err, &validation) {
		t.Fatalf("file repo error = %v, want *domain.ValidationError", err)
	}
	if validation.Field != "repository_uri" {
		t.Fatalf("error field = %q, want repository_uri", validation.Field)
	}

	local, err := fixture.service.CreateProject(ctx, fixture.tenant.ID, "alice", ProjectInput{
		Provider: "git", RepositoryURI: "file:///tmp/patchproof-repo", Classification: "trusted-local",
	})
	if err != nil {
		t.Fatalf("create trusted-local project: %v", err)
	}
	if local.Classification != "trusted-local" {
		t.Fatalf("classification = %q, want trusted-local", local.Classification)
	}
}

func TestPutPolicy(t *testing.T) {
	ctx := context.Background()
	fixture := newAdminFixture(t)

	policy, err := fixture.service.PutPolicy(ctx, fixture.tenant.ID, "", "alice", []byte(adminPolicyYAML))
	if err != nil {
		t.Fatalf("put policy: %v", err)
	}
	if policy.Source != domain.PolicySourceAdmin {
		t.Fatalf("source = %q, want ADMIN", policy.Source)
	}
	if !policy.Trusted {
		t.Fatal("trusted = false, want true")
	}
	if policy.Version != "v1" {
		t.Fatalf("version = %q, want v1", policy.Version)
	}
	if policy.ProjectID != "" {
		t.Fatalf("project ID = %q, want empty", policy.ProjectID)
	}
	document, err := domain.ParsePolicyDocument([]byte(adminPolicyYAML))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	expectedDigest, err := document.Digest()
	if err != nil {
		t.Fatalf("policy digest: %v", err)
	}
	if policy.Digest != expectedDigest {
		t.Fatalf("digest = %q, want %q", policy.Digest, expectedDigest)
	}
	if string(policy.Document) != adminPolicyYAML {
		t.Fatal("stored policy document differs from the submitted bytes")
	}
	if !policy.CreatedAt.Equal(adminTestNow) || policy.CreatedBy != "alice" {
		t.Fatalf("policy metadata = %+v", policy)
	}

	// The digest is stable across repeated writes.
	repeated, err := fixture.service.PutPolicy(ctx, fixture.tenant.ID, "", "alice", []byte(adminPolicyYAML))
	if err != nil {
		t.Fatalf("put policy again: %v", err)
	}
	if repeated.Digest != policy.Digest {
		t.Fatalf("digest changed across writes: %q vs %q", repeated.Digest, policy.Digest)
	}

	// A project-scoped policy must reference an existing project.
	_, err = fixture.service.PutPolicy(ctx, fixture.tenant.ID, domain.NewProjectID(), "alice", []byte(adminPolicyYAML))
	var notFound *domain.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("unknown project error = %v, want *domain.NotFoundError", err)
	}

	// Invalid YAML is rejected.
	_, err = fixture.service.PutPolicy(ctx, fixture.tenant.ID, "", "alice", []byte("apiVersion: ["))
	var validation *domain.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("invalid yaml error = %v, want *domain.ValidationError", err)
	}
	if validation.Field != "policy" {
		t.Fatalf("error field = %q, want policy", validation.Field)
	}

	// A structurally invalid policy is rejected.
	_, err = fixture.service.PutPolicy(ctx, fixture.tenant.ID, "", "alice", []byte("apiVersion: patchproof.io/v1\nkind: VerificationPolicy\nmetadata:\n  name: empty\n"))
	if !errors.As(err, &validation) {
		t.Fatalf("invalid policy error = %v, want *domain.ValidationError", err)
	}
	if validation.Field != "policy.spec.verification.required" {
		t.Fatalf("error field = %q, want policy.spec.verification.required", validation.Field)
	}

	var audited bool
	for _, entry := range fixture.store.recorded() {
		if entry.Action == "policy.put" && entry.ResourceID == policy.Digest {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("no policy.put audit entry recorded; entries = %+v", fixture.store.recorded())
	}
}

func TestValidatePolicy(t *testing.T) {
	fixture := newAdminFixture(t)
	validation, err := fixture.service.ValidatePolicy([]byte(adminPolicyYAML))
	if err != nil {
		t.Fatalf("validate policy: %v", err)
	}
	document, err := domain.ParsePolicyDocument([]byte(adminPolicyYAML))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	expectedDigest, err := document.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if validation.Digest != expectedDigest {
		t.Fatalf("digest = %q, want %q", validation.Digest, expectedDigest)
	}
	if validation.Name != "baseline" {
		t.Fatalf("name = %q, want baseline", validation.Name)
	}
	if len(validation.RequiredChecks) != 1 || validation.RequiredChecks[0] != "compile" {
		t.Fatalf("required checks = %v", validation.RequiredChecks)
	}
	if len(validation.OptionalChecks) != 1 || validation.OptionalChecks[0] != "regression-test" {
		t.Fatalf("optional checks = %v", validation.OptionalChecks)
	}
	if validation.Contracts != 1 {
		t.Fatalf("contracts = %d, want 1", validation.Contracts)
	}
	if validation.IsolationTier != string(domain.IsolationContainer) {
		t.Fatalf("isolation tier = %q, want %q", validation.IsolationTier, domain.IsolationContainer)
	}
	if validation.SubjectNetwork != string(domain.NetworkNone) {
		t.Fatalf("subject network = %q, want %q", validation.SubjectNetwork, domain.NetworkNone)
	}
	if _, err := fixture.service.ValidatePolicy([]byte("not a policy")); err == nil {
		t.Fatal("ValidatePolicy accepted garbage input")
	}
}

func TestListClustersPassThrough(t *testing.T) {
	ctx := context.Background()
	fixture := newAdminFixture(t)
	clusters := []domain.Cluster{
		{ID: "cluster-b", Region: "region-a", ExecutorKind: "k8s"},
		{ID: "cluster-a", Region: "region-a", ExecutorKind: "k8s"},
		{ID: "cluster-c", Region: "region-b", ExecutorKind: "k8s"},
	}
	for _, cluster := range clusters {
		if err := fixture.store.UpsertCluster(ctx, cluster); err != nil {
			t.Fatalf("upsert cluster %s: %v", cluster.ID, err)
		}
	}

	regionA, err := fixture.service.ListClusters(ctx, "region-a")
	if err != nil {
		t.Fatalf("list clusters: %v", err)
	}
	if len(regionA) != 2 {
		t.Fatalf("region-a clusters = %d, want 2", len(regionA))
	}
	if regionA[0].ID != "cluster-a" || regionA[1].ID != "cluster-b" {
		t.Fatalf("region-a cluster order = %s,%s", regionA[0].ID, regionA[1].ID)
	}

	all, err := fixture.service.ListClusters(ctx, "")
	if err != nil {
		t.Fatalf("list all clusters: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all clusters = %d, want 3", len(all))
	}
	none, err := fixture.service.ListClusters(ctx, "region-z")
	if err != nil {
		t.Fatalf("list missing region: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("region-z clusters = %d, want 0", len(none))
	}
}
