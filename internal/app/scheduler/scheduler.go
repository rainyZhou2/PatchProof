package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/patchproof/patchproof/internal/actioncache"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/telemetry"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

type Config struct {
	Region             string
	OwnerID            string
	ShardCount         int
	BatchSize          int
	LeaseTTL           time.Duration
	ShardLeaseTTL      time.Duration
	PollInterval       time.Duration
	MaxStepsPerTenant  int
	MaxStepsPerCluster int
	ClusterStaleAfter  time.Duration
	MaxRunWallSeconds  int
	CallbackURL        string
	AttemptTokenTTL    time.Duration
	ActionCacheEnabled bool
	ActionCacheTTL     time.Duration
	CacheEpoch         string
	EngineVersions     map[string]string
	WorkerImage        string
	Clock              ports.Clock
	Logger             *slog.Logger
}

type Service struct {
	store     ports.MetadataStore
	executors ports.ExecutorFactory
	placement ports.PlacementStrategy
	tokens    ports.AttemptTokens
	cas       ports.ArtifactStore
	clock     ports.Clock
	logger    *slog.Logger
	metrics   *telemetry.Metrics
	config    Config
	owned     map[int]struct{}
	mu        sync.Mutex
}

type trackedExecution struct {
	ref           domain.AttemptRef
	execRef       ports.ExecutionRef
	spec          domain.StepSpec
	cacheKey      string
	engineVersion string
	startedAt     time.Time
	deadline      time.Time
	cluster       domain.Cluster
}

func New(store ports.MetadataStore, executors ports.ExecutorFactory, placement ports.PlacementStrategy, tokens ports.AttemptTokens, cas ports.ArtifactStore, config Config, metrics *telemetry.Metrics) (*Service, error) {
	if store == nil || executors == nil || placement == nil || tokens == nil || cas == nil {
		return nil, fmt.Errorf("scheduler: all dependencies are required")
	}
	if config.ShardCount <= 0 {
		return nil, fmt.Errorf("scheduler: shard count must be positive")
	}
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 64
	}
	if config.LeaseTTL <= 0 {
		config.LeaseTTL = 2 * time.Minute
	}
	if config.ShardLeaseTTL <= 0 {
		config.ShardLeaseTTL = 30 * time.Second
	}
	if config.PollInterval <= 0 {
		config.PollInterval = time.Second
	}
	if config.AttemptTokenTTL <= 0 {
		config.AttemptTokenTTL = 30 * time.Minute
	}
	if config.ActionCacheTTL <= 0 {
		config.ActionCacheTTL = 168 * time.Hour
	}
	if config.MaxStepsPerTenant <= 0 {
		config.MaxStepsPerTenant = 64
	}
	if config.ClusterStaleAfter <= 0 {
		config.ClusterStaleAfter = 90 * time.Second
	}
	if config.EngineVersions == nil {
		config.EngineVersions = map[string]string{}
	}
	return &Service{
		store: store, executors: executors, placement: placement, tokens: tokens, cas: cas,
		clock: config.Clock, logger: config.Logger, metrics: metrics, config: config,
		owned: make(map[int]struct{}),
	}, nil
}

func (s *Service) Run(ctx context.Context) error {
	tracker := newTracker()
	shardTicker := time.NewTicker(s.config.ShardLeaseTTL / 3)
	expiryTicker := time.NewTicker(s.config.LeaseTTL / 4)
	cacheTicker := time.NewTicker(time.Hour)
	defer shardTicker.Stop()
	defer expiryTicker.Stop()
	defer cacheTicker.Stop()
	if err := s.refreshShards(ctx); err != nil {
		s.logger.Warn("initial shard ownership failed", slog.String("error", err.Error()))
	}
	for {
		if err := s.leaseSteps(ctx, tracker); err != nil && ctx.Err() == nil {
			s.logger.Warn("lease batch failed", slog.String("error", err.Error()))
		}
		if err := s.reconcile(ctx, tracker); err != nil && ctx.Err() == nil {
			s.logger.Warn("reconcile batch failed", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-shardTicker.C:
			if err := s.refreshShards(ctx); err != nil && ctx.Err() == nil {
				s.logger.Warn("shard refresh failed", slog.String("error", err.Error()))
			}
		case <-expiryTicker.C:
			if err := s.expireLeases(ctx, tracker); err != nil && ctx.Err() == nil {
				s.logger.Warn("lease expiry scan failed", slog.String("error", err.Error()))
			}
		case <-cacheTicker.C:
			if _, err := s.store.PurgeActionCache(ctx, s.clock.Now(), 500); err != nil && ctx.Err() == nil {
				s.logger.Warn("action cache purge failed", slog.String("error", err.Error()))
			}
		case <-time.After(s.config.PollInterval):
		}
	}
}

func (s *Service) refreshShards(ctx context.Context) error {
	shards := make([]int, 0, s.config.ShardCount)
	for shard := 0; shard < s.config.ShardCount; shard++ {
		shards = append(shards, shard)
	}
	owned, err := s.store.AcquireShardLeases(ctx, shards, s.config.OwnerID, s.config.ShardLeaseTTL, s.clock.Now())
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.owned = make(map[int]struct{}, len(owned))
	for _, shard := range owned {
		s.owned[shard] = struct{}{}
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) ownedShards() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.owned))
	for shard := range s.owned {
		out = append(out, shard)
	}
	sort.Ints(out)
	return out
}

func (s *Service) leaseSteps(ctx context.Context, tracker *tracker) error {
	shards := s.ownedShards()
	if len(shards) == 0 {
		return nil
	}
	now := s.clock.Now()
	candidates := make([]domain.Step, 0, s.config.BatchSize)
	seen := map[domain.StepID]struct{}{}
	for _, shard := range shards {
		steps, err := s.store.ListReadySteps(ctx, domain.ReadyQuery{
			Limit: s.config.BatchSize, Shard: domain.ShardID(shard), ShardCount: s.config.ShardCount, Now: now,
		})
		if err != nil {
			return err
		}
		for _, step := range steps {
			if _, ok := seen[step.ID]; ok {
				continue
			}
			seen[step.ID] = struct{}{}
			candidates = append(candidates, step)
		}
		if len(candidates) >= s.config.BatchSize*2 {
			break
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority > candidates[j].Priority
		}
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})
	if len(candidates) > s.config.BatchSize {
		candidates = candidates[:s.config.BatchSize]
	}
	clusters, err := s.store.ListClusters(ctx, "")
	if err != nil {
		return err
	}
	live := make([]domain.Cluster, 0, len(clusters))
	for _, cluster := range clusters {
		if cluster.Live(now, s.config.ClusterStaleAfter) {
			live = append(live, cluster)
		}
	}
	clusters = live
	activeByCluster := tracker.countsByCluster()
	tenantCounts := map[domain.TenantID]int{}
	runCache := map[domain.RunID]domain.Run{}
	for _, step := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		run, ok := runCache[step.RunID]
		if !ok {
			run, err = s.store.GetRun(ctx, step.TenantID, step.RunID)
			if err != nil {
				s.logger.Warn("scheduler cannot load run", slog.String("step_id", step.ID.String()), slog.String("error", err.Error()))
				continue
			}
			runCache[step.RunID] = run
		}
		if run.Status != domain.RunQueued && run.Status != domain.RunRunning {
			continue
		}
		if s.config.ActionCacheEnabled && actioncache.Cacheable(step) {
			if hit, cacheErr := s.tryCachedStep(ctx, step, run, now); cacheErr != nil {
				s.logger.Warn("action cache lookup failed", slog.String("step_id", step.ID.String()), slog.String("error", cacheErr.Error()))
			} else if hit {
				continue
			}
		}
		limit := s.config.MaxStepsPerTenant
		if tenant, err := s.store.GetTenant(ctx, step.TenantID); err == nil && tenant.Quota.MaxConcurrentSteps > 0 {
			limit = minInt(limit, tenant.Quota.MaxConcurrentSteps)
		}
		if limit > 0 {
			count, ok := tenantCounts[step.TenantID]
			if !ok {
				count, err = s.store.CountActiveSteps(ctx, step.TenantID)
				if err != nil {
					continue
				}
				tenantCounts[step.TenantID] = count
			}
			if count >= limit {
				continue
			}
		}
		cluster, ok := s.selectCluster(step, clusters, activeByCluster)
		if !ok {
			continue
		}
		if s.config.MaxStepsPerCluster > 0 && activeByCluster[cluster.ID] >= s.config.MaxStepsPerCluster {
			continue
		}
		executor, err := s.executors.ForCluster(cluster)
		if err != nil {
			s.logger.Warn("no executor for cluster", slog.String("cluster", cluster.ID.String()), slog.String("error", err.Error()))
			continue
		}
		attemptID := domain.NewAttemptID()
		lease, err := s.store.AcquireLease(ctx, domain.LeaseRequest{
			TenantID: step.TenantID, RunID: step.RunID, StepID: step.ID, AttemptID: attemptID,
			ExpectedGeneration: step.Generation, Owner: s.config.OwnerID, TTL: s.config.LeaseTTL,
			ClusterID: cluster.ID, Now: now, Priority: step.Priority,
			MaxParallelSteps: run.Budget.MaxParallelSteps, MaxCPUSeconds: run.Budget.CPUSeconds,
		})
		if err != nil {
			var leaseErr *domain.LeaseError
			var fenceErr *domain.FencingError
			if !errors.As(err, &leaseErr) && !errors.As(err, &fenceErr) {
				s.logger.Warn("lease acquisition failed", slog.String("step_id", step.ID.String()), slog.String("error", err.Error()))
			}
			continue
		}
		tenantCounts[step.TenantID]++
		spec, err := s.buildSpec(ctx, run, step, lease)
		if err != nil {
			_, failErr := s.store.FailAttempt(ctx, domain.AttemptFailure{
				Ref: lease.Ref(), ErrorClass: domain.ErrValidationFailed, Message: err.Error(), Retryable: false, At: s.clock.Now(),
			})
			if failErr != nil {
				s.logger.Warn("fail invalid attempt", slog.String("attempt_id", lease.AttemptID.String()), slog.String("error", failErr.Error()))
			}
			continue
		}
		execRef, err := executor.Start(ctx, ports.ExecutionRequest{
			Spec: spec, AttemptToken: spec.AttemptToken, CallbackURL: spec.CallbackURL,
		})
		if err != nil {
			s.logger.Warn("executor start failed", slog.String("step_id", step.ID.String()), slog.String("cluster", cluster.ID.String()), slog.String("error", err.Error()))
			_, _ = s.store.FailAttempt(ctx, domain.AttemptFailure{
				Ref: lease.Ref(), ErrorClass: domain.ErrExecutorError, Message: err.Error(), Retryable: true, At: s.clock.Now(),
			})
			continue
		}
		if s.metrics != nil {
			s.metrics.StepQueueWait.WithLabelValues(s.region(run), string(step.Kind), cluster.ID.String()).Observe(now.Sub(step.CreatedAt).Seconds())
		}
		cacheKey := ""
		engineVersion := ""
		if s.config.ActionCacheEnabled && actioncache.Cacheable(step) {
			engineVersion = s.engineVersion(step)
			if engineVersion == "" {
				s.logger.Warn("cacheable step has no engine version; skipping action cache", slog.String("step_id", step.ID.String()), slog.String("engine", step.Requirements.Engine))
			} else if key, keyErr := actioncache.KeyForStep(step, run, s.config.CacheEpoch, engineVersion, s.config.WorkerImage); keyErr == nil {
				cacheKey = key
			}
		}
		tracker.add(trackedExecution{ref: lease.Ref(), execRef: execRef, spec: spec, cacheKey: cacheKey, engineVersion: engineVersion, startedAt: now, deadline: spec.Deadline, cluster: cluster})
		activeByCluster[cluster.ID]++
	}
	return nil
}

func (s *Service) engineVersion(step domain.Step) string {
	return strings.TrimSpace(s.config.EngineVersions[step.Requirements.Engine])
}

func (s *Service) selectCluster(step domain.Step, clusters []domain.Cluster, active map[domain.ClusterID]int) (domain.Cluster, bool) {
	candidates := make([]domain.Cluster, 0, len(clusters))
	for _, cluster := range clusters {
		if !cluster.Live(s.clock.Now(), s.config.ClusterStaleAfter) {
			continue
		}
		if !cluster.Supports(step.Requirements) {
			continue
		}
		if s.config.MaxStepsPerCluster > 0 && active[cluster.ID] >= s.config.MaxStepsPerCluster {
			continue
		}
		if step.TargetCluster != "" && cluster.ID != step.TargetCluster {
			continue
		}
		candidates = append(candidates, cluster)
	}
	if len(candidates) == 0 {
		return domain.Cluster{}, false
	}
	selection, err := s.placement.Select(step, candidates)
	if err != nil {
		return domain.Cluster{}, false
	}
	return selection, true
}

func (s *Service) buildSpec(ctx context.Context, run domain.Run, step domain.Step, lease domain.Lease) (domain.StepSpec, error) {
	deadline := s.clock.Now().Add(step.Requirements.Deadline())
	if run.StartedAt != nil && s.config.MaxRunWallSeconds > 0 {
		runDeadline := run.StartedAt.Add(time.Duration(s.config.MaxRunWallSeconds) * time.Second)
		if runDeadline.Before(deadline) {
			deadline = runDeadline
		}
	}
	token, _, err := s.tokens.Issue(ctx, lease.Ref(), s.config.AttemptTokenTTL)
	if err != nil {
		return domain.StepSpec{}, err
	}
	spec := domain.StepSpec{
		RunID:               run.ID,
		StepID:              step.ID,
		AttemptID:           lease.AttemptID,
		TenantID:            run.TenantID,
		Generation:          lease.Generation,
		Kind:                step.Kind,
		Language:            stepopts.Get(step.Options, stepopts.Language),
		Engine:              stepopts.Get(step.Options, stepopts.Engine),
		PolicyDigest:        run.PolicyDigest,
		InputManifestDigest: step.InputManifestDigest,
		Inputs:              nil,
		Requirements:        step.Requirements,
		Options:             copyOptions(step.Options),
		Deadline:            deadline,
		AttemptToken:        token,
		CallbackURL:         s.config.CallbackURL,
		WorkspaceDigest:     run.SnapshotDigest,
	}
	if spec.Language == "" {
		spec.Language = "go"
	}
	if step.Kind.RequiresLanguage() {
		named, err := s.namedInputs(ctx, run)
		if err != nil {
			return domain.StepSpec{}, err
		}
		spec.NamedInputs = named
	}
	extra, err := s.declaredInputs(ctx, run, step)
	if err != nil {
		return domain.StepSpec{}, err
	}
	if len(extra) > 0 {
		if spec.NamedInputs == nil {
			spec.NamedInputs = make(map[string]domain.InputBinding, len(extra))
		}
		for name, binding := range extra {
			spec.NamedInputs[name] = binding
		}
	}
	return spec, nil
}

func (s *Service) declaredInputs(ctx context.Context, run domain.Run, step domain.Step) (map[string]domain.InputBinding, error) {
	out := make(map[string]domain.InputBinding)
	var dag *domain.DAG
	for key, value := range step.Options {
		if !strings.HasPrefix(key, stepopts.InputPrefix) {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(key, stepopts.InputPrefix))
		if name == "" {
			return nil, &domain.ValidationError{Field: "input", Reason: "input name must not be empty"}
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "cas://") {
			ref, err := domain.ParseCASURI(value)
			if err != nil {
				return nil, fmt.Errorf("input %s: %w", name, err)
			}
			scope := ports.SecurityScope{TenantID: run.TenantID, TrustDomain: "run", Region: run.Region}
			info, err := s.cas.Stat(ctx, scope, ref)
			if err != nil {
				return nil, fmt.Errorf("input %s: %w", name, err)
			}
			out[name] = domain.InputBinding{Ref: info.Ref, Kind: "file"}
			continue
		}
		separator := strings.IndexByte(value, '/')
		if separator <= 0 || separator == len(value)-1 {
			return nil, &domain.ValidationError{Field: "input", Reason: fmt.Sprintf("input %s must be a CAS URI or <step>/<output>", name)}
		}
		stepKey, outputKey := value[:separator], value[separator+1:]
		if dag == nil {
			loaded, err := s.store.GetDAG(ctx, run.TenantID, run.ID)
			if err != nil {
				return nil, err
			}
			dag = &loaded
		}
		source, ok := dagStepByKey(*dag, stepKey)
		if !ok {
			return nil, &domain.ValidationError{Field: "input", Reason: fmt.Sprintf("input %s references unknown step %q", name, stepKey)}
		}
		if !dependencyOf(*dag, step.ID, source.ID) {
			return nil, &domain.ValidationError{Field: "input", Reason: fmt.Sprintf("input %s references step %q which is not a dependency", name, stepKey)}
		}
		manifest, err := domain.ParseOutputManifest(source.OutputManifest)
		if err != nil {
			return nil, fmt.Errorf("input %s: %w", name, err)
		}
		ref, ok := manifest.ArtifactForOutput(outputKey)
		if !ok {
			return nil, fmt.Errorf("input %s: step %q has no artifact for output %q", name, stepKey, outputKey)
		}
		out[name] = domain.InputBinding{Ref: ref, Kind: "file"}
	}
	return out, nil
}

func dagStepByKey(dag domain.DAG, key string) (domain.Step, bool) {
	for _, step := range dag.Steps {
		if step.Key == key {
			return step, true
		}
	}
	return domain.Step{}, false
}

func dependencyOf(dag domain.DAG, stepID, candidate domain.StepID) bool {
	for _, dependency := range dag.Dependencies[stepID] {
		if dependency == candidate {
			return true
		}
	}
	return false
}

func (s *Service) namedInputs(ctx context.Context, run domain.Run) (map[string]domain.InputBinding, error) {
	if run.BaseSnapshot == "" || run.HeadSnapshot == "" {
		return nil, fmt.Errorf("run snapshots are not frozen")
	}
	scope := ports.SecurityScope{TenantID: run.TenantID, TrustDomain: "run", Region: run.Region}
	base, err := s.statBlob(ctx, scope, run.BaseSnapshot)
	if err != nil {
		return nil, fmt.Errorf("base snapshot: %w", err)
	}
	head, err := s.statBlob(ctx, scope, run.HeadSnapshot)
	if err != nil {
		return nil, fmt.Errorf("head snapshot: %w", err)
	}
	return map[string]domain.InputBinding{
		"base": {Ref: base, Kind: "tar.gz"},
		"head": {Ref: head, Kind: "tar.gz"},
	}, nil
}

func (s *Service) statBlob(ctx context.Context, scope ports.SecurityScope, uri string) (domain.CASRef, error) {
	ref, err := domain.ParseCASURI(uri)
	if err != nil {
		return domain.CASRef{}, err
	}
	info, err := s.cas.Stat(ctx, scope, ref)
	if err != nil {
		return domain.CASRef{}, err
	}
	return info.Ref, nil
}

func (s *Service) reconcile(ctx context.Context, tracker *tracker) error {
	executions := tracker.snapshot()
	for _, execution := range executions {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = s.store.RenewLease(ctx, domain.LeaseRenewal{
			Ref: execution.ref, Owner: s.config.OwnerID, Extension: s.config.LeaseTTL, Now: s.clock.Now(),
		})
		executor, err := s.executors.ForCluster(execution.cluster)
		if err != nil {
			s.logger.Warn("reconciler cannot resolve executor", slog.String("cluster", execution.cluster.ID.String()), slog.String("error", err.Error()))
			continue
		}
		observation, err := executor.Observe(ctx, execution.execRef)
		if err != nil {
			s.logger.Warn("observe execution failed", slog.String("attempt_id", execution.ref.AttemptID.String()), slog.String("error", err.Error()))
			continue
		}
		switch observation.State {
		case ports.ExecutionCompleted:
			if observation.Result == nil {
				if step, stepErr := s.store.GetStep(ctx, execution.ref.TenantID, execution.ref.StepID); stepErr == nil && step.Status.Terminal() {
					tracker.remove(execution.ref.AttemptID)
					continue
				}
				if s.clock.Now().After(execution.deadline.Add(2 * time.Minute)) {
					_, _ = s.store.FailAttempt(ctx, domain.AttemptFailure{
						Ref: execution.ref, ErrorClass: domain.ErrTimeout, Message: "execution finished without a result", Retryable: true, At: s.clock.Now(),
					})
					tracker.remove(execution.ref.AttemptID)
				}
				continue
			}
			if err := s.commitResult(ctx, execution, *observation.Result); err != nil {
				s.logger.Warn("commit result failed", slog.String("attempt_id", execution.ref.AttemptID.String()), slog.String("error", err.Error()))
				continue
			}
			tracker.remove(execution.ref.AttemptID)
		case ports.ExecutionFailed, ports.ExecutionLost, ports.ExecutionCancelled:
			class := observation.ErrorClass
			if class == "" {
				class = domain.ErrExecutorError
			}
			retryable := observation.State != ports.ExecutionCancelled
			_, _ = s.store.FailAttempt(ctx, domain.AttemptFailure{
				Ref: execution.ref, ErrorClass: class, Message: observation.Message, Retryable: retryable, At: s.clock.Now(),
			})
			tracker.remove(execution.ref.AttemptID)
		case ports.ExecutionPending, ports.ExecutionRunning:
			if step, err := s.store.GetStep(ctx, execution.ref.TenantID, execution.ref.StepID); err == nil && step.Status.Terminal() {
				tracker.remove(execution.ref.AttemptID)
				continue
			}
			if s.clock.Now().After(execution.deadline.Add(5 * time.Minute)) {
				_ = executor.Cancel(ctx, execution.execRef)
				_, _ = s.store.FailAttempt(ctx, domain.AttemptFailure{
					Ref: execution.ref, ErrorClass: domain.ErrTimeout, Message: "execution exceeded its deadline", Retryable: true, At: s.clock.Now(),
				})
				tracker.remove(execution.ref.AttemptID)
			}
		}
	}
	return nil
}

func (s *Service) commitResult(ctx context.Context, execution trackedExecution, result domain.StepResult) error {
	outcome, err := s.store.CommitAttempt(ctx, domain.AttemptCommit{Ref: execution.ref, Result: result, At: s.clock.Now()})
	if err != nil {
		var fencing *domain.FencingError
		if errors.As(err, &fencing) {
			return nil
		}
		return err
	}
	if execution.cacheKey != "" && execution.engineVersion != "" && actioncache.Storable(result) {
		engineVersion := execution.engineVersion
		for _, item := range result.Evidence {
			if item.Engine.Version != "" {
				engineVersion = item.Engine.Version
				break
			}
		}
		if engineVersion != execution.engineVersion {
			s.logger.Warn("engine version changed during execution; skipping action cache write",
				slog.String("step_id", execution.ref.StepID.String()),
				slog.String("expected", execution.engineVersion), slog.String("actual", engineVersion))
		} else {
			stepForEntry := domain.Step{Kind: execution.spec.Kind, Requirements: execution.spec.Requirements}
			entry, entryErr := actioncache.NewEntry(execution.ref.TenantID, execution.cacheKey, stepForEntry, engineVersion, result, s.config.ActionCacheTTL, s.clock.Now())
			if entryErr == nil {
				if putErr := s.store.PutActionCache(ctx, entry); putErr != nil {
					s.logger.Warn("action cache write failed", slog.String("step_id", execution.ref.StepID.String()), slog.String("error", putErr.Error()))
				}
			}
		}
	}
	if s.metrics != nil {
		s.metrics.StepDuration.WithLabelValues(execution.spec.Engine, string(execution.spec.Kind), execution.cluster.ID.String()).Observe(
			s.clock.Now().Sub(execution.startedAt).Seconds())
		if outcome.Step.FailureClass != "" {
			s.metrics.StepRetries.WithLabelValues(string(outcome.Step.FailureClass), s.config.Region).Inc()
			s.metrics.EngineErrors.WithLabelValues(execution.spec.Engine, string(outcome.Step.FailureClass)).Inc()
		}
	}
	return nil
}

func (s *Service) tryCachedStep(ctx context.Context, step domain.Step, run domain.Run, now time.Time) (bool, error) {
	engineVersion := s.engineVersion(step)
	if engineVersion == "" {
		return false, nil
	}
	key, err := actioncache.KeyForStep(step, run, s.config.CacheEpoch, engineVersion, s.config.WorkerImage)
	if err != nil {
		return false, err
	}
	entry, err := s.store.GetActionCache(ctx, step.TenantID, key, now)
	if err != nil {
		if domain.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if entry.EngineVersion != engineVersion {
		s.logger.Warn("action cache entry engine version mismatch", slog.String("step_id", step.ID.String()),
			slog.String("expected", engineVersion), slog.String("stored", entry.EngineVersion))
		return false, nil
	}
	attemptID := domain.NewAttemptID()
	lease, err := s.store.AcquireLease(ctx, domain.LeaseRequest{
		TenantID: step.TenantID, RunID: step.RunID, StepID: step.ID, AttemptID: attemptID,
		ExpectedGeneration: step.Generation, Owner: s.config.OwnerID, TTL: s.config.LeaseTTL,
		Now: now, Priority: step.Priority,
		MaxParallelSteps: run.Budget.MaxParallelSteps, MaxCPUSeconds: run.Budget.CPUSeconds,
	})
	if err != nil {
		return false, nil
	}
	result, err := actioncache.CloneForStep(entry, run.ID, step.ID, step.TenantID, s.clock.Now())
	if err != nil {
		return false, err
	}
	if _, err := s.store.CommitAttempt(ctx, domain.AttemptCommit{Ref: lease.Ref(), Result: result, At: s.clock.Now()}); err != nil {
		return false, err
	}
	if s.metrics != nil {
		s.metrics.CacheHits.WithLabelValues("action", step.Requirements.Engine).Inc()
	}
	s.logger.Info("action cache hit", slog.String("step_id", step.ID.String()), slog.String("kind", string(step.Kind)))
	return true, nil
}

func (s *Service) expireLeases(ctx context.Context, tracker *tracker) error {
	expired, err := s.store.ExpireLeases(ctx, s.clock.Now(), 256)
	if err != nil {
		return err
	}
	for _, expiry := range expired {
		tracker.remove(expiry.AttemptID)
		if s.metrics != nil {
			s.metrics.LeaseExpired.WithLabelValues(s.config.Region).Inc()
		}
	}
	return nil
}

func (s *Service) region(run domain.Run) string {
	if run.Region != "" {
		return run.Region
	}
	return s.config.Region
}

func copyOptions(options map[string]string) map[string]string {
	if len(options) == 0 {
		return nil
	}
	out := make(map[string]string, len(options))
	for key, value := range options {
		out[key] = value
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type tracker struct {
	mu   sync.Mutex
	data map[domain.AttemptID]trackedExecution
}

func newTracker() *tracker {
	return &tracker{data: make(map[domain.AttemptID]trackedExecution)}
}

func (t *tracker) add(execution trackedExecution) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data[execution.ref.AttemptID] = execution
}

func (t *tracker) remove(attemptID domain.AttemptID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.data, attemptID)
}

func (t *tracker) snapshot() []trackedExecution {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]trackedExecution, 0, len(t.data))
	for _, execution := range t.data {
		out = append(out, execution)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].startedAt.Before(out[j].startedAt) })
	return out
}

func (t *tracker) countsByCluster() map[domain.ClusterID]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[domain.ClusterID]int{}
	for _, execution := range t.data {
		out[execution.cluster.ID]++
	}
	return out
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }
