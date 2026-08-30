// Package id generates time-ordered UUIDv7 identifiers.
package id

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

var generatorState struct {
	sync.Mutex
	millisecond int64
	sequence    uint16
}

// New returns an RFC 9562 UUIDv7 string. The 12-bit rand_a field increments
// within one logical millisecond so identifiers remain strictly ordered.
func New() (string, error) {
	var b [16]byte
	generatorState.Lock()
	defer generatorState.Unlock()
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	millisecond := time.Now().UnixMilli()
	randomSequence := uint16(b[6]&0x0f)<<8 | uint16(b[7])
	switch {
	case millisecond > generatorState.millisecond:
		generatorState.millisecond = millisecond
		generatorState.sequence = randomSequence
	case generatorState.sequence < 0x0fff:
		millisecond = generatorState.millisecond
		generatorState.sequence++
	default:
		generatorState.millisecond++
		millisecond = generatorState.millisecond
		generatorState.sequence = randomSequence
	}
	millisecond = generatorState.millisecond
	b[0] = byte(millisecond >> 40)
	b[1] = byte(millisecond >> 32)
	b[2] = byte(millisecond >> 24)
	b[3] = byte(millisecond >> 16)
	b[4] = byte(millisecond >> 8)
	b[5] = byte(millisecond)
	b[6] = 0x70 | byte(generatorState.sequence>>8)
	b[7] = byte(generatorState.sequence)
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
