package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

func testCacheEntry(tenant domain.TenantID, key string, now time.Time) domain.ActionCacheEntry {
	return domain.ActionCacheEntry{
		TenantID: tenant, ActionKey: key, StepKind: domain.StepKindCompile, Engine: "go-build", EngineVersion: "1",
		Result:    domain.StepResult{Outcome: domain.OutcomeSucceeded, ResultDigest: "sha256:" + strings.Repeat("a", 64)},
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
}

func TestActionCacheLifecycle(t *testing.T) {
	store := New()
	ctx := context.Background()
	tenant := domain.TenantID("11111111-1111-4111-8111-111111111111")
	key := "sha256:" + strings.Repeat("b", 64)
	now := time.Now().UTC()
	entry := testCacheEntry(tenant, key, now)
	if err := store.PutActionCache(ctx, entry); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetActionCache(ctx, tenant, key, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got.Result.ResultDigest != entry.Result.ResultDigest {
		t.Fatalf("unexpected entry: %#v", got)
	}
	if _, err := store.GetActionCache(ctx, tenant, key, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired entry was returned")
	}
	otherTenant := domain.TenantID("22222222-2222-4222-8222-222222222222")
	if _, err := store.GetActionCache(ctx, otherTenant, key, now); err == nil {
		t.Fatal("cross-tenant entry was returned")
	}
	if err := store.PutActionCache(ctx, domain.ActionCacheEntry{TenantID: tenant, ActionKey: "bad"}); err == nil {
		t.Fatal("invalid entry accepted")
	}
}
