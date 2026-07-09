package outbox

import (
	"crypto/rand"
	"encoding/binary"
	"math"
	"time"
)

const (
	baseBackoff        = 5 * time.Second
	maxBackoff         = 10 * time.Minute
	jitterPercent      = 0.20
	DefaultMaxAttempts = 8
)

// computeBackoff returns the backoff duration for the given attempt number.
// Formula: min(5s * 2^attempt, 10m) ± 20% jitter.
// attempt=0 → ~5s, attempt=1 → ~10s, ..., attempt>=7 → ~10m (capped).
func computeBackoff(attempt int) time.Duration {
	raw := float64(baseBackoff) * math.Pow(2, float64(attempt))
	if raw > float64(maxBackoff) {
		raw = float64(maxBackoff)
	}
	jitter := raw * jitterPercent * (2*cryptoFloat64() - 1) // [-20%, +20%]
	d := time.Duration(raw + jitter)
	if d < 0 {
		d = time.Duration(raw)
	}
	return d
}

// cryptoFloat64 returns a cryptographically random float64 in [0, 1).
func cryptoFloat64() float64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return float64(binary.LittleEndian.Uint64(b[:])>>11) / (1 << 53)
}
