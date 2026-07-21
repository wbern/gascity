package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

func addPackCommandsToRoot(root *cobra.Command, entries []config.PackCommandInfo, cityPath, cityName string, stdout, stderr io.Writer) {
	discovered := make([]config.DiscoveredCommand, 0, len(entries))
	for _, entry := range entries {
		discovered = append(discovered, discoveredCommandFromPackCommandInfo(entry))
	}
	addDiscoveredCommandsToRoot(root, discovered, cityPath, cityName, stdout, stderr, true)
}

func discoveredCommandFromPackCommandInfo(info config.PackCommandInfo) config.DiscoveredCommand {
	helpFile := strings.TrimSpace(info.Entry.LongDescription)
	if helpFile != "" && !filepath.IsAbs(helpFile) {
		helpFile = filepath.Join(info.PackDir, helpFile)
	}
	return config.DiscoveredCommand{
		Name:        info.Entry.Name,
		Command:     []string{info.Entry.Name},
		Description: info.Entry.Description,
		RunScript:   info.Entry.Script,
		HelpFile:    helpFile,
		SourceDir:   info.PackDir,
		PackDir:     info.PackDir,
		PackName:    info.PackName,
		BindingName: info.PackName,
	}
}

// quietLoadCityConfig loads city config with log output suppressed.
// ExpandCityPacks logs "not found, skipping" for uncached remote packs
// which is confusing during cobra command-tree setup (before gc start
// has fetched them). The expander already skips missing packs gracefully;
// we just silence the log noise.
func quietLoadCityConfig(cityPath string) (*config.City, error) {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prev)
	return loadCityConfig(cityPath, io.Discard)
}

// registerPackCommands attempts to discover the city, load config, and
// register pack-provided CLI commands as top-level subcommands. Fails
// silently if not in a city or config fails to load — core commands
// always work.
//
// A built-in bd invocation does not need this discovery: packs cannot shadow
// the built-in command, and doBd loads the city config later for its actual
// routing and safety checks. Skipping this optional first load removes one full
// config expansion from every gc bd command without changing its behavior.
func registerPackCommands(root *cobra.Command, stdout, stderr io.Writer, invocationArgs []string) {
	if isBuiltinBdInvocation(invocationArgs) {
		return
	}
	cityPath, err := resolveCity()
	if err != nil {
		return
	}
	cfg, err := quietLoadCityConfig(cityPath)
	if err != nil {
		return
	}

	if len(cfg.PackCommands) == 0 {
		return
	}

	addDiscoveredCommandsToRoot(root, cfg.PackCommands, cityPath, loadedCityName(cfg, cityPath), stdout, stderr, false)
}

func isBuiltinBdInvocation(args []string) bool {
	command, ok := firstRootCommand(args)
	return ok && command == "bd"
}

// firstRootCommand identifies the first command token after supported root
// persistent flags. It deliberately returns false for unknown or malformed
// root syntax, preserving eager discovery whenever command interpretation is
// uncertain.
func firstRootCommand(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return "", false
		case arg == "--city" || arg == "--rig":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", false
			}
			i++
		case strings.HasPrefix(arg, "--city="):
			if strings.TrimSpace(strings.TrimPrefix(arg, "--city=")) == "" {
				return "", false
			}
		case strings.HasPrefix(arg, "--rig="):
			if strings.TrimSpace(strings.TrimPrefix(arg, "--rig=")) == "" {
				return "", false
			}
		case strings.HasPrefix(arg, "-"):
			return "", false
		default:
			return arg, true
		}
	}
	return "", false
}

// coreCommandNames returns the set of built-in command names that packs
// must not shadow.
func coreCommandNames(root *cobra.Command) map[string]bool {
	names := make(map[string]bool)
	for _, c := range root.Commands() {
		names[c.Name()] = true
		for _, alias := range c.Aliases {
			names[alias] = true
		}
	}
	// Also reserve "help" and "completion" which cobra may add.
	names["help"] = true
	names["completion"] = true
	return names
}

// stdin returns os.Stdin. Extracted for testability (tests can override).
var stdin = func() io.Reader { return os.Stdin }

// expandScriptTemplate expands Go text/template variables in the script
// path. On any error, returns the raw script string (graceful fallback).
func expandScriptTemplate(script, cityPath, cityName, packDir string) string {
	if !strings.Contains(script, "{{") {
		return script
	}
	ctx := SessionSetupContext{
		CityRoot:  cityPath,
		CityName:  cityName,
		ConfigDir: packDir,
	}
	tmpl, err := template.New("script").Parse(script)
	if err != nil {
		return script
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return script
	}
	return buf.String()
}

// tryPackCommandFallback is a lazy fallback for the root command's RunE.
// If eager discovery missed a pack command (e.g. config changed), try
// one more time. Returns true if a pack command was found and executed.
func tryPackCommandFallback(args []string, stdout, stderr io.Writer) bool {
	if len(args) == 0 {
		return false
	}

	cityPath, err := resolveCity()
	if err != nil {
		return false
	}
	cfg, err := quietLoadCityConfig(cityPath)
	if err != nil {
		return false
	}

	return tryDiscoveredCommandFallback(args, cfg, cityPath, stdout, stderr)
}
