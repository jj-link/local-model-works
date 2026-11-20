package server

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// Login throttling is keyed on username, not client IP: behind
// `tailscale serve` every request arrives from 127.0.0.1, so an IP key
// would throttle all clients as one.
const (
	loginFailureLimit   = 5
	loginFailureWindow  = 15 * time.Minute
	globalFailureLimit  = 100
	globalFailureWindow = time.Minute
	loginRetryAfter     = "900"
)

type bucket struct {
	failures int
	resetAt  time.Time
}

// loginLimiter counts failed password logins per lowercased username, plus a
// global backstop against username spraying. The zero value is ready to use.
type loginLimiter struct {
	mu      sync.Mutex
	now     func() time.Time // nil means time.Now
	buckets map[string]*bucket
	global  bucket
}

func (l *loginLimiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// globalLocked reports whether the global failure backstop is tripped.
func (l *loginLimiter) globalLocked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.globalLockedLocked(l.clock())
}

// locked reports whether username is throttled, either by its own failures
// or by the global backstop.
func (l *loginLimiter) locked(username string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if l.globalLockedLocked(now) {
		return true
	}
	b := l.buckets[strings.ToLower(username)]
	return b != nil && now.Before(b.resetAt) && b.failures >= loginFailureLimit
}

func (l *loginLimiter) globalLockedLocked(now time.Time) bool {
	return now.Before(l.global.resetAt) && l.global.failures >= globalFailureLimit
}

// fail records one failed password for username.
func (l *loginLimiter) fail(username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if !now.Before(l.global.resetAt) {
		l.global = bucket{resetAt: now.Add(globalFailureWindow)}
		// Once per global window, drop expired per-user buckets so failed
		// usernames do not accumulate without bound.
		for key, b := range l.buckets {
			if !now.Before(b.resetAt) {
				delete(l.buckets, key)
			}
		}
	}
	l.global.failures++

	if l.buckets == nil {
		l.buckets = make(map[string]*bucket)
	}
	key := strings.ToLower(username)
	b := l.buckets[key]
	if b == nil || !now.Before(b.resetAt) {
		b = &bucket{resetAt: now.Add(loginFailureWindow)}
		l.buckets[key] = b
	}
	b.failures++
}

// succeed clears username's failure count.
func (l *loginLimiter) succeed(username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, strings.ToLower(username))
}

func writeTooManyLoginAttempts(w http.ResponseWriter) {
	w.Header().Set("Retry-After", loginRetryAfter)
	writeErr(w, http.StatusTooManyRequests, "auth.too_many_attempts", "too many failed login attempts; try again later")
}
