package domain

import (
	"fmt"
	"sort"
)

type DAG struct {
	RunID        RunID               `json:"run_id"`
	Steps        []Step              `json:"steps"`
	Dependencies map[StepID][]StepID `json:"dependencies"`
	Claims       []Claim             `json:"claims"`
}

func (d DAG) Validate() error {
	if err := d.RunID.Validate(); err != nil {
		return err
	}
	if len(d.Steps) == 0 {
		return &ValidationError{Field: "dag", Reason: "must contain at least one step"}
	}
	ids := make(map[StepID]struct{}, len(d.Steps))
	keys := make(map[string]StepID, len(d.Steps))
	for _, step := range d.Steps {
		if err := step.Validate(); err != nil {
			return err
		}
		if step.RunID != d.RunID {
			return &ValidationError{Field: "dag.step.run_id", Reason: "all steps must belong to the run"}
		}
		if _, dup := ids[step.ID]; dup {
			return &ValidationError{Field: "dag.steps", Reason: fmt.Sprintf("duplicate step id %s", step.ID)}
		}
		if other, dup := keys[step.Key]; dup {
			return &ValidationError{Field: "dag.steps", Reason: fmt.Sprintf("duplicate step key %q (%s, %s)", step.Key, other, step.ID)}
		}
		ids[step.ID] = struct{}{}
		keys[step.Key] = step.ID
	}
	for stepID, deps := range d.Dependencies {
		if _, ok := ids[stepID]; !ok {
			return &ValidationError{Field: "dag.dependencies", Reason: fmt.Sprintf("dependency source %s is not a step", stepID)}
		}
		for _, dep := range deps {
			if dep == stepID {
				return &ValidationError{Field: "dag.dependencies", Reason: fmt.Sprintf("step %s depends on itself", stepID)}
			}
			if _, ok := ids[dep]; !ok {
				return &ValidationError{Field: "dag.dependencies", Reason: fmt.Sprintf("dependency target %s is not a step", dep)}
			}
		}
	}
	if _, err := d.TopologicalOrder(); err != nil {
		return err
	}
	claimIDs := make(map[ClaimID]struct{}, len(d.Claims))
	for _, claim := range d.Claims {
		if err := claim.Validate(); err != nil {
			return err
		}
		if claim.RunID != d.RunID {
			return &ValidationError{Field: "dag.claims", Reason: "all claims must belong to the run"}
		}
		if _, dup := claimIDs[claim.ID]; dup {
			return &ValidationError{Field: "dag.claims", Reason: fmt.Sprintf("duplicate claim id %s", claim.ID)}
		}
		claimIDs[claim.ID] = struct{}{}
	}
	return nil
}

func (d DAG) TopologicalOrder() ([]StepID, error) {
	indegree := make(map[StepID]int, len(d.Steps))
	dependents := make(map[StepID][]StepID, len(d.Steps))
	for _, step := range d.Steps {
		indegree[step.ID] = 0
	}
	for stepID, deps := range d.Dependencies {
		for _, dep := range deps {
			indegree[stepID]++
			dependents[dep] = append(dependents[dep], stepID)
		}
	}
	ready := make([]StepID, 0, len(d.Steps))
	for id, deg := range indegree {
		if deg == 0 {
			ready = append(ready, id)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
	order := make([]StepID, 0, len(d.Steps))
	for len(ready) > 0 {
		current := ready[0]
		ready = ready[1:]
		order = append(order, current)
		for _, next := range dependents[current] {
			indegree[next]--
			if indegree[next] == 0 {
				ready = append(ready, next)
			}
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
	}
	if len(order) != len(d.Steps) {
		return nil, &ValidationError{Field: "dag", Reason: "contains a dependency cycle"}
	}
	return order, nil
}

func (d DAG) StepByKey(key string) (Step, bool) {
	for _, step := range d.Steps {
		if step.Key == key {
			return step, true
		}
	}
	return Step{}, false
}

func (d DAG) DependentsOf(stepID StepID) []StepID {
	var out []StepID
	for id, deps := range d.Dependencies {
		for _, dep := range deps {
			if dep == stepID {
				out = append(out, id)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func AllStepsTerminal(steps []Step) bool {
	for _, step := range steps {
		if !step.Status.Terminal() {
			return false
		}
	}
	return len(steps) > 0
}
