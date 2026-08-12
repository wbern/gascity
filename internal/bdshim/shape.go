package bdshim

import (
	"sort"
	"strings"
)

const unknownCommandShapeFlag = "unknown"

const unknownCommandVerb = "unknown"

// commandShapeVerbs is the fixed, non-user-controlled vocabulary permitted in
// JSONL. A malformed invocation's first positional can otherwise become the
// apparent verb, leaking an issue ID, path, or free text through the existing
// verb field. New bd commands remain observable as "unknown" until deliberately
// added to this list.
var commandShapeVerbs = map[string]struct{}{
	"admin": {}, "ado": {}, "assign": {}, "audit": {}, "backup": {}, "batch": {}, "blocked": {}, "bootstrap": {}, "branch": {},
	"children": {}, "close": {}, "comment": {}, "comments": {}, "compact": {}, "completion": {}, "config": {}, "context": {},
	"cook": {}, "count": {}, "create": {}, "create-form": {}, "defer": {}, "delete": {}, "dep": {}, "diff": {}, "doctor": {},
	"dolt": {}, "duplicate": {}, "duplicates": {}, "edit": {}, "epic": {}, "export": {}, "federation": {}, "find-duplicates": {},
	"flatten": {}, "forget": {}, "formula": {}, "gate": {}, "gc": {}, "github": {}, "gitlab": {}, "graph": {}, "help": {},
	"heartbeat": {}, "history": {}, "hooks": {}, "human": {}, "import": {}, "info": {}, "init": {}, "init-safety": {}, "jira": {},
	"kv": {}, "label": {}, "link": {}, "linear": {}, "lint": {}, "list": {}, "log": {}, "mail": {}, "memories": {},
	"merge-slot": {}, "metrics": {}, "migrate": {}, "mol": {}, "note": {}, "notion": {}, "onboard": {}, "orphans": {}, "ping": {},
	"preflight": {}, "prime": {}, "priority": {}, "promote": {}, "prune": {}, "purge": {}, "q": {}, "query": {}, "quickstart": {},
	"ready": {}, "recall": {}, "recompute-blocked": {}, "remember": {}, "release-if-current": {}, "rename": {}, "rename-prefix": {},
	"repo": {}, "reopen": {}, "restore": {}, "rules": {}, "search": {}, "set-state": {}, "setup": {}, "ship": {}, "show": {},
	"sql": {}, "stale": {}, "state": {}, "status": {}, "statuses": {}, "supersede": {}, "swarm": {}, "sync": {}, "tag": {},
	"todo": {}, "types": {}, "undefer": {}, "update": {}, "upgrade": {}, "vc": {}, "version": {}, "where": {}, "worktree": {},
}

// commandShapeFlags is the closed, value-free vocabulary emitted into the
// world-readable shim JSONL. Values and positionals are deliberately absent:
// issue IDs, paths, labels, descriptions, metadata, credentials, and arbitrary
// future flags must never turn a routing observation into a data leak.
var commandShapeFlags = map[string]string{
	"--all":               "--all",
	"--api-key":           "--api-key",
	"--assignee":          "--assignee",
	"--bearer":            "--bearer",
	"-a":                  "--assignee",
	"--claim":             "--claim",
	"--defer-until":       "--defer-until",
	"--description":       "--description",
	"-d":                  "--description",
	"--ephemeral":         "--ephemeral",
	"--exclude-type":      "--exclude-type",
	"--force":             "--force",
	"--from":              "--from",
	"--has-metadata-key":  "--has-metadata-key",
	"--help":              "--help",
	"-h":                  "--help",
	"--include-ephemeral": "--include-ephemeral",
	"--json":              "--json",
	"--label":             "--label",
	"-l":                  "--label",
	"--limit":             "--limit",
	"-n":                  "--limit",
	"--metadata":          "--metadata",
	"--metadata-field":    "--metadata-field",
	"--no-assignee":       "--no-assignee",
	"--no-history":        "--no-history",
	"--note":              "--note",
	"--notes":             "--notes",
	"--offset":            "--offset",
	"--parent":            "--parent",
	"--password":          "--password",
	"--persistent":        "--persistent",
	"--priority":          "--priority",
	"--readonly":          "--readonly",
	"--remote-password":   "--remote-password",
	"--remove-label":      "--remove-label",
	"--sandbox":           "--sandbox",
	"--set-metadata":      "--set-metadata",
	"--sort":              "--sort",
	"--status":            "--status",
	"--summary-json":      "--summary-json",
	"-s":                  "--status",
	"--token":             "--token",
	"--type":              "--type",
	"-t":                  "--type",
	"--title":             "--title",
	"--unset-metadata":    "--unset-metadata",
	"--unassigned":        "--unassigned",
	"--version":           "--version",
}

// commandShapeValueFlags is the subset of the safe vocabulary that consumes a
// following token when not written --flag=value. Skipping that token is crucial:
// a legitimate value may begin with "--" and must not be mistaken for a flag
// and emitted into the shape.
var commandShapeValueFlags = map[string]bool{
	"--assignee":         true,
	"--api-key":          true,
	"--bearer":           true,
	"--defer-until":      true,
	"--description":      true,
	"--exclude-type":     true,
	"--from":             true,
	"--has-metadata-key": true,
	"--label":            true,
	"--limit":            true,
	"--metadata":         true,
	"--metadata-field":   true,
	"--note":             true,
	"--notes":            true,
	"--offset":           true,
	"--parent":           true,
	"--password":         true,
	"--priority":         true,
	"--remote-password":  true,
	"--remove-label":     true,
	"--set-metadata":     true,
	"--sort":             true,
	"--status":           true,
	"--token":            true,
	"--type":             true,
	"--title":            true,
	"--unset-metadata":   true,
}

// CommandShape returns a stable, redacted description of an invocation's flag
// structure. It never includes positional arguments or option values. Known
// aliases normalize to one long flag, order and duplication do not matter, and
// any flag outside the fixed safe vocabulary collapses to "unknown" to bound
// cardinality and avoid leaking future caller-controlled tokens.
func CommandShape(args []string) string {
	seen := make(map[string]struct{})
	for i := 0; i < len(args); i++ {
		token := args[i]
		if !strings.HasPrefix(token, "-") || token == "-" {
			continue
		}
		name, _, hasInlineValue := strings.Cut(token, "=")
		canonical, ok := commandShapeFlags[name]
		if !ok {
			seen[unknownCommandShapeFlag] = struct{}{}
			// Unknown options may consume a value that happens to begin with
			// "-". Conservatively skip the next token so that value cannot be
			// misclassified as another observable flag. This favors privacy over
			// a more detailed (but unsafe) shape for future bd options.
			if !hasInlineValue && i+1 < len(args) {
				i++
			}
			continue
		}
		seen[canonical] = struct{}{}
		if !hasInlineValue && commandShapeValueFlags[canonical] && i+1 < len(args) {
			i++
		}
	}
	if len(seen) == 0 {
		return "flags=none"
	}
	flags := make([]string, 0, len(seen))
	for flag := range seen {
		flags = append(flags, flag)
	}
	sort.Strings(flags)
	return "flags=" + strings.Join(flags, ",")
}

// CommandVerb returns a fixed-vocabulary form of verb suitable for the
// world-readable shim JSONL. It never returns caller-controlled text.
func CommandVerb(verb string) string {
	if _, ok := commandShapeVerbs[verb]; ok {
		return verb
	}
	return unknownCommandVerb
}
