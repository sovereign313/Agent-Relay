package agent

import (
	"regexp"
	"testing"
)

func TestNewSessionIDReturnsUUID(t *testing.T) {
	first, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !pattern.MatchString(first) {
		t.Fatalf("session ID = %q", first)
	}
	if first == second {
		t.Fatal("generated duplicate session IDs")
	}
}
