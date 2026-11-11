package memory

import (
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// cancelDependentStepsLocked cancels blocked steps that depend transitively on
// a step which can no longer succeed. Existing terminal failures are traversed
// so repeated calls after a controller failover converge on the same state.
func (s *Store) cancelDependentStepsLocked(state *runState, roots []domain.StepID, at time.Time) ([]domain.Step, []domain.OutboxEvent, error) {
	if len(roots) == 0 {
		return nil, nil, nil
	}
	reachable := make(map[domain.StepID]struct{}, len(roots))
	for _, root := range roots {
		reachable[root] = struct{}{}
	}
	for progressed := true; progressed; {
		progressed = false
		for _, id := range state.stepIDs {
			if _, ok := reachable[id]; ok {
				continue
			}
			step, ok := s.steps[id]
			if !ok || !traversable(step.Status) {
				continue
			}
			for _, dependency := range state.deps[id] {
				if _, ok := reachable[dependency]; ok {
					reachable[id] = struct{}{}
					progressed = true
					break
				}
			}
		}
	}

	propagated := make([]domain.Step, 0)
	events := make([]domain.OutboxEvent, 0)
	for _, id := range state.stepIDs {
		step, ok := s.steps[id]
		if !ok || step.Status != domain.StepBlocked {
			continue
		}
		if _, ok := reachable[id]; !ok {
			continue
		}
		event, err := newStepEvent(*step, "", domain.EventStepFailed, domain.StepCancelled, at)
		if err != nil {
			return nil, nil, err
		}
		if err := step.Transition(domain.StepCancelled, at); err != nil {
			return nil, nil, err
		}
		step.FailureClass = domain.ErrDependencyFailed
		step.FailureMessage = "a required dependency did not succeed"
		step.FinishedAt = timePtr(at)
		events = append(events, event)
		propagated = append(propagated, cloneStep(*step))
	}
	return propagated, events, nil
}

func traversable(status domain.StepStatus) bool {
	switch status {
	case domain.StepBlocked, domain.StepFailed, domain.StepCancelled, domain.StepTimedOut:
		return true
	default:
		return false
	}
}
