package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/patchproof/patchproof/internal/domain"
)

type Limiter struct {
	mu         sync.Mutex
	buckets    map[domain.TenantID]*bucket
	limit      rate.Limit
	burst      int
	lastGC     time.Time
	maxEntries int
	now        func() time.Time
}

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func New(rps int, burst int) *Limiter {
	if rps <= 0 {
		rps = 20
	}
	if burst <= 0 {
		burst = rps * 2
	}
	return &Limiter{
		buckets: make(map[domain.TenantID]*bucket), limit: rate.Limit(rps), burst: burst,
		maxEntries: 10000, now: func() time.Time { return time.Now() },
	}
}

func (l *Limiter) Allow(tenantID domain.TenantID) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	entry, ok := l.buckets[tenantID]
	if !ok {
		entry = &bucket{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.buckets[tenantID] = entry
	}
	entry.lastSeen = now
	if now.Sub(l.lastGC) > 10*time.Minute || len(l.buckets) > l.maxEntries {
		for key, candidate := range l.buckets {
			if now.Sub(candidate.lastSeen) > 10*time.Minute {
				delete(l.buckets, key)
			}
		}
		l.lastGC = now
	}
	return entry.limiter.AllowN(now, 1)
}
