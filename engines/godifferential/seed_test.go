package godifferential

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patchproof/patchproof/engines/godifferential/harness"
	"github.com/patchproof/patchproof/pkg/contract"
)

func intType(bits int) harness.TypeDesc {
	return harness.TypeDesc{Kind: "int", Name: "int", Bits: bits}
}

func containsValue(t *testing.T, values []any, predicate func(any) bool) bool {
	t.Helper()
	for _, value := range values {
		if predicate(value) {
			return true
		}
	}
	return false
}

func TestBoundaryValuesCoverTypes(t *testing.T) {
	booleans := boundaryValues(harness.TypeDesc{Kind: "bool", Name: "bool"})
	if len(booleans) != 2 {
		t.Fatalf("bool boundaries = %v", booleans)
	}

	integers := boundaryValues(intType(64))
	for _, want := range []string{"0", "1", "-1", "-9223372036854775808", "9223372036854775807"} {
		if !containsValue(t, integers, func(value any) bool {
			number, ok := value.(json.Number)
			return ok && number.String() == want
		}) {
			t.Fatalf("integer boundaries missing %s: %v", want, integers)
		}
	}

	floats := boundaryValues(harness.TypeDesc{Kind: "float64", Name: "float64", Bits: 64})
	for _, want := range []string{"NaN", "+Inf", "-Inf"} {
		if !containsValue(t, floats, func(value any) bool {
			special, ok := value.(map[string]any)
			return ok && special["__float__"] == want
		}) {
			t.Fatalf("float boundaries missing %s: %v", want, floats)
		}
	}

	strings := boundaryValues(harness.TypeDesc{Kind: "string", Name: "string"})
	if !containsValue(t, strings, func(value any) bool { return value == "" }) {
		t.Fatalf("string boundaries missing empty string: %v", strings)
	}
	if !containsValue(t, strings, func(value any) bool {
		text, ok := value.(string)
		return ok && len(text) == 4096
	}) {
		t.Fatalf("string boundaries missing 4 KiB value")
	}

	slice := boundaryValues(harness.TypeDesc{Kind: "slice", Name: "[]int", Elem: &harness.TypeDesc{Kind: "int64", Name: "int", Bits: 64}})
	if !containsValue(t, slice, func(value any) bool { return value == nil }) {
		t.Fatalf("slice boundaries missing nil")
	}
	if !containsValue(t, slice, func(value any) bool {
		items, ok := value.([]any)
		return ok && len(items) == 1024
	}) {
		t.Fatalf("slice boundaries missing 1024 elements")
	}

	pointer := boundaryValues(harness.TypeDesc{Kind: "ptr", Name: "*int", Elem: &harness.TypeDesc{Kind: "int", Name: "int", Bits: 64}})
	if !containsValue(t, pointer, func(value any) bool { return value == nil }) || !containsValue(t, pointer, func(value any) bool { return value != nil }) {
		t.Fatalf("pointer boundaries must include nil and non-nil: %v", pointer)
	}

	duration := boundaryValues(harness.TypeDesc{Kind: "int64", Name: "time.Duration", Bits: 64})
	if !containsValue(t, duration, func(value any) bool {
		number, ok := value.(json.Number)
		return ok && number.String() == "1000000000"
	}) {
		t.Fatalf("duration boundaries missing one second: %v", duration)
	}
}

func TestGeneratorIsDeterministic(t *testing.T) {
	signature := harness.Signature{Arity: 1, Params: []harness.TypeDesc{intType(64)}}
	explicit := []contract.Case{{Label: "seed", Args: []json.RawMessage{json.RawMessage("5")}}}
	first := newGenerator(signature, explicit, []string{"duration"}, 40, 99)
	second := newGenerator(signature, explicit, []string{"duration"}, 40, 99)
	seen := make(map[string]struct{})
	for index := 0; index < 40; index++ {
		left, leftOK := first.Next()
		right, rightOK := second.Next()
		if leftOK != rightOK {
			t.Fatalf("generator %d availability mismatch: %v vs %v", index, leftOK, rightOK)
		}
		if !leftOK {
			break
		}
		if string(left.raw) != string(right.raw) {
			t.Fatalf("generator %d is not deterministic: %s vs %s", index, left.raw, right.raw)
		}
		seen[string(left.raw)] = struct{}{}
	}
	if len(seen) < 30 {
		t.Fatalf("generator produced only %d distinct cases", len(seen))
	}
	if first.seedCount < 1 {
		t.Fatal("seed corpus must include the contract seed")
	}
}

func TestGeneratorStopsAtCaseBudget(t *testing.T) {
	signature := harness.Signature{Arity: 1, Params: []harness.TypeDesc{intType(64)}}
	generator := newGenerator(signature, nil, nil, 7, 1)
	count := 0
	for {
		if _, ok := generator.Next(); !ok {
			break
		}
		count++
	}
	if count != 7 {
		t.Fatalf("generated %d cases, want 7", count)
	}
}

func TestShrinkCandidatesPreserveArity(t *testing.T) {
	signature := harness.Signature{Arity: 2, Params: []harness.TypeDesc{intType(64), {Kind: "string", Name: "string"}}}
	args := []any{json.Number("-8"), strings.Repeat("a", 16)}
	for stage := 0; stage < 2; stage++ {
		candidates := shrinkCandidates(args, signature, stage)
		if len(candidates) == 0 {
			t.Fatalf("stage %d produced no candidates", stage)
		}
		for _, candidate := range candidates {
			if len(candidate) != len(args) {
				t.Fatalf("candidate changed arity: %v", candidate)
			}
			encoded, err := json.Marshal(candidate)
			if err != nil {
				t.Fatalf("candidate is not JSON encodable: %v", err)
			}
			if string(encoded) == `["-8","aaaaaaaaaaaaaaaa"]` {
				t.Fatalf("candidate did not shrink: %s", encoded)
			}
		}
	}
}
