package scheduler

import (
	"fmt"
	"sort"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

type ScorePlacement struct{}

func (ScorePlacement) Select(step domain.Step, candidates []domain.Cluster) (domain.Cluster, error) {
	if len(candidates) == 0 {
		return domain.Cluster{}, fmt.Errorf("placement: no candidate clusters")
	}
	best := candidates[0]
	bestScore := best.Score(step.Requirements)
	for _, candidate := range candidates[1:] {
		score := candidate.Score(step.Requirements)
		if score > bestScore || (score == bestScore && candidate.ID < best.ID) {
			best = candidate
			bestScore = score
		}
	}
	if bestScore < 0 {
		return domain.Cluster{}, fmt.Errorf("placement: no cluster has capacity")
	}
	return best, nil
}

type FirstFitPlacement struct{}

func (FirstFitPlacement) Select(step domain.Step, candidates []domain.Cluster) (domain.Cluster, error) {
	sorted := append([]domain.Cluster(nil), candidates...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, candidate := range sorted {
		if candidate.Supports(step.Requirements) {
			return candidate, nil
		}
	}
	return domain.Cluster{}, fmt.Errorf("placement: no candidate cluster supports the step")
}

func NewPlacement(name string) (ports.PlacementStrategy, error) {
	switch name {
	case "", "score":
		return ScorePlacement{}, nil
	case "first-fit":
		return FirstFitPlacement{}, nil
	default:
		return nil, fmt.Errorf("placement: unknown strategy %q", name)
	}
}
