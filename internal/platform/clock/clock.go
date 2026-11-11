package clock

import (
	"sync"
	"time"
)

type System struct{}

func (System) Now() time.Time { return time.Now().UTC() }

type Fake struct {
	mu  sync.RWMutex
	now time.Time
}

func NewFake(start time.Time) *Fake {
	return &Fake{now: start.UTC()}
}

func (f *Fake) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.now
}

func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}
