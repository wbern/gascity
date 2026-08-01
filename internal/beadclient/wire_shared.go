package beadclient

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
)

// This file holds the small, bead-scoped pieces this leaf needs that live in
// package api's client/decode files (which also carry the rich CLI view types
// and the huma server). They are ported verbatim here so the thin client can
// route bd calls without compiling the control-plane server.

// CachedRead wraps a decoded read body with the supervisor CachingStore age
// (X-GC-Cache-Age-S header) so callers can surface staleness. AgeSeconds is 0
// for fallback paths that omit the header.
type CachedRead[T any] struct {
	Body       T
	AgeSeconds float64
}

const cacheAgeHeader = "X-GC-Cache-Age-S"

// cacheAgeFromResponse extracts the CachingStore age from the X-GC-Cache-Age-S
// response header, returning 0 when absent or unparseable.
func cacheAgeFromResponse(r *http.Response) float64 {
	if r == nil {
		return 0
	}
	v := r.Header.Get(cacheAgeHeader)
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// routeMissingError marks a request to a route an older controller does not
// serve, so the caller can fall back to a legacy leg.
type routeMissingError struct{ path string }

// Error reports the missing route path.
func (e *routeMissingError) Error() string { return fmt.Sprintf("route missing: %s", e.path) }

// IsRouteMissing reports whether err is a routeMissingError.
func IsRouteMissing(err error) bool {
	var rm *routeMissingError
	return errors.As(err, &rm)
}

// beadFromGen converts a generated-client bead into the domain beads.Bead.
func beadFromGen(g genclient.Bead) beads.Bead {
	out := beads.Bead{
		ID:        g.Id,
		Title:     g.Title,
		Status:    g.Status,
		Type:      g.IssueType,
		CreatedAt: g.CreatedAt,
	}
	if g.UpdatedAt != nil {
		out.UpdatedAt = *g.UpdatedAt
	}
	if g.DeferUntil != nil {
		deferUntil := *g.DeferUntil
		out.DeferUntil = &deferUntil
	}
	if g.Ephemeral != nil {
		out.Ephemeral = *g.Ephemeral
	}
	if g.Priority != nil {
		p := int(*g.Priority)
		out.Priority = &p
	}
	if g.Assignee != nil {
		out.Assignee = *g.Assignee
	}
	if g.From != nil {
		out.From = *g.From
	}
	if g.Parent != nil {
		out.ParentID = *g.Parent
	}
	if g.Ref != nil {
		out.Ref = *g.Ref
	}
	if g.Description != nil {
		out.Description = *g.Description
	}
	if g.Needs != nil {
		out.Needs = append([]string(nil), *g.Needs...)
	}
	if g.Labels != nil {
		out.Labels = append([]string(nil), *g.Labels...)
	}
	if g.Metadata != nil {
		out.Metadata = make(map[string]string, len(*g.Metadata))
		for k, v := range *g.Metadata {
			out.Metadata[k] = v
		}
	}
	if g.AwaitType != nil {
		out.AwaitType = *g.AwaitType
	}
	if g.CreatedBy != nil {
		out.CreatedBy = *g.CreatedBy
	}
	if g.Owner != nil {
		out.Owner = *g.Owner
	}
	if g.Notes != nil {
		out.Notes = *g.Notes
	}
	if g.Dependencies != nil {
		out.Dependencies = make([]beads.Dep, 0, len(*g.Dependencies))
		for _, d := range *g.Dependencies {
			out.Dependencies = append(out.Dependencies, beads.Dep{
				IssueID:     d.IssueId,
				DependsOnID: d.DependsOnId,
				Type:        d.Type,
			})
		}
	}
	return out
}
