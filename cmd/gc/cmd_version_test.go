package main

import (
	"runtime/debug"
	"testing"
)

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "v0.13.0", want: "0.13.0"},
		{in: "0.13.0", want: "0.13.0"},
		{in: "v0.13.0-rc2.0.20260317225312-41a12e4914cb+dirty", want: "0.13.0-rc2"},
		{in: "v0.0.0-20260317225312-41a12e4914cb", want: "dev"},
		{in: "(devel)", want: "dev"},
		{in: "", want: "dev"},
	}
	for _, tt := range tests {
		if got := normalizeVersion(tt.in); got != tt.want {
			t.Fatalf("normalizeVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResolveBuildMetadataUsesModuleVersion(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{
			Version: "v0.13.0",
		},
	}
	version, commit, date := resolveBuildMetadata("dev", "unknown", "unknown", true, info)
	if version != "0.13.0" {
		t.Fatalf("version = %q, want %q", version, "0.13.0")
	}
	if commit != "unknown" {
		t.Fatalf("commit = %q, want unknown", commit)
	}
	if date != "unknown" {
		t.Fatalf("date = %q, want unknown", date)
	}
}

func TestResolveBuildMetadataUsesForkRevisionForPseudoModuleVersion(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{
			Version: "v1.1.1-0.20260706165804-77688848f81f+dirty",
		},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "77688848f81faba45e6e2e5a4c9e72241c275b25"},
			{Key: "vcs.time", Value: "2026-07-06T16:58:04Z"},
		},
	}
	version, commit, date := resolveBuildMetadata("dev", "unknown", "unknown", true, info)
	if version != "fork-77688848f" {
		t.Fatalf("version = %q, want %q", version, "fork-77688848f")
	}
	if commit != "77688848f81faba45e6e2e5a4c9e72241c275b25" {
		t.Fatalf("commit = %q, want vcs revision", commit)
	}
	if date != "2026-07-06T16:58:04Z" {
		t.Fatalf("date = %q, want vcs time", date)
	}
}

func TestResolveBuildMetadataUsesVCSSettings(t *testing.T) {
	info := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
			{Key: "vcs.time", Value: "2026-03-17T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	version, commit, date := resolveBuildMetadata("dev", "unknown", "unknown", true, info)
	if version != "dev" {
		t.Fatalf("version = %q, want dev", version)
	}
	if commit != "abc123-dirty" {
		t.Fatalf("commit = %q, want %q", commit, "abc123-dirty")
	}
	if date != "2026-03-17T00:00:00Z" {
		t.Fatalf("date = %q, want %q", date, "2026-03-17T00:00:00Z")
	}
}
