// Package id generates time-ordered UUIDv7 identifiers.
package id

import (
	"crypto/rand"
	"fmt"
	"time"
)

// New returns a UUIDv7 string: 48 bits of millisecond time, version 7,
// variant 10, then 72 random bits. Lexicographic order matches creation
// order, which keeps ledger listings stable.
func New() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	now := time.Now().UnixMilli()
	b[0] = byte(now >> 28)
	b[1] = byte(now >> 20)
	b[2] = byte(now >> 12)
	b[3] = byte(now >> 4)
	b[4] = byte(now<<4)&0x0F | 0x70 // version 7
	b[6] = b[6]&0x3F | 0x80         // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
