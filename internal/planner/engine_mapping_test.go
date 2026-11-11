package planner_test

import (
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/planner"
)

func TestPolicyEngineMappingRoutesStepsAndClaims(t *testing.T) {
	request := testRequest()
	request.Policy.Spec.Verification.Engines = map[string]domain.EngineSpec{
		"analyze": {Name: "python-analyzer", Language: "python"},
		"impact":  {Name: "python-analyzer", Language: "python"},
		"compile": {Name: "python-build", Language: "python"},
	}
	result := plan(t, newPlanner(t, planner.Limits{}), request)
	var analyze, impact, compile bool
	for _, step := range result.Steps {
		switch step.Key {
		case "analyze":
			analyze = true
			if step.Requirements.Engine != "python-analyzer" || step.Requirements.Language != "python" {
				t.Fatalf("analyze mapped incorrectly: %#v", step.Requirements)
			}
			if step.Options["language"] != "python" {
				t.Fatalf("analyze option language = %q", step.Options["language"])
			}
		case "impact":
			impact = true
			if step.Requirements.Engine != "python-analyzer" || step.Requirements.Language != "python" {
				t.Fatalf("impact mapped incorrectly: %#v", step.Requirements)
			}
		case "compile-head":
			compile = true
			if step.Requirements.Engine != "python-build" || step.Requirements.Language != "python" {
				t.Fatalf("compile mapped incorrectly: %#v", step.Requirements)
			}
		}
	}
	if !analyze || !impact || !compile {
		t.Fatalf("missing mapped steps: analyze=%t impact=%t compile=%t", analyze, impact, compile)
	}
	for _, claim := range result.Claims {
		if claim.ID == "static-analysis" && claim.Language != "python" {
			t.Fatalf("claim language = %q", claim.Language)
		}
		if claim.ID == "compile" && claim.Language != "python" {
			t.Fatalf("compile claim language = %q", claim.Language)
		}
	}
}
