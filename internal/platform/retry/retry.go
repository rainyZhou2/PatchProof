package retry

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

type Policy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      float64
	IsRetryable func(error) bool
}

func Default() Policy {
	return Policy{
		MaxAttempts: 5,
		BaseDelay:   250 * time.Millisecond,
		MaxDelay:    10 * time.Second,
		Jitter:      0.2,
		IsRetryable: func(error) bool { return true },
	}
}

func (p Policy) normalized() Policy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = 100 * time.Millisecond
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 30 * time.Second
	}
	if p.Jitter < 0 {
		p.Jitter = 0
	}
	if p.Jitter > 1 {
		p.Jitter = 1
	}
	if p.IsRetryable == nil {
		p.IsRetryable = func(error) bool { return true }
	}
	return p
}

func Delay(policy Policy, attempt int) time.Duration {
	p := policy.normalized()
	if attempt < 1 {
		attempt = 1
	}
	delay := p.BaseDelay
	for i := 1; i < attempt && delay < p.MaxDelay; i++ {
		delay *= 2
	}
	if delay > p.MaxDelay {
		delay = p.MaxDelay
	}
	if p.Jitter > 0 {
		spread := float64(delay) * p.Jitter
		delay = time.Duration(float64(delay) - spread + rand.Float64()*2*spread)
	}
	if delay < 0 {
		delay = 0
	}
	return delay
}

func Do(ctx context.Context, policy Policy, fn func(context.Context) error) error {
	p := policy.normalized()
	var last error
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, last)
		}
		last = fn(ctx)
		if last == nil {
			return nil
		}
		if !p.IsRetryable(last) || attempt == p.MaxAttempts {
			return last
		}
		timer := time.NewTimer(Delay(p, attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(ctx.Err(), last)
		case <-timer.C:
		}
	}
	return last
}
