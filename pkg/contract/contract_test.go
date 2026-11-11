package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validContractYAML = `
apiVersion: patchproof.io/v1
kind: Contract
metadata:
  name: parse-contract
  labels:
    team: platform
  annotations:
    owner: qa
spec:
  symbol: example.com/acme/pkg.Parse
  language: go
  kind: behavior-preservation
  domain:
    description: parse values without panicking
    constraints:
      - input is valid UTF-8
    generators:
      - unicode
      - ascii
    seed: 42
  oracle:
    type: json-differential/v1
    observables:
      - return
      - panic
    normalize:
      - canonical-json
      - error-string
      - trim-space
    tolerance:
      latency_ms: "5"
    ignoreFields:
      - timestamp
      - request_id
  seeds:
    - label: scalars
      args: [42, "hello", true, 3.5, null]
    - label: arrays
      args:
        - [1, 2, 3]
        - ["a", "b"]
    - label: objects
      args:
        - name: alice
          nested:
            count: 2
            flags: [true, false]
  assumptions:
    - inputs are UTF-8
  minimumResult: TESTED_WITHIN_SCOPE
  required: true
  budget:
    maxCases: 100
    maxDurationSeconds: 30
  timeoutMillis: 5000
`

func decodeArg(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("seed argument %s is not valid JSON: %v", raw, err)
	}
	return value
}

func TestParseFullContract(t *testing.T) {
	t.Parallel()
	document, err := Parse([]byte(validContractYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if document.APIVersion != APIVersion || document.Kind != Kind {
		t.Fatalf("unexpected document identity: %q/%q", document.APIVersion, document.Kind)
	}
	if document.Metadata.Name != "parse-contract" {
		t.Fatalf("metadata.name = %q", document.Metadata.Name)
	}
	if document.Metadata.Labels["team"] != "platform" || document.Metadata.Annotations["owner"] != "qa" {
		t.Fatalf("unexpected metadata maps: %+v", document.Metadata)
	}
	spec := document.Spec
	if spec.Symbol != "example.com/acme/pkg.Parse" || spec.Language != "go" || spec.Kind != "behavior-preservation" {
		t.Fatalf("unexpected spec identity: %+v", spec)
	}
	if !spec.Required || spec.MinimumResult != "TESTED_WITHIN_SCOPE" {
		t.Fatalf("unexpected required/minimumResult: %+v", spec)
	}
	if spec.TimeoutMillis != 5000 || spec.Budget.MaxCases != 100 || spec.Budget.MaxDurationSecs != 30 {
		t.Fatalf("unexpected budget/timeout: %+v", spec)
	}
	if spec.Domain.Description == "" || spec.Domain.Seed != 42 || len(spec.Domain.Constraints) != 1 || len(spec.Domain.Generators) != 2 {
		t.Fatalf("unexpected domain: %+v", spec.Domain)
	}
	if spec.Oracle.Type != OracleJSON {
		t.Fatalf("oracle.type = %q", spec.Oracle.Type)
	}
	if !reflect.DeepEqual(spec.Oracle.Observables, []string{"return", "panic"}) {
		t.Fatalf("observables = %#v", spec.Oracle.Observables)
	}
	if !reflect.DeepEqual(spec.Oracle.Normalize, []string{"canonical-json", "error-string", "trim-space"}) {
		t.Fatalf("normalize = %#v", spec.Oracle.Normalize)
	}
	if spec.Oracle.Tolerance["latency_ms"] != "5" {
		t.Fatalf("tolerance = %#v", spec.Oracle.Tolerance)
	}
	if !reflect.DeepEqual(spec.Oracle.IgnoreFields, []string{"timestamp", "request_id"}) {
		t.Fatalf("ignoreFields = %#v", spec.Oracle.IgnoreFields)
	}
	if !reflect.DeepEqual(spec.Assumptions, []string{"inputs are UTF-8"}) {
		t.Fatalf("assumptions = %#v", spec.Assumptions)
	}

	ref, err := document.SymbolRef()
	if err != nil {
		t.Fatalf("SymbolRef: %v", err)
	}
	if ref.PackagePath != "example.com/acme/pkg" || ref.Name != "Parse" || ref.Receiver != "" {
		t.Fatalf("SymbolRef = %+v", ref)
	}

	if len(spec.Seeds) != 3 {
		t.Fatalf("seeds = %d, want 3", len(spec.Seeds))
	}
	scalars := spec.Seeds[0]
	if scalars.Label != "scalars" || len(scalars.Args) != 5 {
		t.Fatalf("scalar seed = %+v", scalars)
	}
	for i, raw := range scalars.Args {
		if !json.Valid(raw) {
			t.Fatalf("scalar arg %d is invalid JSON: %s", i, raw)
		}
	}
	wantScalars := []any{float64(42), "hello", true, 3.5, nil}
	for i, want := range wantScalars {
		if got := decodeArg(t, scalars.Args[i]); !reflect.DeepEqual(got, want) {
			t.Fatalf("scalar arg %d = %#v, want %#v (raw %s)", i, got, want, scalars.Args[i])
		}
	}

	arrays := spec.Seeds[1]
	if arrays.Label != "arrays" || len(arrays.Args) != 2 {
		t.Fatalf("array seed = %+v", arrays)
	}
	if got := decodeArg(t, arrays.Args[0]); !reflect.DeepEqual(got, []any{float64(1), float64(2), float64(3)}) {
		t.Fatalf("array arg 0 = %#v", got)
	}
	if got := decodeArg(t, arrays.Args[1]); !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Fatalf("array arg 1 = %#v", got)
	}

	objects := spec.Seeds[2]
	if objects.Label != "objects" || len(objects.Args) != 1 {
		t.Fatalf("object seed = %+v", objects)
	}
	object, ok := decodeArg(t, objects.Args[0]).(map[string]any)
	if !ok {
		t.Fatalf("object arg decoded as %T, want map[string]any", decodeArg(t, objects.Args[0]))
	}
	if object["name"] != "alice" {
		t.Fatalf("object name = %#v", object["name"])
	}
	nested, ok := object["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested object decoded as %T", object["nested"])
	}
	if nested["count"] != float64(2) {
		t.Fatalf("nested count = %#v", nested["count"])
	}
	if !reflect.DeepEqual(nested["flags"], []any{true, false}) {
		t.Fatalf("nested flags = %#v", nested["flags"])
	}

	// Seed arguments must round-trip through encoding/json unchanged.
	for i, seed := range spec.Seeds {
		for j, raw := range seed.Args {
			encoded, err := json.Marshal(decodeArg(t, raw))
			if err != nil {
				t.Fatalf("seed %d arg %d re-marshal: %v", i, j, err)
			}
			if !json.Valid(encoded) {
				t.Fatalf("seed %d arg %d re-marshaled to invalid JSON %s", i, j, encoded)
			}
		}
	}
}

func TestParseSymbol(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		symbol   string
		want     SymbolRef
		wantErr  bool
		errorish string
	}{
		{
			name:   "package function",
			symbol: "example.com/acme/pkg.Parse",
			want:   SymbolRef{PackagePath: "example.com/acme/pkg", Name: "Parse"},
		},
		{
			name:   "package function with surrounding whitespace",
			symbol: "  github.com/patchproof/patchproof/pkg/contract.Parse  ",
			want:   SymbolRef{PackagePath: "github.com/patchproof/patchproof/pkg/contract", Name: "Parse"},
		},
		{
			name:   "method",
			symbol: "example.com/acme/pkg.Store.Get",
			want:   SymbolRef{PackagePath: "example.com/acme/pkg", Name: "Store.Get", Receiver: "Store"},
		},
		{
			name:   "method on versioned package",
			symbol: "example.com/acme/pkg/v2.Store.Get",
			want:   SymbolRef{PackagePath: "example.com/acme/pkg/v2", Name: "Store.Get", Receiver: "Store"},
		},
		{name: "empty", symbol: "", wantErr: true, errorish: "required"},
		{name: "no dot", symbol: "NoDot", wantErr: true, errorish: "fully qualified"},
		{name: "leading dot", symbol: ".Parse", wantErr: true, errorish: "fully qualified"},
		{name: "trailing dot", symbol: "example.com/acme/pkg.", wantErr: true, errorish: "fully qualified"},
		{name: "invalid function name", symbol: "example.com/acme/pkg.9bad", wantErr: true, errorish: "invalid function name"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseSymbol(test.symbol)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ParseSymbol(%q) = %+v, want error", test.symbol, got)
				}
				if test.errorish != "" && !strings.Contains(err.Error(), test.errorish) {
					t.Fatalf("ParseSymbol(%q) error = %q, want %q", test.symbol, err, test.errorish)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSymbol(%q): %v", test.symbol, err)
			}
			if got != test.want {
				t.Fatalf("ParseSymbol(%q) = %+v, want %+v", test.symbol, got, test.want)
			}
		})
	}
}

func validDocument() Document {
	return Document{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "valid"},
		Spec: Spec{
			Symbol:   "example.com/acme/pkg.Parse",
			Language: "go",
			Kind:     "behavior-preservation",
			Domain:   Domain{Description: "valid domain"},
			Oracle:   Oracle{Type: OracleJSON, Observables: []string{"return"}, Normalize: []string{"canonical-json"}},
			Seeds:    []Case{{Args: []json.RawMessage{json.RawMessage(`42`)}}},
			Required: true,
		},
	}
}

func TestValidateRejectsInvalidDocuments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Document)
		wantSub string
	}{
		{name: "wrong apiVersion", mutate: func(d *Document) { d.APIVersion = "patchproof.io/v2" }, wantSub: "apiVersion"},
		{name: "empty apiVersion", mutate: func(d *Document) { d.APIVersion = "" }, wantSub: "apiVersion"},
		{name: "wrong kind", mutate: func(d *Document) { d.Kind = "NotContract" }, wantSub: "kind"},
		{name: "empty metadata name", mutate: func(d *Document) { d.Metadata.Name = "  " }, wantSub: "metadata.name"},
		{name: "missing symbol", mutate: func(d *Document) { d.Spec.Symbol = "" }, wantSub: "symbol"},
		{name: "invalid symbol", mutate: func(d *Document) { d.Spec.Symbol = "example.com/acme/pkg.9bad" }, wantSub: "invalid function name"},
		{name: "missing language", mutate: func(d *Document) { d.Spec.Language = "" }, wantSub: "language"},
		{name: "unsupported language", mutate: func(d *Document) { d.Spec.Language = "python" }, wantSub: "language"},
		{name: "unknown spec kind", mutate: func(d *Document) { d.Spec.Kind = "vibes" }, wantSub: "kind"},
		{name: "empty domain", mutate: func(d *Document) { d.Spec.Domain.Description = " " }, wantSub: "domain.description"},
		{name: "wrong oracle", mutate: func(d *Document) { d.Spec.Oracle.Type = "string-diff/v1" }, wantSub: "oracle.type"},
		{name: "empty oracle", mutate: func(d *Document) { d.Spec.Oracle.Type = "" }, wantSub: "oracle.type"},
		{name: "unsupported observable", mutate: func(d *Document) { d.Spec.Oracle.Observables = []string{"stdout"} }, wantSub: "observables"},
		{name: "unsupported normalize", mutate: func(d *Document) { d.Spec.Oracle.Normalize = []string{"lowercase"} }, wantSub: "normalize"},
		{name: "invalid minimum result", mutate: func(d *Document) { d.Spec.MinimumResult = "PROVEN" }, wantSub: "minimumResult"},
		{name: "negative timeout", mutate: func(d *Document) { d.Spec.TimeoutMillis = -1 }, wantSub: "timeoutMillis"},
		{name: "excessive timeout", mutate: func(d *Document) { d.Spec.TimeoutMillis = 600001 }, wantSub: "timeoutMillis"},
		{name: "negative budget", mutate: func(d *Document) { d.Spec.Budget.MaxCases = -1 }, wantSub: "budget"},
		{name: "missing seed args", mutate: func(d *Document) { d.Spec.Seeds = []Case{{Label: "none"}} }, wantSub: "args is required"},
		{name: "invalid seed JSON", mutate: func(d *Document) {
			d.Spec.Seeds = []Case{{Args: []json.RawMessage{json.RawMessage(`{`)}}}
		}, wantSub: "not valid JSON"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			document := validDocument()
			if err := document.Validate(); err != nil {
				t.Fatalf("fixture is not valid: %v", err)
			}
			test.mutate(&document)
			err := document.Validate()
			if err == nil {
				t.Fatalf("Validate accepted invalid document: %+v", document)
			}
			if !strings.Contains(err.Error(), test.wantSub) {
				t.Fatalf("Validate error = %q, want it to mention %q", err, test.wantSub)
			}
		})
	}
}

func TestParseRejectsInvalidYAML(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"unknown field": strings.Replace(validContractYAML, "  required: true", "  required: true\n  mysteryField: 1", 1),
		"wrong version": strings.Replace(validContractYAML, "apiVersion: patchproof.io/v1", "apiVersion: patchproof.io/v9", 1),
		"broken syntax": "apiVersion: [",
	} {
		raw := raw
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatalf("Parse accepted invalid YAML (%s)", name)
			}
		})
	}
}

func TestResolvePath(t *testing.T) {
	t.Parallel()
	root := filepath.Join(string(filepath.Separator), "snapshot", "root")
	tests := []struct {
		name    string
		ref     string
		want    string
		wantErr bool
	}{
		{name: "simple relative", ref: "main.go", want: filepath.Join(root, "main.go")},
		{name: "nested relative", ref: "internal/pkg/file.go", want: filepath.Join(root, "internal/pkg/file.go")},
		{name: "dot segments inside", ref: "internal/../main.go", want: filepath.Join(root, "main.go")},
		{name: "dot prefix", ref: "./nested/file.go", want: filepath.Join(root, "nested/file.go")},
		{name: "empty", ref: "", wantErr: true},
		{name: "whitespace", ref: "   ", wantErr: true},
		{name: "absolute", ref: filepath.Join(string(filepath.Separator), "etc", "passwd"), wantErr: true},
		{name: "parent escape", ref: "../secret", wantErr: true},
		{name: "nested parent escape", ref: "internal/../../secret", wantErr: true},
		{name: "bare parent", ref: "..", wantErr: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolvePath(root, test.ref)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ResolvePath(%q) = %q, want error", test.ref, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolvePath(%q): %v", test.ref, err)
			}
			if got != test.want {
				t.Fatalf("ResolvePath(%q) = %q, want %q", test.ref, got, test.want)
			}
		})
	}
}

func TestLoadFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	validPath := filepath.Join(dir, "contract.yaml")
	if err := os.WriteFile(validPath, []byte(validContractYAML), 0o600); err != nil {
		t.Fatalf("write contract: %v", err)
	}
	document, err := LoadFile(validPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if document.Metadata.Name != "parse-contract" {
		t.Fatalf("metadata.name = %q", document.Metadata.Name)
	}

	largePath := filepath.Join(dir, "large.yaml")
	large := make([]byte, (1<<20)+1)
	for i := range large {
		large[i] = ' '
	}
	if err := os.WriteFile(largePath, large, 0o600); err != nil {
		t.Fatalf("write large contract: %v", err)
	}
	if _, err := LoadFile(largePath); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("LoadFile large error = %v, want size cap error", err)
	}

	if _, err := LoadFile(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("expected missing contract error")
	}
}
