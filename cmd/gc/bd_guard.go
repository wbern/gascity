package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pathutil"
)

const (
	bdGuardMarkerEnv   = "GC_BD_HQ_GUARD"
	bdGuardMarkerValue = "1"
	bdGuardAccessEnv   = "GC_BD_HQ_ACCESS"
	bdGuardCityEnv     = "GC_BD_HQ_GUARD_CITY"
	bdGuardFingerprint = "bd_guard:hq"
)

// bdGuardRefusal reports whether a managed session must refuse the resolved gc
// bd target. The marker's city and the command's effective city must identify
// the same directory; disagreement fails closed.
func bdGuardRefusal(guardCity, executionCity, access string, target execStoreTarget) (string, bool) {
	guardCity = pathutil.NormalizePathForCompare(strings.TrimSpace(guardCity))
	executionCity = pathutil.NormalizePathForCompare(strings.TrimSpace(executionCity))
	if guardCity == "" || executionCity == "" || guardCity != executionCity {
		return fmt.Sprintf("managed-session HQ guard city %q does not match effective city %q", guardCity, executionCity), true
	}
	if target.ScopeKind == "city" ||
		pathutil.NormalizePathForCompare(target.ScopeRoot) == executionCity {
		if strings.TrimSpace(access) == bdGuardMarkerValue {
			return "", false
		}
		return "refusing city (HQ) store routing from this managed session; use a rig-scoped gc bd command or ask the operator", true
	}
	return "", false
}

func bdGuardDirectoryRefusal(cfg *config.City, cityPath, directory string, target execStoreTarget) (string, bool) {
	if strings.TrimSpace(directory) == "" {
		return "", false
	}
	var directoryTarget execStoreTarget
	rig, ok, err := resolveRigForDir(cfg, cityPath, directory)
	switch {
	case err != nil:
		return fmt.Sprintf("cannot verify bd directory %q against the resolved store: %v", directory, err), true
	case ok:
		directoryTarget = bdRigScopeTarget(cityPath, rig)
	case pathutil.PathWithin(cityPath, directory):
		directoryTarget = bdCityScopeTarget(cityPath, cfg)
	default:
		return fmt.Sprintf("bd directory %q is outside the resolved %s store %q", directory, target.ScopeKind, target.ScopeRoot), true
	}
	if directoryTarget.ScopeKind != target.ScopeKind ||
		!pathutil.SamePath(directoryTarget.ScopeRoot, target.ScopeRoot) {
		return fmt.Sprintf(
			"bd directory %q selects %s store %q but command routing resolved %s store %q",
			directory,
			directoryTarget.ScopeKind,
			directoryTarget.ScopeRoot,
			target.ScopeKind,
			target.ScopeRoot,
		), true
	}
	return "", false
}

func activeBdGuardRefusal(cfg *config.City, cityPath string, args []string, target execStoreTarget) (string, bool) {
	if strings.TrimSpace(os.Getenv(bdGuardMarkerEnv)) != bdGuardMarkerValue {
		return "", false
	}
	if msg, refuse := bdGuardRefusal(
		os.Getenv(bdGuardCityEnv),
		cityPath,
		os.Getenv(bdGuardAccessEnv),
		target,
	); refuse {
		return msg, true
	}
	return bdGuardDirectoryRefusal(cfg, cityPath, extractBdDirectoryFlag(args), target)
}
