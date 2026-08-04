package bdshim

import (
	"fmt"
	"strings"
)

// PRGateCreate identifies the existing bd gate-create shape that needs the
// target-bead self-deadlock check in gc. It is shared by the thin bd shim and
// gc bd so both supported command spellings converge on one parser.
type PRGateCreate struct {
	TargetID string
	PRNumber string
}

// ParsePRGateCreateArgs returns match=true only for a gh:pr gate creation.
// Other gate types remain bd's responsibility. Once gh:pr is selected, unknown
// or incomplete flags fail closed because forwarding an invocation the guard
// cannot prove safe would recreate the bypass this parser exists to remove.
func ParsePRGateCreateArgs(args []string) (PRGateCreate, bool, error) {
	if len(args) < 2 || args[0] != "gate" || args[1] != "create" {
		return PRGateCreate{}, false, nil
	}

	gateType := ""
	for i := 2; i < len(args); i++ {
		switch {
		case args[i] == "--type" || args[i] == "-t":
			if i+1 < len(args) {
				gateType = args[i+1]
				i++
			}
		case strings.HasPrefix(args[i], "--type="):
			gateType = strings.TrimPrefix(args[i], "--type=")
		case strings.HasPrefix(args[i], "-t="):
			gateType = strings.TrimPrefix(args[i], "-t=")
		}
	}
	if gateType != "gh:pr" {
		return PRGateCreate{}, false, nil
	}

	valueFlags := map[string]bool{
		"--actor": true, "--await-id": true, "--blocks": true, "-C": true,
		"--db": true, "--dolt-auto-commit": true, "--reason": true, "-r": true,
		"--timeout": true, "--type": true, "-t": true,
	}
	boolFlags := map[string]bool{
		"--global": true, "--help": true, "-h": true, "--ignore-schema-skew": true,
		"--json": true, "--profile": true, "--quiet": true, "-q": true,
		"--readonly": true, "--sandbox": true, "--verbose": true, "-v": true,
	}
	values := make(map[string]string)
	for i := 2; i < len(args); i++ {
		arg := args[i]
		name, value, hasEquals := strings.Cut(arg, "=")
		if valueFlags[name] {
			if !hasEquals {
				if i+1 >= len(args) {
					return PRGateCreate{}, false, fmt.Errorf("%s requires a value", name)
				}
				i++
				value = args[i]
			}
			values[name] = value
			continue
		}
		if boolFlags[name] && !hasEquals {
			continue
		}
		return PRGateCreate{}, false, fmt.Errorf("cannot safely inspect gate create flag %q", arg)
	}

	targetID := values["--blocks"]
	prNumber := values["--await-id"]
	if targetID == "" || prNumber == "" {
		return PRGateCreate{}, false, fmt.Errorf("gh:pr gate requires --blocks and --await-id")
	}
	if strings.IndexFunc(prNumber, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return PRGateCreate{}, false, fmt.Errorf("gh:pr --await-id must be a numeric PR number")
	}
	return PRGateCreate{TargetID: targetID, PRNumber: prNumber}, true, nil
}
