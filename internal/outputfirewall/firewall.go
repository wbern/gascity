// Package outputfirewall bounds managed-session known-read output before it
// reaches an agent transcript.
package outputfirewall

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	managedEnv       = "GC_MANAGED_OUTPUT_FIREWALL"
	budgetEnv        = "GC_MANAGED_OUTPUT_FIREWALL_BUDGET"
	verbsEnv         = "GC_MANAGED_OUTPUT_FIREWALL_READ_VERBS"
	spillRootEnv     = "GC_MANAGED_OUTPUT_FIREWALL_SPILL_ROOT"
	spillPathEnv     = "GC_MANAGED_OUTPUT_FIREWALL_SPILL_PATH"
	retentionEnv     = "GC_MANAGED_OUTPUT_FIREWALL_RETENTION"
	spillModeEnv     = "GC_MANAGED_OUTPUT_FIREWALL_SPILL_MODE"
	defaultBudget    = 32 << 10
	defaultRetention = 24 * time.Hour
)

var afterSpillWrite func()

var writeSpillFile = func(f *os.File, payload []byte) error {
	n, err := f.Write(payload)
	if err == nil && n != len(payload) {
		return io.ErrShortWrite
	}
	return err
}

type spillManifest struct {
	Mode      string `json:"mode"`
	Path      string `json:"path,omitempty"`
	ExpiresAt string `json:"expires_at"`
}

// Active reports whether verb is protected by the injected managed policy.
func Active(verb string) bool { return budgetForVerb(verb) > 0 }

// BudgetForVerb reports the managed byte budget for verb, or zero when the
// managed output firewall does not apply.
func BudgetForVerb(verb string) int { return budgetForVerb(verb) }

// WriteJSON serializes value wholly before admitting it to stdout.
func WriteJSON(ctx context.Context, commandClass, verb string, value any, stdout, stderr io.Writer, marshal func(any) ([]byte, error)) int {
	if err := ctx.Err(); err != nil {
		return canceled(stderr, err)
	}
	payload, err := marshal(value)
	if err != nil {
		fmt.Fprintf(stderr, "gc output firewall: encoding: %v\n", err) //nolint:errcheck
		return writeMinimal(ctx, stdout, stderr, "encoding_failed")
	}
	return write(ctx, commandClass, append(payload, '\n'), stdout, stderr, budgetForVerb(verb))
}

// WriteJSONWithBudget serializes value under an explicit byte budget.
func WriteJSONWithBudget(ctx context.Context, commandClass string, value any, stdout, stderr io.Writer, budget int, marshal func(any) ([]byte, error)) int {
	if err := ctx.Err(); err != nil {
		return canceled(stderr, err)
	}
	payload, err := marshal(value)
	if err != nil {
		fmt.Fprintf(stderr, "gc output firewall: encoding: %v\n", err) //nolint:errcheck
		return writeMinimal(ctx, stdout, stderr, "encoding_failed")
	}
	return write(ctx, commandClass, append(payload, '\n'), stdout, stderr, budget)
}

// Write admits an already-rendered known-read response. Structured responses
// must be valid JSON so the fail-closed response remains protocol-safe.
func Write(ctx context.Context, commandClass, verb string, payload []byte, structured bool, stdout, stderr io.Writer) int {
	if structured && !json.Valid(payload) {
		return writeMinimal(ctx, stdout, stderr, "invalid_json")
	}
	return write(ctx, commandClass, payload, stdout, stderr, budgetForVerb(verb))
}

// WriteWithBudget is the direct compatibility helper for callers that supply
// an explicit bound independent of managed-session environment injection.
func WriteWithBudget(ctx context.Context, commandClass string, payload []byte, stdout, stderr io.Writer, budget int) int {
	return write(ctx, commandClass, payload, stdout, stderr, budget)
}

func budgetForVerb(verb string) int {
	if os.Getenv(managedEnv) != "1" || !verbEnabled(verb) {
		return 0
	}
	if raw := os.Getenv(budgetEnv); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 512 {
			return n
		}
	}
	return defaultBudget
}

func verbEnabled(verb string) bool {
	scope := strings.TrimSpace(os.Getenv(verbsEnv))
	if scope == "" || verb == "" {
		return true
	}
	for _, configured := range strings.Split(scope, ",") {
		if strings.TrimSpace(configured) == verb {
			return true
		}
	}
	return false
}

func retention() time.Duration {
	if raw := os.Getenv(retentionEnv); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return defaultRetention
}

func write(ctx context.Context, commandClass string, payload []byte, stdout, stderr io.Writer, budget int) int {
	if err := ctx.Err(); err != nil {
		return canceled(stderr, err)
	}
	if budget <= 0 || len(payload) <= budget {
		return publish(ctx, payload, stdout, stderr)
	}
	digest := sha256.Sum256(payload)
	mode := os.Getenv(spillModeEnv)
	if mode == "" {
		mode = "secure"
	}
	spill := spillManifest{Mode: mode, ExpiresAt: time.Now().Add(retention()).UTC().Format(time.RFC3339)}
	var root *spillRoot
	if mode != "disabled" {
		spill.Mode = "unavailable"
		root = prepareSpill()
		if root != nil {
			defer root.root.Close() //nolint:errcheck
			if name, err := artifactName(); err == nil {
				spill.Mode, spill.Path = "secure", filepath.Join(root.dir, name)
			}
		}
	}
	manifestPayload, ok := marshalManifest(commandClass, budget, len(payload), fmt.Sprintf("%x", digest), spill, stderr)
	if !ok || len(manifestPayload) > budget {
		return writeMinimal(ctx, stdout, stderr, "budget_too_small")
	}
	requiredFailed := false
	if root != nil && spill.Mode == "secure" {
		if !writeSpill(ctx, root.root, filepath.Base(spill.Path), payload) {
			spill.Mode, spill.Path = "unavailable", ""
			requiredFailed = mode == "required"
		} else {
			if afterSpillWrite != nil {
				afterSpillWrite()
			}
			if !root.matchesPath() {
				removeSpill(root, spill)
				spill.Mode, spill.Path = "unavailable", ""
				requiredFailed = mode == "required"
			}
		}
	}
	if spill.Mode == "unavailable" && mode == "required" {
		requiredFailed = true
	}
	if spill.Mode == "unavailable" {
		var present bool
		manifestPayload, present = marshalManifest(commandClass, budget, len(payload), fmt.Sprintf("%x", digest), spill, stderr)
		if !present || len(manifestPayload) > budget {
			return writeMinimal(ctx, stdout, stderr, "budget_too_small")
		}
	}
	if err := ctx.Err(); err != nil {
		removeSpill(root, spill)
		return canceled(stderr, err)
	}
	if code := publish(ctx, manifestPayload, stdout, stderr); code != 0 {
		removeSpill(root, spill)
		return code
	}
	if requiredFailed {
		_, _ = fmt.Fprintln(stderr, "gc output firewall: required evidence spill unavailable")
		return 1
	} //nolint:errcheck
	return 0
}

func removeSpill(root *spillRoot, spill spillManifest) {
	if root != nil && spill.Mode == "secure" {
		_ = root.root.Remove(filepath.Base(spill.Path))
	}
}

func marshalManifest(commandClass string, budget, bytes int, digest string, spill spillManifest, stderr io.Writer) ([]byte, bool) {
	manifest := struct {
		SchemaVersion   string        `json:"schema_version"`
		Kind            string        `json:"kind"`
		Reason          string        `json:"reason"`
		CommandClass    string        `json:"command_class"`
		BudgetBytes     int           `json:"budget_bytes"`
		SerializedBytes int           `json:"serialized_bytes"`
		SHA256          string        `json:"sha256"`
		Spill           spillManifest `json:"spill"`
		Remediation     string        `json:"remediation,omitempty"`
	}{"1", "gc.output_firewall", "byte_budget_exceeded", commandClass, budget, bytes, digest, spill, ""}
	payload, err := json.Marshal(manifest)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc output firewall: encoding manifest: %v\n", err)
		return nil, false
	} //nolint:errcheck
	payload = append(payload, '\n')
	if len(payload) > budget {
		return nil, false
	}
	manifest.Remediation = "Use --allow-unbounded to request the full payload; its evidence is available at spill.path when present."
	withRemediation, err := json.Marshal(manifest)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc output firewall: encoding manifest: %v\n", err)
		return nil, false
	} //nolint:errcheck
	withRemediation = append(withRemediation, '\n')
	if len(withRemediation) <= budget {
		return withRemediation, true
	}
	return payload, true
}

func publish(ctx context.Context, payload []byte, stdout, stderr io.Writer) int {
	if err := ctx.Err(); err != nil {
		return canceled(stderr, err)
	}
	if _, err := stdout.Write(payload); err != nil {
		_, _ = fmt.Fprintf(stderr, "gc output firewall: writing output: %v\n", err)
		return 1
	} //nolint:errcheck
	return 0
}

func writeMinimal(ctx context.Context, stdout, stderr io.Writer, reason string) int {
	return publish(ctx, []byte(`{"kind":"gc.output_firewall","reason":"`+reason+`"}`+"\n"), stdout, stderr)
}

func canceled(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "gc output firewall: staging canceled: %v\n", err)
	return 1
} //nolint:errcheck

type spillRoot struct {
	root *os.Root
	dir  string
}

func (s *spillRoot) matchesPath() bool {
	bound, err := s.root.Stat(".")
	if err != nil {
		return false
	}
	current, err := os.Lstat(s.dir)
	return err == nil && current.Mode()&os.ModeSymlink == 0 && os.SameFile(bound, current)
}

func prepareSpill() *spillRoot {
	cityRoot, spillPath := os.Getenv(spillRootEnv), os.Getenv(spillPathEnv)
	if !filepath.IsAbs(cityRoot) || spillPath == "" || filepath.IsAbs(spillPath) || spillPath == "." || strings.HasPrefix(filepath.Clean(spillPath), "..") {
		return nil
	}
	root, err := openRoot(cityRoot, spillPath)
	if err != nil {
		return nil
	}
	info, err := root.Stat(".")
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		_ = root.Close()
		return nil
	}
	cleanup(root, retention())
	return &spillRoot{root, filepath.Join(cityRoot, spillPath)}
}

func openRoot(cityRoot, spillPath string) (*os.Root, error) {
	root, err := os.OpenRoot(cityRoot)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(filepath.Clean(spillPath), string(filepath.Separator)) {
		before, err := root.Lstat(part)
		if os.IsNotExist(err) {
			if err = root.Mkdir(part, 0o700); err != nil && !os.IsExist(err) {
				_ = root.Close()
				return nil, err
			}
			before, err = root.Lstat(part)
		}
		if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
			_ = root.Close()
			return nil, fmt.Errorf("unsafe spill directory component %q", part)
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			_ = root.Close()
			return nil, err
		}
		after, afterErr := next.Stat(".")
		latest, latestErr := root.Lstat(part)
		if afterErr != nil || latestErr != nil || !os.SameFile(before, after) || !os.SameFile(before, latest) {
			_ = next.Close()
			_ = root.Close()
			return nil, fmt.Errorf("spill directory changed while opening %q", part)
		}
		_ = root.Close()
		root = next
	}
	return root, nil
}

func artifactName() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "output-" + hex.EncodeToString(random), nil
}

func writeSpill(ctx context.Context, root *os.Root, finalName string, payload []byte) bool {
	temporary := "." + finalName + ".tmp"
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	if err = writeSpillFile(f, payload); err == nil {
		if info, statErr := f.Stat(); statErr != nil {
			err = statErr
		} else if info.Size() != int64(len(payload)) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = f.Close()
	} else {
		_ = f.Close()
	}
	if err != nil {
		_ = root.Remove(temporary)
		return false
	}
	if err = root.Rename(temporary, finalName); err != nil {
		_ = root.Remove(temporary)
		return false
	}
	if ctx.Err() != nil {
		_ = root.Remove(finalName)
		return false
	}
	return true
}

func cleanup(root *os.Root, ttl time.Duration) {
	entries, err := iofs.ReadDir(root.FS(), ".")
	if err != nil || ttl <= 0 {
		return
	}
	cutoff := time.Now().Add(-ttl)
	for _, entry := range entries {
		if !artifact(entry.Name()) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := root.Lstat(entry.Name())
		if err == nil && info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
			_ = root.Remove(entry.Name())
		}
	}
}

func artifact(name string) bool {
	if !strings.HasPrefix(name, "output-") || len(name) != len("output-")+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(name, "output-"))
	return err == nil
}
