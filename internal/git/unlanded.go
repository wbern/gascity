package git

import (
	"fmt"
	"strings"
)

// trunkCandidates are the remote branch names treated as integration targets
// when origin/HEAD is unset, in precedence order. Mirrors the candidate-ref
// fallback DefaultBranch uses, plus develop for repos that integrate there.
var trunkCandidates = []string{"develop", "main", "master"}

// HasUnlandedCommits reports whether HEAD carries committed work whose content
// exists nowhere else. If the probe fails, it returns true to fail closed.
func (g *Git) HasUnlandedCommits() bool {
	has, err := g.HasUnlandedCommitsResult()
	if err != nil {
		return true
	}
	return has
}

// HasUnlandedCommitsResult is like HasUnlandedCommits but preserves git probe
// errors for callers that need to expose the precise failure reason.
//
// It answers the question a worktree-removal safety gate actually asks — would
// removing this tree destroy work? — which plain reachability cannot. Under a
// squash-merge or rebase-merge workflow the integrated commit reaches the
// trunk under a new SHA, so the original is unreachable from every remote
// forever and a reachability probe holds the worktree for the rest of time.
// Patch-id equivalence sees through the rewrite: the content is preserved, so
// the tree is releasable.
//
// The result strictly narrows HasUnpushedCommitsResult and never widens it.
// A commit any remote already carries is landed without further probing, so
// nothing this package previously called safe becomes unsafe here.
//
// Three rules fail closed, because each is a case where "landed" cannot be
// established rather than one where it is disproved:
//   - any probe error
//   - an unpushed merge commit, whose content no single patch-id describes
//   - no resolvable remote trunk to compare against
func (g *Git) HasUnlandedCommitsResult() (bool, error) {
	unpushed, err := g.unpushedCommits()
	if err != nil {
		return false, err
	}
	if len(unpushed) == 0 {
		return false, nil
	}

	merges, err := g.run("log", "--format=%H", "--merges", "HEAD", "--not", "--remotes")
	if err != nil {
		return false, fmt.Errorf("checking unpushed merge commits: %w", err)
	}
	if strings.TrimSpace(merges) != "" {
		return true, nil
	}

	trunks := g.remoteTrunkRefs()
	if len(trunks) == 0 {
		return true, nil
	}

	landed := make(map[string]bool, len(unpushed))
	for _, trunk := range trunks {
		equivalent, err := g.patchEquivalentCommits(trunk)
		if err != nil {
			return false, err
		}
		for sha := range equivalent {
			landed[sha] = true
		}
		if allLanded(unpushed, landed) {
			return false, nil
		}
	}
	return true, nil
}

// allLanded reports whether every commit in shas has a patch-equivalent
// recorded in landed.
func allLanded(shas []string, landed map[string]bool) bool {
	for _, sha := range shas {
		if !landed[sha] {
			return false
		}
	}
	return true
}

// unpushedCommits returns the non-merge commits reachable from HEAD that no
// remote-tracking ref carries.
func (g *Git) unpushedCommits() ([]string, error) {
	out, err := g.run("log", "--format=%H", "--no-merges", "HEAD", "--not", "--remotes")
	if err != nil {
		return nil, fmt.Errorf("checking unpushed commits: %w", err)
	}
	return strings.Fields(out), nil
}

// patchEquivalentCommits returns the commits in trunk..HEAD whose patch already
// has an equivalent on trunk, as reported by git cherry's "-" marker.
func (g *Git) patchEquivalentCommits(trunk string) (map[string]bool, error) {
	out, err := g.run("cherry", trunk, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("comparing patches against %s: %w", trunk, err)
	}
	equivalent := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		mark, sha, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || mark != "-" {
			continue
		}
		equivalent[strings.TrimSpace(sha)] = true
	}
	return equivalent, nil
}

// remoteTrunkRefs returns the remote-tracking refs to test patch equivalence
// against: the origin/HEAD symref target first, then any of trunkCandidates
// that exist. An empty result means no trunk could be resolved, which callers
// treat as fail-closed rather than as "nothing landed".
func (g *Git) remoteTrunkRefs() []string {
	var refs []string
	seen := make(map[string]bool)
	add := func(ref string) {
		if ref != "" && !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}

	if out, err := g.run("symbolic-ref", "refs/remotes/origin/HEAD"); err == nil {
		add(strings.TrimSpace(out))
	}
	for _, candidate := range trunkCandidates {
		ref := "refs/remotes/origin/" + candidate
		if _, err := g.run("show-ref", "--verify", "--quiet", ref); err == nil {
			add(ref)
		}
	}
	return refs
}
