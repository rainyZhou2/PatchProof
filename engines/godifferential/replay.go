package godifferential

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/patchproof/patchproof/engines/godifferential/harness"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/engine"
	"github.com/patchproof/patchproof/pkg/stepopts"
)

type replayInput struct {
	raw    []byte
	caseID string
}

func loadReplayInput(request engine.Request, workspace string) (*replayInput, *engine.Result) {
	path, ok := stagedPath(request, "replay", stepopts.ReplayInput)
	if !ok {
		return nil, nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, failure(domain.ErrValidationFailed, "resolve replay input: "+err.Error(), false, nil)
	}
	if !withinAny(resolved, []string{workspace}) {
		return nil, failure(domain.ErrValidationFailed, "replay input is outside the step workspace", false, nil)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return nil, failure(domain.ErrValidationFailed, "replay input is not a regular file", false, nil)
	}
	if info.Size() > harness.MaxInputBytes {
		return nil, failure(domain.ErrValidationFailed, "replay input exceeds the harness input limit", false, nil)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return nil, failure(domain.ErrValidationFailed, "read replay input: "+err.Error(), false, nil)
	}
	raw = bytes.TrimSpace(raw)
	var args []json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, failure(domain.ErrValidationFailed, "replay input must be a JSON argument array: "+err.Error(), false, nil)
	}
	return &replayInput{raw: raw, caseID: strings.TrimSpace(stepopts.Get(request.Spec.Options, stepopts.ReplayCase))}, nil
}

func (c *config) executeReplay(ctx context.Context, runner *executor, stats *statistics) *engine.Result {
	item := caseInput{raw: c.replay.raw}
	base, head, infra := runner.runSingle(ctx, item)
	if infra != "" {
		return &engine.Result{
			Outcome:    domain.OutcomeRetryable,
			ErrorClass: domain.ErrExecutorError,
			Message:    "replay: " + infra,
			Retryable:  true,
		}
	}
	stats.executed = 1
	if base.Status == harness.StatusInvalid || head.Status == harness.StatusInvalid {
		stats.invalidCases = 1
		stats.limits = append(stats.limits, "replay input could not be decoded into the target signature")
		return nil
	}
	stats.validCases = 1
	finding := isFinding(base, head, c.contract.Spec.Kind, c.oracle)
	stats.caseRecords = append(stats.caseRecords, caseRecord{
		CaseID:     item.id(),
		Input:      item.raw,
		BaseStatus: base.Status,
		HeadStatus: head.Status,
		Diff:       finding,
	})
	if !finding {
		stats.limits = append(stats.limits, "replay input no longer differs between base and head; the stored counterexample may be fixed or non-deterministic")
		return nil
	}
	stats.diffCases = 1
	counterexample, record, err := c.buildCounterexample(ctx, item, base, head, false, 0)
	if err != nil {
		result := casFailure(fmt.Errorf("store replay counterexample: %w", err))
		return &result
	}
	stats.counterexamples = append(stats.counterexamples, counterexample)
	stats.ceRecords = append(stats.ceRecords, record)
	return nil
}
