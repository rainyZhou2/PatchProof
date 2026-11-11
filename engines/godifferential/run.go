package godifferential

import (
	"context"
	"fmt"
	"time"

	"github.com/patchproof/patchproof/engines/godifferential/harness"
)

// executor owns the two built harness binaries and runs cases against them in
// batched, isolated processes (concurrency is intentionally one).
type executor struct {
	env          harness.Environment
	baseBinary   string
	headBinary   string
	baseDir      string
	headDir      string
	signature    harness.Signature
	contractKind string
	oracle       oracleConfig
	batchTimeout time.Duration
}

func (x *executor) maxStdout(caseCount int) int {
	limit := 4 << 20
	expected := caseCount * (harness.MaxObservationBytes + 256)
	if expected > limit {
		limit = expected
	}
	if limit > 64<<20 {
		limit = 64 << 20
	}
	return limit
}

func (x *executor) runCases(ctx context.Context, cases []caseInput) ([]harness.Observation, []harness.Observation, string) {
	stdin := make([]byte, 0, len(cases)*64)
	for _, item := range cases {
		stdin = append(stdin, item.raw...)
		stdin = append(stdin, '\n')
	}
	baseResult := x.env.RunHarness(ctx, x.baseBinary, x.baseDir, stdin, true, x.batchTimeout, x.maxStdout(len(cases)))
	if !baseResult.Successful() {
		return nil, nil, describeInfra("base", baseResult)
	}
	baseObservations, err := harness.ParseObservations(baseResult.Stdout, len(cases))
	if err != nil {
		return nil, nil, "base harness output: " + err.Error()
	}
	headResult := x.env.RunHarness(ctx, x.headBinary, x.headDir, stdin, true, x.batchTimeout, x.maxStdout(len(cases)))
	if !headResult.Successful() {
		return nil, nil, describeInfra("head", headResult)
	}
	headObservations, err := harness.ParseObservations(headResult.Stdout, len(cases))
	if err != nil {
		return nil, nil, "head harness output: " + err.Error()
	}
	return baseObservations, headObservations, ""
}

func (x *executor) runSingle(ctx context.Context, item caseInput) (harness.Observation, harness.Observation, string) {
	timeout := x.batchTimeout
	if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	baseResult := x.env.RunHarness(ctx, x.baseBinary, x.baseDir, item.raw, false, timeout, 2*harness.MaxObservationBytes)
	if !baseResult.Successful() {
		return harness.Observation{}, harness.Observation{}, describeInfra("base", baseResult)
	}
	baseObservations, err := harness.ParseObservations(baseResult.Stdout, 1)
	if err != nil {
		return harness.Observation{}, harness.Observation{}, "base harness output: " + err.Error()
	}
	headResult := x.env.RunHarness(ctx, x.headBinary, x.headDir, item.raw, false, timeout, 2*harness.MaxObservationBytes)
	if !headResult.Successful() {
		return harness.Observation{}, harness.Observation{}, describeInfra("head", headResult)
	}
	headObservations, err := harness.ParseObservations(headResult.Stdout, 1)
	if err != nil {
		return harness.Observation{}, harness.Observation{}, "head harness output: " + err.Error()
	}
	return baseObservations[0], headObservations[0], ""
}

// reproduce reruns a minimized case in fresh single-case processes. A
// counterexample is only reported when the difference survives this step.
func (x *executor) reproduce(ctx context.Context, item caseInput) (harness.Observation, harness.Observation, bool, string) {
	baseObservation, headObservation, infra := x.runSingle(ctx, item)
	if infra != "" {
		return harness.Observation{}, harness.Observation{}, false, "reproduction infrastructure failure: " + infra
	}
	if baseObservation.Status == harness.StatusInvalid || headObservation.Status == harness.StatusInvalid {
		return baseObservation, headObservation, false, "reproduction produced an invalid case"
	}
	if !isFinding(baseObservation, headObservation, x.contractKind, x.oracle) {
		return baseObservation, headObservation, false, "observations matched on fresh processes"
	}
	return baseObservation, headObservation, true, ""
}

func describeInfra(side string, result harness.ProcessResult) string {
	reason := result.Diagnostic()
	if result.TimedOut {
		reason = "TIMEOUT: " + reason
	}
	return fmt.Sprintf("%s harness: %s", side, reason)
}
