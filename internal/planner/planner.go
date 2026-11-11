package planner

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/evidence"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

type Limits struct {
	MaxStepCount       int
	MaxParallelSteps   int
	MaxStepWallSeconds int
	MaxRunWallSeconds  int
	DefaultIsolation   domain.IsolationTier
	MaxIsolation       domain.IsolationTier
	Architecture       string
}

type Planner struct {
	shardCount   int
	limits       Limits
	architecture string
}

func New(shardCount int, limits Limits) (*Planner, error) {
	if shardCount <= 0 {
		return nil, fmt.Errorf("planner: shard count must be positive")
	}
	if limits.MaxStepCount <= 0 {
		limits.MaxStepCount = 64
	}
	if limits.Architecture == "" {
		limits.Architecture = "amd64"
	}
	return &Planner{shardCount: shardCount, limits: limits, architecture: limits.Architecture}, nil
}

func (p *Planner) Plan(ctx context.Context, request ports.PlanRequest) (ports.PlanResult, error) {
	if err := ctx.Err(); err != nil {
		return ports.PlanResult{}, err
	}
	if err := request.Run.Validate(); err != nil {
		return ports.PlanResult{}, err
	}
	if !request.Decision.Allowed {
		return ports.PlanResult{}, &domain.PolicyError{Code: "POLICY_BLOCKED", Reason: "policy decision rejected the run before planning"}
	}
	if request.Run.PolicyDigest == "" {
		return ports.PlanResult{}, &domain.ValidationError{Field: "policy_digest", Reason: "must not be empty"}
	}

	required := make(map[string]struct{}, len(request.Decision.RequiredChecks))
	for _, check := range request.Decision.RequiredChecks {
		required[domain.NormalizeCheckName(check)] = struct{}{}
	}
	required["compile"] = struct{}{}
	if _, ok := required["bounded-smt"]; ok {
		return ports.PlanResult{}, &domain.ValidationError{
			Field:  "policy.spec.verification.required",
			Reason: "bounded-smt has no executable engine in this build; refusing to plan a claim that can never be verified",
		}
	}

	isolation := request.Decision.Isolation
	if !isolation.Valid() {
		isolation = p.limits.DefaultIsolation
	}
	if p.limits.MaxIsolation.Valid() && isolation.Rank() > p.limits.MaxIsolation.Rank() {
		return ports.PlanResult{}, &domain.ValidationError{
			Field:  "execution.maxIsolation",
			Reason: fmt.Sprintf("isolation %q exceeds the configured maximum %q", isolation, p.limits.MaxIsolation),
		}
	}
	subjectNetwork := request.Decision.SubjectNetwork
	if subjectNetwork == "" {
		subjectNetwork = domain.NetworkNone
	}
	platformNetwork := domain.NetworkRestricted
	var features []string
	if request.Decision.ArtifactTransport == domain.TransportBrokeredOffline {
		platformNetwork = domain.NetworkNone
		features = []string{"brokered-offline-io"}
	}

	builder := &dagBuilder{
		run:          request.Run,
		policy:       request.Policy,
		decision:     request.Decision,
		planner:      p,
		now:          request.Now,
		steps:        make([]domain.Step, 0, 16),
		deps:         make(map[domain.StepID][]domain.StepID, 16),
		claims:       make([]domain.Claim, 0, 8),
		keys:         make(map[string]struct{}, 16),
		claimIDs:     make(map[domain.ClaimID]struct{}, 8),
		shardCount:   p.shardCount,
		architecture: p.architecture,
		isolation:    isolation,
		network:      platformNetwork,
		subject:      subjectNetwork,
		features:     features,
	}
	if request.Now.IsZero() {
		builder.now = time.Now().UTC()
	}
	if err := builder.build(ctx, required); err != nil {
		return ports.PlanResult{}, err
	}
	if len(builder.steps) == 0 {
		return ports.PlanResult{}, &domain.ValidationError{Field: "plan", Reason: "no executable verification steps were selected"}
	}
	if p.limits.MaxStepCount > 0 && len(builder.steps) > p.limits.MaxStepCount {
		return ports.PlanResult{}, &domain.ValidationError{Field: "plan", Reason: fmt.Sprintf("plan has %d steps, limit is %d", len(builder.steps), p.limits.MaxStepCount)}
	}
	ensureDependencies(builder.steps, builder.deps)
	return ports.PlanResult{Steps: builder.steps, Dependencies: builder.deps, Claims: builder.claims}, nil
}

type dagBuilder struct {
	run          domain.Run
	policy       domain.PolicyDocument
	decision     ports.PolicyDecision
	planner      *Planner
	now          time.Time
	steps        []domain.Step
	deps         map[domain.StepID][]domain.StepID
	claims       []domain.Claim
	keys         map[string]struct{}
	claimIDs     map[domain.ClaimID]struct{}
	shardCount   int
	architecture string
	isolation    domain.IsolationTier
	network      domain.NetworkMode
	subject      domain.NetworkMode
	features     []string
}

func (b *dagBuilder) build(ctx context.Context, required map[string]struct{}) error {
	analyzeEngine, analyzeLanguage := b.engineFor("analyze", "go-analyzer", "go")
	impactEngine, impactLanguage := b.engineFor("impact", analyzeEngine, analyzeLanguage)
	compileEngine, compileLanguage := b.engineFor("compile", "go-build", "go")
	regressionEngine, regressionLanguage := b.engineFor("regression", compileEngine, compileLanguage)
	securityEngine, securityLanguage := b.engineFor("security", "go-security", "go")
	fuzzEngine, fuzzLanguage := b.engineFor("fuzz", "go-native-fuzz", "go")

	staticClaim, err := b.addClaim(claimDefinition{
		id:        "static-analysis",
		kind:      domain.ClaimKindProvenance,
		title:     "Static analysis produced an explicit impact set",
		predicate: "changed symbols and affected callers are identified with explicit resolution",
		domain:    "packages in the snapshot; dynamic calls are treated conservatively",
		language:  analyzeLanguage,
		min:       domain.ClaimTested,
		required:  false,
	})
	if err != nil {
		return err
	}
	analyze, err := b.addStep(stepDefinition{
		key:      "analyze",
		kind:     domain.StepKindAnalyze,
		engine:   analyzeEngine,
		language: analyzeLanguage,
		priority: 60,
		cpu:      1000,
		memory:   2048,
		disk:     5,
		timeout:  600,
		options:  map[string]string{stepopts.ClaimID: string(staticClaim), "analysis.mode": "full", "cache.enabled": "true"},
	})
	if err != nil {
		return err
	}
	impact, err := b.addStep(stepDefinition{
		key:       "impact",
		kind:      domain.StepKindImpact,
		engine:    impactEngine,
		language:  impactLanguage,
		priority:  55,
		cpu:       1000,
		memory:    2048,
		disk:      5,
		timeout:   600,
		dependsOn: []domain.StepID{analyze},
		options:   map[string]string{"cache.enabled": "true"},
	})
	if err != nil {
		return err
	}

	compileBase, err := b.addStep(stepDefinition{
		key:      "compile-base",
		kind:     domain.StepKindCompile,
		engine:   compileEngine,
		language: compileLanguage,
		priority: 90,
		cpu:      2000,
		memory:   4096,
		disk:     10,
		timeout:  900,
		options:  map[string]string{stepopts.ClaimID: "compile", "target": "base", "cache.enabled": "true", stepopts.CheckName: "compile"},
	})
	if err != nil {
		return err
	}
	compileHead, err := b.addStep(stepDefinition{
		key:      "compile-head",
		kind:     domain.StepKindCompile,
		engine:   compileEngine,
		language: compileLanguage,
		priority: 95,
		cpu:      2000,
		memory:   4096,
		disk:     10,
		timeout:  900,
		options:  map[string]string{stepopts.ClaimID: "compile", "target": "head", "cache.enabled": "true", stepopts.CheckName: "compile"},
	})
	if err != nil {
		return err
	}
	if _, err := b.addClaim(claimDefinition{
		id:        "compile",
		kind:      domain.ClaimKindCompile,
		title:     "Base and head snapshots compile",
		predicate: "go build succeeds for all declared package patterns",
		domain:    "all packages matched by the configured build pattern",
		language:  compileLanguage,
		min:       b.minimumEvidence("compile", domain.ClaimTested),
		required:  true,
	}); err != nil {
		return err
	}

	if _, ok := required["regression-test"]; ok {
		if _, err := b.addClaim(claimDefinition{
			id:        "regression",
			kind:      domain.ClaimKindTest,
			title:     "Existing test suite passes on the patched snapshot",
			predicate: "go test succeeds for the selected package patterns",
			domain:    "packages selected by static impact analysis; falls back to the configured pattern set",
			language:  regressionLanguage,
			min:       b.minimumEvidence("regression-test", domain.ClaimTested),
			required:  true,
		}); err != nil {
			return err
		}
		if _, err := b.addStep(stepDefinition{
			key:         "regression",
			kind:        domain.StepKindRegression,
			engine:      regressionEngine,
			language:    regressionLanguage,
			priority:    85,
			cpu:         2000,
			memory:      4096,
			disk:        20,
			timeout:     900,
			preemptible: true,
			dependsOn:   []domain.StepID{compileHead, impact},
			options: map[string]string{
				stepopts.ClaimID:                "regression",
				stepopts.CheckName:              "regression-test",
				stepopts.InputPrefix + "impact": "impact/impact.digest",
			},
		}); err != nil {
			return err
		}
	}

	if _, ok := required["differential-fuzz"]; ok {
		contracts := b.policy.Spec.Verification.Contracts
		if len(contracts) == 0 {
			return &domain.ValidationError{Field: "policy.contracts", Reason: "differential-fuzz requires at least one contract"}
		}
	}
	fuzzContracts := 0
	contracts := b.policy.Spec.Verification.Contracts
	if _, ok := required["differential-fuzz"]; ok {
		if b.run.Replay != nil {
			fuzzContracts++
		} else {
			fuzzContracts += len(contracts)
		}
	}
	if _, ok := required["native-fuzz"]; ok {
		if len(contracts) == 0 {
			return &domain.ValidationError{Field: "policy.contracts", Reason: "native-fuzz requires at least one contract"}
		}
		fuzzContracts += len(contracts)
	}
	fuzzSeconds, fuzzTimeout, maxCases := b.fuzzAllocation(fuzzContracts, 600)

	if _, ok := required["differential-fuzz"]; ok {
		replayMatched := false
		for _, contract := range contracts {
			if b.run.Replay != nil && b.run.Replay.ClaimID != domain.ClaimID(contract.ID) {
				continue
			}
			if b.run.Replay != nil {
				replayMatched = true
			}
			contractRef := contract.DomainRef
			if contractRef == "" {
				contractRef = contract.OracleRef
			}
			if contractRef == "" {
				return &domain.ValidationError{Field: "contract.domainRef", Reason: "contract " + contract.ID + " has no contract document reference"}
			}
			claimKind := domain.ClaimKindRegression
			if strings.EqualFold(contract.Kind, "fix-invariant") || strings.EqualFold(contract.Kind, "fix") {
				claimKind = domain.ClaimKindFix
			}
			differentialLanguage := contract.Language
			if differentialLanguage == "" {
				differentialLanguage = "go"
			}
			differentialEngine, _ := b.engineFor("differential", "go-differential", differentialLanguage)
			minimum := domain.ClaimTested
			if contract.MinimumResult == string(domain.ClaimProven) {
				minimum = domain.ClaimProven
			}
			minimum = b.minimumEvidence("differential-fuzz", minimum)
			domainText := contract.Domain
			if strings.TrimSpace(domainText) == "" {
				domainText = "declared contract input domain (" + contractRef + ")"
			}
			oracleText := contract.Oracle
			if strings.TrimSpace(oracleText) == "" {
				oracleText = contractRef
			}
			claimID, err := b.addClaim(claimDefinition{
				id:         contract.ID,
				kind:       claimKind,
				title:      contractTitle(contract),
				predicate:  "observable behavior in the declared domain matches the contract",
				domain:     domainText,
				oracle:     oracleText,
				inputRef:   contractRef,
				observable: contract.Observable,
				symbol:     contract.Symbol,
				min:        minimum,
				required:   contract.Required || policyCheckRequired(b.decision, "differential-fuzz"),
			})
			if err != nil {
				return err
			}
			options := map[string]string{
				stepopts.ClaimID:       string(claimID),
				stepopts.CheckName:     "differential-fuzz",
				stepopts.ContractID:    contract.ID,
				stepopts.ContractRef:   contractRef,
				stepopts.TargetSymbol:  contract.Symbol,
				stepopts.InputDomain:   contract.Domain,
				stepopts.Observable:    contract.Observable,
				stepopts.FuzzSeconds:   fmt.Sprintf("%d", fuzzSeconds),
				stepopts.MinimumResult: string(minimum),
			}
			if maxCases > 0 {
				options[stepopts.FuzzMaxCases] = fmt.Sprintf("%d", maxCases)
			}
			if b.run.Replay != nil {
				options[stepopts.FuzzSeconds] = "1"
				options[stepopts.ReplayCase] = b.run.Replay.CaseID
				options[stepopts.ReplayCodec] = b.run.Replay.InputCodec
				options[stepopts.InputPrefix+"replay"] = b.run.Replay.InputRef.URI()
			}
			stepTimeout := fuzzTimeout
			if b.run.Replay != nil {
				stepTimeout = 600
			}
			if _, err := b.addStep(stepDefinition{
				key:         "differential-" + slang(contract.ID),
				kind:        domain.StepKindDifferential,
				engine:      differentialEngine,
				language:    differentialLanguage,
				priority:    75,
				cpu:         2000,
				memory:      4096,
				disk:        10,
				timeout:     stepTimeout,
				preemptible: true,
				dependsOn:   []domain.StepID{compileBase, compileHead, impact},
				options:     options,
			}); err != nil {
				return err
			}
		}
		if b.run.Replay != nil && !replayMatched {
			return &domain.ValidationError{Field: "replay.claim_id", Reason: "replay claim " + string(b.run.Replay.ClaimID) + " does not match any contract"}
		}
	}
	if _, ok := required["native-fuzz"]; ok {
		if _, err := b.addClaim(claimDefinition{
			id:        "native-fuzz",
			kind:      domain.ClaimKindTest,
			title:     "Native fuzzing found no crashes",
			predicate: "the head fuzz target does not panic for the declared contracts",
			domain:    "contract seeds and type-directed generated inputs",
			language:  fuzzLanguage,
			min:       b.minimumEvidence("native-fuzz", domain.ClaimTested),
			required:  true,
		}); err != nil {
			return err
		}
		for _, contract := range contracts {
			contractRef := contract.DomainRef
			if contractRef == "" {
				contractRef = contract.OracleRef
			}
			if contractRef == "" {
				return &domain.ValidationError{Field: "contract.domainRef", Reason: "contract " + contract.ID + " has no contract document reference"}
			}
			if _, err := b.addStep(stepDefinition{
				key:         "native-fuzz-" + slang(contract.ID),
				kind:        domain.StepKindDifferential,
				engine:      fuzzEngine,
				language:    fuzzLanguage,
				priority:    74,
				cpu:         2000,
				memory:      4096,
				disk:        10,
				timeout:     fuzzTimeout,
				preemptible: true,
				dependsOn:   []domain.StepID{compileHead, impact},
				options: map[string]string{
					stepopts.ClaimID:      "native-fuzz",
					stepopts.CheckName:    "native-fuzz",
					stepopts.ContractRef:  contractRef,
					stepopts.TargetSymbol: contract.Symbol,
					stepopts.InputDomain:  contract.Domain,
					stepopts.FuzzSeconds:  fmt.Sprintf("%d", fuzzSeconds),
				},
			}); err != nil {
				return err
			}
		}
	}

	if _, ok := required["security-scan"]; ok {
		claimID, err := b.addClaim(claimDefinition{
			id:        "security-scan",
			kind:      domain.ClaimKindSecurity,
			title:     "Security scan found no policy violations",
			predicate: "configured scanner reports no high severity findings",
			domain:    "changed packages and their direct dependencies",
			language:  securityLanguage,
			min:       b.minimumEvidence("security-scan", domain.ClaimTested),
			required:  true,
		})
		if err != nil {
			return err
		}
		if _, err := b.addStep(stepDefinition{
			key:       "security",
			kind:      domain.StepKindSecurity,
			engine:    securityEngine,
			language:  securityLanguage,
			priority:  70,
			cpu:       1000,
			memory:    2048,
			disk:      5,
			timeout:   600,
			dependsOn: []domain.StepID{impact},
			options: map[string]string{
				stepopts.ClaimID:                string(claimID),
				stepopts.CheckName:              "security-scan",
				stepopts.InputPrefix + "impact": "impact/impact.digest",
			},
		}); err != nil {
			return err
		}
	}

	if _, ok := required["protected-assets"]; ok {
		if _, err := b.addClaim(claimDefinition{
			id:        "protected-assets",
			kind:      domain.ClaimKindSecurity,
			title:     "Patch does not modify protected assets",
			predicate: "no changed path matches a protected path pattern",
			domain:    "all files changed between base and head",
			min:       b.minimumEvidence("protected-assets", domain.ClaimTested),
			required:  true,
		}); err != nil {
			return err
		}
	}

	if _, ok := required["bounded-smt"]; ok {
		if _, err := b.addClaim(claimDefinition{
			id:        "bounded-smt",
			kind:      domain.ClaimKindFix,
			title:     "Bounded SMT properties hold",
			predicate: "declared properties are proven within the encoded model",
			domain:    "supported integer and boolean function subset",
			min:       b.minimumEvidence("bounded-smt", domain.ClaimProven),
			required:  false,
		}); err != nil {
			return err
		}
	}
	return nil
}

type stepDefinition struct {
	key         string
	kind        domain.StepKind
	engine      string
	priority    int
	cpu         int64
	memory      int64
	disk        int64
	timeout     int
	preemptible bool
	dependsOn   []domain.StepID
	options     map[string]string
	language    string
}

func (b *dagBuilder) addStep(definition stepDefinition) (domain.StepID, error) {
	if _, exists := b.keys[definition.key]; exists {
		return "", &domain.ValidationError{Field: "plan", Reason: "duplicate step key " + definition.key}
	}
	b.keys[definition.key] = struct{}{}
	deadline := definition.timeout
	if deadline <= 0 {
		deadline = 600
	}
	if limit := b.policy.Spec.Execution.MaxStepWallSeconds; limit > 0 && deadline > limit {
		deadline = limit
	}
	if limit := b.planner.limits.MaxStepWallSeconds; limit > 0 && deadline > limit {
		deadline = limit
	}
	language := definition.language
	if language == "" {
		language = "go"
	}
	requirements := domain.ResourceRequirements{
		Region:            b.run.Region,
		Architecture:      b.architecture,
		Language:          language,
		Engine:            definition.engine,
		Isolation:         b.isolation,
		CPUMilli:          definition.cpu,
		MemoryMiB:         definition.memory,
		EphemeralDiskGiB:  definition.disk,
		Network:           b.network,
		SubjectNetwork:    b.subject,
		DeadlineSeconds:   deadline,
		Preemptible:       definition.preemptible,
		RuntimeClass:      b.policy.Spec.Execution.RuntimeClass,
		ArtifactTransport: b.decision.ArtifactTransport,
		RequiredFeatures:  append([]string(nil), b.features...),
	}
	if requirements.ArtifactTransport == "" {
		requirements.ArtifactTransport = domain.TransportDirect
	}
	if err := requirements.Validate(); err != nil {
		return "", err
	}
	options := make(map[string]string, len(definition.options)+5)
	for key, value := range definition.options {
		options[key] = value
	}
	options[stepopts.Language] = language
	options[stepopts.Engine] = definition.engine
	options[stepopts.BaseCommit] = b.run.BaseCommit
	options[stepopts.HeadCommit] = b.run.HeadCommit
	if b.run.BaseSnapshot != "" {
		options[stepopts.BaseSnapshot] = b.run.BaseSnapshot
	}
	if b.run.HeadSnapshot != "" {
		options[stepopts.HeadSnapshot] = b.run.HeadSnapshot
	}
	manifestDigest, err := b.inputDigest(definition.key, options)
	if err != nil {
		return "", err
	}
	step := domain.Step{
		ID:                  domain.NewStepID(),
		RunID:               b.run.ID,
		TenantID:            b.run.TenantID,
		Key:                 definition.key,
		Kind:                definition.kind,
		Status:              domain.StepBlocked,
		MaxAttempts:         maxAttemptsFor(definition.kind),
		Requirements:        requirements,
		InputManifestDigest: manifestDigest,
		Options:             options,
		Priority:            definition.priority,
		Shard:               shardFor(b.run.ID, definition.key, b.shardCount),
		CreatedAt:           b.now,
		UpdatedAt:           b.now,
	}
	if len(definition.dependsOn) == 0 {
		if err := step.Transition(domain.StepReady, b.now); err != nil {
			return "", err
		}
	} else {
		dependencies := make([]domain.StepID, 0, len(definition.dependsOn))
		for _, dependency := range definition.dependsOn {
			if dependency == "" {
				return "", &domain.ValidationError{Field: "plan.dependencies", Reason: "empty dependency for " + definition.key}
			}
			dependencies = append(dependencies, dependency)
		}
		sort.Slice(dependencies, func(i, j int) bool { return dependencies[i] < dependencies[j] })
		b.deps[step.ID] = dependencies
	}
	if err := step.Validate(); err != nil {
		return "", err
	}
	b.steps = append(b.steps, step)
	return step.ID, nil
}

type claimDefinition struct {
	id         string
	kind       domain.ClaimKind
	title      string
	predicate  string
	domain     string
	oracle     string
	inputRef   string
	observable string
	symbol     string
	language   string
	min        domain.ClaimResult
	required   bool
}

func (b *dagBuilder) addClaim(definition claimDefinition) (domain.ClaimID, error) {
	claimID := domain.ClaimID(definition.id)
	if _, exists := b.claimIDs[claimID]; exists {
		return "", &domain.ValidationError{Field: "claim.id", Reason: "duplicate claim " + definition.id}
	}
	b.claimIDs[claimID] = struct{}{}
	claim := domain.Claim{
		ID:            claimID,
		RunID:         b.run.ID,
		TenantID:      b.run.TenantID,
		ContractID:    definition.id,
		Kind:          definition.kind,
		Title:         definition.title,
		Predicate:     definition.predicate,
		Domain:        definition.domain,
		Assumptions:   []string{"the declared input domain is representative of intended behavior", "base and head execute under the same toolchain digest"},
		Observable:    definition.observable,
		OracleRef:     definition.inputRef,
		Language:      claimLanguage(definition.language),
		Symbol:        definition.symbol,
		Required:      definition.required,
		MinimumResult: definition.min,
		CreatedAt:     b.now,
	}
	if claim.Observable == "" {
		claim.Observable = "return-value"
	}
	if err := claim.Validate(); err != nil {
		return "", err
	}
	b.claims = append(b.claims, claim)
	return claimID, nil
}

func (b *dagBuilder) inputDigest(stepKey string, options map[string]string) (string, error) {
	return evidence.CanonicalDigest(map[string]any{
		// The input manifest describes the action, not the run that happens to
		// execute it. Including run_id here made every action-cache key unique
		// per run and disabled cross-run result reuse.
		"step_key":      stepKey,
		"base_commit":   b.run.BaseCommit,
		"head_commit":   b.run.HeadCommit,
		"base_snapshot": b.run.BaseSnapshot,
		"head_snapshot": b.run.HeadSnapshot,
		"policy_digest": b.run.PolicyDigest,
		"options":       options,
	})
}

func (b *dagBuilder) minimumEvidence(check string, fallback domain.ClaimResult) domain.ClaimResult {
	return domain.MaxClaimResult(fallback, b.policy.MinimumLevel(check))
}

// fuzzAllocation divides the run fuzz budget across every fuzz step so the
// declared budget is not multiplied by the number of contracts.
func (b *dagBuilder) fuzzAllocation(steps, fallbackTimeout int) (seconds, timeout int, maxCases int64) {
	if steps < 1 {
		steps = 1
	}
	seconds = 30
	timeout = fallbackTimeout
	if total := b.policy.Spec.Budgets.FuzzCPUSeconds; total > 0 {
		seconds = int(total) / steps
		if seconds < 1 {
			seconds = 1
		}
		if seconds > 600 {
			seconds = 600
		}
		timeout = seconds + 120
		if timeout > fallbackTimeout {
			timeout = fallbackTimeout
		}
	}
	if totalCases := b.policy.Spec.Budgets.MaxFuzzCases; totalCases > 0 {
		maxCases = totalCases / int64(steps)
		if maxCases < 1 {
			maxCases = 1
		}
	}
	return seconds, timeout, maxCases
}

func (b *dagBuilder) engineFor(slot, fallbackEngine, fallbackLanguage string) (string, string) {
	if configured, ok := b.policy.Spec.Verification.Engines[slot]; ok {
		language := configured.Language
		if language == "" {
			language = fallbackLanguage
		}
		return configured.Name, language
	}
	return fallbackEngine, fallbackLanguage
}

func claimLanguage(language string) string {
	if strings.TrimSpace(language) == "" {
		return "go"
	}
	return language
}

func policyCheckRequired(decision ports.PolicyDecision, check string) bool {
	check = domain.NormalizeCheckName(check)
	for _, required := range decision.RequiredChecks {
		if domain.NormalizeCheckName(required) == check {
			return true
		}
	}
	return false
}

func contractTitle(contract domain.ContractSpec) string {
	if strings.TrimSpace(contract.Title) != "" {
		return contract.Title
	}
	return "Behavior contract " + contract.ID
}

func maxAttemptsFor(kind domain.StepKind) int {
	switch kind {
	case domain.StepKindDifferential, domain.StepKindRegression, domain.StepKindCompile,
		domain.StepKindAnalyze, domain.StepKindImpact:
		return 2
	default:
		return 3
	}
}

func shardFor(runID domain.RunID, key string, shardCount int) int {
	if shardCount <= 0 {
		return 0
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(runID.String()))
	_, _ = hasher.Write([]byte("/"))
	_, _ = hasher.Write([]byte(key))
	return int(hasher.Sum32() % uint32(shardCount))
}

func slang(value string) string {
	lowered := strings.ToLower(value)
	var builder strings.Builder
	for _, r := range lowered {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	out := strings.Trim(builder.String(), "-")
	if out == "" {
		return "contract"
	}
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

func ensureDependencies(steps []domain.Step, deps map[domain.StepID][]domain.StepID) {
	known := make(map[domain.StepID]struct{}, len(steps))
	for _, step := range steps {
		known[step.ID] = struct{}{}
	}
	for stepID, dependencies := range deps {
		if _, ok := known[stepID]; !ok {
			delete(deps, stepID)
			continue
		}
		filtered := dependencies[:0]
		for _, dependency := range dependencies {
			if _, ok := known[dependency]; ok {
				filtered = append(filtered, dependency)
			}
		}
		deps[stepID] = filtered
	}
}
