package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const PolicyAPIVersion = "patchproof.io/v1"

type PolicyDocument struct {
	APIVersion string         `yaml:"apiVersion" json:"apiVersion"`
	Kind       string         `yaml:"kind" json:"kind"`
	Metadata   PolicyMetadata `yaml:"metadata" json:"metadata"`
	Spec       PolicySpec     `yaml:"spec" json:"spec"`
}

type PolicySpec struct {
	TrustedSource  TrustedSource    `yaml:"trustedSource" json:"trustedSource"`
	Sources        SourceLimits     `yaml:"sources" json:"sources"`
	Verification   VerificationSpec `yaml:"verification" json:"verification"`
	Execution      ExecutionSpec    `yaml:"execution" json:"execution"`
	Budgets        PolicyBudgets    `yaml:"budgets" json:"budgets"`
	ProtectedPaths []string         `yaml:"protectedPaths" json:"protectedPaths"`
	Report         ReportSpec       `yaml:"report" json:"report"`
}

type PolicyMetadata struct {
	Name        string            `yaml:"name" json:"name"`
	Labels      map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty" json:"annotations,omitempty"`
}

type TrustedSource struct {
	Type                  string `yaml:"type" json:"type"`
	RequireSignedRevision bool   `yaml:"requireSignedRevision" json:"requireSignedRevision"`
}

type SourceLimits struct {
	MaxRepositorySizeGiB             float64 `yaml:"maxRepositorySizeGiB" json:"maxRepositorySizeGiB"`
	MaxChangedFiles                  int     `yaml:"maxChangedFiles" json:"maxChangedFiles"`
	MaxFileSizeMiB                   int     `yaml:"maxFileSizeMiB" json:"maxFileSizeMiB"`
	DisallowSubmodulesUnlessApproved bool    `yaml:"disallowSubmodulesUnlessApproved" json:"disallowSubmodulesUnlessApproved"`
}

type VerificationSpec struct {
	Required              []string              `yaml:"required" json:"required"`
	Optional              []string              `yaml:"optional" json:"optional"`
	AllowInconclusiveOn   []string              `yaml:"allowInconclusiveOn" json:"allowInconclusiveOn"`
	MinimumEvidenceLevels map[string]string     `yaml:"minimumEvidenceLevels" json:"minimumEvidenceLevels"`
	Engines               map[string]EngineSpec `yaml:"engines,omitempty" json:"engines,omitempty"`
	Contracts             []ContractSpec        `yaml:"contracts,omitempty" json:"contracts,omitempty"`
	Execution             ExecutionSpec         `yaml:"execution" json:"execution"`
	Budgets               PolicyBudgets         `yaml:"budgets" json:"budgets"`
}

type EngineSpec struct {
	Name     string `yaml:"name" json:"name"`
	Language string `yaml:"language" json:"language"`
}

type ContractSpec struct {
	ID            string   `yaml:"id" json:"id"`
	Symbol        string   `yaml:"symbol" json:"symbol"`
	Language      string   `yaml:"language" json:"language"`
	Kind          string   `yaml:"kind" json:"kind"`
	Title         string   `yaml:"title,omitempty" json:"title,omitempty"`
	Domain        string   `yaml:"domain,omitempty" json:"domain,omitempty"`
	DomainRef     string   `yaml:"domainRef,omitempty" json:"domainRef,omitempty"`
	Observable    string   `yaml:"observable,omitempty" json:"observable,omitempty"`
	Oracle        string   `yaml:"oracle,omitempty" json:"oracle,omitempty"`
	OracleRef     string   `yaml:"oracleRef,omitempty" json:"oracleRef,omitempty"`
	InputSchema   string   `yaml:"inputSchema,omitempty" json:"inputSchema,omitempty"`
	Assumptions   []string `yaml:"assumptions,omitempty" json:"assumptions,omitempty"`
	Required      bool     `yaml:"required" json:"required"`
	MinimumResult string   `yaml:"minimumResult,omitempty" json:"minimumResult,omitempty"`
}

type ExecutionSpec struct {
	IsolationTier      IsolationTier     `yaml:"isolationTier" json:"isolationTier"`
	AllowedRegions     []string          `yaml:"allowedRegions" json:"allowedRegions"`
	DefaultCPUMilli    int64             `yaml:"defaultCpuMilli" json:"defaultCpuMilli"`
	DefaultMemoryMiB   int64             `yaml:"defaultMemoryMiB" json:"defaultMemoryMiB"`
	DefaultDiskGiB     int64             `yaml:"defaultDiskGiB" json:"defaultDiskGiB"`
	MaxStepWallSeconds int               `yaml:"maxStepWallSeconds" json:"maxStepWallSeconds"`
	MaxRunWallSeconds  int               `yaml:"maxRunWallSeconds" json:"maxRunWallSeconds"`
	SubjectNetwork     NetworkMode       `yaml:"subjectNetwork" json:"subjectNetwork"`
	ArtifactTransport  ArtifactTransport `yaml:"artifactTransport" json:"artifactTransport"`
	WorkerImage        string            `yaml:"workerImage" json:"workerImage"`
	RuntimeClass       string            `yaml:"runtimeClass,omitempty" json:"runtimeClass,omitempty"`
	AllowNetworkEgress bool              `yaml:"allowNetworkEgress" json:"allowNetworkEgress"`
}

type PolicyBudgets struct {
	TotalCPUSeconds  float64 `yaml:"totalCpuSeconds" json:"totalCpuSeconds"`
	FuzzCPUSeconds   float64 `yaml:"fuzzCpuSeconds" json:"fuzzCpuSeconds"`
	MaxParallelSteps int     `yaml:"maxParallelSteps" json:"maxParallelSteps"`
	MaxStepCount     int     `yaml:"maxStepCount" json:"maxStepCount"`
	MaxFuzzCases     int64   `yaml:"maxFuzzCases" json:"maxFuzzCases"`
}

type ReportSpec struct {
	Formats                []string `yaml:"formats" json:"formats"`
	RequireAttestation     bool     `yaml:"requireAttestation" json:"requireAttestation"`
	IncludeCounterexamples bool     `yaml:"includeCounterexamples" json:"includeCounterexamples"`
}

var knownChecks = map[string]StepKind{
	"compile":           StepKindCompile,
	"regression-test":   StepKindRegression,
	"differential-fuzz": StepKindDifferential,
	"bounded-smt":       StepKindBoundedSMT,
	"protected-assets":  StepKindPolicy,
	"security-scan":     StepKindSecurity,
}

func KnownChecks() []string {
	out := make([]string, 0, len(knownChecks))
	for name := range knownChecks {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func CheckStepKind(name string) (StepKind, bool) {
	kind, ok := knownChecks[NormalizeCheckName(name)]
	return kind, ok
}

func ParsePolicyDocument(raw []byte) (PolicyDocument, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var doc PolicyDocument
	if err := dec.Decode(&doc); err != nil {
		return PolicyDocument{}, &ValidationError{Field: "policy", Reason: err.Error()}
	}
	if err := doc.Validate(); err != nil {
		return PolicyDocument{}, err
	}
	return doc, nil
}

func (d PolicyDocument) Validate() error {
	if d.APIVersion != PolicyAPIVersion {
		return &ValidationError{Field: "policy.apiVersion", Reason: fmt.Sprintf("must be %s", PolicyAPIVersion)}
	}
	if d.Kind != "VerificationPolicy" {
		return &ValidationError{Field: "policy.kind", Reason: "must be VerificationPolicy"}
	}
	if err := RequireNonEmpty("policy.metadata.name", d.Metadata.Name); err != nil {
		return err
	}
	if len(d.Spec.Verification.Required) == 0 {
		return &ValidationError{Field: "policy.spec.verification.required", Reason: "at least one check is required"}
	}
	seen := map[string]struct{}{}
	for _, check := range d.Spec.Verification.Required {
		name := NormalizeCheckName(check)
		if _, ok := knownChecks[name]; !ok {
			return &ValidationError{Field: "policy.spec.verification.required", Reason: fmt.Sprintf("unknown check %q", check)}
		}
		if _, dup := seen[name]; dup {
			return &ValidationError{Field: "policy.spec.verification.required", Reason: fmt.Sprintf("duplicate check %q", check)}
		}
		seen[name] = struct{}{}
	}
	optional := map[string]struct{}{}
	for _, check := range d.Spec.Verification.Optional {
		name := NormalizeCheckName(check)
		if _, ok := knownChecks[name]; !ok {
			return &ValidationError{Field: "policy.spec.verification.optional", Reason: fmt.Sprintf("unknown check %q", check)}
		}
		if _, dup := seen[name]; dup {
			return &ValidationError{Field: "policy.spec.verification.optional", Reason: fmt.Sprintf("check %q is both required and optional", check)}
		}
		if _, dup := optional[name]; dup {
			return &ValidationError{Field: "policy.spec.verification.optional", Reason: fmt.Sprintf("duplicate check %q", check)}
		}
		optional[name] = struct{}{}
	}
	for _, check := range d.Spec.Verification.AllowInconclusiveOn {
		name := NormalizeCheckName(check)
		if _, ok := optional[name]; !ok {
			return &ValidationError{Field: "policy.spec.verification.allowInconclusiveOn", Reason: fmt.Sprintf("%q is not an optional check", check)}
		}
	}
	for check, level := range d.Spec.Verification.MinimumEvidenceLevels {
		if _, ok := knownChecks[NormalizeCheckName(check)]; !ok {
			return &ValidationError{Field: "policy.spec.verification.minimumEvidenceLevels", Reason: fmt.Sprintf("unknown check %q", check)}
		}
		if !ClaimResult(level).Valid() || (ClaimResult(level) != ClaimTested && ClaimResult(level) != ClaimProven) {
			return &ValidationError{Field: "policy.spec.verification.minimumEvidenceLevels", Reason: fmt.Sprintf("invalid level for %q", check)}
		}
	}
	if err := d.Spec.Execution.Validate(); err != nil {
		return err
	}
	if d.Spec.Budgets.TotalCPUSeconds < 0 || d.Spec.Budgets.FuzzCPUSeconds < 0 || d.Spec.Budgets.MaxParallelSteps < 0 {
		return &ValidationError{Field: "policy.spec.budgets", Reason: "values must not be negative"}
	}
	engineSlots := map[string]struct{}{
		"analyze": {}, "impact": {}, "compile": {}, "regression": {}, "differential": {}, "security": {},
	}
	for slot, engine := range d.Spec.Verification.Engines {
		if _, ok := engineSlots[slot]; !ok {
			return &ValidationError{Field: "policy.spec.verification.engines", Reason: "unknown step slot " + slot}
		}
		if strings.TrimSpace(engine.Name) == "" {
			return &ValidationError{Field: "policy.spec.verification.engines." + slot, Reason: "engine name is required"}
		}
		if strings.TrimSpace(engine.Language) == "" {
			return &ValidationError{Field: "policy.spec.verification.engines." + slot, Reason: "language is required"}
		}
	}
	contractIDs := map[string]struct{}{}
	for i, contract := range d.Spec.Verification.Contracts {
		if !claimIDPattern.MatchString(contract.ID) {
			return &ValidationError{Field: fmt.Sprintf("policy.spec.verification.contracts[%d].id", i), Reason: "must be a stable identifier"}
		}
		if _, dup := contractIDs[contract.ID]; dup {
			return &ValidationError{Field: "policy.spec.verification.contracts", Reason: fmt.Sprintf("duplicate contract %q", contract.ID)}
		}
		contractIDs[contract.ID] = struct{}{}
		if err := RequireNonEmpty("contract.symbol", contract.Symbol); err != nil {
			return err
		}
		hasContractDoc := contract.DomainRef != "" || contract.OracleRef != ""
		if contract.DomainRef == "" && contract.Domain == "" && !hasContractDoc {
			return &ValidationError{Field: "contract.domain", Reason: "domain, domainRef or a contract document reference is required"}
		}
		if contract.OracleRef == "" && contract.Oracle == "" && !hasContractDoc {
			return &ValidationError{Field: "contract.oracle", Reason: "oracle, oracleRef or a contract document reference is required"}
		}
		if contract.MinimumResult != "" && !ClaimResult(contract.MinimumResult).Valid() {
			return &ValidationError{Field: "contract.minimumResult", Reason: "unknown evidence level"}
		}
	}
	for _, pattern := range d.Spec.ProtectedPaths {
		if strings.TrimSpace(pattern) == "" {
			return &ValidationError{Field: "policy.spec.protectedPaths", Reason: "patterns must not be empty"}
		}
	}
	return nil
}

func (e ExecutionSpec) Validate() error {
	if !e.IsolationTier.Valid() {
		return &ValidationError{Field: "policy.spec.execution.isolationTier", Reason: "unknown isolation tier"}
	}
	if e.DefaultCPUMilli <= 0 || e.DefaultMemoryMiB <= 0 {
		return &ValidationError{Field: "policy.spec.execution", Reason: "cpu and memory defaults must be positive"}
	}
	if e.MaxStepWallSeconds < 30 || e.MaxRunWallSeconds < e.MaxStepWallSeconds {
		return &ValidationError{Field: "policy.spec.execution", Reason: "invalid wall clock limits"}
	}
	if !e.SubjectNetwork.Valid() {
		return &ValidationError{Field: "policy.spec.execution.subjectNetwork", Reason: "must be none or restricted"}
	}
	if !e.ArtifactTransport.Valid() {
		return &ValidationError{Field: "policy.spec.execution.artifactTransport", Reason: "unknown artifact transport"}
	}
	if e.ArtifactTransport == TransportBrokeredOffline && e.SubjectNetwork != NetworkNone {
		return &ValidationError{Field: "policy.spec.execution", Reason: "brokered offline IO requires subjectNetwork=none"}
	}
	if e.AllowNetworkEgress && e.SubjectNetwork == NetworkNone {
		return &ValidationError{Field: "policy.spec.execution", Reason: "egress cannot be enabled when subject network is disabled"}
	}
	return nil
}

func (d PolicyDocument) RequiredChecks() []string {
	out := make([]string, 0, len(d.Spec.Verification.Required))
	for _, check := range d.Spec.Verification.Required {
		out = append(out, NormalizeCheckName(check))
	}
	sort.Strings(out)
	return out
}

func (d PolicyDocument) AllChecks() []string {
	out := append([]string{}, d.RequiredChecks()...)
	for _, check := range d.Spec.Verification.Optional {
		out = append(out, NormalizeCheckName(check))
	}
	sort.Strings(out)
	return out
}

func (d PolicyDocument) Digest() (string, error) {
	encoded, err := yaml.Marshal(d)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (d PolicyDocument) MinimumLevel(check string) ClaimResult {
	check = NormalizeCheckName(check)
	if raw, ok := d.Spec.Verification.MinimumEvidenceLevels[check]; ok {
		return ClaimResult(raw)
	}
	if check == "compile" {
		return ClaimTested
	}
	return ClaimTested
}

func (d PolicyDocument) Budget(time.Time) Budget {
	return Budget{
		CPUSeconds:       d.Spec.Budgets.TotalCPUSeconds,
		WallSeconds:      int64(d.Spec.Execution.MaxRunWallSeconds),
		FuzzCPUSeconds:   d.Spec.Budgets.FuzzCPUSeconds,
		MaxParallelSteps: d.Spec.Budgets.MaxParallelSteps,
	}
}
