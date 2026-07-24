package remote

import "time"

// Limiter is a minimal token-bucket rate limit for remote transfers
// (section 13: worth having specifically because "remote" may cross a
// WAN; an unthrottled initial full sync could saturate the link).
type Limiter struct {
	bytesPerSec int64
	avail       float64
	last        time.Time
}

// NewLimiter returns nil for rate <= 0 (unlimited).
func NewLimiter(bytesPerSec int64) *Limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	return &Limiter{bytesPerSec: bytesPerSec, last: time.Now()}
}

// Wait blocks until n bytes may be sent.
func (l *Limiter) Wait(n int) {
	if l == nil {
		return
	}
	for {
		now := time.Now()
		l.avail += now.Sub(l.last).Seconds() * float64(l.bytesPerSec)
		l.last = now
		if max := float64(l.bytesPerSec); l.avail > max {
			l.avail = max
		}
		if l.avail >= float64(n) {
			l.avail -= float64(n)
			return
		}
		missing := float64(n) - l.avail
		time.Sleep(time.Duration(missing / float64(l.bytesPerSec) * float64(time.Second)))
	}
}
