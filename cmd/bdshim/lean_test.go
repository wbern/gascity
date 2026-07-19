package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Dependency-footprint guards for the bd shim. The shim is a hot-path thin
// client (one process per `bd` call), so it must not link the control-plane
// server or the OTLP/grpc telemetry exporter. The compiler and golangci-lint do
// not flag dependency growth, so these guard it explicitly:
//
//   - TestBdshimForbiddenImports (hard gate): forbids the server and OTLP
//     exporter packages anywhere in the import graph, failing with the exact
//     import chain.
//   - TestBdshimBinaryStaysSmall (advisory): reports the built binary size and
//     warns past a soft budget without failing.

// bdshimPkg is the import path of the shim's main package, the root of the
// dependency walk below.
const bdshimPkg = "github.com/gastownhall/gascity/cmd/bdshim"

// forbiddenImports lists packages that must never appear in the shim's import
// graph, each mapped to the reason it is banned (printed on failure). grpc
// itself is not listed: it is pulled transitively via github.com/steveyegge/
// beads but linker-eliminated because nothing reachable calls it. The realistic
// ways it becomes reachable are importing the server or the OTLP exporter, both
// forbidden here.
var forbiddenImports = map[string]string{
	"github.com/gastownhall/gascity/internal/api":                  "the huma control-plane SERVER; the shim talks to the controller via internal/beadclient (a leaf client), never the server package",
	"github.com/gastownhall/gascity/internal/telemetry/otlpexport": "the OTLP/HTTP exporter, which links ~160 grpc packages; only telemetry-exporting binaries (gc) may import it, never record-only tools",
}

// shimSoftBudgetBytes is the advisory size budget for the stripped shim. The
// floor for a Go binary that makes an HTTPS request is ~5.5MB (runtime, TLS, the
// FIPS crypto module); the shim's own code adds ~1.9MB. Crossing this budget
// does not fail the build; it flags that a heavy dependency may have become
// reachable.
const shimSoftBudgetBytes = 10 << 20

// TestBdshimForbiddenImports fails if the shim's transitive import graph
// contains any package in forbiddenImports, printing the exact import chain that
// pulled it in.
func TestBdshimForbiddenImports(t *testing.T) {
	graph := loadImportGraph(t)
	for pkg, why := range forbiddenImports {
		if chain := shortestImportChain(graph, bdshimPkg, pkg); chain != nil {
			t.Errorf("bd shim must not import %s\n  reason: %s\n  chain:  %s",
				pkg, why, strings.Join(chain, "\n       → "))
		}
	}
}

// TestBdshimBinaryStaysSmall is advisory: it builds the shim as production does
// (CGO_ENABLED=0, -ldflags "-s -w"), logs the size, and warns past
// shimSoftBudgetBytes without failing. Skipped in -short (it links a binary).
func TestBdshimBinaryStaysSmall(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary-size build in -short mode")
	}
	out := filepath.Join(t.TempDir(), "bdshim")
	cmd := exec.Command("go", "build", "-ldflags", "-s -w", "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if buildOut, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building shim: %v\n%s", err, buildOut)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat built shim: %v", err)
	}
	sizeMiB := float64(info.Size()) / (1 << 20)
	t.Logf("stripped bd shim: %.1f MiB (soft budget %.0f MiB, floor ~5.5 MiB)", sizeMiB, float64(shimSoftBudgetBytes)/(1<<20))
	if info.Size() > shimSoftBudgetBytes {
		// Advisory only: report, do not fail. A heavy dependency likely became
		// reachable; `make deadcode` and a size analyzer show what grew.
		t.Logf("ADVISORY: bd shim is %.1f MiB, over the %.0f MiB soft budget — check for a heavy dependency regression.",
			sizeMiB, float64(shimSoftBudgetBytes)/(1<<20))
	}
}

// loadImportGraph returns each package in the shim's dependency closure mapped
// to its direct imports, via `go list -deps -json`.
func loadImportGraph(t *testing.T) map[string][]string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", "-json", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	graph := make(map[string][]string)
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var pkg struct {
			ImportPath string
			Imports    []string
		}
		if err := dec.Decode(&pkg); err != nil {
			t.Fatalf("decoding go list output: %v", err)
		}
		graph[pkg.ImportPath] = pkg.Imports
	}
	return graph
}

// shortestImportChain returns the shortest import path from → to within graph
// (inclusive of both endpoints), or nil if to is unreachable. Breadth-first, so
// the chain shown on failure is the most direct route the dependency crept in.
func shortestImportChain(graph map[string][]string, from, to string) []string {
	if from == to {
		return []string{from}
	}
	prev := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range graph[cur] {
			if _, seen := prev[next]; seen {
				continue
			}
			prev[next] = cur
			if next == to {
				return buildChain(prev, from, to)
			}
			queue = append(queue, next)
		}
	}
	return nil
}

// buildChain reconstructs the from→to path from the BFS predecessor map.
func buildChain(prev map[string]string, from, to string) []string {
	var chain []string
	for at := to; at != ""; at = prev[at] {
		chain = append(chain, at)
		if at == from {
			break
		}
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}
