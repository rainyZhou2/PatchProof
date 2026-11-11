package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

func TestAllowBurstThenDeny(t *testing.T) {
	t.Parallel()
	limiter := New(2, 2)
	tenant := domain.NewTenantID()
	for i := 0; i < 2; i++ {
		if !limiter.Allow(tenant) {
			t.Fatalf("request %d was denied within the burst", i+1)
		}
	}
	if limiter.Allow(tenant) {
		t.Fatal("request exceeded the configured burst")
	}
}

func TestAllowRefillsAfterInjectedTime(t *testing.T) {
	t.Parallel()
	limiter := New(2, 2)
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	limiter.now = func() time.Time { return now }
	tenant := domain.NewTenantID()

	if !limiter.Allow(tenant) || !limiter.Allow(tenant) {
		t.Fatal("initial burst was not allowed")
	}
	if limiter.Allow(tenant) {
		t.Fatal("third request should be denied before refill")
	}

	now = now.Add(500 * time.Millisecond)
	if !limiter.Allow(tenant) {
		t.Fatal("one token should refill after 500ms at 2 rps")
	}
	if limiter.Allow(tenant) {
		t.Fatal("only one token should have refilled")
	}

	now = now.Add(time.Second)
	if !limiter.Allow(tenant) || !limiter.Allow(tenant) {
		t.Fatal("two tokens should refill after one second")
	}
	if limiter.Allow(tenant) {
		t.Fatal("burst capacity should cap the refill at two tokens")
	}
}

func TestAllowIsolatedPerTenant(t *testing.T) {
	t.Parallel()
	limiter := New(1, 1)
	first := domain.NewTenantID()
	second := domain.NewTenantID()
	if !limiter.Allow(first) {
		t.Fatal("first tenant initial request denied")
	}
	if limiter.Allow(first) {
		t.Fatal("first tenant exceeded its burst")
	}
	if !limiter.Allow(second) {
		t.Fatal("second tenant should have its own bucket")
	}
	if limiter.Allow(second) {
		t.Fatal("second tenant exceeded its burst")
	}
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	limiter := New(0, 0)
	limiter.now = func() time.Time { return time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC) }
	if limiter.limit != 20 || limiter.burst != 40 {
		t.Fatalf("defaults = %v/%d, want 20/40", limiter.limit, limiter.burst)
	}
	tenant := domain.NewTenantID()
	for i := 0; i < 40; i++ {
		if !limiter.Allow(tenant) {
			t.Fatalf("default burst denied request %d", i+1)
		}
	}
	if limiter.Allow(tenant) {
		t.Fatal("default burst exceeded 40 requests")
	}

	negative := New(-3, -1)
	negative.now = limiter.now
	if negative.limit != 20 || negative.burst != 40 {
		t.Fatalf("negative defaults = %v/%d, want 20/40", negative.limit, negative.burst)
	}
}

func TestNilLimiterAllows(t *testing.T) {
	t.Parallel()
	var limiter *Limiter
	if !limiter.Allow(domain.NewTenantID()) {
		t.Fatal("nil limiter should allow requests")
	}
}

func TestStaleBucketsAreCollected(t *testing.T) {
	t.Parallel()
	limiter := New(10, 10)
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	limiter.now = func() time.Time { return now }
	first := domain.NewTenantID()
	second := domain.NewTenantID()

	if !limiter.Allow(first) {
		t.Fatal("first tenant denied")
	}
	now = now.Add(11 * time.Minute)
	if !limiter.Allow(second) {
		t.Fatal("second tenant denied")
	}
	limiter.mu.Lock()
	_, firstPresent := limiter.buckets[first]
	_, secondPresent := limiter.buckets[second]
	limiter.mu.Unlock()
	if firstPresent {
		t.Fatal("stale bucket was not garbage collected")
	}
	if !secondPresent {
		t.Fatal("active bucket was removed by garbage collection")
	}
}

func TestConcurrentAllowNeverExceedsBurst(t *testing.T) {
	t.Parallel()
	const (
		burst    = 25
		requests = 200
	)
	limiter := New(1000, burst)
	fixed := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	limiter.now = func() time.Time { return fixed }
	tenant := domain.NewTenantID()

	var allowed atomic.Int64
	var group sync.WaitGroup
	group.Add(requests)
	for i := 0; i < requests; i++ {
		go func() {
			defer group.Done()
			if limiter.Allow(tenant) {
				allowed.Add(1)
			}
		}()
	}
	group.Wait()

	if got := allowed.Load(); got != burst {
		t.Fatalf("allowed %d concurrent requests, want exactly %d", got, burst)
	}
}

func TestLimiterUsesPerTenantResolver(t *testing.T) {
	limiter := New(100, 100)
	limiter.SetResolver(func(domain.TenantID) (int, int) { return 1, 1 })
	tenant := domain.NewTenantID()
	if !limiter.Allow(tenant) {
		t.Fatal("first request should be allowed")
	}
	if limiter.Allow(tenant) {
		t.Fatal("second request should be limited by the tenant resolver")
	}
	other := domain.NewTenantID()
	if !limiter.Allow(other) {
		t.Fatal("another tenant should have an independent bucket")
	}
}
