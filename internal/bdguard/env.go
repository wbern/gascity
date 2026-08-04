// Package bdguard defines the environment contract shared by the gc bd command
// and the standalone bd PATH shim.
package bdguard

const (
	// MarkerEnv enables the managed-session HQ store fence.
	MarkerEnv = "GC_BD_HQ_GUARD"
	// AccessEnv grants positive HQ authorization to the managed session.
	AccessEnv = "GC_BD_HQ_ACCESS"
	// CityEnv identifies the city whose HQ store is fenced.
	CityEnv = "GC_BD_HQ_GUARD_CITY"
)
