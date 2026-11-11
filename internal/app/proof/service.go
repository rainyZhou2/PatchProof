package proof

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/telemetry"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/evidence"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

type Config struct {
	Region             string
	Version            string
	RequireAttestation bool
	Clock              ports.Clock
	Logger             *slog.Logger
}

type Service struct {
	store   ports.MetadataStore
	cas     ports.ArtifactStore
	signer  ports.Signer
	clock   ports.Clock
	config  Config
	logger  *slog.Logger
	metrics *telemetry.Metrics
}

func New(store ports.MetadataStore, cas ports.ArtifactStore, signer ports.Signer, config Config, metrics *telemetry.Metrics, logger *slog.Logger) (*Service, error) {
	if store == nil || cas == nil {
		return nil, fmt.Errorf("proof service: metadata store and artifact store are required")
	}
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	if config.Version == "" {
		config.Version = "dev"
	}
	if logger == nil {
		logger = slog.Default()
	}
	if signer == nil && config.RequireAttestation {
		return nil, fmt.Errorf("proof service: attestation is required but no signer is configured")
	}
	return &Service{store: store, cas: cas, signer: signer, clock: config.Clock, config: config, logger: logger, metrics: metrics}, nil
}

func (s *Service) Aggregate(ctx context.Context, run domain.Run) (domain.Report, error) {
	dag, err := s.store.GetDAG(ctx, run.TenantID, run.ID)
	if err != nil {
		return domain.Report{}, err
	}
	allEvidence, err := s.store.ListEvidence(ctx, run.TenantID, run.ID)
	if err != nil {
		return domain.Report{}, err
	}
	counterexamples, err := s.store.ListCounterexamples(ctx, run.TenantID, run.ID)
	if err != nil {
		return domain.Report{}, err
	}
	policyDocument, policyErr := s.loadPolicy(ctx, run)
	if policyErr != nil {
		s.logger.Warn("policy document unavailable during aggregation", slog.String("run_id", run.ID.String()), slog.String("error", policyErr.Error()))
	}

	validEvidence := make([]domain.Evidence, 0, len(allEvidence))
	invalidCount := 0
	for _, item := range allEvidence {
		if err := item.Validate(); err != nil {
			invalidCount++
			s.logger.Warn("invalid evidence excluded", slog.String("evidence_id", item.ID.String()), slog.String("error", err.Error()))
			continue
		}
		validEvidence = append(validEvidence, item)
	}
	policyBlocked := detectPolicyBlock(dag.Claims, validEvidence)
	infrastructureFailed := runFailureIsInfra(run) || stepFailureIsInfra(dag.Steps)
	verdict := domain.ComputeVerdict(domain.VerdictInput{
		Claims:               dag.Claims,
		Evidence:             validEvidence,
		Counterexamples:      counterexamples,
		PolicyBlocked:        policyBlocked,
		PolicyReason:         "the patch modifies protected assets or violates a trusted policy invariant",
		InfrastructureFailed: infrastructureFailed,
		Cancelled:            run.Status == domain.RunCancelled || run.Status == domain.RunCancelling,
	})
	verdict = downgradePartialClaimEvidence(dag, verdict)
	if invalidCount > 0 {
		verdict.Verdict = domain.VerdictInconclusive
		verdict.Findings = append(verdict.Findings, domain.Finding{
			Code:     "INVALID_EVIDENCE",
			Severity: domain.SeverityHigh,
			Summary:  "some evidence documents failed validation and were excluded",
			Detail:   fmt.Sprintf("%d evidence documents were invalid", invalidCount),
		})
	}

	report, err := s.buildReport(ctx, run, dag, validEvidence, counterexamples, verdict, policyDocument)
	if err != nil {
		return domain.Report{}, err
	}
	if err := s.store.PutReport(ctx, report); err != nil {
		return domain.Report{}, fmt.Errorf("store report: %w", err)
	}
	if err := s.emitReportEvent(ctx, report); err != nil {
		s.logger.Warn("report event enqueue failed", slog.String("run_id", run.ID.String()), slog.String("error", err.Error()))
	}
	if s.metrics != nil {
		s.metrics.RunVerdicts.WithLabelValues(s.region(run), string(report.Verdict)).Inc()
	}
	return report, nil
}

func (s *Service) BuildRejectedReport(ctx context.Context, run domain.Run, reason string) (domain.Report, error) {
	return s.BuildRejectionReport(ctx, run, domain.VerdictPolicyBlocked, "POLICY_BLOCKED", "run was rejected by trusted verification policy", reason)
}

func (s *Service) BuildRejectionReport(ctx context.Context, run domain.Run, verdict domain.Verdict, code, summary, reason string) (domain.Report, error) {
	policyDocument, _ := s.loadPolicy(ctx, run)
	claim := domain.ClaimAssessment{
		ClaimID:       "policy-admission",
		Kind:          domain.ClaimKindSecurity,
		Title:         "Trusted verification policy admitted the run",
		Required:      true,
		MinimumResult: domain.ClaimTested,
		Result:        domain.ClaimViolated,
		Reason:        reason,
		Scope:         "run admission",
	}
	if verdict != domain.VerdictPolicyBlocked {
		claim.Result = domain.ClaimUnknown
	}
	claims := []domain.ClaimAssessment{claim}
	result := domain.VerdictResult{
		Verdict: verdict,
		Claims:  claims,
		Findings: []domain.Finding{{
			Code:     code,
			Severity: domain.SeverityHigh,
			Summary:  summary,
			Detail:   reason,
		}},
	}
	report, err := s.buildReport(ctx, run, domain.DAG{RunID: run.ID, Claims: nil, Steps: nil}, nil, nil, result, policyDocument)
	if err != nil {
		return domain.Report{}, err
	}
	if err := s.store.PutReport(ctx, report); err != nil {
		return domain.Report{}, err
	}
	return report, nil
}

func (s *Service) buildReport(ctx context.Context, run domain.Run, dag domain.DAG, evidenceList []domain.Evidence, counterexamples []domain.Counterexample, verdict domain.VerdictResult, policy *domain.PolicyDocument) (domain.Report, error) {
	now := s.clock.Now()
	completed := completedChecks(dag.Steps)
	for _, claim := range verdict.Claims {
		if claim.ClaimID == "protected-assets" && claim.Result != domain.ClaimNotRun && claim.Result != domain.ClaimUnknown {
			completed = unionStrings(completed, []string{"protected-assets"})
		}
	}
	requested := append([]string{}, run.RequestedChecks...)
	if policy != nil {
		requested = unionStrings(requested, policy.RequiredChecks())
	}
	report := domain.Report{
		SchemaVersion:   domain.ReportSchemaV1,
		ReportID:        domain.NewReportID(),
		RunID:           run.ID,
		TenantID:        run.TenantID,
		ProjectID:       run.ProjectID,
		Verdict:         verdict.Verdict,
		BaseCommit:      run.BaseCommit,
		HeadCommit:      run.HeadCommit,
		SnapshotDigest:  run.SnapshotDigest,
		BaseSnapshot:    run.BaseSnapshot,
		HeadSnapshot:    run.HeadSnapshot,
		PolicyDigest:    run.PolicyDigest,
		RequestedChecks: requested,
		CompletedChecks: completed,
		Claims:          verdict.Claims,
		Counterexamples: counterexamples,
		Findings:        verdict.Findings,
		Assumptions:     collectAssumptions(dag.Claims),
		Limitations:     s.collectLimitations(dag, counterexamples, verdict, policy),
		Metrics:         reportMetrics(run, dag, now),
		Provenance:      s.provenance(run, evidenceList, policy),
		Attestation:     domain.AttestationPending,
		GeneratedAt:     now,
	}
	if s.cas != nil {
		manifestRef, root, err := s.persistBundle(ctx, run, report.Claims, evidenceList, counterexamples)
		if err != nil {
			return domain.Report{}, err
		}
		report.Manifest = manifestRef
		report.MerkleRoot = root
	}
	if s.signer != nil {
		signed, err := evidence.SignReport(ctx, report, s.signer)
		if err != nil {
			report.Attestation = domain.AttestationPending
			report.Findings = append(report.Findings, domain.Finding{
				Code: "ATTESTATION_PENDING", Severity: domain.SeverityHigh,
				Summary: "report could not be attested", Detail: err.Error(),
			})
		} else {
			report = signed
		}
	}
	if s.config.RequireAttestation && report.Attestation != domain.AttestationSigned {
		report.Findings = append(report.Findings, domain.Finding{
			Code: "ATTESTATION_REQUIRED", Severity: domain.SeverityHigh,
			Summary: "policy requires a signed report but no trustworthy signature was produced",
		})
	}
	if report.MerkleRoot == "" {
		return domain.Report{}, fmt.Errorf("proof: report has no merkle root")
	}
	if err := report.Validate(); err != nil {
		return domain.Report{}, err
	}
	return report, nil
}

func (s *Service) persistBundle(ctx context.Context, run domain.Run, claims []domain.ClaimAssessment, evidenceList []domain.Evidence, counterexamples []domain.Counterexample) (domain.CASRef, string, error) {
	scope := ports.SecurityScope{TenantID: run.TenantID, TrustDomain: "report", Region: s.region(run)}
	if err := scope.Validate(); err != nil {
		return domain.CASRef{}, "", err
	}
	leaves := make([]evidence.Leaf, 0, len(evidenceList)+len(counterexamples)+4)
	put := func(path string, value any) error {
		ref, err := s.putJSON(ctx, scope, value)
		if err != nil {
			return err
		}
		leaves = append(leaves, evidence.Leaf{Path: path, Digest: ref.Digest, Size: ref.SizeBytes, MediaType: "application/json"})
		return nil
	}
	attestation := evidence.RunAttestation{
		RunID:           run.ID.String(),
		TenantID:        run.TenantID.String(),
		ProjectID:       run.ProjectID.String(),
		BaseCommit:      run.BaseCommit,
		HeadCommit:      run.HeadCommit,
		SnapshotDigest:  run.SnapshotDigest,
		PolicyDigest:    run.PolicyDigest,
		RequestedChecks: append([]string{}, run.RequestedChecks...),
	}
	if err := put("run.json", attestation); err != nil {
		return domain.CASRef{}, "", err
	}
	if err := put("claims.json", claims); err != nil {
		return domain.CASRef{}, "", err
	}
	for _, item := range evidenceList {
		if err := put("evidence/"+item.ID.String()+".json", item); err != nil {
			return domain.CASRef{}, "", err
		}
	}
	for _, counterexample := range counterexamples {
		if err := put("counterexamples/"+counterexample.CaseID+".json", counterexample); err != nil {
			return domain.CASRef{}, "", err
		}
	}
	manifest, err := evidence.BuildManifest(leaves)
	if err != nil {
		return domain.CASRef{}, "", err
	}
	manifestRef, err := s.putJSON(ctx, scope, manifest)
	if err != nil {
		return domain.CASRef{}, "", err
	}
	return manifestRef, manifest.Root, nil
}

func (s *Service) putJSON(ctx context.Context, scope ports.SecurityScope, value any) (domain.CASRef, error) {
	payload, err := evidence.Canonicalize(value)
	if err != nil {
		return domain.CASRef{}, err
	}
	return s.cas.Put(ctx, scope, strings.NewReader(string(payload)))
}

func (s *Service) emitReportEvent(ctx context.Context, report domain.Report) error {
	payload := map[string]any{
		"report_id":   report.ReportID.String(),
		"run_id":      report.RunID.String(),
		"verdict":     string(report.Verdict),
		"manifest":    report.Manifest.URI(),
		"merkle":      report.MerkleRoot,
		"attestation": string(report.Attestation),
	}
	event, err := domain.NewOutboxEvent(report.TenantID, domain.EventReportReady, report.RunID.String(), 0, payload, s.clock.Now())
	if err != nil {
		return err
	}
	return s.store.AppendOutbox(ctx, []domain.OutboxEvent{event})
}

// downgradePartialClaimEvidence prevents a single successful step from
// satisfying a claim that is shared by several steps when another step for the
// same claim failed permanently. Compile base/head and multi-contract native
// fuzzing use shared claims; without this guard, a partial execution could be
// reported as VERIFIED_WITHIN_SCOPE.
func downgradePartialClaimEvidence(dag domain.DAG, verdict domain.VerdictResult) domain.VerdictResult {
	failed := make(map[domain.ClaimID]struct{})
	for _, step := range dag.Steps {
		switch step.Status {
		case domain.StepFailed, domain.StepTimedOut, domain.StepCancelled:
		default:
			continue
		}
		claimID := domain.ClaimID(stepopts.Get(step.Options, stepopts.ClaimID))
		if claimID != "" {
			failed[claimID] = struct{}{}
		}
	}
	if len(failed) == 0 {
		return verdict
	}
	downgraded := false
	for i := range verdict.Claims {
		claim := &verdict.Claims[i]
		if !claim.Required {
			continue
		}
		if _, ok := failed[claim.ClaimID]; !ok {
			continue
		}
		if claim.Result != domain.ClaimTested && claim.Result != domain.ClaimProven {
			continue
		}
		claim.Result = domain.ClaimUnknown
		claim.Reason = "one or more steps for this claim failed permanently without contributing evidence"
		verdict.Findings = append(verdict.Findings, domain.Finding{
			Code: "CLAIM_STEP_FAILED", Severity: domain.SeverityHigh, ClaimID: claim.ClaimID,
			Summary: "claim has positive evidence from only part of its planned execution",
			Detail:  "at least one step sharing this claim failed or timed out",
		})
		downgraded = true
	}
	if downgraded && verdict.Verdict == domain.VerdictVerified {
		verdict.Verdict = domain.VerdictInconclusive
	}
	return verdict
}

func (s *Service) loadPolicy(ctx context.Context, run domain.Run) (*domain.PolicyDocument, error) {
	if run.PolicyDigest == "" {
		return nil, fmt.Errorf("run has no policy digest")
	}
	stored, err := s.store.GetPolicy(ctx, run.TenantID, run.PolicyDigest)
	if err != nil {
		return nil, err
	}
	document, err := domain.ParsePolicyDocument(stored.Document)
	if err != nil {
		return nil, err
	}
	return &document, nil
}

func (s *Service) provenance(run domain.Run, evidenceList []domain.Evidence, policy *domain.PolicyDocument) domain.Provenance {
	engines := map[string]string{}
	for _, item := range evidenceList {
		if item.Engine.Name != "" {
			engines[item.Engine.Name] = item.Engine.Version
		}
	}
	provenance := domain.Provenance{
		Region:              s.region(run),
		OrchestratorVersion: s.config.Version,
		EngineVersions:      engines,
		CreatedBy:           run.CreatedBy,
	}
	return provenance
}

func (s *Service) collectLimitations(dag domain.DAG, counterexamples []domain.Counterexample, verdict domain.VerdictResult, policy *domain.PolicyDocument) []string {
	limitations := []string{
		"Verification is bounded by the declared claim domains, budgets, and executed engine versions.",
		"A TESTED_WITHIN_SCOPE result is not a formal proof of program correctness.",
	}
	blocked := 0
	for _, step := range dag.Steps {
		if step.Status == domain.StepBlocked {
			blocked++
		}
	}
	if blocked > 0 {
		limitations = append(limitations, fmt.Sprintf("%d dependent steps did not run because an upstream check failed", blocked))
	}
	unreproduced := 0
	for _, counterexample := range counterexamples {
		if !counterexample.Reproduced {
			unreproduced++
		}
	}
	if unreproduced > 0 {
		limitations = append(limitations, fmt.Sprintf("%d counterexamples were not reproduced in a fresh process", unreproduced))
	}
	if policy != nil && len(policy.Spec.Verification.AllowInconclusiveOn) > 0 {
		limitations = append(limitations, "Optional checks may complete as INCONCLUSIVE per policy: "+strings.Join(policy.Spec.Verification.AllowInconclusiveOn, ", "))
	}
	if verdict.Verdict == domain.VerdictVerified {
		limitations = append(limitations, "No concurrency, clock, network, or foreign-system behavior is modeled unless a claim explicitly states it.")
	}
	sort.Strings(limitations)
	return limitations
}

func completedChecks(steps []domain.Step) []string {
	set := map[string]struct{}{}
	for _, step := range steps {
		if step.Status != domain.StepSucceeded {
			continue
		}
		if check := stepopts.Get(step.Options, stepopts.CheckName); check != "" {
			set[domain.NormalizeCheckName(check)] = struct{}{}
			continue
		}
		switch step.Kind {
		case domain.StepKindCompile:
			set["compile"] = struct{}{}
		case domain.StepKindRegression:
			set["regression-test"] = struct{}{}
		case domain.StepKindDifferential:
			set["differential-fuzz"] = struct{}{}
		case domain.StepKindSecurity:
			set["security-scan"] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func collectAssumptions(claims []domain.Claim) []string {
	set := map[string]struct{}{}
	for _, claim := range claims {
		for _, assumption := range claim.Assumptions {
			set[assumption] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func reportMetrics(run domain.Run, dag domain.DAG, now time.Time) domain.RunMetrics {
	metrics := domain.RunMetrics{StepCount: len(dag.Steps)}
	for _, step := range dag.Steps {
		if step.AttemptCount > 1 {
			metrics.RetryCount += step.AttemptCount - 1
		}
	}
	metrics.WallSeconds = int64(now.Sub(run.CreatedAt).Seconds())
	if metrics.WallSeconds < 0 {
		metrics.WallSeconds = 0
	}
	return metrics
}

func runFailureIsInfra(run domain.Run) bool {
	if run.FailureClass == "" {
		return false
	}
	category := run.FailureClass.Category()
	return category == domain.CategoryInfra || category == domain.CategoryTool
}

func stepFailureIsInfra(steps []domain.Step) bool {
	for _, step := range steps {
		if step.FailureClass == "" {
			continue
		}
		category := step.FailureClass.Category()
		if category == domain.CategoryInfra || category == domain.CategoryTool {
			return true
		}
	}
	return false
}

func detectPolicyBlock(claims []domain.Claim, evidenceList []domain.Evidence) bool {
	securityClaims := map[domain.ClaimID]struct{}{}
	for _, claim := range claims {
		if claim.ID == "protected-assets" || claim.Kind == domain.ClaimKindSecurity {
			securityClaims[claim.ID] = struct{}{}
		}
	}
	for _, item := range evidenceList {
		if item.Kind != domain.EvidencePolicy {
			continue
		}
		if _, ok := securityClaims[item.ClaimID]; ok && item.Result == domain.ClaimViolated {
			return true
		}
	}
	return false
}

func unionStrings(left, right []string) []string {
	set := map[string]struct{}{}
	for _, value := range append(append([]string{}, left...), right...) {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (s *Service) region(run domain.Run) string {
	if run.Region != "" {
		return run.Region
	}
	if s.config.Region != "" {
		return s.config.Region
	}
	return "region-a"
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }
