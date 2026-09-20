package config

// WorkKind defines declarative workability rules for a kind of work.
type WorkKind struct {
	Name               string   `toml:"name,omitempty"`
	RequireMetadata    []string `toml:"require_metadata,omitempty"`
	RequireRouteTarget string   `toml:"require_route_target,omitempty"`
	FreshnessBinding   string   `toml:"freshness_binding,omitempty"`
	MatchMetadata      []string `toml:"match_metadata,omitempty"`
}

// DeepCopyWorkKinds returns a deep copy of a WorkKinds map.
func DeepCopyWorkKinds(in map[string]WorkKind) map[string]WorkKind {
	if in == nil {
		return nil
	}
	out := make(map[string]WorkKind, len(in))
	for k, v := range in {
		wk := v
		wk.RequireMetadata = append([]string(nil), v.RequireMetadata...)
		wk.MatchMetadata = append([]string(nil), v.MatchMetadata...)
		out[k] = wk
	}
	return out
}
