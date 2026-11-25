// Package version reports which PolyDB this is.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is the release, set by the release build:
//
//	go build -ldflags "-X github.com/sandgorgon/polydb/version.Version=1.0.0"
//
// Left empty, it comes from the module version a `go install …@v1.0.0`
// records, or is the development version.
var Version = ""

// dev is the version a build from a checkout reports.
const dev = "1.0.0-dev"

// String returns the version, for example "1.0.0" or "1.0.0-dev".
func String() string {
	if Version != "" {
		return strings.TrimPrefix(Version, "v")
	}
	if bi, ok := debug.ReadBuildInfo(); ok && released(bi.Main.Version) {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return dev
}

// released reports whether a module version is a release tag, not the
// pseudo-version (v0.0.0-20261125…-abcdef, maybe +dirty) a build from
// a checkout records.
func released(v string) bool {
	return v != "" && v != "(devel)" && !strings.Contains(v, "+") && strings.Count(v, "-") < 2
}

// Build describes the build: the version, the commit and its time when
// the build recorded them, and the Go version.
func Build() string {
	out := String()
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return out
	}
	var rev, at string
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			at = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	var parts []string
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		if dirty {
			rev += "+changes"
		}
		parts = append(parts, "commit "+rev)
	}
	if at != "" {
		parts = append(parts, at)
	}
	parts = append(parts, bi.GoVersion)
	return out + " (" + strings.Join(parts, ", ") + ")"
}
