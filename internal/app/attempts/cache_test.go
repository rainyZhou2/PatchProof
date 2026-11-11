package attempts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/actioncache"
	"github.com/patchproof/patchproof/internal/domain"
)

func TestCompleteWritesActionCache(t *testing.T) {
	fixture := newAttemptFixture(t)
	fixture.service.ConfigureActionCache(true, time.Hour, "epoch-1", "worker@sha256:test")
	ctx := context.Background()
	if err := fixture.service.Start(ctx, fixture.ref); err != nil {
		t.Fatal(err)
	}
	result := domain.StepResult{
		Outcome: domain.OutcomeSucceeded, ResultDigest: "sha256:" + strings.Repeat("a", 64),
		FinishedAt: attemptTestNow,
	}
	if _, err := fixture.service.Complete(ctx, fixture.ref, result); err != nil {
		t.Fatal(err)
	}
	step, err := fixture.store.GetStep(ctx, fixture.tenantID, fixture.stepID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.GetRun(ctx, fixture.tenantID, fixture.runID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := actioncache.KeyForStep(step, run, "epoch-1", "worker@sha256:test")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := fixture.store.GetActionCache(ctx, fixture.tenantID, key, attemptTestNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("cache entry not written: %v", err)
	}
	if entry.StepKind != domain.StepKindCompile || entry.Engine != "go-build" {
		t.Fatalf("unexpected cache entry: %#v", entry)
	}
}
