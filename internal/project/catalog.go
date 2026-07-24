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
	roots    []catalogRoot
}

type catalogRoot struct {
	ID   string
	Path string
}

type Directory struct {
	RelativePath string
	Entries      []Project
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

	return &Catalog{projects: projects, ordered: ordered, roots: makeCatalogRoots(canonicalRoots)}, nil
}

func (c *Catalog) Get(id string) (Project, bool) {
	project, ok := c.projects[NormalizeID(id)]
	return project, ok
}

func (c *Catalog) List() []Project {
	return append([]Project(nil), c.ordered...)
}

func (c *Catalog) ResolveDirectory(value string) (Project, error) {
	value = strings.TrimSpace(value)
	if !strings.ContainsAny(value, `/\`) {
		if configured, ok := c.Get(value); ok {
			return configured, nil
		}
	}
	root, relative, err := c.directoryRoot(value)
	if err != nil {
		return Project{}, err
	}
	resolved, err := resolveDirectory(root.Path, relative)
	if err != nil {
		return Project{}, err
	}
	canonicalRelative, err := filepath.Rel(root.Path, resolved)
	if err != nil || canonicalRelative == ".." || strings.HasPrefix(canonicalRelative, ".."+string(filepath.Separator)) {
		return Project{}, errors.New("directory is outside the configured project roots")
	}
	id := filepath.ToSlash(canonicalRelative)
	if id == "." {
		id = "."
	}
	if len(c.roots) > 1 {
		if id == "." {
			id = root.ID
		} else {
			id = root.ID + "/" + id
		}
	}
	return Project{
		ID:           id,
		Name:         filepath.Base(resolved),
		Path:         resolved,
		RelativePath: id,
	}, nil
}

func (c *Catalog) BrowseDirectory(value string) (Directory, error) {
	value = strings.TrimSpace(value)
	if value == "" && len(c.roots) > 1 {
		entries := make([]Project, 0, len(c.roots))
		for _, root := range c.roots {
			entries = append(entries, Project{
				ID: root.ID, Name: filepath.Base(root.Path), Path: root.Path, RelativePath: root.ID,
			})
		}
		return Directory{Entries: entries}, nil
	}
	current, err := c.ResolveDirectory(value)
	if err != nil {
		return Directory{}, err
	}
	entries, err := os.ReadDir(current.Path)
	if err != nil {
		return Directory{}, fmt.Errorf("read directory: %w", err)
	}
	directories := make([]Project, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		candidate, err := canonicalDir(filepath.Join(current.Path, entry.Name()))
		if err != nil {
			continue
		}
		root, ok := c.rootForPath(candidate)
		if !ok {
			continue
		}
		relative, err := filepath.Rel(root.Path, candidate)
		if err != nil {
			continue
		}
		id := filepath.ToSlash(relative)
		if len(c.roots) > 1 {
			id = root.ID + "/" + id
		}
		directories = append(directories, Project{
			ID:           id,
			Name:         entry.Name(),
			Path:         candidate,
			RelativePath: id,
		})
	}
	sort.Slice(directories, func(i, j int) bool {
		return strings.ToLower(directories[i].Name) < strings.ToLower(directories[j].Name)
	})
	return Directory{RelativePath: current.ID, Entries: directories}, nil
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

func makeCatalogRoots(paths []string) []catalogRoot {
	counts := make(map[string]int)
	roots := make([]catalogRoot, 0, len(paths))
	for _, root := range paths {
		base := NormalizeID(filepath.Base(root))
		counts[base]++
		id := base
		if counts[base] > 1 {
			id = fmt.Sprintf("%s-%d", base, counts[base])
		}
		roots = append(roots, catalogRoot{ID: id, Path: root})
	}
	return roots
}

func (c *Catalog) directoryRoot(value string) (catalogRoot, string, error) {
	if len(c.roots) == 0 {
		return catalogRoot{}, "", errors.New("no project roots are configured")
	}
	if filepath.IsAbs(value) {
		return catalogRoot{}, "", errors.New("project paths must be relative to a configured root")
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return catalogRoot{}, "", errors.New("project path escapes the configured root")
	}
	if clean == "." {
		clean = ""
	}
	if len(c.roots) == 1 {
		return c.roots[0], clean, nil
	}
	parts := strings.SplitN(filepath.ToSlash(clean), "/", 2)
	for _, root := range c.roots {
		if strings.EqualFold(parts[0], root.ID) {
			relative := ""
			if len(parts) == 2 {
				relative = filepath.FromSlash(parts[1])
			}
			return root, relative, nil
		}
	}
	return catalogRoot{}, "", errors.New("path must begin with a configured root name")
}

func (c *Catalog) rootForPath(candidate string) (catalogRoot, bool) {
	for _, root := range c.roots {
		if withinRoot(root.Path, candidate) {
			return root, true
		}
	}
	return catalogRoot{}, false
}

func resolveDirectory(root, relative string) (string, error) {
	current := root
	if relative == "" {
		return current, nil
	}
	for _, segment := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
		if segment == "" || segment == "." {
			continue
		}
		if segment == ".." {
			return "", errors.New("project path escapes the configured root")
		}
		exact := filepath.Join(current, segment)
		if info, err := os.Stat(exact); err == nil && info.IsDir() {
			current = exact
		} else {
			entries, readErr := os.ReadDir(current)
			if readErr != nil {
				return "", errors.New("project directory was not found")
			}
			match := ""
			for _, entry := range entries {
				if !strings.EqualFold(entry.Name(), segment) {
					continue
				}
				info, statErr := os.Stat(filepath.Join(current, entry.Name()))
				if statErr != nil || !info.IsDir() {
					continue
				}
				if match != "" {
					return "", errors.New("project path is ambiguous")
				}
				match = entry.Name()
			}
			if match == "" {
				return "", errors.New("project directory was not found")
			}
			current = filepath.Join(current, match)
		}
		canonical, err := canonicalDir(current)
		if err != nil || !withinRoot(root, canonical) {
			return "", errors.New("project path escapes the configured root")
		}
		current = canonical
	}
	return current, nil
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
