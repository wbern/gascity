package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/spf13/cobra"
)

// The adapter delegates trust to this accepted verifier, not to a caller's
// executable or signing-key selection. Python isolation excludes sibling and
// environment imports; direct-chat verification uses only its standard library.
const (
	custodyVerifierSHA = "afae55c13c7d1c4e9953c7b9f91554fcfe0d92feac07849019f3c91483d2b409"
	custodyMaxPacket   = 1 << 20
)

type custodySession struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Agent string `json:"agent"`
	Claim string `json:"claim"`
}
type custodyAdmission struct {
	Custodian       string    `json:"custodian"`
	PredecessorDead bool      `json:"predecessor_dead"`
	SoleWriter      bool      `json:"sole_writer"`
	Scheduling      string    `json:"scheduling"`
	NotBefore       time.Time `json:"not_before"`
	ExpiresAt       time.Time `json:"expires_at"`
}
type custodyOperation struct {
	Schema       string           `json:"schema_version"`
	Purpose      string           `json:"purpose"`
	City         string           `json:"city"`
	StoreRef     string           `json:"store_ref"`
	Source       string           `json:"source_id"`
	Status       string           `json:"expected_status"`
	Assignee     *string          `json:"expected_assignee"`
	Revision     int64            `json:"expected_revision"`
	PriorSession string           `json:"prior_session_id"`
	PriorOwner   string           `json:"prior_workspace_owner"`
	ClaimedAt    string           `json:"first_claimed_at"`
	Branch       string           `json:"branch"`
	Base         string           `json:"base_oid"`
	Head         string           `json:"head_oid"`
	Workspace    string           `json:"workspace"`
	Successor    custodySession   `json:"successor"`
	Dependencies []beads.Dep      `json:"preserved_dependencies"`
	Admission    custodyAdmission `json:"admission"`
}
type custodyResult struct {
	Schema   string `json:"schema_version"`
	Outcome  string `json:"outcome"`
	Source   string `json:"source_id"`
	Manifest string `json:"manifest_sha256"`
	// Committed means acknowledged persistence; CommitUncertain distinguishes
	// an error/lost acknowledgement from definite no-write.
	Committed       bool   `json:"committed"`
	CommitUncertain bool   `json:"commit_uncertain,omitempty"`
	WorkerEntry     bool   `json:"worker_entry"`
	Error           string `json:"error,omitempty"`
}
type custodyChecks struct {
	now      func() time.Time
	physical func(custodyOperation) error
	runtime  func(custodyOperation) error
}

func newBeadsAdoptCustodyCmd(stdout, stderr io.Writer) *cobra.Command {
	var packet, verifier string
	cmd := &cobra.Command{Use: "adopt-custody <bead-id>", Short: "Record separately authorized custody while retaining dependencies and entry holds", Args: cobra.ExactArgs(1)}
	cmd.Flags().StringVar(&packet, "manifest", "", "separately signed operation manifest (required)")
	cmd.Flags().StringVar(&verifier, "verifier", "", "absolute accepted verifier path (required, content pinned)")
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		result := custodyResult{Schema: "existing-source-custody-result.v1", Outcome: "refused", Source: args[0]}
		runErr := func() (returnErr error) {
			ctx, err := resolveContext()
			if err != nil {
				return err
			}
			op, hash, err := verifyCustodyPacket(ctx.CityPath, packet, verifier)
			if err != nil {
				return err
			}
			result.Manifest = hash
			if op.Source != args[0] {
				return fmt.Errorf("operation source differs")
			}
			cfg, err := loadCityConfig(ctx.CityPath, stderr)
			if err != nil {
				return err
			}
			root, ref, err := resolveBeadsMetadataCASStore(cfg, ctx.CityPath, op.StoreRef)
			if err != nil {
				return err
			}
			if ref != op.StoreRef {
				return fmt.Errorf("store identity differs")
			}
			store, err := openBeadsMetadataCASStore(root, ctx.CityPath)
			if err != nil {
				return err
			}
			defer func() {
				if e := closeBeadsMetadataCASStore(store); e != nil {
					returnErr = fmt.Errorf("closing custody store: %w", e)
					if result.Committed {
						result.Outcome = "committed-unadmitted"
					}
				}
			}()
			cityStore, err := openCityStoreAtWithConfig(ctx.CityPath, cfg)
			if err != nil {
				return err
			}
			defer func() {
				if e := closeBeadStoreHandle(cityStore); e != nil {
					returnErr = fmt.Errorf("closing session store: %w", e)
					if result.Committed {
						result.Outcome = "committed-unadmitted"
					}
				}
			}()
			sp, err := newSessionProviderForCity(cfg, ctx.CityPath)
			if err != nil {
				return err
			}
			catalog, err := workerSessionCatalogWithConfig(ctx.CityPath, cliSessionStore(cityStore, cfg, ctx.CityPath), sp, cfg)
			if err != nil {
				return err
			}
			checks := custodyChecks{now: time.Now, physical: checkCustodyWorkspace, runtime: func(o custodyOperation) error {
				infos, e := catalog.List("all", "")
				if e != nil {
					return e
				}
				return checkCustodyRuntime(infos, sp.IsRunning, o)
			}}
			result, err = applyCustodyOperation(store, op, hash, checks)
			return err
		}()
		if runErr != nil {
			result.Error = runErr.Error()
		}
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			return err
		}
		if runErr != nil {
			return errExit
		}
		return nil
	}
	return cmd
}

func validateCustodyOperation(o custodyOperation, city string, now time.Time) error {
	oid := regexp.MustCompile(`^[0-9a-f]{40}$`)
	if o.Schema != "existing-source-custody.v1" || o.Purpose != "adopt-existing-source-custody" || o.City != city || !filepath.IsAbs(city) {
		return fmt.Errorf("operation purpose/city differs")
	}
	if !strings.HasPrefix(o.StoreRef, "rig:") {
		return fmt.Errorf("custody source requires exact rig store")
	}
	if _, _, err := parseBeadsMetadataCASStoreRef(o.StoreRef); err != nil {
		return err
	}
	if o.Source == "" || o.Status != "open" || o.Assignee == nil || *o.Assignee != "" || o.Revision == 0 || o.PriorSession == "" || o.PriorOwner == "" || o.ClaimedAt == "" || o.Branch == "" || !oid.MatchString(o.Base) || !oid.MatchString(o.Head) || !filepath.IsAbs(o.Workspace) || filepath.Clean(o.Workspace) != o.Workspace {
		return fmt.Errorf("mandatory exact custody preconditions missing")
	}
	if _, err := time.Parse(time.RFC3339, o.ClaimedAt); err != nil {
		return fmt.Errorf("first claim timestamp invalid")
	}
	s := o.Successor
	if s.ID == "" || s.Name == "" || s.Agent == "" || s.Claim == "" || s.ID == o.PriorSession {
		return fmt.Errorf("successor identity missing or unchanged")
	}
	a := o.Admission
	if a.Custodian == "" || !a.PredecessorDead || !a.SoleWriter || a.Scheduling != "custody-only-no-entry" || a.NotBefore.IsZero() || a.ExpiresAt.Sub(a.NotBefore) <= 0 || a.ExpiresAt.Sub(a.NotBefore) > time.Hour || now.Before(a.NotBefore) || !now.Before(a.ExpiresAt) {
		return fmt.Errorf("signed external admission missing or expired")
	}
	seen := map[string]bool{}
	for _, d := range o.Dependencies {
		key := d.DependsOnID + "/" + d.Type
		if d.IssueID != o.Source || d.DependsOnID == "" || d.Type == "" || seen[key] {
			return fmt.Errorf("dependency snapshot invalid")
		}
		seen[key] = true
	}
	if o.Dependencies == nil {
		return fmt.Errorf("exact dependency snapshot missing")
	}
	return nil
}

// rejectDuplicateJSON avoids a verifier/Go parser disagreement about authority.
func custodyJSON(data []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				k, e := dec.Token()
				if e != nil {
					return e
				}
				key := k.(string)
				if seen[key] {
					return fmt.Errorf("duplicate JSON field %q", key)
				}
				seen[key] = true
				if e = value(); e != nil {
					return e
				}
			}
		case '[':
			for dec.More() {
				if e := value(); e != nil {
					return e
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter")
		}
		_, err = dec.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return json.Unmarshal(data, target)
}

func verifyCustodyPacket(city, filename, verifier string) (op custodyOperation, hash string, returnErr error) {
	data, err := readCustodyFile(filename, custodyMaxPacket)
	if err != nil {
		return op, "", err
	}
	var envelope struct {
		Approval struct {
			Mode    string `json:"mode"`
			Receipt string `json:"receipt_sha256"`
		} `json:"approval"`
		Constraints map[string]bool `json:"execution_constraints"`
		Operation   json.RawMessage `json:"custody_operation"`
	}
	// Execution constraints contain descriptive values too; decode separately.
	var fields map[string]json.RawMessage
	if err = custodyJSON(data, &fields); err != nil {
		return op, "", err
	}
	if err = json.Unmarshal(fields["approval"], &envelope.Approval); err != nil {
		return op, "", err
	}
	if envelope.Approval.Mode != "direct_chat_preapproval" {
		return op, "", fmt.Errorf("direct-chat operation authority required")
	}
	var limits map[string]any
	if err = json.Unmarshal(fields["execution_constraints"], &limits); err != nil {
		return op, "", err
	}
	for _, key := range []string{"source_only", "no_adoption_execution", "no_runtime_apply"} {
		if value, present := limits[key]; present && value != false {
			return op, "", fmt.Errorf("source-only authority cannot adopt custody")
		}
	}
	if limits["custody_adoption_authorized"] != true {
		return op, "", fmt.Errorf("explicit custody operation permission missing")
	}
	operation := fields["custody_operation"]
	decoder := json.NewDecoder(bytes.NewReader(operation))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&op); err != nil {
		return op, "", err
	}
	if err = validateCustodyOperation(op, city, time.Now()); err != nil {
		return op, "", err
	}
	helper, err := readCustodyFile(verifier, custodyMaxPacket)
	if err != nil {
		return op, "", err
	}
	sum := sha256.Sum256(helper)
	if hex.EncodeToString(sum[:]) != custodyVerifierSHA {
		return op, "", fmt.Errorf("accepted verifier content differs")
	}
	temp, err := os.MkdirTemp("", "custody-verify-")
	if err != nil {
		return op, "", err
	}
	defer func() {
		if e := os.RemoveAll(temp); e != nil && returnErr == nil {
			returnErr = fmt.Errorf("removing verifier snapshot: %w", e)
		}
	}()
	snapshot := filepath.Join(temp, "manifest.json")
	script := filepath.Join(temp, "verifier.py")
	if err = os.WriteFile(snapshot, data, 0o600); err != nil {
		return op, "", err
	}
	if err = os.WriteFile(script, helper, 0o600); err != nil {
		return op, "", err
	}
	run := func(command string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/usr/bin/python3", "-I", "-S", script, command, "--city", city, "--manifest", snapshot)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "SSH_ASKPASS_REQUIRE=never"}
		var b custodyOutput
		cmd.Stdout = &b
		cmd.Stderr = io.Discard
		if e := cmd.Run(); e != nil {
			return nil, fmt.Errorf("trusted authority verifier refused: %w", e)
		}
		return b.Bytes(), nil
	}
	verified, err := run("activation-metadata")
	if err != nil {
		return op, "", err
	}
	hashed, err := run("hash")
	if err != nil {
		return op, "", err
	}
	hash = strings.TrimSpace(string(hashed))
	var result struct {
		Rig     string `json:"rig"`
		Sources []struct {
			ID       string `json:"id"`
			Metadata struct {
				Hash    string `json:"approved_manifest_hash"`
				Receipt string `json:"authority.receipt_sha256"`
			} `json:"metadata"`
		} `json:"sources"`
	}
	if err = custodyJSON(verified, &result); err != nil {
		return op, "", err
	}
	kind, rig, _ := strings.Cut(op.StoreRef, ":")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hash) || kind != "rig" || result.Rig != rig || len(result.Sources) != 1 || result.Sources[0].ID != op.Source || result.Sources[0].Metadata.Hash != hash || result.Sources[0].Metadata.Receipt != envelope.Approval.Receipt {
		return op, "", fmt.Errorf("verified operation manifest identity differs")
	}
	return op, hash, nil
}

type custodyOutput struct{ bytes.Buffer }

func (b *custodyOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > custodyMaxPacket {
		return 0, fmt.Errorf("verifier output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func readCustodyFile(path string, limit int64) (contents []byte, returnErr error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("absolute input required")
	}
	for p := path; ; p = filepath.Dir(p) {
		s, e := os.Lstat(p)
		if e != nil {
			return nil, e
		}
		if s.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlink input refused")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	// Nonblocking open prevents a FIFO from stalling before descriptor validation.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if e := f.Close(); e != nil && returnErr == nil {
			returnErr = fmt.Errorf("closing input: %w", e)
		}
	}()
	s, err := f.Stat()
	if err != nil || !s.Mode().IsRegular() {
		return nil, fmt.Errorf("regular input required")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("input exceeds bound")
	}
	return b, nil
}

func checkCustodyWorkspace(o custodyOperation) error {
	resolved, err := filepath.EvalSymlinks(o.Workspace)
	if err != nil || resolved != o.Workspace {
		return fmt.Errorf("workspace identity differs")
	}
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/usr/bin/git", append([]string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "core.untrackedCache=false", "-C", o.Workspace}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_OPTIONAL_LOCKS=0"}
		var b custodyOutput
		cmd.Stdout = &b
		cmd.Stderr = io.Discard
		err := cmd.Run()
		return strings.TrimSpace(b.String()), err
	}
	// Status can execute clean/process filters even when reporting dirty files.
	// Refuse configured filters rather than changing content normalization or
	// running task-owned commands. The signed external sole-writer admission
	// remains the precondition against concurrent repository/config mutation.
	keys, err := run("config", "--null", "--name-only", "--list")
	if err != nil {
		return fmt.Errorf("cannot qualify workspace Git configuration")
	}
	for _, key := range strings.Split(keys, "\x00") {
		if strings.HasPrefix(strings.ToLower(key), "filter.") {
			return fmt.Errorf("workspace configured filter refused")
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{{[]string{"rev-parse", "--show-toplevel"}, o.Workspace}, {[]string{"rev-parse", "HEAD"}, o.Head}, {[]string{"symbolic-ref", "--short", "HEAD"}, o.Branch}, {[]string{"status", "--porcelain=v1", "--untracked-files=all"}, ""}} {
		s, e := run(c.args...)
		if e != nil || s != c.want {
			return fmt.Errorf("physical workspace drift at %s", c.args[0])
		}
	}
	if _, err = run("merge-base", "--is-ancestor", o.Base, o.Head); err != nil {
		return fmt.Errorf("recorded base is not ancestor")
	}
	return nil
}

func custodyDeps(deps []beads.Dep) string {
	rows := make([]string, 0, len(deps))
	for _, d := range deps {
		rows = append(rows, d.IssueID+"\x00"+d.DependsOnID+"\x00"+d.Type+"\x00"+d.Status)
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

func checkCustodyBead(b beads.Bead, o custodyOperation) error {
	if b.ID != o.Source || b.Status != o.Status || o.Assignee == nil || b.Assignee != *o.Assignee || b.Revision != o.Revision {
		return fmt.Errorf("exact source revision/status/assignee differs")
	}
	for k, v := range map[string]string{"gc.session_id": o.PriorSession, "gc.workspace_owner": o.PriorOwner, "gc.claimed_at": o.ClaimedAt, "branch": o.Branch, "gc.work_branch": o.Branch, "gc.base_sha": o.Base, "gc.task_worktree": o.Workspace, "gc.workspace_mode": "task"} {
		if b.Metadata[k] != v {
			return fmt.Errorf("native custody binding differs: %s", k)
		}
	}
	return nil
}

func applyCustodyOperation(store beads.Store, o custodyOperation, hash string, c custodyChecks) (custodyResult, error) {
	result := custodyResult{Schema: "existing-source-custody-result.v1", Outcome: "refused", Source: o.Source, Manifest: hash}
	writer, ok := beads.ConditionalWriterFor(store)
	if !ok {
		return result, fmt.Errorf("conditional store unavailable")
	}
	var before beads.Bead
	check := func() error {
		if e := validateCustodyOperation(o, o.City, c.now()); e != nil {
			return e
		}
		b, e := store.Get(o.Source)
		if e != nil {
			return e
		}
		if e = checkCustodyBead(b, o); e != nil {
			return e
		}
		if e := c.runtime(o); e != nil {
			return e
		}
		if e := c.physical(o); e != nil {
			return e
		}
		deps, e := store.DepList(o.Source, "down")
		if e != nil {
			return e
		}
		if custodyDeps(deps) != custodyDeps(o.Dependencies) {
			return fmt.Errorf("dependency snapshot differs")
		}
		before = b
		return nil
	}
	if e := check(); e != nil {
		return result, e
	}
	if e := check(); e != nil {
		return result, e
	}
	transitionTime := c.now()
	if e := validateCustodyOperation(o, o.City, transitionTime); e != nil {
		return result, e
	}
	status := "in_progress"
	claim := o.Successor.Claim
	patch := map[string]string{"gc.session_id": o.Successor.ID, "gc.session_name": o.Successor.Name, "gc.workspace_owner": o.Successor.Agent, "gc.workspace_owner_prior": o.PriorOwner, "gc.custody_manifest_sha256": hash, "gc.custody_transferred_at": transitionTime.UTC().Format(time.RFC3339Nano)}
	if e := writer.UpdateIfMatch(o.Source, o.Revision, beads.UpdateOpts{Status: &status, Assignee: &claim, Metadata: patch}); e != nil {
		// A transport error can lose the acknowledgement after persistence.
		// Only typed stale/unsupported responses establish definite no-write.
		if !beads.IsPreconditionFailed(e) && !beads.IsConditionalWriteUnsupported(e) {
			result.Outcome = "commit-uncertain-unadmitted"
			result.CommitUncertain = true
		}
		return result, e
	}
	result.Committed = true
	result.Outcome = "committed-unadmitted"
	// No rollback or worker-entry side effect: external custodian owns admission.
	after, e := store.Get(o.Source)
	if e != nil {
		return result, e
	}
	expected := before
	expected.Status = status
	expected.Assignee = claim
	if after.Revision == before.Revision {
		return result, fmt.Errorf("postcommit revision did not change")
	}
	expected.Revision = after.Revision
	expected.UpdatedAt = after.UpdatedAt
	expected.ClaimFence = after.ClaimFence
	expected.Metadata = make(beads.StringMap, len(before.Metadata)+len(patch))
	for k, v := range before.Metadata {
		expected.Metadata[k] = v
	}
	for k, v := range patch {
		expected.Metadata[k] = v
	}
	if !reflect.DeepEqual(expected, after) {
		return result, fmt.Errorf("postcommit native readback differs")
	}
	deps, e := store.DepList(o.Source, "down")
	if e != nil || custodyDeps(deps) != custodyDeps(o.Dependencies) {
		return result, fmt.Errorf("postcommit dependencies differ")
	}
	if e = validateCustodyOperation(o, o.City, c.now()); e != nil {
		return result, e
	}
	if e = c.runtime(o); e != nil {
		return result, e
	}
	if e = c.physical(o); e != nil {
		return result, e
	}
	result.Outcome = "custody-recorded-no-entry"
	return result, nil
}

func checkCustodyRuntime(infos []worker.SessionInfo, running func(string) bool, o custodyOperation) error {
	found := false
	for _, s := range infos {
		live := running(s.SessionName)
		if s.ID == o.PriorSession && live {
			return fmt.Errorf("predecessor is still running")
		}
		if s.ID == o.Successor.ID {
			claim := session.AssigneeIdentifier(s)
			if s.Closed || string(s.State) != "active" || !live || s.SessionName != o.Successor.Name || claim != o.Successor.Claim || claim != o.Successor.Agent {
				return fmt.Errorf("successor runtime identity is not admitted")
			}
			found = true
		} else if live && custodyWorkspaceMatches(s.WorkDir, o.Workspace) {
			return fmt.Errorf("another runtime occupies source workspace")
		}
	}
	if !found {
		return fmt.Errorf("registered successor missing")
	}
	return nil
}

func custodyWorkspaceMatches(path, target string) bool {
	if path == target {
		return true
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == target
}
