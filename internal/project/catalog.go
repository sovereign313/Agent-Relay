package project

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Project struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	RelativePath string `json:"relative_path"`
}

type Catalog struct {
	projects map[string]Project
	ordered  []Project
}

func Discover(roots []string, aliases map[string]string, maxDepth int) (*Catalog, error) {
	if maxDepth < 1 {
		return nil, errors.New("max discovery depth must be positive")
	}

	var found []foundProject
	var canonicalRoots []string

	for _, configuredRoot := range roots {
		root, err := canonicalDir(configuredRoot)
		if err != nil {
			return nil, fmt.Errorf("project root %q: %w", configuredRoot, err)
		}
		canonicalRoots = append(canonicalRoots, root)
		rootDepth := pathDepth(root)
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == root {
				if isGitRepository(path) {
					found = append(found, makeFound(root, root, path))
				}
				return nil
			}
			depth := pathDepth(path) - rootDepth
			if entry.Name() == ".git" && depth-1 <= maxDepth {
				repoPath := filepath.Dir(path)
				canonical, err := canonicalDir(repoPath)
				if err != nil {
					return err
				}
				if !withinRoot(root, canonical) {
					return fmt.Errorf("discovered repository escapes root: %s", canonical)
				}
				found = append(found, makeFound(root, root, canonical))
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() && depth > maxDepth {
				return filepath.SkipDir
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan project root %q: %w", root, err)
		}
	}

	found = deduplicate(found)
	projects := assignIDs(found)

	for alias, relativeTarget := range aliases {
		aliasID := NormalizeID(alias)
		target, err := resolveAlias(canonicalRoots, relativeTarget)
		if err != nil {
			return nil, fmt.Errorf("project alias %q: %w", alias, err)
		}
		match, ok := findByPath(projects, target)
		if !ok {
			return nil, fmt.Errorf("project alias %q target %q is not a discovered Git repository", alias, relativeTarget)
		}
		if existing, exists := projects[aliasID]; exists && existing.Path != match.Path {
			return nil, fmt.Errorf("project alias %q conflicts with project %q", alias, existing.ID)
		}
		for id, project := range projects {
			if project.Path == match.Path {
				delete(projects, id)
			}
		}
		match.ID = aliasID
		projects[aliasID] = match
	}

	ordered := make([]Project, 0, len(projects))
	seenPaths := make(map[string]bool)
	for _, project := range projects {
		if seenPaths[project.Path] {
			continue
		}
		seenPaths[project.Path] = true
		ordered = append(ordered, project)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	return &Catalog{projects: projects, ordered: ordered}, nil
}

func (c *Catalog) Get(id string) (Project, bool) {
	project, ok := c.projects[NormalizeID(id)]
	return project, ok
}

func (c *Catalog) List() []Project {
	return append([]Project(nil), c.ordered...)
}

func NormalizeID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func canonicalDir(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("not a directory")
	}
	return filepath.Clean(canonical), nil
}

func isGitRepository(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

func makeFound(rootName, root, path string) foundProject {
	relative, _ := filepath.Rel(root, path)
	return foundProject{
		Project: Project{
			Name:         filepath.Base(path),
			Path:         path,
			RelativePath: filepath.ToSlash(relative),
		},
		root: rootName,
	}
}

type foundProject struct {
	Project
	root string
}

func deduplicate(found []foundProject) []foundProject {
	seen := make(map[string]bool)
	result := make([]foundProject, 0, len(found))
	for _, item := range found {
		if seen[item.Path] {
			continue
		}
		seen[item.Path] = true
		result = append(result, item)
	}
	return result
}

func assignIDs(found []foundProject) map[string]Project {
	counts := make(map[string]int)
	for _, item := range found {
		counts[NormalizeID(item.Name)]++
	}
	result := make(map[string]Project)
	for _, item := range found {
		id := NormalizeID(item.Name)
		if counts[id] > 1 {
			id = NormalizeID(filepath.Base(item.root) + "-" + item.RelativePath)
		}
		item.ID = id
		result[id] = item.Project
	}
	return result
}

func resolveAlias(roots []string, target string) (string, error) {
	for _, root := range roots {
		candidate, err := canonicalDir(filepath.Join(root, filepath.FromSlash(target)))
		if err != nil {
			continue
		}
		if withinRoot(root, candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("target %q does not resolve beneath a configured root", target)
}

func findByPath(projects map[string]Project, path string) (Project, bool) {
	for _, project := range projects {
		if project.Path == path {
			return project, true
		}
	}
	return Project{}, false
}

func withinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func pathDepth(path string) int {
	clean := filepath.Clean(path)
	if clean == string(filepath.Separator) {
		return 0
	}
	return len(strings.Split(clean, string(filepath.Separator)))
}
