package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/pathutil"
)

const (
	bdGuardMarkerEnv   = "GC_BD_HQ_GUARD"
	bdGuardMarkerValue = "1"
	bdGuardCityEnv     = "GC_BD_HQ_GUARD_CITY"
	bdGuardFingerprint = "bd_guard:hq"
)

// bdGuardRefusal reports whether a fenced managed session must refuse the
// resolved gc bd target. The marker's city and the command's effective city
// must identify the same directory; disagreement fails closed because the
// process cannot safely identify which HQ it is authorized to avoid.
func bdGuardRefusal(guardCity, executionCity string, target execStoreTarget) (string, bool) {
	guardCity = pathutil.NormalizePathForCompare(strings.TrimSpace(guardCity))
	executionCity = pathutil.NormalizePathForCompare(strings.TrimSpace(executionCity))
	if guardCity == "" || executionCity == "" || guardCity != executionCity {
		return fmt.Sprintf("managed-session HQ guard city %q does not match effective city %q", guardCity, executionCity), true
	}
	if target.ScopeKind == "city" ||
		pathutil.NormalizePathForCompare(target.ScopeRoot) == executionCity {
		return "refusing city (HQ) store routing from this managed session; use a rig-scoped gc bd command or ask the operator", true
	}
	return "", false
}

func activeBdGuardRefusal(cityPath string, target execStoreTarget) (string, bool) {
	if strings.TrimSpace(os.Getenv(bdGuardMarkerEnv)) != bdGuardMarkerValue {
		return "", false
	}
	return bdGuardRefusal(os.Getenv(bdGuardCityEnv), cityPath, target)
}
