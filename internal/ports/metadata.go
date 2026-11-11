package ports

import (
	"context"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

type MetadataStore interface {
	CreateTenant(ctx context.Context, tenant domain.Tenant) error
	GetTenant(ctx context.Context, tenantID domain.TenantID) (domain.Tenant, error)
	UpdateTenantEpoch(ctx context.Context, tenantID domain.TenantID, expectedEpoch, newEpoch int64, homeRegion string) (domain.Tenant, error)

	CreateProject(ctx context.Context, project domain.Project) error
	GetProject(ctx context.Context, tenantID domain.TenantID, projectID domain.ProjectID) (domain.Project, error)
	PutPolicy(ctx context.Context, policy domain.Policy) error
	GetPolicy(ctx context.Context, tenantID domain.TenantID, digest string) (domain.Policy, error)

	CreateRun(ctx context.Context, record domain.CreateRunRecord) (domain.Run, error)
	GetRun(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.Run, error)
	ListRuns(ctx context.Context, filter domain.RunFilter) (domain.RunPage, error)
	UpdateRun(ctx context.Context, update domain.RunUpdate) (domain.Run, error)
	FreezeRunInputs(ctx context.Context, freeze domain.RunFreeze) (domain.Run, error)
	PlanRun(ctx context.Context, tx domain.PlanTransaction) (domain.Run, error)
	GetDAG(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.DAG, error)

	ListSteps(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) ([]domain.Step, error)
	ListReadySteps(ctx context.Context, query domain.ReadyQuery) ([]domain.Step, error)
	PromoteDueSteps(ctx context.Context, now time.Time, limit int) ([]domain.Step, error)
	GetStep(ctx context.Context, tenantID domain.TenantID, stepID domain.StepID) (domain.Step, error)
	ListAttempts(ctx context.Context, tenantID domain.TenantID, stepID domain.StepID) ([]domain.Attempt, error)

	AcquireLease(ctx context.Context, request domain.LeaseRequest) (domain.Lease, error)
	RenewLease(ctx context.Context, renewal domain.LeaseRenewal) error
	StartAttempt(ctx context.Context, ref domain.AttemptRef, at time.Time) error
	CommitAttempt(ctx context.Context, commit domain.AttemptCommit) (domain.CommitOutcome, error)
	FailAttempt(ctx context.Context, failure domain.AttemptFailure) (domain.CommitOutcome, error)
	ExpireLeases(ctx context.Context, now time.Time, limit int) ([]domain.LeaseExpiry, error)
	CancelRunSteps(ctx context.Context, tenantID domain.TenantID, runID domain.RunID, at time.Time) (int, error)

	CountActiveSteps(ctx context.Context, tenantID domain.TenantID) (int, error)
	TenantUsage(ctx context.Context, tenantID domain.TenantID, since time.Time) (domain.TenantUsage, error)

	AppendEvidence(ctx context.Context, evidence []domain.Evidence) error
	ListEvidence(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) ([]domain.Evidence, error)
	ListCounterexamples(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) ([]domain.Counterexample, error)
	PutReport(ctx context.Context, report domain.Report) error
	GetReport(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.Report, error)

	GetActionCache(ctx context.Context, tenantID domain.TenantID, actionKey string, now time.Time) (domain.ActionCacheEntry, error)
	PutActionCache(ctx context.Context, entry domain.ActionCacheEntry) error
	PurgeActionCache(ctx context.Context, before time.Time, limit int) (int, error)

	PendingOutbox(ctx context.Context, limit int) ([]domain.OutboxEvent, error)
	MarkOutboxDelivered(ctx context.Context, ids []domain.EventID, at time.Time) error
	RecordOutboxFailure(ctx context.Context, failure domain.OutboxDeliveryFailure) error
	AppendOutbox(ctx context.Context, events []domain.OutboxEvent) error

	UpsertCluster(ctx context.Context, cluster domain.Cluster) error
	GetCluster(ctx context.Context, clusterID domain.ClusterID) (domain.Cluster, error)
	ListClusters(ctx context.Context, region string) ([]domain.Cluster, error)
	DeleteCluster(ctx context.Context, clusterID domain.ClusterID) error

	AcquireShardLeases(ctx context.Context, shards []int, owner string, ttl time.Duration, now time.Time) ([]int, error)
	ReleaseShardLeases(ctx context.Context, owner string) error

	AppendAudit(ctx context.Context, entry domain.AuditEntry) error

	Ping(ctx context.Context) error
	Close() error
}
