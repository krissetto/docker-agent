package gitreview

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

const maxIgnoreFileBytes = 1 << 20

// ignoreRules models per-directory gitignore precedence without the process-
// global matcher cache used by general file completion. A refresh reads fresh
// rule files and nested rules take precedence over their parents.
type ignoreRules struct{ patterns []gitignore.Pattern }

func (r ignoreRules) child(root, dir, agentRoot string) (ignoreRules, error) {
	out := ignoreRules{patterns: append([]gitignore.Pattern(nil), r.patterns...)}
	names := []string{".gitignore"}
	if filepath.Clean(dir) == filepath.Clean(agentRoot) {
		names = append(names, ".agentsignore")
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return out, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return out, err
		}
		data, err := io.ReadAll(io.LimitReader(f, maxIgnoreFileBytes+1))
		closeErr := f.Close()
		if err != nil {
			return out, err
		}
		if closeErr != nil {
			return out, closeErr
		}
		if len(data) > maxIgnoreFileBytes {
			return out, ErrTooLarge
		}
		domain := []string(nil)
		if name == ".gitignore" {
			rel, e := filepath.Rel(root, dir)
			if e != nil {
				return out, e
			}
			if rel != "." {
				domain = strings.Split(filepath.ToSlash(rel), "/")
			}
		}
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			out.patterns = append(out.patterns, gitignore.ParsePattern(line, domain))
		}
		if err := scanner.Err(); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (r ignoreRules) matches(root, path string, isDir bool) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return gitignore.NewMatcher(r.patterns).Match(parts, isDir)
}

// walkContained scans directories incrementally. No ReadDir call materializes
// an unbounded directory, and ignore rules are scoped to the containing
// checkout rather than a process-global cached repository.
func walkContained(ctx context.Context, traversalRoot, ruleRoot, agentRoot string, initial ignoreRules, maxEntries, maxDepth int, visit func(path, rel string, entry os.DirEntry, rules ignoreRules) (descend bool, err error)) (truncated bool, err error) {
	entries := 0
	var walk func(string, string, ignoreRules, int) error
	walk = func(dir, rel string, inherited ignoreRules, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > maxDepth {
			return nil
		}
		rules, err := inherited.child(ruleRoot, dir, agentRoot)
		if err != nil {
			return err
		}
		f, err := os.Open(dir)
		if err != nil {
			return nil
		}
		defer f.Close()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			batch, readErr := f.ReadDir(128)
			for _, de := range batch {
				entries++
				if entries > maxEntries {
					truncated = true
					return nil
				}
				if de.Name() == ".git" || heavyDirectories[de.Name()] || de.Type()&os.ModeSymlink != 0 {
					continue
				}
				path := filepath.Join(dir, de.Name())
				childRel := filepath.Join(rel, de.Name())
				info, infoErr := de.Info()
				if infoErr != nil {
					continue
				}
				isDir := info.IsDir()
				if rules.matches(ruleRoot, path, isDir) {
					continue
				}
				descend, err := visit(path, childRel, de, rules)
				if err != nil {
					return err
				}
				if isDir && descend {
					if err := walk(path, childRel, rules, depth+1); err != nil {
						return err
					}
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	return truncated, walk(traversalRoot, ".", initial, 0)
}

func ignoreRulesForPath(root, dir, agentRoot string) (ignoreRules, error) {
	if !within(root, dir) {
		return ignoreRules{}, ErrOutsideScope
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return ignoreRules{}, err
	}
	cur := root
	rules := ignoreRules{}
	rules, err = rules.child(root, root, agentRoot)
	if err != nil {
		return rules, err
	}
	if rel == "." {
		return rules, nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		rules, err = rules.child(root, cur, agentRoot)
		if err != nil {
			return rules, err
		}
	}
	return rules, nil
}
