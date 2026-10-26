package main

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// builtinDisposableIgnored is the ignored content the closed-bead reaper may
// delete with a worktree without operator configuration: files gc itself
// materializes into every task worktree, and dependency/bytecode caches that
// are rebuilt on demand. Anything project-specific belongs in
// daemon.auto_reap_disposable_ignored instead. Symlinks are disposable
// everywhere (removal unlinks them and never touches the target), so the
// materialized skill links under .claude/skills/ and .agents/skills/ need no
// entry here. Files gc merges into rather than owns outright
// (.codex/hooks.json, .codex/config.toml, .mcp.json keep custom content
// across gc's rewrites) are deliberately absent.
var builtinDisposableIgnored = []string{
	".beads/redirect",
	".gc/settings.json",
	".gc/mcp-managed/",
	".gc/mcp-locks/",
	".gc/tmp/",
	".codex/.hooks.json.gascity.lock",
	".claude/skills/.gc-skill-ownership.json",
	".agents/skills/.gc-skill-ownership.json",
	"**/node_modules/",
	"**/__pycache__/",
}

// nonDisposableIgnored returns the ignored paths under worktreePath that the
// reaper must not delete, given git's (possibly collapsed) ignored roots and
// operator patterns that extend builtinDisposableIgnored. An empty result
// means every ignored byte is disposable.
//
// git collapses a wholly-ignored directory to one "dir/" root, which says
// nothing about what lies beneath it — and a non-force `git worktree remove`
// deletes ignored content wholesale, nested worktrees and repositories
// included. So the classification is made on the filesystem, never on git's
// path alone:
//   - a symlink is disposable and never followed;
//   - an entry named .git (a nested repository or linked worktree) is kept;
//   - a path matching a pattern is disposable, but a matched directory is
//     still walked and kept if anything inside it is a .git;
//   - an unmatched directory is disposable only if all of its children are;
//   - an unmatched file is kept.
//
// Every filesystem error keeps the path (fail closed).
//
// The work is split so a kept tree stays cheap on every controller tick: a
// first pass classifies everything outside matched directories and stops at
// the first kept path; only when that pass keeps nothing are the matched
// directories (node_modules can hold hundreds of thousands of entries) walked
// for nested .git entries.
func nonDisposableIgnored(worktreePath string, roots, extraPatterns []string) []string {
	patterns := make([]string, 0, len(builtinDisposableIgnored)+len(extraPatterns))
	patterns = append(patterns, builtinDisposableIgnored...)
	patterns = append(patterns, extraPatterns...)

	var kept, matchedDirs []string
	for _, root := range roots {
		rel := strings.TrimSuffix(filepath.ToSlash(root), "/")
		if rel == "" || rel == "." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
			kept = append(kept, root)
			continue
		}
		if p := firstNonDisposable(worktreePath, rel, patterns, &matchedDirs); p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		for _, rel := range matchedDirs {
			abs := filepath.Join(worktreePath, filepath.FromSlash(rel))
			if p := firstNestedGit(abs, rel); p != "" {
				kept = append(kept, p)
			}
		}
	}
	sort.Strings(kept)
	return kept
}

// walkMatchedDirsFn walks a matched directory for nested .git entries. A
// package variable so a test can prove a kept tree never pays for the walk.
var walkMatchedDirsFn = filepath.WalkDir

// firstNonDisposable returns rel (or the first offending path beneath it) if
// it must be kept, or "" if it is disposable as far as a shallow pass can
// tell. Matched directories are appended to matchedDirs for the deep
// nested-.git walk instead of being walked here.
func firstNonDisposable(worktreePath, rel string, patterns []string, matchedDirs *[]string) string {
	abs := filepath.Join(worktreePath, filepath.FromSlash(rel))
	info, err := os.Lstat(abs)
	if err != nil {
		return rel
	}
	if strings.EqualFold(path.Base(rel), ".git") {
		return rel
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return ""
	}
	isDir := info.IsDir()
	if isDir && looksLikeBareRepository(abs) {
		return rel
	}
	if matchesDisposable(rel, isDir, patterns) {
		if isDir {
			*matchedDirs = append(*matchedDirs, rel)
		}
		return ""
	}
	if !isDir {
		return rel
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return rel
	}
	for _, e := range entries {
		if p := firstNonDisposable(worktreePath, rel+"/"+e.Name(), patterns, matchedDirs); p != "" {
			return p
		}
	}
	return ""
}

// errNestedGit stops a walk at the first nested repository or worktree.
var errNestedGit = errors.New("nested .git")

// firstNestedGit walks a matched directory without following symlinks and
// returns the worktree-relative path of the first .git entry, or of the first
// unreadable entry, or "" if there is neither.
func firstNestedGit(absDir, relDir string) string {
	found := ""
	err := walkMatchedDirsFn(absDir, func(p string, d fs.DirEntry, walkErr error) error {
		sub, relErr := filepath.Rel(absDir, p)
		if relErr != nil {
			found = relDir
			return relErr
		}
		relPath := relDir
		if sub != "." {
			relPath = relDir + "/" + filepath.ToSlash(sub)
		}
		if walkErr != nil {
			found = relPath
			return walkErr
		}
		if strings.EqualFold(d.Name(), ".git") || (d.IsDir() && looksLikeBareRepository(p)) {
			found = relPath
			return errNestedGit
		}
		return nil
	})
	if err != nil && found == "" {
		return relDir
	}
	return found
}

// matchesDisposable reports whether rel matches any pattern. A trailing "/"
// restricts the pattern to directories; a leading "**/" lets it match at any
// segment boundary; otherwise it is anchored at the worktree root.
func matchesDisposable(rel string, isDir bool, patterns []string) bool {
	for _, pat := range patterns {
		pat = strings.TrimSpace(filepath.ToSlash(pat))
		dirOnly := strings.HasSuffix(pat, "/")
		pat = strings.TrimSuffix(pat, "/")
		if pat == "" || (dirOnly && !isDir) {
			continue
		}
		if rest, anyDepth := strings.CutPrefix(pat, "**/"); anyDepth {
			segs := strings.Split(rel, "/")
			for i := range segs {
				if ok, _ := path.Match(rest, strings.Join(segs[i:], "/")); ok {
					return true
				}
			}
			continue
		}
		if ok, _ := path.Match(pat, rel); ok {
			return true
		}
	}
	return false
}

// describeNonDisposable renders kept paths for a protection reason, naming at
// most three so the reason stays one line.
func describeNonDisposable(kept []string) string {
	const shown = 3
	if len(kept) <= shown {
		return strings.Join(kept, ", ")
	}
	return strings.Join(kept[:shown], ", ") + " (+" + strconv.Itoa(len(kept)-shown) + " more)"
}

// looksLikeBareRepository conservatively protects Git object/ref storage even
// when a nested repository has no .git marker.
func looksLikeBareRepository(dir string) bool {
	head, e1 := os.Lstat(filepath.Join(dir, "HEAD"))
	objects, e2 := os.Lstat(filepath.Join(dir, "objects"))
	cfg, e3 := os.Lstat(filepath.Join(dir, "config"))
	return e1 == nil && e2 == nil && e3 == nil && head.Mode().IsRegular() && objects.IsDir() && cfg.Mode().IsRegular()
}
