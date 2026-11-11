package godifferential

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/patchproof/patchproof/engines/godifferential/harness"
	"github.com/patchproof/patchproof/pkg/contract"
	evidenceutil "github.com/patchproof/patchproof/pkg/evidence"
)

// oracleConfig applies the contract's normalization rules to observations
// before they are compared. Canonical JSON is always used for structural
// equality, so the "canonical-json" rule needs no separate switch.
type oracleConfig struct {
	trimSpace   bool
	errorString bool
	floatEps    bool
	epsilon     float64
	ignore      map[string]struct{}
	tolerance   map[string]string
}

func newOracleConfig(document contract.Document, defaultEpsilon float64) (oracleConfig, error) {
	config := oracleConfig{
		epsilon:   defaultEpsilon,
		ignore:    make(map[string]struct{}, len(document.Spec.Oracle.IgnoreFields)),
		tolerance: document.Spec.Oracle.Tolerance,
	}
	for _, rule := range document.Spec.Oracle.Normalize {
		switch rule {
		case "float-eps":
			config.floatEps = true
		case "trim-space":
			config.trimSpace = true
		case "error-string":
			config.errorString = true
		case "canonical-json":
		default:
			return oracleConfig{}, fmt.Errorf("unsupported normalize rule %q", rule)
		}
	}
	for _, field := range document.Spec.Oracle.IgnoreFields {
		field = strings.TrimSpace(field)
		if field == "" {
			return oracleConfig{}, fmt.Errorf("oracle.ignoreFields must not contain empty names")
		}
		config.ignore[field] = struct{}{}
	}
	for _, key := range []string{"float-eps", "epsilon"} {
		raw, ok := config.tolerance[key]
		if !ok {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
			return oracleConfig{}, fmt.Errorf("oracle.tolerance.%s must be a non-negative finite number", key)
		}
		config.epsilon = parsed
	}
	if config.epsilon < 0 || math.IsNaN(config.epsilon) || math.IsInf(config.epsilon, 0) {
		return oracleConfig{}, fmt.Errorf("float epsilon must be a non-negative finite number")
	}
	return config, nil
}

func (o oracleConfig) decodeValue(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func (o oracleConfig) normalizeDiagnostic(text string) string {
	if o.trimSpace {
		text = strings.TrimSpace(text)
	}
	if o.errorString {
		if index := strings.Index(text, ": "); index >= 0 {
			text = text[index+2:]
		}
	}
	return text
}

func (o oracleConfig) normalize(value any) any {
	switch typed := value.(type) {
	case string:
		if o.trimSpace {
			return strings.TrimSpace(typed)
		}
		return typed
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = o.normalize(item)
		}
		return out
	case map[string]any:
		if o.errorString {
			if raw, ok := typed["__error__"].(string); ok {
				return o.normalizeDiagnostic(raw)
			}
		}
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if _, ignored := o.ignore[key]; ignored {
				continue
			}
			out[key] = o.normalize(item)
		}
		return out
	default:
		return value
	}
}

// sameObservation reports whether two observations are equivalent after
// normalization. Timing data is deliberately not part of the comparison.
func (o oracleConfig) sameObservation(base, head harness.Observation) bool {
	if base.Status != head.Status {
		return false
	}
	if o.normalizeDiagnostic(base.Panic) != o.normalizeDiagnostic(head.Panic) {
		return false
	}
	baseValue, baseErr := o.decodeValue(base.Value)
	headValue, headErr := o.decodeValue(head.Value)
	if baseErr != nil || headErr != nil {
		if (baseErr == nil) != (headErr == nil) {
			return false
		}
		return bytes.Equal(base.Value, head.Value)
	}
	baseValue = o.normalize(baseValue)
	headValue = o.normalize(headValue)
	if !o.floatEps {
		baseCanonical, err := evidenceutil.Canonicalize(baseValue)
		if err != nil {
			return false
		}
		headCanonical, err := evidenceutil.Canonicalize(headValue)
		if err != nil {
			return false
		}
		return bytes.Equal(baseCanonical, headCanonical)
	}
	return o.equalValues(baseValue, headValue)
}

func (o oracleConfig) equalValues(base, head any) bool {
	switch baseValue := base.(type) {
	case nil:
		return head == nil
	case bool:
		headValue, ok := head.(bool)
		return ok && baseValue == headValue
	case string:
		headValue, ok := head.(string)
		return ok && baseValue == headValue
	case json.Number:
		headValue, ok := head.(json.Number)
		return ok && o.equalNumbers(baseValue, headValue)
	case []any:
		headValue, ok := head.([]any)
		if !ok || len(baseValue) != len(headValue) {
			return false
		}
		for index := range baseValue {
			if !o.equalValues(baseValue[index], headValue[index]) {
				return false
			}
		}
		return true
	case map[string]any:
		headValue, ok := head.(map[string]any)
		if !ok || len(baseValue) != len(headValue) {
			return false
		}
		for key, item := range baseValue {
			other, present := headValue[key]
			if !present || !o.equalValues(item, other) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (o oracleConfig) equalNumbers(base, head json.Number) bool {
	baseFloat, baseErr := base.Float64()
	headFloat, headErr := head.Float64()
	if baseErr != nil || headErr != nil {
		return base.String() == head.String()
	}
	if math.IsNaN(baseFloat) && math.IsNaN(headFloat) {
		return true
	}
	if math.IsNaN(baseFloat) || math.IsNaN(headFloat) {
		return false
	}
	if o.epsilon == 0 {
		return baseFloat == headFloat
	}
	scale := math.Max(1, math.Max(math.Abs(baseFloat), math.Abs(headFloat)))
	return math.Abs(baseFloat-headFloat) <= o.epsilon*scale
}

// isFinding decides whether a pair of observations is a reportable behavioral
// difference. Invalid cases are never findings because they indicate arguments
// the harness could not decode rather than observable behavior. For safety
// contracts a panic on the head side is a finding even if the base panics too,
// because the contract asserts that a panic cannot occur.
func isFinding(base, head harness.Observation, kind string, oracle oracleConfig) bool {
	if base.Status == harness.StatusInvalid || head.Status == harness.StatusInvalid {
		return false
	}
	if kind == "safety" && head.Status == harness.StatusPanic {
		return true
	}
	return !oracle.sameObservation(base, head)
}
