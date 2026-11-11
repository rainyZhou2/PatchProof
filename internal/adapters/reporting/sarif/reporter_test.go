package sarif

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

type decodedLog struct {
	Version string       `json:"version"`
	Schema  string       `json:"$schema"`
	Runs    []decodedRun `json:"runs"`
}

type decodedRun struct {
	Tool       decodedTool     `json:"tool"`
	Results    []decodedResult `json:"results"`
	Properties map[string]any  `json:"properties,omitempty"`
}

type decodedTool struct {
	Driver decodedDriver `json:"driver"`
}

type decodedDriver struct {
	Name           string        `json:"name"`
	Version        string        `json:"version"`
	InformationURI string        `json:"informationUri"`
	Rules          []decodedRule `json:"rules"`
}

type decodedRule struct {
	ID                   string               `json:"id"`
	Name                 string               `json:"name"`
	ShortDescription     decodedMessage       `json:"shortDescription"`
	DefaultConfiguration decodedConfiguration `json:"defaultConfiguration"`
	Properties           map[string]string    `json:"properties,omitempty"`
}

type decodedConfiguration struct {
	Level string `json:"level"`
}

type decodedMessage struct {
	Text string `json:"text"`
}

type decodedResult struct {
	RuleID     string            `json:"ruleId"`
	Level      string            `json:"level"`
	Message    decodedMessage    `json:"message"`
	Locations  []decodedLocation `json:"locations,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
}

type decodedLocation struct {
	LogicalLocations []decodedLogicalLocation `json:"logicalLocations,omitempty"`
}

type decodedLogicalLocation struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func testReport(t *testing.T) domain.Report {
	t.Helper()
	runID := domain.NewRunID()
	tenantID := domain.NewTenantID()
	claimID := domain.ClaimID("claim:behavior")
	securityClaimID := domain.ClaimID("claim:security")
	digest := strings.Repeat("a", 64)
	return domain.Report{
		SchemaVersion:  domain.ReportSchemaV1,
		ReportID:       domain.NewReportID(),
		RunID:          runID,
		TenantID:       tenantID,
		ProjectID:      domain.NewProjectID(),
		Verdict:        domain.VerdictCounterexample,
		SnapshotDigest: "sha256:" + digest,
		PolicyDigest:   "sha256:" + digest,
		Claims: []domain.ClaimAssessment{{
			ClaimID: claimID, Kind: domain.ClaimKindRegression, Required: true,
			MinimumResult: domain.ClaimTested, Result: domain.ClaimViolated,
		}},
		Findings: []domain.Finding{
			{
				Code: "SECURITY_REGRESSION", Severity: domain.SeverityCritical, ClaimID: securityClaimID,
				Summary: "security regression", Detail: "policy failed",
			},
			{
				Code: "COUNTEREXAMPLE_FOUND", Severity: domain.SeverityHigh, ClaimID: claimID,
				Summary: "claim violated | in scope", Detail: "diff mismatch",
			},
			{
				Code: "INSUFFICIENT_EVIDENCE", Severity: domain.SeverityMedium, ClaimID: claimID,
				Summary: "insufficient evidence",
			},
			{
				Code: "INSUFFICIENT_EVIDENCE", Severity: domain.SeverityMedium, ClaimID: securityClaimID,
				Summary: "insufficient evidence for second claim",
			},
			{
				Code: "COVERAGE_NOTE", Severity: domain.SeverityLow,
				Summary: "low coverage",
			},
			{
				Code: "INFO_NOTE", Severity: domain.SeverityInfo,
				Summary: "informational note",
			},
		},
		Limitations: []string{"scope | bounded"},
		Provenance:  domain.Provenance{Region: "region-a", OrchestratorVersion: "v1.2.3"},
		Manifest:    domain.NewCASRef(digest, 2048),
		MerkleRoot:  "sha256:" + digest,
		Attestation: domain.AttestationSigned,
		GeneratedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
	}
}

func renderLog(t *testing.T, report domain.Report) decodedLog {
	t.Helper()
	rendered, err := New().Render(t.Context(), report, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !json.Valid(rendered) {
		t.Fatalf("rendered SARIF is not valid JSON:\n%s", rendered)
	}
	var document decodedLog
	if err := json.Unmarshal(rendered, &document); err != nil {
		t.Fatalf("unmarshal SARIF: %v", err)
	}
	return document
}

func TestReporterMetadata(t *testing.T) {
	t.Parallel()
	reporter := New()
	if got := reporter.Format(); got != "sarif" {
		t.Fatalf("Format = %q, want sarif", got)
	}
	if got := reporter.ContentType(); got != "application/sarif+json" {
		t.Fatalf("ContentType = %q", got)
	}
}

func TestRenderSARIFDocument(t *testing.T) {
	t.Parallel()
	report := testReport(t)
	document := renderLog(t, report)

	if document.Version != "2.1.0" {
		t.Fatalf("version = %q, want 2.1.0", document.Version)
	}
	if document.Schema != "https://docs.oasis-open.org/sarif/sarif/v2.1.0/errata01/os/schemas/sarif-schema-2.1.0.json" {
		t.Fatalf("$schema = %q", document.Schema)
	}
	if len(document.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(document.Runs))
	}
	run := document.Runs[0]
	if run.Tool.Driver.Name != "patchproof" || run.Tool.Driver.Version != "v1.2.3" {
		t.Fatalf("driver = %+v", run.Tool.Driver)
	}
	if run.Tool.Driver.InformationURI != "https://patchproof.io" {
		t.Fatalf("informationUri = %q", run.Tool.Driver.InformationURI)
	}

	if len(run.Results) != len(report.Findings) {
		t.Fatalf("results = %d, want %d", len(run.Results), len(report.Findings))
	}

	// One rule per unique finding code, sorted by ID.
	wantRuleIDs := []string{
		"COUNTEREXAMPLE_FOUND",
		"COVERAGE_NOTE",
		"INFO_NOTE",
		"INSUFFICIENT_EVIDENCE",
		"SECURITY_REGRESSION",
	}
	gotRuleIDs := make([]string, 0, len(run.Tool.Driver.Rules))
	for _, rule := range run.Tool.Driver.Rules {
		gotRuleIDs = append(gotRuleIDs, rule.ID)
		if rule.Name != rule.ID {
			t.Fatalf("rule name = %q, want %q", rule.Name, rule.ID)
		}
		if rule.ShortDescription.Text == "" {
			t.Fatalf("rule %q has an empty short description", rule.ID)
		}
	}
	if !reflect.DeepEqual(gotRuleIDs, wantRuleIDs) {
		t.Fatalf("rule IDs = %#v, want %#v", gotRuleIDs, wantRuleIDs)
	}
	levels := map[string]string{
		"SECURITY_REGRESSION":   "error",
		"COUNTEREXAMPLE_FOUND":  "error",
		"INSUFFICIENT_EVIDENCE": "warning",
		"COVERAGE_NOTE":         "note",
		"INFO_NOTE":             "note",
	}
	for _, rule := range run.Tool.Driver.Rules {
		if rule.DefaultConfiguration.Level != levels[rule.ID] {
			t.Fatalf("rule %q default level = %q, want %q", rule.ID, rule.DefaultConfiguration.Level, levels[rule.ID])
		}
	}

	if run.Properties["run_id"] != report.RunID.String() {
		t.Fatalf("run properties run_id = %v", run.Properties["run_id"])
	}
	if run.Properties["verdict"] != string(report.Verdict) {
		t.Fatalf("run properties verdict = %v", run.Properties["verdict"])
	}
	if run.Properties["merkle_root"] != report.MerkleRoot {
		t.Fatalf("run properties merkle_root = %v", run.Properties["merkle_root"])
	}
	if run.Properties["attestation"] != string(report.Attestation) {
		t.Fatalf("run properties attestation = %v", run.Properties["attestation"])
	}

	for i, result := range run.Results {
		finding := report.Findings[i]
		if result.RuleID != finding.Code {
			t.Fatalf("result %d ruleId = %q, want %q", i, result.RuleID, finding.Code)
		}
		if result.Level != levels[finding.Code] {
			t.Fatalf("result %d level = %q, want %q", i, result.Level, levels[finding.Code])
		}
		if result.Message.Text == "" {
			t.Fatalf("result %d has an empty message", i)
		}
		if result.Properties["run_id"] != report.RunID.String() || result.Properties["verdict"] != string(report.Verdict) {
			t.Fatalf("result %d properties = %#v", i, result.Properties)
		}
		if result.Properties["severity"] != string(finding.Severity) {
			t.Fatalf("result %d severity = %q, want %q", i, result.Properties["severity"], finding.Severity)
		}
		if finding.ClaimID != "" {
			if result.Properties["claim_id"] != finding.ClaimID.String() {
				t.Fatalf("result %d claim_id = %q, want %q", i, result.Properties["claim_id"], finding.ClaimID)
			}
			if len(result.Locations) != 1 || len(result.Locations[0].LogicalLocations) != 1 {
				t.Fatalf("result %d locations = %#v", i, result.Locations)
			}
			location := result.Locations[0].LogicalLocations[0]
			if location.Name != finding.ClaimID.String() || location.Kind != "claim" {
				t.Fatalf("result %d logical location = %#v", i, location)
			}
		} else {
			if len(result.Locations) != 0 {
				t.Fatalf("result %d unexpectedly has locations: %#v", i, result.Locations)
			}
			if _, ok := result.Properties["claim_id"]; ok {
				t.Fatalf("result %d unexpectedly has claim_id property", i)
			}
		}
		if finding.Detail != "" {
			if !strings.Contains(result.Message.Text, finding.Detail) {
				t.Fatalf("result %d message = %q, want detail %q", i, result.Message.Text, finding.Detail)
			}
		}
	}
}

func TestRenderWithoutFindings(t *testing.T) {
	t.Parallel()
	report := testReport(t)
	report.Findings = nil
	document := renderLog(t, report)
	if len(document.Runs) != 1 {
		t.Fatalf("runs = %d", len(document.Runs))
	}
	if len(document.Runs[0].Results) != 0 {
		t.Fatalf("results = %d, want 0", len(document.Runs[0].Results))
	}
	if len(document.Runs[0].Tool.Driver.Rules) != 0 {
		t.Fatalf("rules = %d, want 0", len(document.Runs[0].Tool.Driver.Rules))
	}
}
