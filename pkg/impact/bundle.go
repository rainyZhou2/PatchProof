// Package impact defines the stable wire format shared by the impact analyzer
// and the engines that consume its output.
package impact

const SchemaVersion = "patchproof.impact/v1"

type Coverage struct {
	Exact   int `json:"exact"`
	Approx  int `json:"approx"`
	Unknown int `json:"unknown"`
}

type Bundle struct {
	SchemaVersion    string   `json:"schema_version"`
	ChangedFiles     []string `json:"changed_files"`
	ChangedSymbols   []string `json:"changed_symbols"`
	AffectedSymbols  []string `json:"affected_symbols"`
	AffectedPackages []string `json:"affected_packages"`
	TestCandidates   []string `json:"test_candidates"`
	Resolution       Coverage `json:"resolution"`
	Limits           []string `json:"limits"`
	Depth            int      `json:"depth"`
	Truncated        bool     `json:"truncated"`
}
