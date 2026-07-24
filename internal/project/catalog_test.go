package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverAliasesNestedRepositories(t *testing.T) {
	root := t.TempDir()
	harness := filepath.Join(root, "WaspLogic", "HarnessStudio")
	rapid := filepath.Join(root, "WaspLogic", "RapidRFQ")
	for _, path := range []string{harness, rapid} {
		if err := os.MkdirAll(filepath.Join(path, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	catalog, err := Discover(
		[]string{root},
		map[string]string{
			"harness-studio": "WaspLogic/HarnessStudio",
			"rapidrfq":       "WaspLogic/RapidRFQ",
		},
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.List()) != 2 {
		t.Fatalf("discovered %d projects, want 2", len(catalog.List()))
	}
	if project, ok := catalog.Get("harness-studio"); !ok || project.Path != harness {
		t.Fatalf("harness alias = %#v, %v", project, ok)
	}
	if project, ok := catalog.Get("rapidrfq"); !ok || project.Path != rapid {
		t.Fatalf("rapidrfq alias = %#v, %v", project, ok)
	}
}

func TestDiscoverRejectsAliasOutsideRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "projects")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(filepath.Join(root, "inside", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := Discover([]string{root}, map[string]string{"escape": "../outside"}, 3)
	if err == nil {
		t.Fatal("Discover allowed an alias outside its configured root")
	}
}

func TestDiscoverSupportsGitWorktreeFile(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "worktree")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: elsewhere"), 0o600); err != nil {
		t.Fatal(err)
	}

	catalog, err := Discover([]string{root}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog.Get("worktree"); !ok {
		t.Fatal("worktree repository was not discovered")
	}
}
