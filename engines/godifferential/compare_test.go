package godifferential

import (
	"encoding/json"
	"testing"

	"github.com/patchproof/patchproof/engines/godifferential/harness"
	"github.com/patchproof/patchproof/pkg/contract"
)

func observationFromJSON(t *testing.T, status, raw string) harness.Observation {
	t.Helper()
	if !json.Valid([]byte(raw)) {
		t.Fatalf("invalid test JSON: %s", raw)
	}
	return harness.Observation{Status: status, Value: json.RawMessage(raw)}
}

func testOracle(t *testing.T, normalize ...string) oracleConfig {
	t.Helper()
	document := contract.Document{Spec: contract.Spec{Oracle: contract.Oracle{Normalize: normalize}}}
	oracle, err := newOracleConfig(document, 1e-9)
	if err != nil {
		t.Fatalf("new oracle: %v", err)
	}
	return oracle
}

func TestCompareStatusAndPanic(t *testing.T) {
	oracle := testOracle(t, "canonical-json")
	base := observationFromJSON(t, harness.StatusReturn, "[1]")
	head := observationFromJSON(t, harness.StatusPanic, "[]")
	head.Panic = "string: boom"
	if oracle.sameObservation(base, head) {
		t.Fatal("panic vs return must differ")
	}
	if !oracle.sameObservation(base, observationFromJSON(t, harness.StatusReturn, "[1]")) {
		t.Fatal("identical observations must match")
	}
	different := observationFromJSON(t, harness.StatusReturn, "[2]")
	if oracle.sameObservation(base, different) {
		t.Fatal("different return values must differ")
	}
}

func TestCompareFloatEpsilon(t *testing.T) {
	exact := testOracle(t, "canonical-json")
	near := observationFromJSON(t, harness.StatusReturn, "[1.0000001]")
	base := observationFromJSON(t, harness.StatusReturn, "[1]")
	if exact.sameObservation(base, near) {
		t.Fatal("values outside epsilon must differ")
	}
	tolerantDocument := contract.Document{Spec: contract.Spec{Oracle: contract.Oracle{
		Normalize: []string{"float-eps"},
		Tolerance: map[string]string{"float-eps": "1e-3"},
	}}}
	tolerant, err := newOracleConfig(tolerantDocument, 1e-9)
	if err != nil {
		t.Fatalf("new tolerant oracle: %v", err)
	}
	if !tolerant.sameObservation(base, near) {
		t.Fatal("float-eps must treat values within tolerance as equal")
	}
	far := observationFromJSON(t, harness.StatusReturn, "[2]")
	if tolerant.sameObservation(base, far) {
		t.Fatal("float-eps must not hide large differences")
	}
	nan := observationFromJSON(t, harness.StatusReturn, `[{"__unserializable__":"float64:NaN"}]`)
	if !tolerant.sameObservation(nan, nan) {
		t.Fatal("identical NaN encodings must match")
	}
}

func TestCompareTrimSpaceAndErrorString(t *testing.T) {
	plain := testOracle(t, "canonical-json")
	padded := observationFromJSON(t, harness.StatusReturn, `[" a "]`)
	trimmed := observationFromJSON(t, harness.StatusReturn, `["a"]`)
	if plain.sameObservation(padded, trimmed) {
		t.Fatal("trim-space must be opt-in")
	}
	trimming := testOracle(t, "trim-space")
	if !trimming.sameObservation(padded, trimmed) {
		t.Fatal("trim-space must normalize strings")
	}

	errorTypeOne := observationFromJSON(t, harness.StatusReturn, `[{"__error__":"*fmt.wrapError: boom"}]`)
	errorTypeTwo := observationFromJSON(t, harness.StatusReturn, `[{"__error__":"errors.errorString: boom"}]`)
	if plain.sameObservation(errorTypeOne, errorTypeTwo) {
		t.Fatal("error type text must matter without error-string")
	}
	errorString := testOracle(t, "error-string")
	if !errorString.sameObservation(errorTypeOne, errorTypeTwo) {
		t.Fatal("error-string must compare error messages")
	}
}

func TestCompareIgnoreFields(t *testing.T) {
	document := contract.Document{Spec: contract.Spec{Oracle: contract.Oracle{IgnoreFields: []string{"elapsed"}}}}
	oracle, err := newOracleConfig(document, 1e-9)
	if err != nil {
		t.Fatalf("new oracle: %v", err)
	}
	base := observationFromJSON(t, harness.StatusReturn, `[{"id":1,"elapsed":10}]`)
	head := observationFromJSON(t, harness.StatusReturn, `[{"id":1,"elapsed":20}]`)
	if !oracle.sameObservation(base, head) {
		t.Fatal("ignoreFields must drop the configured field")
	}
	different := observationFromJSON(t, harness.StatusReturn, `[{"id":2,"elapsed":10}]`)
	if oracle.sameObservation(base, different) {
		t.Fatal("ignoreFields must not hide other differences")
	}
}

func TestOracleRejectsUnknownNormalizeRule(t *testing.T) {
	document := contract.Document{Spec: contract.Spec{Oracle: contract.Oracle{Normalize: []string{"invented-rule"}}}}
	if _, err := newOracleConfig(document, 1e-9); err == nil {
		t.Fatal("unknown normalize rule must be rejected")
	}
}
