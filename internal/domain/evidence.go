package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	EvidenceSchemaV1 = "patchproof.evidence/v1"
	ReportSchemaV1   = "patchproof.report/v1"
	PolicySchemaV1   = "patchproof.policy/v1"
	IRSchemaV1       = "patchproof.ir/v1"
)

var claimIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,127}$`)

type CASRef struct {
	Algorithm   string `json:"algorithm" yaml:"algorithm"`
	Digest      string `json:"digest" yaml:"digest"`
	SizeBytes   int64  `json:"size_bytes" yaml:"size_bytes"`
	TenantScope string `json:"tenant_scope,omitempty" yaml:"tenant_scope,omitempty"`
}

func (r CASRef) Validate() error {
	if r.Algorithm != "sha256" {
		return &ValidationError{Field: "cas.algorithm", Reason: "only sha256 is supported"}
	}
	d := r.Digest
	if strings.HasPrefix(d, "sha256:") {
		d = strings.TrimPrefix(d, "sha256:")
	}
	if len(d) != 64 {
		return &ValidationError{Field: "cas.digest", Reason: "must be a 64 character sha256 hex digest"}
	}
	for _, c := range d {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return &ValidationError{Field: "cas.digest", Reason: "must be lowercase hexadecimal"}
		}
	}
	if r.SizeBytes < 0 {
		return &ValidationError{Field: "cas.size_bytes", Reason: "must not be negative"}
	}
	return nil
}

func (r CASRef) Hex() string {
	return strings.TrimPrefix(r.Digest, "sha256:")
}

func (r CASRef) URI() string {
	return "cas://sha256/" + r.Hex()
}

func (r CASRef) String() string { return r.URI() }

func NewCASRef(digest string, size int64) CASRef {
	return CASRef{Algorithm: "sha256", Digest: strings.TrimPrefix(digest, "sha256:"), SizeBytes: size}
}

func ParseCASURI(uri string) (CASRef, error) {
	const prefix = "cas://"
	if !strings.HasPrefix(uri, prefix) {
		return CASRef{}, &ValidationError{Field: "cas_uri", Reason: "must start with cas://"}
	}
	rest := strings.TrimPrefix(uri, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return CASRef{}, &ValidationError{Field: "cas_uri", Reason: "must be cas://sha256/<digest>"}
	}
	ref := CASRef{Algorithm: parts[0], Digest: parts[1]}
	if err := ref.Validate(); err != nil {
		return CASRef{}, err
	}
	return ref, nil
}

type Claim struct {
	ID            ClaimID     `json:"claim_id"`
	RunID         RunID       `json:"run_id"`
	TenantID      TenantID    `json:"tenant_id"`
	ContractID    string      `json:"contract_id"`
	Kind          ClaimKind   `json:"kind"`
	Title         string      `json:"title"`
	Predicate     string      `json:"predicate"`
	Domain        string      `json:"domain"`
	Assumptions   []string    `json:"assumptions,omitempty"`
	Observable    string      `json:"observable"`
	OracleRef     string      `json:"oracle_ref,omitempty"`
	InputSchema   string      `json:"input_schema,omitempty"`
	Language      string      `json:"language"`
	Symbol        string      `json:"symbol,omitempty"`
	Required      bool        `json:"required"`
	MinimumResult ClaimResult `json:"minimum_result"`
	CreatedAt     time.Time   `json:"created_at"`
}

func (c Claim) Validate() error {
	if !claimIDPattern.MatchString(string(c.ID)) {
		return &ValidationError{Field: "claim_id", Reason: "must be a stable identifier of at most 128 characters"}
	}
	if err := c.RunID.Validate(); err != nil {
		return err
	}
	if err := c.TenantID.Validate(); err != nil {
		return err
	}
	if !c.Kind.Valid() {
		return &ValidationError{Field: "claim.kind", Reason: "unknown claim kind"}
	}
	if err := RequireNonEmpty("claim.title", c.Title); err != nil {
		return err
	}
	if err := RequireNonEmpty("claim.domain", c.Domain); err != nil {
		return err
	}
	if !c.MinimumResult.Valid() {
		return &ValidationError{Field: "claim.minimum_result", Reason: "unknown minimum result"}
	}
	if c.MinimumResult == ClaimViolated || c.MinimumResult == ClaimInvalidEvidence {
		return &ValidationError{Field: "claim.minimum_result", Reason: "must be a positive evidence level"}
	}
	return nil
}

type EngineInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	ImageDigest string `json:"image_digest,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
}

type EnvironmentInfo struct {
	Arch            string            `json:"arch"`
	SandboxProfile  string            `json:"sandbox_profile"`
	ToolchainDigest string            `json:"toolchain_digest,omitempty"`
	OS              string            `json:"os,omitempty"`
	Region          string            `json:"region,omitempty"`
	Extra           map[string]string `json:"extra,omitempty"`
}

type EvidenceScope struct {
	InputDomain    string            `json:"input_domain"`
	SeedCount      int64             `json:"seed_count,omitempty"`
	GeneratedCases int64             `json:"generated_cases,omitempty"`
	ElapsedMS      int64             `json:"elapsed_ms,omitempty"`
	Budget         string            `json:"budget,omitempty"`
	Details        map[string]string `json:"details,omitempty"`
}

type Evidence struct {
	SchemaVersion string          `json:"schema_version"`
	ID            EvidenceID      `json:"evidence_id"`
	TenantID      TenantID        `json:"tenant_id"`
	RunID         RunID           `json:"run_id"`
	StepID        StepID          `json:"step_id,omitempty"`
	ClaimID       ClaimID         `json:"claim_id"`
	Kind          EvidenceKind    `json:"kind"`
	Result        ClaimResult     `json:"result"`
	Scope         EvidenceScope   `json:"scope"`
	Assumptions   []string        `json:"assumptions,omitempty"`
	Engine        EngineInfo      `json:"engine"`
	Environment   EnvironmentInfo `json:"environment"`
	Inputs        []CASRef        `json:"inputs,omitempty"`
	Artifacts     []CASRef        `json:"artifacts,omitempty"`
	Limits        []string        `json:"limits,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

func (e Evidence) Validate() error {
	if e.SchemaVersion != EvidenceSchemaV1 {
		return &ValidationError{Field: "evidence.schema_version", Reason: fmt.Sprintf("must be %s", EvidenceSchemaV1)}
	}
	if err := e.ID.Validate(); err != nil {
		return err
	}
	if err := e.RunID.Validate(); err != nil {
		return err
	}
	if err := e.TenantID.Validate(); err != nil {
		return err
	}
	if !claimIDPattern.MatchString(string(e.ClaimID)) {
		return &ValidationError{Field: "evidence.claim_id", Reason: "must be a claim identifier"}
	}
	if !e.Kind.Valid() {
		return &ValidationError{Field: "evidence.kind", Reason: "unknown evidence kind"}
	}
	if !e.Result.Valid() {
		return &ValidationError{Field: "evidence.result", Reason: "unknown claim result"}
	}
	if err := RequireNonEmpty("evidence.scope.input_domain", e.Scope.InputDomain); err != nil {
		return err
	}
	if err := RequireNonEmpty("evidence.engine.name", e.Engine.Name); err != nil {
		return err
	}
	if err := RequireNonEmpty("evidence.engine.version", e.Engine.Version); err != nil {
		return err
	}
	if err := RequireNonEmpty("evidence.environment.arch", e.Environment.Arch); err != nil {
		return err
	}
	if err := RequireNonEmpty("evidence.environment.sandbox_profile", e.Environment.SandboxProfile); err != nil {
		return err
	}
	for _, ref := range e.Inputs {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	for _, ref := range e.Artifacts {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	if e.Result == ClaimViolated && len(e.Artifacts) == 0 {
		return &ValidationError{Field: "evidence.artifacts", Reason: "violation evidence requires a counterexample artifact"}
	}
	return nil
}

type Counterexample struct {
	CaseID                string    `json:"case_id"`
	TenantID              TenantID  `json:"tenant_id"`
	RunID                 RunID     `json:"run_id"`
	ClaimID               ClaimID   `json:"claim_id"`
	InputCodec            string    `json:"input_codec"`
	InputRef              CASRef    `json:"input_ref"`
	BaseObservationRef    CASRef    `json:"base_observation_ref"`
	HeadObservationRef    CASRef    `json:"head_observation_ref"`
	ReproducerImageDigest string    `json:"reproducer_image_digest,omitempty"`
	Reproduced            bool      `json:"reproduced"`
	Scope                 string    `json:"scope"`
	Nondeterminism        []string  `json:"nondeterminism,omitempty"`
	Minimized             bool      `json:"minimized"`
	CreatedAt             time.Time `json:"created_at"`
}

func (c Counterexample) Validate() error {
	if err := RequireNonEmpty("case_id", c.CaseID); err != nil {
		return err
	}
	if err := c.RunID.Validate(); err != nil {
		return err
	}
	if err := c.TenantID.Validate(); err != nil {
		return err
	}
	if !claimIDPattern.MatchString(string(c.ClaimID)) {
		return &ValidationError{Field: "counterexample.claim_id", Reason: "must be a claim identifier"}
	}
	if err := c.InputRef.Validate(); err != nil {
		return err
	}
	if err := c.BaseObservationRef.Validate(); err != nil {
		return err
	}
	if err := c.HeadObservationRef.Validate(); err != nil {
		return err
	}
	if err := RequireNonEmpty("counterexample.input_codec", c.InputCodec); err != nil {
		return err
	}
	return nil
}

type ClaimAssessment struct {
	ClaimID       ClaimID      `json:"claim_id"`
	Kind          ClaimKind    `json:"kind"`
	Title         string       `json:"title"`
	Required      bool         `json:"required"`
	MinimumResult ClaimResult  `json:"minimum_result"`
	Result        ClaimResult  `json:"result"`
	EvidenceIDs   []EvidenceID `json:"evidence_ids,omitempty"`
	Scope         string       `json:"scope,omitempty"`
	Reason        string       `json:"reason,omitempty"`
}

type Severity string

const (
	SeverityCritical Severity = "CRITICAL"
	SeverityHigh     Severity = "HIGH"
	SeverityMedium   Severity = "MEDIUM"
	SeverityLow      Severity = "LOW"
	SeverityInfo     Severity = "INFO"
)

type Finding struct {
	Code        string       `json:"code"`
	Severity    Severity     `json:"severity"`
	ClaimID     ClaimID      `json:"claim_id,omitempty"`
	Summary     string       `json:"summary"`
	Detail      string       `json:"detail,omitempty"`
	EvidenceIDs []EvidenceID `json:"evidence_ids,omitempty"`
}

type RunMetrics struct {
	CPUSeconds       float64 `json:"cpu_seconds"`
	MemoryGiBSeconds float64 `json:"memory_gib_seconds"`
	CASWrittenBytes  int64   `json:"cas_written_bytes"`
	EgressBytes      int64   `json:"egress_bytes"`
	WallSeconds      int64   `json:"wall_seconds"`
	StepCount        int     `json:"step_count"`
	RetryCount       int     `json:"retry_count"`
}

type Provenance struct {
	Region              string            `json:"region"`
	OrchestratorVersion string            `json:"orchestrator_version"`
	ToolchainDigest     string            `json:"toolchain_digest,omitempty"`
	EngineVersions      map[string]string `json:"engine_versions,omitempty"`
	SourceRepository    string            `json:"source_repository,omitempty"`
	CreatedBy           string            `json:"created_by,omitempty"`
	PolicySource        PolicySource      `json:"policy_source,omitempty"`
	ExecutedImage       string            `json:"executed_image,omitempty"`
}

type Signature struct {
	ID              SignatureID `json:"signature_id"`
	KeyID           string      `json:"key_id"`
	Algorithm       string      `json:"algorithm"`
	Subject         string      `json:"subject"`
	Signature       []byte      `json:"signature"`
	SignedAt        time.Time   `json:"signed_at"`
	Certificate     string      `json:"certificate,omitempty"`
	TransparencyLog string      `json:"transparency_log,omitempty"`
}

type Report struct {
	SchemaVersion       string            `json:"schema_version"`
	ReportID            ReportID          `json:"report_id"`
	RunID               RunID             `json:"run_id"`
	TenantID            TenantID          `json:"tenant_id"`
	ProjectID           ProjectID         `json:"project_id"`
	Verdict             Verdict           `json:"verdict"`
	BaseCommit          string            `json:"base_commit"`
	HeadCommit          string            `json:"head_commit"`
	SnapshotDigest      string            `json:"snapshot_digest"`
	BaseSnapshot        string            `json:"base_snapshot_digest,omitempty"`
	HeadSnapshot        string            `json:"head_snapshot_digest,omitempty"`
	PolicyDigest        string            `json:"policy_digest"`
	RequestedChecks     []string          `json:"requested_checks"`
	CompletedChecks     []string          `json:"completed_checks"`
	AllowInconclusiveOn []string          `json:"allow_inconclusive_on,omitempty"`
	Claims              []ClaimAssessment `json:"claims"`
	Counterexamples     []Counterexample  `json:"counterexamples,omitempty"`
	Findings            []Finding         `json:"findings,omitempty"`
	Assumptions         []string          `json:"assumptions,omitempty"`
	Limitations         []string          `json:"limitations,omitempty"`
	Metrics             RunMetrics        `json:"metrics"`
	Provenance          Provenance        `json:"provenance"`
	Manifest            CASRef            `json:"manifest"`
	MerkleRoot          string            `json:"merkle_root"`
	Signatures          []Signature       `json:"signatures,omitempty"`
	Attestation         AttestationStatus `json:"attestation_status"`
	GeneratedAt         time.Time         `json:"generated_at"`
}

func (r Report) Validate() error {
	if r.SchemaVersion != ReportSchemaV1 {
		return &ValidationError{Field: "report.schema_version", Reason: fmt.Sprintf("must be %s", ReportSchemaV1)}
	}
	if err := r.ReportID.Validate(); err != nil {
		return err
	}
	if err := r.RunID.Validate(); err != nil {
		return err
	}
	if err := r.TenantID.Validate(); err != nil {
		return err
	}
	if !r.Verdict.Valid() {
		return &ValidationError{Field: "report.verdict", Reason: "unknown verdict"}
	}
	if !r.Attestation.Valid() {
		return &ValidationError{Field: "report.attestation", Reason: "unknown attestation status"}
	}
	if err := r.Manifest.Validate(); err != nil {
		return err
	}
	if err := RequireNonEmpty("report.merkle_root", r.MerkleRoot); err != nil {
		return err
	}
	if len(r.Claims) == 0 {
		return &ValidationError{Field: "report.claims", Reason: "must not be empty"}
	}
	for _, c := range r.Claims {
		if !c.Result.Valid() {
			return &ValidationError{Field: "report.claim.result", Reason: "unknown claim result"}
		}
	}
	if r.Attestation == AttestationSigned && len(r.Signatures) == 0 {
		return &ValidationError{Field: "report.signatures", Reason: "signed reports require at least one signature"}
	}
	return nil
}
