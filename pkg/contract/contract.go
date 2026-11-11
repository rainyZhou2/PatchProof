package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	APIVersion    = "patchproof.io/v1"
	Kind          = "Contract"
	OracleJSON    = "json-differential/v1"
	InputJSONArgs = "json-args/v1"
)

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Document struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion"`
	Kind       string   `yaml:"kind" json:"kind"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       Spec     `yaml:"spec" json:"spec"`
}

type Metadata struct {
	Name        string            `yaml:"name" json:"name"`
	Labels      map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty" json:"annotations,omitempty"`
}

type Spec struct {
	Symbol        string   `yaml:"symbol" json:"symbol"`
	Language      string   `yaml:"language" json:"language"`
	Kind          string   `yaml:"kind" json:"kind"`
	Domain        Domain   `yaml:"domain" json:"domain"`
	Oracle        Oracle   `yaml:"oracle" json:"oracle"`
	Seeds         []Case   `yaml:"seeds,omitempty" json:"seeds,omitempty"`
	Assumptions   []string `yaml:"assumptions,omitempty" json:"assumptions,omitempty"`
	MinimumResult string   `yaml:"minimumResult,omitempty" json:"minimumResult,omitempty"`
	Required      bool     `yaml:"required" json:"required"`
	Budget        Budget   `yaml:"budget,omitempty" json:"budget,omitempty"`
	TimeoutMillis int      `yaml:"timeoutMillis,omitempty" json:"timeoutMillis,omitempty"`
}

type Domain struct {
	Description string   `yaml:"description" json:"description"`
	Constraints []string `yaml:"constraints,omitempty" json:"constraints,omitempty"`
	Generators  []string `yaml:"generators,omitempty" json:"generators,omitempty"`
	Seed        int64    `yaml:"seed,omitempty" json:"seed,omitempty"`
}

type Oracle struct {
	Type         string            `yaml:"type" json:"type"`
	Observables  []string          `yaml:"observables,omitempty" json:"observables,omitempty"`
	Normalize    []string          `yaml:"normalize,omitempty" json:"normalize,omitempty"`
	Tolerance    map[string]string `yaml:"tolerance,omitempty" json:"tolerance,omitempty"`
	IgnoreFields []string          `yaml:"ignoreFields,omitempty" json:"ignoreFields,omitempty"`
}

type Case struct {
	Label string            `yaml:"label,omitempty" json:"label,omitempty"`
	Args  []json.RawMessage `yaml:"args" json:"args"`
}

func (c *Case) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Label string      `yaml:"label"`
		Args  []yaml.Node `yaml:"args"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	c.Label = raw.Label
	c.Args = make([]json.RawMessage, 0, len(raw.Args))
	for _, node := range raw.Args {
		var decoded any
		if err := node.Decode(&decoded); err != nil {
			return err
		}
		normalized, err := jsonCompatible(decoded)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return fmt.Errorf("contract seed argument: %w", err)
		}
		c.Args = append(c.Args, encoded)
	}
	return nil
}

func jsonCompatible(value any) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized, err := jsonCompatible(item)
			if err != nil {
				return nil, err
			}
			out[key] = normalized
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("contract seed map keys must be strings")
			}
			normalized, err := jsonCompatible(item)
			if err != nil {
				return nil, err
			}
			out[name] = normalized
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			normalized, err := jsonCompatible(item)
			if err != nil {
				return nil, err
			}
			out = append(out, normalized)
		}
		return out, nil
	default:
		return value, nil
	}
}

type Budget struct {
	MaxCases        int `yaml:"maxCases,omitempty" json:"maxCases,omitempty"`
	MaxDurationSecs int `yaml:"maxDurationSeconds,omitempty" json:"maxDurationSeconds,omitempty"`
}

type SymbolRef struct {
	PackagePath string
	Name        string
	Receiver    string
}

func Parse(raw []byte) (Document, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var doc Document
	if err := decoder.Decode(&doc); err != nil {
		return Document{}, fmt.Errorf("parse contract: %w", err)
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func LoadFile(path string) (Document, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Document{}, fmt.Errorf("stat contract: %w", err)
	}
	if info.Size() > 1<<20 {
		return Document{}, fmt.Errorf("contract file exceeds 1 MiB")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Document{}, fmt.Errorf("read contract: %w", err)
	}
	return Parse(raw)
}

func (d Document) Validate() error {
	if d.APIVersion != APIVersion {
		return fmt.Errorf("contract.apiVersion must be %s", APIVersion)
	}
	if d.Kind != Kind {
		return fmt.Errorf("contract.kind must be %s", Kind)
	}
	if strings.TrimSpace(d.Metadata.Name) == "" {
		return fmt.Errorf("contract.metadata.name is required")
	}
	if _, err := d.SymbolRef(); err != nil {
		return err
	}
	if d.Spec.Language == "" {
		return fmt.Errorf("contract.spec.language is required")
	}
	if d.Spec.Language != "go" {
		return fmt.Errorf("contract.spec.language %q is not supported by this engine", d.Spec.Language)
	}
	switch d.Spec.Kind {
	case "behavior-preservation", "fix-invariant", "safety":
	default:
		return fmt.Errorf("contract.spec.kind %q is unknown", d.Spec.Kind)
	}
	if strings.TrimSpace(d.Spec.Domain.Description) == "" {
		return fmt.Errorf("contract.spec.domain.description is required")
	}
	if d.Spec.Oracle.Type != OracleJSON {
		return fmt.Errorf("contract.spec.oracle.type must be %s", OracleJSON)
	}
	for _, observable := range d.Spec.Oracle.Observables {
		switch observable {
		case "return", "panic":
		default:
			return fmt.Errorf("contract.spec.oracle.observables: unsupported observable %q", observable)
		}
	}
	for _, normalize := range d.Spec.Oracle.Normalize {
		switch normalize {
		case "error-string", "float-eps", "trim-space", "canonical-json":
		default:
			return fmt.Errorf("contract.spec.oracle.normalize: unsupported rule %q", normalize)
		}
	}
	if d.Spec.MinimumResult != "" {
		switch d.Spec.MinimumResult {
		case "TESTED_WITHIN_SCOPE", "PROVEN_WITHIN_MODEL":
		default:
			return fmt.Errorf("contract.spec.minimumResult is invalid")
		}
	}
	if d.Spec.TimeoutMillis < 0 || d.Spec.TimeoutMillis > 600000 {
		return fmt.Errorf("contract.spec.timeoutMillis must be between 0 and 600000")
	}
	if d.Spec.Budget.MaxCases < 0 || d.Spec.Budget.MaxDurationSecs < 0 {
		return fmt.Errorf("contract.spec.budget values must not be negative")
	}
	for i, seed := range d.Spec.Seeds {
		if seed.Args == nil {
			return fmt.Errorf("contract.spec.seeds[%d].args is required", i)
		}
		for j, arg := range seed.Args {
			if !json.Valid(arg) {
				return fmt.Errorf("contract.spec.seeds[%d].args[%d] is not valid JSON", i, j)
			}
		}
	}
	return nil
}

func (d Document) SymbolRef() (SymbolRef, error) {
	return ParseSymbol(d.Spec.Symbol)
}

func ParseSymbol(symbol string) (SymbolRef, error) {
	trimmed := strings.TrimSpace(symbol)
	if trimmed == "" {
		return SymbolRef{}, fmt.Errorf("contract.spec.symbol is required")
	}
	lastDot := strings.LastIndex(trimmed, ".")
	if lastDot <= 0 || lastDot == len(trimmed)-1 {
		return SymbolRef{}, fmt.Errorf("contract.spec.symbol must be a fully qualified package function")
	}
	ref := SymbolRef{PackagePath: trimmed[:lastDot], Name: trimmed[lastDot+1:]}
	if !identifier.MatchString(ref.Name) {
		return SymbolRef{}, fmt.Errorf("contract.spec.symbol has an invalid function name")
	}
	pkg := ref.PackagePath
	if idx := strings.LastIndex(pkg, "."); idx > 0 && strings.Contains(pkg[:idx], "/") {
		candidate := pkg[idx+1:]
		if identifier.MatchString(candidate) {
			ref.Receiver = candidate
			ref.PackagePath = pkg[:idx]
			ref.Name = pkg[idx+1:] + "." + ref.Name
		}
	}
	if strings.TrimSpace(ref.PackagePath) == "" {
		return SymbolRef{}, fmt.Errorf("contract.spec.symbol must include a package import path")
	}
	return ref, nil
}

func ResolvePath(root, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("contract reference is empty")
	}
	clean := filepath.Clean(ref)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("contract reference %q escapes the snapshot root", ref)
	}
	full := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("contract reference %q escapes the snapshot root", ref)
	}
	return full, nil
}
