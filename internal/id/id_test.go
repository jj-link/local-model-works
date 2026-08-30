package id

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNewProducesRFC9562UUIDv7(t *testing.T) {
	before := time.Now().UnixMilli()
	value, err := New()
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UnixMilli()
	parsed, err := uuid.Parse(value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	if parsed.Version() != 7 {
		t.Fatalf("version = %d, want 7", parsed.Version())
	}
	if parsed.Variant() != uuid.RFC4122 {
		t.Fatalf("variant = %v, want RFC 4122", parsed.Variant())
	}
	var timestampBytes [8]byte
	copy(timestampBytes[2:], parsed[0:6])
	embedded := int64(binary.BigEndian.Uint64(timestampBytes[:]))
	if embedded < before || embedded > after+1 {
		t.Fatalf("embedded timestamp = %d, call window = [%d,%d]", embedded, before, after)
	}
}

func TestNewIsUniqueAndStrictlyLexicallyIncreasing(t *testing.T) {
	const count = 10_000
	seen := make(map[string]struct{}, count)
	previous := ""
	for index := 0; index < count; index++ {
		value, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := seen[value]; exists {
			t.Fatalf("duplicate UUID at index %d: %s", index, value)
		}
		if previous != "" && value <= previous {
			t.Fatalf("UUID %d is not increasing: %s <= %s", index, value, previous)
		}
		seen[value] = struct{}{}
		previous = value
	}
}
