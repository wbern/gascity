// Package beadclient contains the Gas City supervisor API client adapter.
//
// This file is a thin adapter over the generated client in
// internal/api/genclient. The adapter preserves the small surface that
// CLI commands depend on (Client, NewClient, NewCityScopedClient, the
// 14 mutation/lookup methods, ShouldFallback, IsConnError) while pushing
// all wire-level work (request construction, JSON serialization, URL
// escaping, Problem Details parsing) into the generated client.
//
// Regenerate the generated client by running `go generate ./internal/api/genclient`
// after server changes.
package beadclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
)

// connError wraps transport-level errors (connection refused, timeout, etc.)
// to distinguish them from API-level error responses.
type connError struct {
	err error
}

func (e *connError) Error() string { return e.err.Error() }
func (e *connError) Unwrap() error { return e.err }

// IsConnError reports whether err is a transport-level connection failure
// (e.g., connection refused, timeout) rather than an API-level error response.
func IsConnError(err error) bool {
	var ce *connError
	return errors.As(err, &ce)
}

// readOnlyError indicates the API server rejected a mutation because it's
// running in read-only mode (non-localhost bind).
type readOnlyError struct {
	msg string
}

func (e *readOnlyError) Error() string { return e.msg }

// clientInitError indicates the client failed to construct its generated
// transport (typically a malformed base URL). It is treated as a fallback
// condition so CLI ladders can fall through to direct file mutation.
type clientInitError struct {
	err error
}

func (e *clientInitError) Error() string {
	if e.err == nil {
		return "api: client not initialized"
	}
	return "api: client not initialized: " + e.err.Error()
}
func (e *clientInitError) Unwrap() error { return e.err }

// cacheNotLiveError indicates the supervisor returned 503 because its
// read-path CachingStore has not yet reached the live state. Read handlers
// return this shape during startup/reconcile rather than serve stale or
// empty data; the CLI classifies it as fallbackable so reads land on raw
// bd instead.
type cacheNotLiveError struct {
	msg string
}

func (e *cacheNotLiveError) Error() string {
	if e.msg == "" {
		return "cache not yet live"
	}
	return e.msg
}

// storeSlowError indicates the supervisor returned 503 because a mail read
// exceeded its internal store deadline. It is intentionally not fallbackable:
// the local store path is affected by the same contention.
type storeSlowError struct {
	msg string
}

// StoreSlowErrorCode is the stable problem-detail prefix for mail read
// timeouts that must not fall back to the local store path.
const StoreSlowErrorCode = "store_slow"

func (e *storeSlowError) Error() string {
	if e.msg == "" {
		return "store slow: try again when load drops"
	}
	return e.msg
}

// IsStoreSlowError reports whether err originated from an API mail store
// timeout. Callers must not fall back to the local store for this error.
func IsStoreSlowError(err error) bool {
	var sse *storeSlowError
	return errors.As(err, &sse)
}

// MaintenanceInProgressError indicates the supervisor returned 409 because
// a Dolt store maintenance cycle is already executing. StartedAt carries
// the in-flight run's start time from the server's typed body so CLI
// callers can display it verbatim. Callers classify it via IsMaintenanceInProgress.
type MaintenanceInProgressError struct {
	StartedAt string // RFC3339 UTC; empty when server did not include it
	msg       string
}

// Error implements the error interface. The rendered message always leads
// with "already in progress" so callers can grep for it reliably; the raw
// server detail (in e.msg) is retained for debugging but not shown in the
// user-facing text.
func (e *MaintenanceInProgressError) Error() string {
	if e == nil {
		return "<nil maintenance-in-progress>"
	}
	if e.StartedAt == "" {
		return "maintenance already in progress"
	}
	return fmt.Sprintf("maintenance already in progress (started %s)", e.StartedAt)
}

// IsMaintenanceInProgress reports whether err originates from a 409 with a
// maintenance-in-progress typed body, so the CLI can emit exit code 3 and
// a targeted message instead of a generic error.
func IsMaintenanceInProgress(err error) bool {
	var e *MaintenanceInProgressError
	return errors.As(err, &e)
}

// MaintenanceDisabledError indicates the server returned 503 because
// [maintenance.dolt] enabled=false in city.toml. The CLI surfaces this as
// a short message pointing at the runbook rather than rolling the 503 into
// the generic cache-not-live fallback bucket (no local fallback path
// exists for maintenance operations).
type MaintenanceDisabledError struct{}

// Error implements the error interface.
func (e *MaintenanceDisabledError) Error() string {
	return "maintenance disabled: set [maintenance.dolt] enabled = true in city.toml and restart the controller"
}

// IsMaintenanceDisabled reports whether err indicates the server rejected
// a maintenance request because the loop is not enabled.
func IsMaintenanceDisabled(err error) bool {
	var e *MaintenanceDisabledError
	return errors.As(err, &e)
}

// serverError indicates a generic 5xx API response without a recognized
// 503 detail prefix such as cache_not_live or store_slow. Read-path callers
// classify it as fallbackable via ShouldFallbackForRead so the CLI lands on
// direct bd when the supervisor is unhealthy. Mutation callers continue to
// surface it as a hard error (ShouldFallback returns false) because writes
// with unknown server-side state are unsafe to silently retry locally.
type serverError struct {
	status int
	msg    string
}

func (e *serverError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("API returned %d", e.status)
	}
	return e.msg
}

// Status reports the HTTP status carried by the server error (always 5xx).
func (e *serverError) Status() int { return e.status }

// IsServerError reports whether err originates from a 5xx API response the
// read-path CLI should treat as fallbackable. Independent of ShouldFallback
// so mutation paths retain their strict no-fallback-on-5xx semantics.
func IsServerError(err error) bool {
	var se *serverError
	return errors.As(err, &se)
}

// ShouldFallbackForRead reports whether err indicates a read-path command
// should fall back to direct bd. Read-path commands tolerate generic 5xx
// server errors (IsServerError) in addition to the cases ShouldFallback
// already covers.
//
// c is the client that produced err (nil-safe). Any error from a REMOTE client
// is non-fallbackable regardless of type: a remote read has no local store to
// fall back to, and silently reading a local store instead would be the exact
// hazard the remote-city design exists to prevent (gate G1). errors.As unwraps
// transport wrappers, so the remoteness of the error cannot be recovered from
// err alone — it must come from the client. Pass the client you called; pass
// nil for a pure error-classification check (treated as local).
func ShouldFallbackForRead(c *Client, err error) bool {
	if c.IsRemote() {
		return false
	}
	if ShouldFallback(c, err) {
		return true
	}
	if IsRouteMissing(err) {
		return true
	}
	return IsServerError(err)
}

// ShouldFallback reports whether err indicates the CLI should fall back to
// direct file mutation (or, for reads, to raw bd). True for transport-level
// failures (connection refused, timeout), read-only API rejections (server
// bound to non-localhost, mutations disabled), client-init failures
// (malformed base URL), and cache-not-live 503 responses during supervisor
// priming. Always false for a REMOTE client (gate G1); see ShouldFallbackForRead
// for why the client, not the error, carries remoteness. c is nil-safe.
func ShouldFallback(c *Client, err error) bool {
	if c.IsRemote() {
		return false
	}
	if IsConnError(err) {
		return true
	}
	var ro *readOnlyError
	if errors.As(err, &ro) {
		return true
	}
	var ci *clientInitError
	if errors.As(err, &ci) {
		return true
	}
	var cnl *cacheNotLiveError
	return errors.As(err, &cnl)
}

// FallbackReason returns a stable reason code for err when
// ShouldFallbackForRead(c, err) is true. The set is closed: "remote",
// "cache-not-live", "read-only", "client-init", "route-missing", "conn-refused".
// A REMOTE client yields "remote" — reported for observability, never used to
// pick a local path (the caller gates on ShouldFallbackForRead first, which
// returns false for remote, so a remote error is surfaced, not fallen back).
// "route-missing" is a new-CLI/old-server route gap (a 404 with no problem+json
// body). Generic 5xx server errors collapse to "conn-refused" since from the
// CLI's read-path perspective an unhealthy server is equivalent to an
// unreachable one. Non-fallbackable error types such as store_slow are
// intentionally absent from this set. Returns "unknown" for non-fallbackable
// errors so callers that invoke FallbackReason unconditionally produce a token
// instead of panicking; gate on ShouldFallbackForRead first to avoid that
// sentinel. c is nil-safe.
func FallbackReason(c *Client, err error) string {
	if c.IsRemote() {
		return "remote"
	}
	var cnl *cacheNotLiveError
	if errors.As(err, &cnl) {
		return "cache-not-live"
	}
	var ro *readOnlyError
	if errors.As(err, &ro) {
		return "read-only"
	}
	var ci *clientInitError
	if errors.As(err, &ci) {
		return "client-init"
	}
	if IsRouteMissing(err) {
		return "route-missing"
	}
	if IsConnError(err) || IsServerError(err) {
		return "conn-refused"
	}
	return "unknown"
}

// Client is an HTTP client for the Gas City API server. It wraps the
// generated typed client so CLI commands can route writes through the API
// when a controller is running.
type Client struct {
	cw       *genclient.ClientWithResponses
	baseURL  string // stored for SSE stream connections
	cityName string // non-empty for city-scoped clients; passed to every per-city call
	initErr  error  // set when NewClient failed to build the transport (malformed baseURL, etc.)

	// Remote-city fields (set only by NewRemoteCityScopedClient). isRemote makes
	// no-fallback a compiler-checkable instance property (gate G1): any error
	// from a remote client is non-fallbackable regardless of type. streamClient
	// is the dedicated SSE transport shape (Timeout:0 + CheckRedirect + TLS);
	// tokenSource is called live before every request AND every SSE (re)connect
	// so a per-attempt 401 re-mint takes effect (never captured once).
	isRemote     bool
	streamClient *http.Client
	tokenSource  TokenSource
	tokenMu      sync.Mutex
	// grantSource, when set, mints a single-use X-GC-City-Write grant for each
	// MUTATING request (gate G18). Like tokenSource it is invoked live per
	// request, never captured. nil means no grant is attached (a city that
	// authenticates on X-GC-Request alone, or one fronted by a bearer edge).
	grantSource GrantSource
}

// IsRemote reports whether this client targets a remote city over the control
// plane. Remote clients never fall back to a local store (gate G1).
func (c *Client) IsRemote() bool { return c != nil && c.isRemote }

// bearerToken returns the current transport bearer from the token source, or ""
// when no source is configured. The call is serialized so a non-reentrant
// source (e.g. one that execs a credential command) is safe under concurrent
// REST + SSE use.
func (c *Client) bearerToken() (string, error) {
	if c == nil || c.tokenSource == nil {
		return "", nil
	}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	return c.tokenSource()
}

// defaultClientTimeout is the overall HTTP timeout for control-plane client
// calls. The read paths (ListBeads, GetBead, GetStatus, ListMailInbox,
// ListConvoys, ...) pass context.Background() and rely solely on this ceiling,
// and several of them federate the city store plus every rig store — a
// dolt-backed rig store can take many seconds, so a 10s ceiling false-timed-out
// healthy-but-slow federated reads. Most calls return in milliseconds; this
// only bounds the slow federated reads and genuinely hung requests.
const defaultClientTimeout = 60 * time.Second

// NewClient creates a new supervisor-scope API client targeting the
// given base URL (e.g., "http://127.0.0.1:8080"). Supervisor-scope
// operations (ListCities, ListServices-via-city, etc.) work through
// this client; per-city calls require NewCityScopedClient.
func NewClient(baseURL string) *Client {
	return newClient(baseURL, "")
}

// NewCityScopedClient creates a client that targets per-city operations
// at "/v0/city/<cityName>/...". The generated client produces those
// paths natively — no prefix rewrite or path editor needed.
func NewCityScopedClient(baseURL, cityName string) *Client {
	return newClient(baseURL, cityName)
}

func newClient(baseURL, cityName string) *Client {
	httpClient := &http.Client{Timeout: defaultClientTimeout}
	cw, err := genclient.NewClientWithResponses(
		baseURL,
		genclient.WithHTTPClient(httpClient),
		genclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("X-GC-Request", "true")
			return nil
		}),
	)
	if err != nil {
		// genclient.NewClient only returns errors for malformed URLs;
		// the CLI hits this on misconfig — return a stub that errors on
		// every method rather than panicking.
		return &Client{initErr: &clientInitError{err: err}}
	}
	return &Client{cw: cw, baseURL: baseURL, cityName: cityName}
}

// requireCityScope reports an error if the client was constructed as a
// supervisor-scope client (empty cityName) but a per-city method was called.
// Centralizes the check so silent `/v0/city//...` request construction is
// impossible.
func (c *Client) requireCityScope() error {
	if c.initErr != nil {
		return c.initErr
	}
	if c.cw == nil {
		return errClientUninitialized
	}
	if c.cityName == "" {
		return fmt.Errorf("api: per-city call requires NewCityScopedClient; use NewCityScopedClient(baseURL, cityName)")
	}
	return nil
}

// --- Lookup methods ---

// ListBeadsOpts is the optional filter set for ListBeads. All fields are
// zero-valued by default; the server falls back to its own defaults when a
// field is empty. All mirrors the CLI --all flag and maps to the server's
// IncludeClosed query semantic.
type ListBeadsOpts struct {
	Status   string
	Type     string
	Label    string
	Assignee string
	Rig      string
	Limit    int
	All      bool
}

// ListBeads fetches beads across all rigs via
// GET /v0/city/{cityName}/beads. Server-side filters mirror the BeadListInput
// query parameters. The CachedRead.AgeSeconds field carries the supervisor
// CachingStore age from the X-GC-Cache-Age-S response header so callers can
// surface _cache_age_s on --json output and a staleness banner on human
// output.
func (c *Client) ListBeads(opts ListBeadsOpts) (CachedRead[[]beads.Bead], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[[]beads.Bead]{}, err
	}
	params := &genclient.GetV0CityByCityNameBeadsParams{}
	if opts.Status != "" {
		params.Status = &opts.Status
	}
	if opts.Type != "" {
		params.Type = &opts.Type
	}
	if opts.Label != "" {
		params.Label = &opts.Label
	}
	if opts.Assignee != "" {
		params.Assignee = &opts.Assignee
	}
	if opts.Rig != "" {
		params.Rig = &opts.Rig
	}
	if opts.Limit > 0 {
		lim := int64(opts.Limit)
		params.Limit = &lim
	}
	if opts.All {
		t := true
		params.All = &t
	}
	resp, err := c.cw.GetV0CityByCityNameBeadsWithResponse(context.Background(), c.cityName, params)
	if err != nil {
		return CachedRead[[]beads.Bead]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[[]beads.Bead]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[[]beads.Bead]{}, err
	}
	return CachedRead[[]beads.Bead]{
		Body:       beadsFromGenList(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// GetBead fetches one bead by ID via
// GET /v0/city/{cityName}/bead/{id}. Returns the bead detail with cache age
// so callers can attach _cache_age_s (JSON) or a staleness banner (human).
func (c *Client) GetBead(id string) (CachedRead[beads.Bead], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[beads.Bead]{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameBeadByIdWithResponse(context.Background(), c.cityName, id)
	if err != nil {
		return CachedRead[beads.Bead]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[beads.Bead]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[beads.Bead]{}, err
	}
	if resp.JSON200 == nil {
		return CachedRead[beads.Bead]{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	return CachedRead[beads.Bead]{
		Body:       beadFromGenPtr(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// --- Mutation methods ---

// SlingRequest carries the parameters of a sling mutation for Client.Sling.
// It mirrors the SlingInput body: Target is required; exactly one of Bead or
// Formula selects the work.
type SlingRequest struct {
	Rig            string
	Target         string
	Bead           string
	Formula        string
	AttachedBeadID string
	Title          string
	Vars           map[string]string
	ScopeKind      string
	ScopeRef       string
	Force          bool
}

// SlingResult is the outcome of a sling mutation.
type SlingResult struct {
	Status         string
	Target         string
	Formula        string
	Bead           string
	WorkflowID     string
	RootBeadID     string
	AttachedBeadID string
	Mode           string
	Warnings       []string
}

var errClientUninitialized = errors.New("api client not initialized")

// ErrClaimRouteUnsupported reports that the controller's backing store cannot
// claim on behalf of an explicit actor (HTTP 501). Callers that route a claim
// through the controller (the bd shim) fall back to a direct claim path on this
// error rather than failing the claim.
var ErrClaimRouteUnsupported = errors.New("claim route unsupported by controller backend")

// checkMutation handles the (resp, err) tuple from a generated mutation
// call and returns the (nil | connError | readOnlyError | generic error)
// shape that ShouldFallback understands. resp may be nil when transportErr
// is set (e.g. connection refused).
func checkMutation(resp interface{ StatusCode() int }, transportErr error) error {
	if transportErr != nil {
		return &connError{err: fmt.Errorf("request failed: %w", transportErr)}
	}
	if resp == nil || isNil(resp) {
		return &connError{err: fmt.Errorf("nil response")}
	}
	return apiErrorFromResponse(resp.StatusCode(), pdOf(resp))
}

// isNil reports whether an interface value holds a nil concrete value.
// Necessary because passing a typed nil pointer satisfies an interface
// without being == nil.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// pdOf extracts the generated client's decoded Problem Details pointer
// from any generated *WithResponse type. An operation that keeps the spec's
// catch-all error decodes it into `ApplicationproblemJSONDefault *ErrorModel`;
// an operation that enumerates its error statuses (the P12 error-contract
// pilot) decodes into `ApplicationproblemJSON<code> *ErrorModel` instead —
// exactly one of which the generator populates, the one matching the HTTP
// status. pdOf returns whichever ErrorModel field is set, so both spec shapes
// are handled uniformly. Returns nil when none is populated (2xx, non-JSON
// error, or an operation with no problem+json error at all).
//
// This is spec-driven: the fields exist because the spec declares the error
// responses to be Problem Details, and the generator decoded them. No
// hand-written JSON parsing happens here or downstream.
func pdOf(resp any) *genclient.ErrorModel {
	if resp == nil {
		return nil
	}
	rv := reflect.ValueOf(resp)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	// Prefer the catch-all field, then fall back to whichever per-status
	// ApplicationproblemJSON<code> field the generator populated.
	if f := rv.FieldByName("ApplicationproblemJSONDefault"); f.IsValid() {
		if pd, _ := f.Interface().(*genclient.ErrorModel); pd != nil {
			return pd
		}
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		if !strings.HasPrefix(rt.Field(i).Name, "ApplicationproblemJSON") {
			continue
		}
		if pd, _ := rv.Field(i).Interface().(*genclient.ErrorModel); pd != nil {
			return pd
		}
	}
	// Fallback: the server returned a status the operation did not enumerate,
	// so the generator has no field to decode the problem+json into (e.g. an
	// infrastructure or middleware 503 like cache_not_live on a read whose
	// declared contract is 404-only). Recover the detail from the raw response
	// body so read-path fallback classification still works. Guarded to bodies
	// that decode as a Problem Details document so 2xx/non-problem payloads do
	// not masquerade as errors.
	if bf := rv.FieldByName("Body"); bf.IsValid() {
		if body, ok := bf.Interface().([]byte); ok && len(body) > 0 {
			var pd genclient.ErrorModel
			if json.Unmarshal(body, &pd) == nil && (pd.Detail != nil || pd.Title != nil || pd.Code != nil) {
				return &pd
			}
		}
	}
	return nil
}

// apiErrorFromResponse returns nil for 2xx responses, a *readOnlyError
// for "read_only:" prefixed Problem Details, and a generic error
// otherwise. pd comes from the generated client's typed decode of the
// spec's default `application/problem+json` response — there is no
// hand-written JSON parsing.
func apiErrorFromResponse(status int, pd *genclient.ErrorModel) error {
	if status >= 200 && status < 300 {
		return nil
	}
	var detail, title string
	if pd != nil {
		if pd.Detail != nil {
			detail = *pd.Detail
		}
		if pd.Title != nil {
			title = *pd.Title
		}
	}
	if strings.HasPrefix(detail, "read_only") {
		msg := detail
		if msg == "" {
			msg = "mutations disabled (read-only server)"
		}
		return &readOnlyError{msg: msg}
	}
	if status == http.StatusServiceUnavailable {
		if strings.HasPrefix(detail, "cache_not_live") {
			msg := detail
			if msg == "" {
				msg = "cache not yet live"
			}
			return &cacheNotLiveError{msg: msg}
		}
		if strings.HasPrefix(detail, StoreSlowErrorCode) {
			msg := detail
			if msg == "" {
				msg = "store slow: try again when load drops"
			}
			return &storeSlowError{msg: msg}
		}
		if strings.HasPrefix(detail, "maintenance_disabled") {
			return &MaintenanceDisabledError{}
		}
	}
	if status == http.StatusConflict && strings.HasPrefix(detail, "maintenance-in-progress") {
		startedAt := extractMaintenanceStartedAt(detail)
		return &MaintenanceInProgressError{StartedAt: startedAt, msg: detail}
	}
	// Generic 5xx (500/501/502/504/... plus 503 without a cache_not_live,
	// store_slow, or maintenance_disabled prefix) wraps into a serverError so
	// read-path callers can classify it as fallbackable via
	// ShouldFallbackForRead. Mutation callers continue to see it as
	// non-fallbackable (ShouldFallback excludes it).
	if status >= 500 {
		msg := detail
		if msg == "" {
			msg = title
		}
		if msg == "" {
			return &serverError{status: status}
		}
		return &serverError{status: status, msg: fmt.Sprintf("API error: %s", msg)}
	}
	if detail != "" {
		return fmt.Errorf("API error: %s", detail)
	}
	if title != "" {
		return fmt.Errorf("API error: %s", title)
	}
	return fmt.Errorf("API returned %d", status)
}

// extractMaintenanceStartedAt parses the JSON body that the
// maintenance 409 handler appends after the "maintenance-in-progress: "
// prefix and returns the started_at field, or empty when absent or
// malformed. The server always emits this prefix via maintenanceConflictFromError,
// so a missing started_at means the in-flight run had a zero-value
// StartedAt (a race during supervisor startup) rather than a protocol
// violation.
func extractMaintenanceStartedAt(detail string) string {
	const prefix = "maintenance-in-progress: "
	idx := strings.Index(detail, prefix)
	if idx < 0 {
		return ""
	}
	payload := detail[idx+len(prefix):]
	var body struct {
		StartedAt string `json:"started_at"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		return ""
	}
	return body.StartedAt
}

// ---- bd-shim v1 client methods (ported from upstream/deploy/sqlite-dispatch-fix @9596b6f08) ----
// Thin HTTP client methods the bd shim routes through; every genclient endpoint
// already exists on develop. EphemeralBeads/ReleaseBeadIfCurrent/ClaimBead are
// intentionally NOT ported in v1 (their endpoints are not yet on this fork).

// EphemeralBeadsOpts filters Client.EphemeralBeads results.
type EphemeralBeadsOpts struct {
	Status   string
	Type     string
	Label    string
	Assignee string
	Parent   string
	All      bool
	Limit    int
}

// EphemeralBeads reads the ephemeral/wisp tier via GET
// /v0/city/{cityName}/beads/ephemeral — the routed form of
// `bd query 'ephemeral=true AND ...'`. Under graph_store=sqlite this reaches
// wisps resident in the SQLite graph backend through the controller's Router.
// The CachedRead.AgeSeconds field carries the supervisor CachingStore age from
// the X-GC-Cache-Age-S response header, mirroring ListBeads.
func (c *Client) EphemeralBeads(opts EphemeralBeadsOpts) (CachedRead[[]beads.Bead], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[[]beads.Bead]{}, err
	}
	params := &genclient.GetV0CityByCityNameBeadsEphemeralParams{}
	if opts.Status != "" {
		params.Status = &opts.Status
	}
	if opts.Type != "" {
		params.Type = &opts.Type
	}
	if opts.Label != "" {
		params.Label = &opts.Label
	}
	if opts.Assignee != "" {
		params.Assignee = &opts.Assignee
	}
	if opts.Parent != "" {
		params.Parent = &opts.Parent
	}
	if opts.Limit > 0 {
		lim := int64(opts.Limit)
		params.Limit = &lim
	}
	if opts.All {
		t := true
		params.All = &t
	}
	resp, err := c.cw.GetV0CityByCityNameBeadsEphemeralWithResponse(context.Background(), c.cityName, params)
	if err != nil {
		return CachedRead[[]beads.Bead]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[[]beads.Bead]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[[]beads.Bead]{}, err
	}
	return CachedRead[[]beads.Bead]{
		Body:       beadsFromGenList(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// BeadGraphDep is one directed edge in a BeadGraph.
type BeadGraphDep struct {
	From string
	To   string
	Kind string
}

// BeadGraph is a molecule/bead topology — the root, its member/step beads, and
// their parent-child edges — as returned by GET /beads/graph/{rootID}.
type BeadGraph struct {
	Root  beads.Bead
	Beads []beads.Bead
	Deps  []BeadGraphDep
}

// GetBeadGraph fetches the molecule/bead graph rooted at rootID — the routed data
// source for `bd mol current`/`progress`. Under graph_store=sqlite this reaches
// molecule topology resident in the SQLite graph backend through the controller's
// Router (the work-only bd cannot see those steps).
func (c *Client) GetBeadGraph(rootID string) (BeadGraph, error) {
	if err := c.requireCityScope(); err != nil {
		return BeadGraph{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameBeadsGraphByRootIdWithResponse(context.Background(), c.cityName, rootID)
	if err != nil {
		return BeadGraph{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return BeadGraph{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return BeadGraph{}, err
	}
	if resp.JSON200 == nil {
		return BeadGraph{}, &connError{err: fmt.Errorf("empty graph response for %q", rootID)}
	}
	g := BeadGraph{Root: beadFromGen(resp.JSON200.Root)}
	if resp.JSON200.Beads != nil {
		for _, b := range *resp.JSON200.Beads {
			g.Beads = append(g.Beads, beadFromGen(b))
		}
	}
	if resp.JSON200.Deps != nil {
		for _, d := range *resp.JSON200.Deps {
			kind := ""
			if d.Kind != nil {
				kind = *d.Kind
			}
			g.Deps = append(g.Deps, BeadGraphDep{From: d.From, To: d.To, Kind: kind})
		}
	}
	return g, nil
}

// ReadyBeads fetches the city's ready work — the federated ready set across the
// controller's bead stores — via GET /v0/city/{cityName}/beads/ready. The
// endpoint applies no assignee/metadata predicates, so callers that need them
// (e.g. the bd shim's discovery queries) post-filter the returned set
// client-side.
func (c *Client) ReadyBeads() (CachedRead[[]beads.Bead], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[[]beads.Bead]{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameBeadsReadyWithResponse(context.Background(), c.cityName, &genclient.GetV0CityByCityNameBeadsReadyParams{})
	if err != nil {
		return CachedRead[[]beads.Bead]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[[]beads.Bead]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[[]beads.Bead]{}, err
	}
	return CachedRead[[]beads.Bead]{
		Body:       beadsFromGenList(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// CloseBead closes a bead via POST /v0/city/{cityName}/bead/{id}/close.
func (c *Client) CloseBead(id string) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	resp, err := c.cw.PostV0CityByCityNameBeadByIdCloseWithResponse(context.Background(), c.cityName, id, nil)
	return checkMutation(resp, err)
}

// ReopenBead reopens a closed bead via POST /v0/city/{cityName}/bead/{id}/reopen.
func (c *Client) ReopenBead(id string) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	resp, err := c.cw.PostV0CityByCityNameBeadByIdReopenWithResponse(context.Background(), c.cityName, id, nil)
	return checkMutation(resp, err)
}

// DeleteBead soft-deletes (closes) a bead via DELETE /v0/city/{cityName}/bead/{id}.
func (c *Client) DeleteBead(id string) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	resp, err := c.cw.DeleteV0CityByCityNameBeadByIdWithResponse(context.Background(), c.cityName, id, nil)
	return checkMutation(resp, err)
}

// UpdateBead applies a field update via POST /v0/city/{cityName}/bead/{id}/update,
// mapping beads.UpdateOpts onto the wire body. nil/empty fields are omitted so the
// server leaves them unchanged.
func (c *Client) UpdateBead(id string, opts beads.UpdateOpts) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	body := genclient.BeadUpdateBody{
		Status:      opts.Status,
		Assignee:    opts.Assignee,
		Title:       opts.Title,
		Type:        opts.Type,
		Description: opts.Description,
		Parent:      opts.ParentID,
	}
	if opts.Priority != nil {
		p := int64(*opts.Priority)
		body.Priority = &p
	}
	if len(opts.Labels) > 0 {
		labels := append([]string(nil), opts.Labels...)
		body.Labels = &labels
	}
	if len(opts.RemoveLabels) > 0 {
		rm := append([]string(nil), opts.RemoveLabels...)
		body.RemoveLabels = &rm
	}
	if len(opts.Metadata) > 0 {
		md := make(map[string]string, len(opts.Metadata))
		for k, v := range opts.Metadata {
			md[k] = v
		}
		body.Metadata = &md
	}
	resp, err := c.cw.PostV0CityByCityNameBeadByIdUpdateWithResponse(context.Background(), c.cityName, id, nil, body)
	return checkMutation(resp, err)
}

// ClaimBead atomically claims a bead for actor via
// POST /v0/city/{cityName}/bead/{id}/claim. It returns (bead, claimed, nil):
// claimed=true when the actor now holds the bead, claimed=false when another
// actor won the race (the returned bead is the current holder). A 501 (backing
// store cannot claim on behalf of an actor) surfaces as ErrClaimRouteUnsupported
// so the shim can fall back to a direct claim. This is the warm-controller
// claim path: the actor travels in the body, not the controller's identity.
func (c *Client) ClaimBead(id, actor string) (beads.Bead, bool, error) {
	if err := c.requireCityScope(); err != nil {
		return beads.Bead{}, false, err
	}
	body := genclient.PostV0CityByCityNameBeadByIdClaimJSONRequestBody{Actor: actor}
	resp, err := c.cw.PostV0CityByCityNameBeadByIdClaimWithResponse(context.Background(), c.cityName, id, nil, body)
	if err != nil {
		return beads.Bead{}, false, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return beads.Bead{}, false, &connError{err: fmt.Errorf("nil response")}
	}
	if resp.StatusCode() == http.StatusNotImplemented {
		return beads.Bead{}, false, ErrClaimRouteUnsupported
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return beads.Bead{}, false, err
	}
	if resp.JSON200 == nil {
		return beads.Bead{}, false, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	return beadFromGen(resp.JSON200.Bead), resp.JSON200.Claimed, nil
}

// CreateBead creates b through the city-scoped API.
func (c *Client) CreateBead(b beads.Bead) (beads.Bead, error) {
	if err := c.requireCityScope(); err != nil {
		return beads.Bead{}, err
	}
	body := genclient.BeadCreateInputBody{Title: b.Title}
	if b.Type != "" {
		body.Type = &b.Type
	}
	if b.Assignee != "" {
		body.Assignee = &b.Assignee
	}
	if b.Description != "" {
		body.Description = &b.Description
	}
	if b.ParentID != "" {
		body.Parent = &b.ParentID
	}
	if b.Priority != nil {
		p := int64(*b.Priority)
		body.Priority = &p
	}
	if b.DeferUntil != nil {
		body.DeferUntil = b.DeferUntil
	}
	if len(b.Labels) > 0 {
		labels := append([]string(nil), b.Labels...)
		body.Labels = &labels
	}
	if len(b.Metadata) > 0 {
		md := make(map[string]string, len(b.Metadata))
		for k, v := range b.Metadata {
			md[k] = v
		}
		body.Metadata = &md
	}
	resp, err := c.cw.CreateBeadWithResponse(context.Background(), c.cityName, &genclient.CreateBeadParams{XGCRequest: "gc"}, body)
	if err != nil {
		return beads.Bead{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return beads.Bead{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return beads.Bead{}, err
	}
	if resp.JSON201 == nil {
		return beads.Bead{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	return beadFromGenPtr(resp.JSON201), nil
}
