package sarif

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/patchproof/patchproof/internal/domain"
)

type Reporter struct{}

func New() *Reporter { return &Reporter{} }

func (*Reporter) Format() string      { return "sarif" }
func (*Reporter) ContentType() string { return "application/sarif+json" }

type sarifLog struct {
	Version string     `json:"version"`
	Schema  string     `json:"$schema"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool       sarifTool      `json:"tool"`
	Results    []sarifResult  `json:"results"`
	Properties map[string]any `json:"properties,omitempty"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version"`
	InformationURI string      `json:"informationUri"`
	Rules          []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID                   string             `json:"id"`
	Name                 string             `json:"name,omitempty"`
	ShortDescription     sarifMessage       `json:"shortDescription"`
	DefaultConfiguration sarifConfiguration `json:"defaultConfiguration"`
	Properties           map[string]string  `json:"properties,omitempty"`
}

type sarifConfiguration struct {
	Level string `json:"level"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID     string            `json:"ruleId"`
	Level      string            `json:"level"`
	Message    sarifMessage      `json:"message"`
	Locations  []sarifLocation   `json:"locations,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
}

type sarifLocation struct {
	LogicalLocations []sarifLogicalLocation `json:"logicalLocations,omitempty"`
}

type sarifLogicalLocation struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func (*Reporter) Render(_ context.Context, report domain.Report, _ []domain.Evidence) ([]byte, error) {
	rules := map[string]sarifRule{}
	results := make([]sarifResult, 0, len(report.Findings))
	for _, finding := range report.Findings {
		level := sarifLevel(finding.Severity)
		if _, ok := rules[finding.Code]; !ok {
			rules[finding.Code] = sarifRule{
				ID: finding.Code, Name: finding.Code,
				ShortDescription:     sarifMessage{Text: finding.Summary},
				DefaultConfiguration: sarifConfiguration{Level: level},
			}
		}
		result := sarifResult{
			RuleID: finding.Code, Level: level,
			Message: sarifMessage{Text: finding.Summary + detailSuffix(finding.Detail)},
			Properties: map[string]string{
				"run_id": report.RunID.String(), "verdict": string(report.Verdict), "severity": string(finding.Severity),
			},
		}
		if finding.ClaimID != "" {
			result.Locations = []sarifLocation{{LogicalLocations: []sarifLogicalLocation{{Name: finding.ClaimID.String(), Kind: "claim"}}}}
			result.Properties["claim_id"] = finding.ClaimID.String()
		}
		results = append(results, result)
	}
	ruleList := make([]sarifRule, 0, len(rules))
	for _, rule := range rules {
		ruleList = append(ruleList, rule)
	}
	sort.Slice(ruleList, func(i, j int) bool { return ruleList[i].ID < ruleList[j].ID })
	document := sarifLog{
		Version: "2.1.0",
		Schema:  "https://docs.oasis-open.org/sarif/sarif/v2.1.0/errata01/os/schemas/sarif-schema-2.1.0.json",
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name: "patchproof", Version: report.Provenance.OrchestratorVersion,
				InformationURI: "https://patchproof.io", Rules: ruleList,
			}},
			Results: results,
			Properties: map[string]any{
				"run_id": report.RunID.String(), "verdict": string(report.Verdict),
				"merkle_root": report.MerkleRoot, "attestation": string(report.Attestation),
			},
		}},
	}
	return json.MarshalIndent(document, "", "  ")
}

func sarifLevel(severity domain.Severity) string {
	switch severity {
	case domain.SeverityCritical, domain.SeverityHigh:
		return "error"
	case domain.SeverityMedium:
		return "warning"
	default:
		return "note"
	}
}

func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}
