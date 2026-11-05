package auth

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Login rate limits (plan 14: "rate-limited per account and IP"; the plan
// sets no values). Every attempt, successful or not, spends one token from
// both the account bucket and the IP bucket.
const (
	AccountAttemptsPerMinute = 5
	IPAttemptsPerMinute      = 20
	limiterIdleEviction      = 15 * time.Minute
	limiterMaxKeys           = 100000
)

// keyedLimiter holds one token bucket per key, evicting idle buckets so
// memory stays bounded under a spray of distinct keys.
type keyedLimiter struct {
	limit rate.Limit
	burst int
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	lim      *rate.Limiter
	lastUsed time.Time
}

func newKeyedLimiter(perMinute int, now func() time.Time) *keyedLimiter {
	return &keyedLimiter{
		limit:   rate.Every(time.Minute / time.Duration(perMinute)),
		burst:   perMinute,
		now:     now,
		buckets: map[string]*bucket{},
	}
}

// allow spends one token for key and reports whether it was available, and
// if not, how long until the next one.
func (k *keyedLimiter) allow(key string) (bool, time.Duration) {
	now := k.now()
	k.mu.Lock()
	defer k.mu.Unlock()
	b, ok := k.buckets[key]
	if !ok {
		if len(k.buckets) >= limiterMaxKeys {
			k.evictLocked(now)
		}
		b = &bucket{lim: rate.NewLimiter(k.limit, k.burst)}
		k.buckets[key] = b
	}
	b.lastUsed = now
	r := b.lim.ReserveN(now, 1)
	if delay := r.DelayFrom(now); delay > 0 {
		r.CancelAt(now)
		return false, delay
	}
	return true, 0
}

func (k *keyedLimiter) evictLocked(now time.Time) {
	for key, b := range k.buckets {
		if now.Sub(b.lastUsed) > limiterIdleEviction {
			delete(k.buckets, key)
		}
	}
	// Still full: drop everything rather than grow without bound. This only
	// resets limits under an extreme spray of distinct keys.
	if len(k.buckets) >= limiterMaxKeys {
		k.buckets = map[string]*bucket{}
	}
}
