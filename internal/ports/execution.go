package ports

import (
	"context"

	"github.com/patchproof/patchproof/internal/domain"
)

type EventBus interface {
	Publish(ctx context.Context, events []domain.OutboxEvent) error
	Consume(ctx context.Context, consumer string, subjects []string, handler func(context.Context, domain.EventEnvelope) error) error
	Health(ctx context.Context) error
	Close() error
}

type EventPublisher interface {
	Publish(ctx context.Context, events []domain.OutboxEvent) error
}

type ExecutionState string

const (
	ExecutionPending   ExecutionState = "PENDING"
	ExecutionRunning   ExecutionState = "RUNNING"
	ExecutionCompleted ExecutionState = "COMPLETED"
	ExecutionFailed    ExecutionState = "FAILED"
	ExecutionLost      ExecutionState = "LOST"
	ExecutionCancelled ExecutionState = "CANCELLED"
)

type ExecutionRequest struct {
	Spec         domain.StepSpec
	AttemptToken string
	CallbackURL  string
	WorkspaceRef domain.CASRef
}

type ExecutionRef struct {
	Executor   string           `json:"executor"`
	ClusterID  domain.ClusterID `json:"cluster_id,omitempty"`
	Namespace  string           `json:"namespace,omitempty"`
	ExternalID string           `json:"external_id"`
}

type ExecutionObservation struct {
	State      ExecutionState     `json:"state"`
	ErrorClass domain.ErrorClass  `json:"error_class,omitempty"`
	Message    string             `json:"message,omitempty"`
	Requeue    bool               `json:"requeue"`
	Result     *domain.StepResult `json:"result,omitempty"`
}

type ExecutorCapabilities struct {
	Name           string                 `json:"name"`
	ExecutorKind   string                 `json:"executor_kind"`
	Region         string                 `json:"region,omitempty"`
	Architectures  []string               `json:"architectures"`
	Languages      []string               `json:"languages"`
	Engines        []string               `json:"engines"`
	IsolationTiers []domain.IsolationTier `json:"isolation_tiers"`
	RuntimeClasses []string               `json:"runtime_classes"`
	Features       []string               `json:"features"`
	MaxConcurrent  int                    `json:"max_concurrent"`
}

type Executor interface {
	Capabilities(ctx context.Context) (ExecutorCapabilities, error)
	Start(ctx context.Context, request ExecutionRequest) (ExecutionRef, error)
	Observe(ctx context.Context, ref ExecutionRef) (ExecutionObservation, error)
	Cancel(ctx context.Context, ref ExecutionRef) error
}

type ExecutorFactory interface {
	ForCluster(cluster domain.Cluster) (Executor, error)
}

type StepRunner interface {
	RunStep(ctx context.Context, spec domain.StepSpec) (domain.StepResult, error)
}
