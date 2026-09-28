package main

import (
	"slices"
	"strings"
)

// rootCommandOptions controls side effects performed while constructing the
// Cobra tree. invocationArgs is always the injected run(args) slice and never
// includes argv[0].
type rootCommandOptions struct {
	invocationArgs            []string
	discoverPackCommands      bool
	eagerPackCommandDiscovery bool
}

func rootCommandOptionsForArgs(args []string) rootCommandOptions {
	command, index, ok := firstRootCommandAt(args)
	discoverPackCommands := !ok || (!rootCommandSkipsPackDiscovery(command) && !managedHookSkipsPackDiscovery(command, args[index+1:]))
	return rootCommandOptions{
		invocationArgs:            append([]string(nil), args...),
		discoverPackCommands:      discoverPackCommands,
		eagerPackCommandDiscovery: discoverPackCommands,
	}
}

// rootCommandSkipsPackDiscovery identifies built-in helpers that remain
// independent of pack config loading while the Beads provider is reloading.
func rootCommandSkipsPackDiscovery(command string) bool {
	switch command {
	case "metrics", "bd", "git-credential", "dolt-state", "dolt-config", "bd-store-bridge":
		return true
	default:
		return false
	}
}

// managedHookSkipsPackDiscovery avoids loading packs before the provider's
// built-in hook invocations. Other children of these command groups still
// discover imports, which may add non-colliding commands beneath them.
func managedHookSkipsPackDiscovery(command string, tail []string) bool {
	switch command {
	case "prime":
		return len(tail) > 0 && tail[0] == "--hook"
	case "nudge":
		return len(tail) > 1 && tail[0] == "drain" && slices.Contains(tail[1:], "--inject")
	case "mail":
		return len(tail) > 1 && tail[0] == "check" && slices.Contains(tail[1:], "--inject")
	case "hook":
		if len(tail) == 0 || tail[0] != "run" {
			return false
		}
		terminator := slices.Index(tail, "--")
		if terminator < 0 || terminator+1 >= len(tail) {
			return false
		}
		child := tail[terminator+1:]
		return managedHookSkipsPackDiscovery(child[0], child[1:])
	default:
		return false
	}
}

func isBuiltinBdInvocation(args []string) bool {
	command, ok := firstRootCommand(args)
	return ok && command == "bd"
}

// firstRootCommand returns the first command word under the root's narrow
// persistent-scope grammar. Unknown flags fail closed because this pre-scan
// cannot know whether a later token is their value. A separate known value
// flag consumes exactly one following token, including "--", matching pflag.
func firstRootCommand(args []string) (string, bool) {
	command, _, ok := firstRootCommandAt(args)
	return command, ok
}

func firstRootCommandAt(args []string) (string, int, bool) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--":
			return "", 0, false
		case isRootPersistentValueFlag(arg):
			if index+1 >= len(args) {
				return "", 0, false
			}
			index++
		case isRootPersistentValueAssignment(arg):
			continue
		case strings.HasPrefix(arg, "-"):
			return "", 0, false
		default:
			return arg, index, true
		}
	}
	return "", 0, false
}

func isRootPersistentValueFlag(arg string) bool {
	switch arg {
	case "--city", "--rig", "--context", "--city-url", "--city-name":
		return true
	default:
		return false
	}
}

func isRootPersistentValueAssignment(arg string) bool {
	name, _, hasValue := strings.Cut(arg, "=")
	return hasValue && isRootPersistentValueFlag(name)
}
