package logging

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingFileKeepsBoundedBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.log")
	writer, err := newRotatingFile(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "12345678" {
		t.Fatalf("backup = %q", backup)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "abcdef" {
		t.Fatalf("current = %q", current)
	}
}
