package postgres

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/patchproof/patchproof/internal/domain"
)

type fakeRow struct {
	values []any
}

func (r fakeRow) Scan(dest ...any) error {
	if len(dest) != len(r.values) {
		return fmt.Errorf("fake row has %d values for %d destinations", len(r.values), len(dest))
	}
	for i, target := range dest {
		value := reflect.ValueOf(target)
		if value.Kind() != reflect.Pointer || value.IsNil() {
			return fmt.Errorf("destination %d is not a pointer", i)
		}
		elem := value.Elem()
		source := r.values[i]
		if source == nil {
			elem.SetZero()
			continue
		}
		sourceValue := reflect.ValueOf(source)
		switch {
		case sourceValue.Type().AssignableTo(elem.Type()):
			elem.Set(sourceValue)
		case sourceValue.Type().ConvertibleTo(elem.Type()):
			elem.Set(sourceValue.Convert(elem.Type()))
		default:
			return fmt.Errorf("value %d of type %s cannot be assigned to %s", i, sourceValue.Type(), elem.Type())
		}
	}
	return nil
}

func TestScanRunPopulatesAllFields(t *testing.T) {
	runID := uuid.NewString()
	tenantID := uuid.NewString()
	projectID := uuid.NewString()
	createdAt := time.Date(2026, 4, 1, 10, 0, 0, 123456789, time.UTC)
	updatedAt := createdAt.Add(time.Minute)
	plannedAt := createdAt.Add(30 * time.Second)

	row := fakeRow{values: []any{
		runID, tenantID, projectID, "req-1", "fp-1", "aaaaaaa", "bbbbbbb", "sha256:patch",
		"sha256:snapshot", "sha256:base", "sha256:head", "sha256:policy", "policy-ref",
		[]byte(`["compile","regression-test"]`),
		[]byte(`{"cpu_seconds":120,"wall_seconds":300,"max_parallel_steps":4}`),
		"VALIDATING", "INCONCLUSIVE", int64(7), "ENGINE_CRASH", "boom", "region-a", "alice",
		createdAt, updatedAt, &plannedAt, nil, nil,
	}}
	run, err := scanRun(row)
	if err != nil {
		t.Fatalf("scanRun: %v", err)
	}
	if run.ID != domain.RunID(runID) || run.TenantID != domain.TenantID(tenantID) || run.ProjectID != domain.ProjectID(projectID) {
		t.Fatalf("identifiers not populated: %+v", run)
	}
	if run.RequestedChecks == nil || len(run.RequestedChecks) != 2 || run.RequestedChecks[1] != "regression-test" {
		t.Fatalf("requested checks not populated: %+v", run.RequestedChecks)
	}
	if run.Budget.CPUSeconds != 120 || run.Budget.WallSeconds != 300 || run.Budget.MaxParallelSteps != 4 {
		t.Fatalf("budget not populated: %+v", run.Budget)
	}
	if run.Status != domain.RunValidating || run.Verdict != domain.VerdictInconclusive {
		t.Fatalf("status not populated: %+v", run)
	}
	if run.StateVersion != 7 || run.FailureClass != domain.ErrEngineCrash || run.FailureMessage != "boom" {
		t.Fatalf("state not populated: %+v", run)
	}
	if !run.CreatedAt.Equal(createdAt) || !run.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("timestamps not populated: %v %v", run.CreatedAt, run.UpdatedAt)
	}
	if run.PlannedAt == nil || !run.PlannedAt.Equal(plannedAt) {
		t.Fatalf("planned_at not populated: %v", run.PlannedAt)
	}
	if run.StartedAt != nil || run.FinishedAt != nil {
		t.Fatalf("nullable timestamps should stay nil: %v %v", run.StartedAt, run.FinishedAt)
	}
}

func TestScanStepPopulatesOptionsAndRequirements(t *testing.T) {
	stepID := uuid.NewString()
	runID := uuid.NewString()
	tenantID := uuid.NewString()
	createdAt := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	deadline := createdAt.Add(2 * time.Minute)
	nextAttempt := createdAt.Add(5 * time.Second)

	row := fakeRow{values: []any{
		stepID, runID, tenantID, "compile:go", "compile", "RETRY_WAIT", int64(3), 2, 5,
		uuid.NewString(), "controller-1", &deadline, "cluster-a",
		[]byte(`{"architecture":"amd64","isolation":"isolated-container","cpu_milli":2000,"memory_mib":4096,"network":"none","deadline_seconds":300}`),
		"sha256:manifest", "sha256:result", "sha256:output", "ENGINE_CRASH", "failed",
		&nextAttempt, 9, 4, []byte(`{"goflags":"-race","tags":"unit"}`), createdAt, createdAt,
		nil, nil,
	}}
	step, err := scanStep(row)
	if err != nil {
		t.Fatalf("scanStep: %v", err)
	}
	if step.ID != domain.StepID(stepID) || step.Key != "compile:go" || step.Kind != domain.StepKindCompile {
		t.Fatalf("step identity not populated: %+v", step)
	}
	if step.Generation != 3 || step.AttemptCount != 2 || step.MaxAttempts != 5 {
		t.Fatalf("attempt counters not populated: %+v", step)
	}
	if step.Requirements.Architecture != "amd64" || step.Requirements.CPUMilli != 2000 {
		t.Fatalf("requirements not populated: %+v", step.Requirements)
	}
	if step.Options["goflags"] != "-race" || step.Options["tags"] != "unit" {
		t.Fatalf("options not populated: %+v", step.Options)
	}
	if step.LeaseDeadline == nil || !step.LeaseDeadline.Equal(deadline) {
		t.Fatalf("lease deadline not populated: %v", step.LeaseDeadline)
	}
	if step.NextAttemptAt == nil || !step.NextAttemptAt.Equal(nextAttempt) {
		t.Fatalf("next attempt not populated: %v", step.NextAttemptAt)
	}
	if step.FailureClass != domain.ErrEngineCrash || step.Shard != 4 || step.Priority != 9 {
		t.Fatalf("state not populated: %+v", step)
	}
}

func TestScanOutboxEvent(t *testing.T) {
	eventID := uuid.NewString()
	tenantID := uuid.NewString()
	runID := uuid.NewString()
	occurredAt := time.Date(2026, 4, 1, 11, 0, 0, 0, time.UTC)
	row := fakeRow{values: []any{
		eventID, tenantID, "patchproof.step.leased.v1", "step-1", runID, int64(2),
		occurredAt, "trace-parent", []byte(`{"step_id":"step-1"}`), nil, 3,
	}}
	event, err := scanOutboxEvent(row)
	if err != nil {
		t.Fatalf("scanOutboxEvent: %v", err)
	}
	if event.ID != domain.EventID(eventID) || event.TenantID != domain.TenantID(tenantID) {
		t.Fatalf("event identity not populated: %+v", event)
	}
	if event.Type != domain.EventStepLeased || event.RunID != domain.RunID(runID) || event.Generation != 2 {
		t.Fatalf("event fields not populated: %+v", event)
	}
	if event.Attempts != 3 || event.DeliveredAt != nil {
		t.Fatalf("delivery state not populated: %+v", event)
	}
	if string(event.Payload) != `{"step_id":"step-1"}` {
		t.Fatalf("payload not populated: %s", event.Payload)
	}
}

func TestUUIDArrayParameterEncoding(t *testing.T) {
	mapping := pgtype.NewMap()
	values := []uuid.UUID{uuid.New(), uuid.New()}
	for _, format := range []int16{pgtype.BinaryFormatCode, pgtype.TextFormatCode} {
		if plan := mapping.PlanEncode(pgtype.UUIDArrayOID, format, values); plan == nil {
			t.Fatalf("no encode plan for []uuid.UUID in format %d", format)
		}
	}
}
