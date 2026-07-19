package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The bd shim is a hot-path thin client: workers invoke it once per `bd` call,
// so it must start fast and stay small (see gcw-wd6g and gcw-fqat, which cut it
// from ~18MB to ~7.4MB by keeping the huma server and the OTLP/grpc exporter out
// of its graph). These tests are the regression guard for that win. They are the
// only thing that will notice if a well-meaning import silently re-bloats the
// shim, since neither the compiler nor golangci-lint flags dependency growth.

// bdshimPkg is the import path of the shim's main package, the root of the
// dependency walk below.
const bdshimPkg = "github.com/gastownhall/gascity/cmd/bdshim"

// forbiddenImports are packages that must never appear anywhere in the shim's
// import graph. Each maps to the reason it is banned, which is printed on
// failure so the fix is obvious. grpc is deliberately NOT listed here: it is
// pulled transitively by github.com/steveyegge/beads but the linker
// dead-code-eliminates it because nothing reachable calls it — so it is guarded
// by TestBdshimBinaryStaysSmall (a reachable-grpc regression balloons the
// binary), not by import presence.
var forbiddenImports = map[string]string{
	"github.com/gastownhall/gascity/internal/api":                  "the huma control-plane SERVER; the shim talks to the controller via internal/beadclient (a leaf client), never the server package",
	"github.com/gastownhall/gascity/internal/telemetry/otlpexport": "the OTLP/HTTP exporter, which links ~160 grpc packages; only telemetry-exporting binaries (gc) may import it, never record-only tools",
}

// maxShimBytes is the size ceiling for the stripped shim binary. It is
// production-representative (CGO disabled, -s -w) and generous: the shim is
// ~7.4MB today, and the dominant regression it guards against — reactivating
// the OTLP/grpc exporter on a reachable path — adds ~10MB, so a 12MB ceiling
// leaves headroom for honest growth while still catching that class of mistake.
const maxShimBytes = 12 << 20

// TestBdshimForbiddenImports fails if the shim's transitive import graph
// contains any banned package, printing the exact import chain that pulled it
// in so the offending edge is easy to find and cut.
func TestBdshimForbiddenImports(t *testing.T) {
	graph := loadImportGraph(t)
	for pkg, why := range forbiddenImports {
		if chain := shortestImportChain(graph, bdshimPkg, pkg); chain != nil {
			t.Errorf("bd shim must not import %s\n  reason: %s\n  chain:  %s",
				pkg, why, strings.Join(chain, "\n       → "))
		}
	}
}

// TestBdshimBinaryStaysSmall builds the shim exactly as production does
// (CGO_ENABLED=0, -ldflags "-s -w") and fails if it exceeds maxShimBytes. This
// is the backstop for regressions that keep the import graph unchanged but make
// a heavy dependency reachable again (e.g. calling an OTLP exporter), which the
// import guard cannot see. Skipped in -short mode because it links a binary.
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
	if info.Size() > maxShimBytes {
		t.Errorf("stripped bd shim is %.1f MiB, over the %.0f MiB ceiling — a heavy dependency became reachable again;\nrun `make deadcode` and `go tool nm` / a size analyzer to find what grew",
			float64(info.Size())/(1<<20), float64(maxShimBytes)/(1<<20))
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
