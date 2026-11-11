package bootstrap

import (
	"context"
	"sync"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

// tenantRateResolver caches each tenant's API RPS quota so the API limiter does
// not hit the metadata store on every request.
type tenantRateResolver struct {
	store    ports.MetadataStore
	fallback int
	ttl      time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[domain.TenantID]tenantRateEntry
}

type tenantRateEntry struct {
	rps     int
	expires time.Time
}

func newTenantRateResolver(store ports.MetadataStore, fallback int) *tenantRateResolver {
	if fallback <= 0 {
		fallback = 20
	}
	return &tenantRateResolver{
		store: store, fallback: fallback, ttl: 30 * time.Second,
		now: func() time.Time { return time.Now() }, entries: map[domain.TenantID]tenantRateEntry{},
	}
}

func (r *tenantRateResolver) Resolve(tenantID domain.TenantID) (int, int) {
	if r == nil || tenantID.Validate() != nil {
		return r.fallback, r.fallback * 2
	}
	now := r.now()
	r.mu.Lock()
	entry, ok := r.entries[tenantID]
	r.mu.Unlock()
	if ok && now.Before(entry.expires) {
		return entry.rps, entry.rps * 2
	}
	rps := r.fallback
	if tenant, err := r.store.GetTenant(context.Background(), tenantID); err == nil && tenant.Quota.APIRPS > 0 {
		rps = tenant.Quota.APIRPS
	}
	r.mu.Lock()
	r.entries[tenantID] = tenantRateEntry{rps: rps, expires: now.Add(r.ttl)}
	if len(r.entries) > 10000 {
		for id, candidate := range r.entries {
			if now.After(candidate.expires) {
				delete(r.entries, id)
			}
		}
	}
	r.mu.Unlock()
	return rps, rps * 2
}
