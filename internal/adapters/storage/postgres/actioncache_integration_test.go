package postgres

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

func TestActionCachePostgres(t *testing.T) {
	dsn := os.Getenv("PATCHPROOF_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PATCHPROOF_TEST_POSTGRES_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := New(ctx, Config{DSN: dsn, MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tenant := domain.NewTenantID()
	if err := store.CreateTenant(ctx, domain.Tenant{
		ID: tenant, HomeRegion: "region-a", SecurityTier: domain.SecurityTierStandard, Status: domain.TenantActive,
		ActiveWriterEpoch: 1, Quota: domain.TenantQuota{MaxConcurrentSteps: 4},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	key := "sha256:" + strings.Repeat("a", 64)
	entry := domain.ActionCacheEntry{
		TenantID: tenant, ActionKey: key, StepKind: domain.StepKindCompile, Engine: "go-build", EngineVersion: "1",
		Result:    domain.StepResult{Outcome: domain.OutcomeSucceeded, ResultDigest: "sha256:" + strings.Repeat("b", 64)},
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := store.PutActionCache(ctx, entry); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetActionCache(ctx, tenant, key, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got.Result.ResultDigest != entry.Result.ResultDigest {
		t.Fatalf("unexpected result: %#v", got)
	}
	if _, err := store.GetActionCache(ctx, tenant, key, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired entry was returned")
	}
	removed, err := store.PurgeActionCache(ctx, now.Add(2*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("expected one purged entry, got %d", removed)
	}
}
