package bdshim

import "strings"

// DepAdd is a parsed `bd dep add` invocation: FromID depends on (is blocked
// by) ToID.
type DepAdd struct {
	FromID string
	ToID   string
}

// ParseDepAddArgs extracts the two bead IDs from a `bd dep add` invocation.
//
// The depends-on side may be positional or carried by --blocked-by /
// --depends-on, which bd documents as aliases meaning the same thing:
//
//	bd dep add issue-123 issue-456
//	bd dep add issue-123 --blocked-by issue-456
//	bd dep add issue-123 --depends-on=issue-456
//
// It returns matches=false for anything that is not a two-ID `dep add`,
// INCLUDING the bulk --file form. That is a deliberate limit rather than an
// oversight: --file takes newline-delimited JSON and may be "-" (stdin), which
// cannot be read here without consuming the input the real bd needs. The
// caller therefore cannot guard bulk wiring, and that gap is documented at the
// call site instead of being hidden behind a half-parse.
func ParseDepAddArgs(args []string) (DepAdd, bool) {
	if len(args) < 3 || args[0] != "dep" || args[1] != "add" {
		return DepAdd{}, false
	}

	// Value-consuming flags for `bd dep add`, so a flag's VALUE is never
	// mistaken for a positional bead ID. Sourced from `bd dep add --help`.
	valueFlags := map[string]bool{
		"--blocked-by": true, "--depends-on": true, "--type": true,
		"--file": true, "-C": true, "--db": true, "--dolt-auto-commit": true,
	}

	var positional []string
	dependsOn := ""
	for i := 2; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--file" || strings.HasPrefix(arg, "--file="):
			// Bulk wiring — cannot be inspected here. See the doc comment.
			return DepAdd{}, false
		case arg == "--blocked-by" || arg == "--depends-on":
			if i+1 >= len(args) {
				return DepAdd{}, false
			}
			dependsOn = args[i+1]
			i++
		case strings.HasPrefix(arg, "--blocked-by="):
			dependsOn = strings.TrimPrefix(arg, "--blocked-by=")
		case strings.HasPrefix(arg, "--depends-on="):
			dependsOn = strings.TrimPrefix(arg, "--depends-on=")
		case strings.HasPrefix(arg, "-"):
			if valueFlags[arg] {
				i++ // skip its value
				continue
			}
			if strings.Contains(arg, "=") {
				continue
			}
			// An unrecognized bare flag might consume the next token. Refuse
			// to guess which positional is which — the caller treats a
			// non-match as "cannot verify", not as "safe".
			return DepAdd{}, false
		default:
			positional = append(positional, arg)
		}
	}

	if dependsOn != "" {
		if len(positional) != 1 {
			return DepAdd{}, false
		}
		return DepAdd{FromID: positional[0], ToID: dependsOn}, true
	}
	if len(positional) != 2 {
		return DepAdd{}, false
	}
	return DepAdd{FromID: positional[0], ToID: positional[1]}, true
}
