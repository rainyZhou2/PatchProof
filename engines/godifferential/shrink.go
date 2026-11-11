package godifferential

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/patchproof/patchproof/engines/godifferential/harness"
)

// shrinkCandidates proposes deterministic delta-debugging candidates. Stage 0
// uses coarse moves (zero, nil, halves); stage 1 uses finer removals. Every
// returned vector keeps the function arity unchanged.
func shrinkCandidates(args []any, signature harness.Signature, stage int) [][]any {
	candidates := make([][]any, 0, len(args)*3)
	seen := make(map[string]struct{})
	emit := func(slot int, value any) {
		clone := cloneArgs(args)
		clone[slot] = value
		encoded, err := json.Marshal(clone)
		if err != nil {
			return
		}
		key := string(encoded)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, clone)
	}

	for slot, current := range args {
		desc := signature.Params[slot]
		switch desc.Kind {
		case "bool":
			if value, ok := current.(bool); ok && value {
				emit(slot, false)
			}
		case "int", "int8", "int16", "int32", "int64":
			value, ok := intValue(current)
			if !ok {
				continue
			}
			if value != 0 {
				emit(slot, numberInt(0))
				if value/2 != 0 && value/2 != value {
					emit(slot, numberInt(clampInt(value/2, desc.Bits)))
				}
			}
			if stage > 0 && value != 0 {
				if value > 0 {
					emit(slot, numberInt(clampInt(value-1, desc.Bits)))
				} else {
					emit(slot, numberInt(clampInt(value+1, desc.Bits)))
				}
			}
		case "uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
			value, ok := uintValue(current)
			if !ok || value == 0 {
				continue
			}
			emit(slot, numberUint(0))
			if value/2 != 0 && value/2 != value {
				emit(slot, numberUint(clampUint(value/2, desc.Bits)))
			}
			if stage > 0 {
				emit(slot, numberUint(clampUint(value-1, desc.Bits)))
			}
		case "float32", "float64":
			value, ok := floatValue(current)
			if !ok || value == 0 {
				continue
			}
			emit(slot, numberFloat(0, desc.Bits))
			if !math.IsNaN(value) && !math.IsInf(value, 0) {
				halved := value / 2
				if halved != 0 && halved != value {
					emit(slot, numberFloat(halved, desc.Bits))
				}
				truncated := math.Trunc(value)
				if truncated != value {
					emit(slot, numberFloat(truncated, desc.Bits))
				}
			}
		case "string":
			text, ok := current.(string)
			if !ok || text == "" {
				continue
			}
			runes := []rune(text)
			emit(slot, "")
			if len(runes) > 1 {
				emit(slot, string(runes[:len(runes)/2]))
				emit(slot, string(runes[len(runes)/2:]))
			}
			if stage > 0 {
				emit(slot, string(runes[:len(runes)-1]))
				emit(slot, string(runes[1:]))
			}
		case "slice":
			items, ok := current.([]any)
			if !ok {
				continue
			}
			if current != nil {
				emit(slot, nil)
			}
			if len(items) == 0 {
				continue
			}
			emit(slot, []any{})
			if len(items) > 1 {
				emit(slot, cloneValue(items[:len(items)/2]))
				emit(slot, cloneValue(items[len(items)/2:]))
			}
			if stage > 0 {
				if len(items) <= 8 {
					for index := range items {
						reduced := append(append([]any{}, cloneValue(items[:index]).([]any)...), cloneValue(items[index+1:]).([]any)...)
						emit(slot, reduced)
					}
				} else {
					quarter := len(items) / 4
					if quarter > 0 {
						emit(slot, cloneValue(items[quarter:]))
						emit(slot, cloneValue(items[:len(items)-quarter]))
					}
				}
			}
		case "array":
			items, ok := current.([]any)
			if !ok || len(items) == 0 {
				continue
			}
			emit(slot, []any{})
			if stage > 0 && len(items) <= 8 {
				for index := range items {
					reduced := append(append([]any{}, cloneValue(items[:index]).([]any)...), cloneValue(items[index+1:]).([]any)...)
					emit(slot, reduced)
				}
			}
		case "map":
			items, ok := current.(map[string]any)
			if !ok {
				continue
			}
			if current != nil {
				emit(slot, nil)
			}
			if len(items) == 0 {
				continue
			}
			emit(slot, map[string]any{})
			keys := make([]string, 0, len(items))
			for key := range items {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if stage > 0 && len(items) <= 8 {
				for _, key := range keys {
					reduced := make(map[string]any, len(items)-1)
					for existing, item := range items {
						if existing != key {
							reduced[existing] = item
						}
					}
					emit(slot, reduced)
				}
			} else {
				reduced := make(map[string]any, len(items)-1)
				for existing, item := range items {
					if existing != keys[0] {
						reduced[existing] = item
					}
				}
				emit(slot, reduced)
			}
		case "ptr":
			if current != nil {
				emit(slot, nil)
			}
		case "struct":
			if items, ok := current.(map[string]any); !ok || len(items) > 0 {
				emit(slot, map[string]any{})
			}
		case "interface":
			if current != nil {
				emit(slot, nil)
			}
		}
	}
	return candidates
}

const (
	maxShrinkCandidatesPerRound = 64
	maxShrinkEvaluations        = 200
	maxShrinkWall               = 2 * time.Minute
)

// minimize shrinks a case while preserving the finding. It returns the best
// case, the number of evaluated candidates and whether the procedure completed
// without hitting an infrastructure failure or the shrink budget.
func (x *executor) minimize(ctx context.Context, original caseInput) (caseInput, int, bool) {
	best := original
	evaluations := 0
	deadline := time.Now().Add(maxShrinkWall)
	for stage := 0; stage < 2; stage++ {
		for {
			if ctx.Err() != nil || evaluations >= maxShrinkEvaluations || !time.Now().Before(deadline) {
				return best, evaluations, false
			}
			candidates := shrinkCandidates(best.args, x.signature, stage)
			if len(candidates) == 0 {
				break
			}
			improved := false
			for len(candidates) > 0 {
				room := maxShrinkEvaluations - evaluations
				if room <= 0 {
					return best, evaluations, false
				}
				chunk := candidates
				if len(chunk) > maxShrinkCandidatesPerRound {
					chunk = chunk[:maxShrinkCandidatesPerRound]
				}
				if len(chunk) > room {
					chunk = chunk[:room]
				}
				candidates = candidates[len(chunk):]
				inputs := make([]caseInput, 0, len(chunk))
				for _, args := range chunk {
					candidate, ok := newCaseInput(args)
					if !ok {
						continue
					}
					inputs = append(inputs, candidate)
				}
				if len(inputs) == 0 {
					continue
				}
				baseObservations, headObservations, infra := x.runCases(ctx, inputs)
				evaluations += len(inputs)
				if infra != "" {
					return best, evaluations, false
				}
				for index := range inputs {
					if isFinding(baseObservations[index], headObservations[index], x.contractKind, x.oracle) {
						best = inputs[index]
						improved = true
						break
					}
				}
				if improved {
					break
				}
			}
			if !improved {
				break
			}
		}
	}
	return best, evaluations, true
}
