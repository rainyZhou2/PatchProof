package domain_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
)

func baseDAG() domain.DAG {
	stepA := makeStep(testStep1, testRunID, testTenantID, "a")
	stepB := makeStep(testStep2, testRunID, testTenantID, "b")
	stepC := makeStep(testStep3, testRunID, testTenantID, "c")
	stepD := makeStep(testStep4, testRunID, testTenantID, "d")
	return domain.DAG{
		RunID: testRunID,
		Steps: []domain.Step{stepA, stepB, stepC, stepD},
		Dependencies: map[domain.StepID][]domain.StepID{
			testStep2: {testStep1},
			testStep3: {testStep1},
			testStep4: {testStep1, testStep2},
		},
		Claims: []domain.Claim{makeClaim("claim-a", testRunID, testTenantID)},
	}
}

func assertValidationError(t *testing.T, err error) {
	t.Helper()
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want *ValidationError", err)
	}
}

func TestDAGValidateHappyPath(t *testing.T) {
	if err := baseDAG().Validate(); err != nil {
		t.Fatalf("valid DAG rejected: %v", err)
	}
}

func TestDAGValidateEmpty(t *testing.T) {
	dag := baseDAG()
	dag.Steps = nil
	assertValidationError(t, dag.Validate())
}

func TestDAGValidateDuplicateStepID(t *testing.T) {
	dag := baseDAG()
	duplicate := makeStep(testStep1, testRunID, testTenantID, "duplicate-key")
	dag.Steps = append(dag.Steps, duplicate)
	assertValidationError(t, dag.Validate())
}

func TestDAGValidateDuplicateStepKey(t *testing.T) {
	dag := baseDAG()
	duplicate := makeStep(domain.StepID("12121212-1212-4212-8212-121212121212"), testRunID, testTenantID, "a")
	dag.Steps = append(dag.Steps, duplicate)
	assertValidationError(t, dag.Validate())
}

func TestDAGValidateMissingDependencyTarget(t *testing.T) {
	dag := baseDAG()
	dag.Dependencies[testStep2] = []domain.StepID{domain.StepID("13131313-1313-4313-8313-131313131313")}
	assertValidationError(t, dag.Validate())
}

func TestDAGValidateUnknownDependencySource(t *testing.T) {
	dag := baseDAG()
	dag.Dependencies[domain.StepID("14141414-1414-4414-8414-141414141414")] = []domain.StepID{testStep1}
	assertValidationError(t, dag.Validate())
}

func TestDAGValidateSelfDependency(t *testing.T) {
	dag := baseDAG()
	dag.Dependencies[testStep2] = []domain.StepID{testStep2}
	assertValidationError(t, dag.Validate())
}

func TestDAGValidateCycle(t *testing.T) {
	dag := baseDAG()
	dag.Dependencies[testStep1] = []domain.StepID{testStep2}
	err := dag.Validate()
	assertValidationError(t, err)
	if err != nil && !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v, want mention of a cycle", err)
	}
}

func TestDAGValidateStepRunMismatch(t *testing.T) {
	dag := baseDAG()
	dag.Steps[0].RunID = testRunB
	assertValidationError(t, dag.Validate())
}

func TestDAGValidateClaimRunMismatch(t *testing.T) {
	dag := baseDAG()
	dag.Claims[0].RunID = testRunB
	assertValidationError(t, dag.Validate())
}

func TestDAGValidateDuplicateClaimID(t *testing.T) {
	dag := baseDAG()
	dag.Claims = append(dag.Claims, makeClaim("claim-a", testRunID, testTenantID))
	assertValidationError(t, dag.Validate())
}

func TestDAGValidateInvalidStep(t *testing.T) {
	dag := baseDAG()
	dag.Steps[0].Key = "Not Valid"
	assertValidationError(t, dag.Validate())
}

func TestDAGValidateInvalidClaim(t *testing.T) {
	dag := baseDAG()
	dag.Claims[0].Title = ""
	assertValidationError(t, dag.Validate())
}

func TestDAGTopologicalOrderDeterministic(t *testing.T) {
	dag := baseDAG()
	first, err := dag.TopologicalOrder()
	if err != nil {
		t.Fatalf("topological order: %v", err)
	}
	second, err := dag.TopologicalOrder()
	if err != nil {
		t.Fatalf("topological order second call: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("order not deterministic: %v vs %v", first, second)
	}
	want := []domain.StepID{testStep1, testStep2, testStep3, testStep4}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("order = %v, want %v", first, want)
	}
}

func TestDAGDependentsOfSorted(t *testing.T) {
	dag := baseDAG()
	want := []domain.StepID{testStep2, testStep3, testStep4}
	if got := dag.DependentsOf(testStep1); !reflect.DeepEqual(got, want) {
		t.Fatalf("dependents = %v, want %v", got, want)
	}
	if got := dag.DependentsOf(testStep4); len(got) != 0 {
		t.Fatalf("leaf dependents = %v, want none", got)
	}
}

func TestDAGStepByKey(t *testing.T) {
	dag := baseDAG()
	step, ok := dag.StepByKey("c")
	if !ok {
		t.Fatal("step c not found")
	}
	if step.ID != testStep3 {
		t.Fatalf("step c id = %s, want %s", step.ID, testStep3)
	}
	if _, ok := dag.StepByKey("missing"); ok {
		t.Fatal("unexpected step for missing key")
	}
}

func TestAllStepsTerminal(t *testing.T) {
	if domain.AllStepsTerminal(nil) {
		t.Fatal("empty step list must not count as all-terminal")
	}
	ready := makeStep(testStep1, testRunID, testTenantID, "a")
	if domain.AllStepsTerminal([]domain.Step{ready}) {
		t.Fatal("ready step must not count as terminal")
	}
	succeeded := ready
	succeeded.Status = domain.StepSucceeded
	failed := makeStep(testStep2, testRunID, testTenantID, "b")
	failed.Status = domain.StepFailed
	if !domain.AllStepsTerminal([]domain.Step{succeeded, failed}) {
		t.Fatal("all terminal steps should report true")
	}
}
