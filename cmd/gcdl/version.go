package main

import (
	"runtime/debug"

	"github.com/Masterminds/semver/v3"
)

// version can be set at build time:
//
//	go build -ldflags "-X main.version=v1.2.3" ./cmd/gcdl
var version string

// getVersion returns the release version, else the version Go stamped from git
// (the tag, or a pseudo-version with the commit hash), else 0.0.0+dev.
func getVersion() *semver.Version {
	// If version was passed as ld flag, use that.
	if version != "" {
		if parsed, err := semver.NewVersion(version); err == nil {
			return parsed
		}
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		if v, err := semver.NewVersion(info.Main.Version); err == nil {
			return v
		}
		// No module version (e.g. a repository without commits): use the
		// commit hash if there is one.
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				if v, err := semver.NewVersion("0.0.0+" + s.Value[:12]); err == nil {
					return v
				}
			}
		}
	}

	return semver.MustParse("0.0.0+dev")
}
