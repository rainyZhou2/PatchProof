package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

type StagedInput struct {
	Name string
	Path string
	Ref  domain.CASRef
	Kind string
}

type Request struct {
	Spec      domain.StepSpec
	TenantID  domain.TenantID
	Workspace string
	Staged    map[string]StagedInput
	Store     ports.ArtifactStore
	Scope     ports.SecurityScope
	Clock     ports.Clock
	Logger    *slog.Logger
}

type Result struct {
	Outcome         domain.StepOutcome
	ErrorClass      domain.ErrorClass
	Message         string
	Retryable       bool
	Evidence        []domain.Evidence
	Counterexamples []domain.Counterexample
	Artifacts       []domain.CASRef
	Metrics         domain.StepMetrics
	Outputs         map[string]string
}

type Engine interface {
	Name() string
	Version() string
	Kinds() []domain.StepKind
	Languages() []string
	Execute(ctx context.Context, request Request) (Result, error)
}

type Capability struct {
	Name          string                 `json:"name"`
	Version       string                 `json:"version"`
	Kinds         []domain.StepKind      `json:"kinds"`
	Languages     []string               `json:"languages"`
	Isolation     []domain.IsolationTier `json:"isolation,omitempty"`
	Deterministic bool                   `json:"deterministic"`
}

type Registry struct {
	mu      sync.RWMutex
	engines []Engine
}

func NewRegistry(engines ...Engine) *Registry {
	return &Registry{engines: engines}
}

func (r *Registry) Register(engine Engine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.engines = append(r.engines, engine)
}

func (r *Registry) ForEngine(kind domain.StepKind, language, engineName string) (Engine, bool) {
	if engineName == "" {
		return r.For(kind, language)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, candidate := range r.engines {
		if candidate.Name() != engineName {
			continue
		}
		if !supportsKind(candidate, kind) {
			continue
		}
		if language != "" && !supportsLanguage(candidate, language) {
			continue
		}
		return candidate, true
	}
	return nil, false
}

func (r *Registry) For(kind domain.StepKind, language string) (Engine, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, candidate := range r.engines {
		if !supportsKind(candidate, kind) {
			continue
		}
		if language != "" && !supportsLanguage(candidate, language) {
			continue
		}
		return candidate, true
	}
	return nil, false
}

func (r *Registry) Capabilities() []Capability {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Capability, 0, len(r.engines))
	for _, candidate := range r.engines {
		out = append(out, Capability{
			Name:      candidate.Name(),
			Version:   candidate.Version(),
			Kinds:     append([]domain.StepKind(nil), candidate.Kinds()...),
			Languages: append([]string(nil), candidate.Languages()...),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) Validate(kind domain.StepKind, language, engineName string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, candidate := range r.engines {
		if engineName != "" && candidate.Name() != engineName {
			continue
		}
		if !supportsKind(candidate, kind) {
			continue
		}
		if language != "" && !supportsLanguage(candidate, language) {
			continue
		}
		return nil
	}
	return fmt.Errorf("%w: kind=%s language=%s engine=%s", domain.ErrUnsupported, kind, language, engineName)
}

func supportsKind(candidate Engine, kind domain.StepKind) bool {
	for _, supported := range candidate.Kinds() {
		if supported == kind {
			return true
		}
	}
	return false
}

func supportsLanguage(candidate Engine, language string) bool {
	languages := candidate.Languages()
	if len(languages) == 0 {
		return true
	}
	for _, supported := range languages {
		if supported == language {
			return true
		}
	}
	return false
}
