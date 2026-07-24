package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckWritableDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doctor", "state")
	if err := checkWritableDirectory(path); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("doctor left temporary files: %#v", entries)
	}
}

func TestUsageIncludesDoctor(t *testing.T) {
	var output bytes.Buffer
	if err := run([]string{"help"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "agent-relay doctor") {
		t.Fatalf("usage does not include doctor:\n%s", output.String())
	}
}
