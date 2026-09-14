// Package buildinfo resolves the build identity of a crier binary: the
// version, the git commit it was built from, the build timestamp, and whether
// the working tree was dirty at build time.
//
// There are two sources, in priority order:
//
//  1. Values injected at link time via
//     -X github.com/crier-dev/crier/internal/buildinfo.Version=... (the
//     Makefile stamps Version, Commit and BuildTime this way).
//  2. runtime/debug.ReadBuildInfo, which the Go toolchain fills in
//     automatically for every main package built inside a git checkout
//     (settings vcs.revision, vcs.time, vcs.modified). This is the fallback
//     that makes a bare `go build` — no ldflags, no wrapper script — report a
//     real commit instead of the "dev" placeholder.
//
// Both binaries (cmd/server, cmd/crier-mcp) and the server's GET /version
// route read their identity from here, so crier has exactly one build
// identity and one identity format.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Sentinel values, reported when nothing was stamped and no VCS metadata is
// available (e.g. a build from a source tarball). They are also the defaults
// the linker-injected variables carry, which is how Resolve tells "stamped"
// apart from "not stamped".
const (
	DefaultVersion   = "dev"
	DefaultCommit    = "unknown"
	DefaultBuildTime = "unknown"
)

// commitLen is how many characters of a git revision an identity carries:
// enough to disambiguate by hand, short enough for a log line.
const commitLen = 8

// Linker-injected identity. The Makefile stamps all three:
//
//	go build -ldflags "-X .../internal/buildinfo.Version=$(VERSION) -X ..."
//
// A bare `go build` leaves them at their sentinels and Resolve falls back to
// the VCS metadata the toolchain already stamped into the binary.
var (
	Version   = DefaultVersion
	Commit    = DefaultCommit
	BuildTime = DefaultBuildTime
)

// readBuildInfo is a package variable so tests can drive the fallback paths
// with synthetic build settings. Production always calls debug.ReadBuildInfo.
var readBuildInfo = debug.ReadBuildInfo

// Info is a binary's resolved build identity. The JSON tags are the wire
// contract of the server's GET /version route.
type Info struct {
	// Version is the stamped version without a leading "v" (String adds it
	// back), or DefaultVersion when no version was stamped.
	Version string `json:"version"`
	// Commit is the short git revision the binary was built from, or
	// DefaultCommit when no revision could be determined.
	Commit string `json:"commit"`
	// BuildTime is the stamped build timestamp, else the commit timestamp
	// from the VCS metadata — RFC 3339 either way — else DefaultBuildTime.
	// A VCS-derived value is when the COMMIT was made, not when the binary
	// was compiled.
	BuildTime string `json:"build_time"`
	// Modified reports that the working tree had uncommitted changes when
	// the binary was built.
	Modified bool `json:"modified"`
}

// Resolve returns the build identity: linker-injected values win, and any
// value still sitting at its sentinel is replaced by what the Go toolchain
// recorded in the binary's build info (vcs.revision / vcs.time /
// vcs.modified). With no build info at all — a stripped or non-VCS build — the
// sentinels are returned as-is, so callers can always distinguish "unknown"
// from a real identity.
func Resolve() Info {
	info := Info{
		Version:   strings.TrimPrefix(strings.TrimSpace(Version), "v"),
		Commit:    strings.TrimSpace(Commit),
		BuildTime: strings.TrimSpace(BuildTime),
	}
	if info.Version == "" {
		info.Version = DefaultVersion
	}
	if info.Commit == "" {
		info.Commit = DefaultCommit
	}
	if info.BuildTime == "" {
		info.BuildTime = DefaultBuildTime
	}

	bi, ok := readBuildInfo()
	if !ok || bi == nil {
		info.Commit = Shorten(info.Commit)
		return info
	}

	// Note: bi.Main.Version is deliberately NOT used as a version fallback.
	// For a working copy the toolchain synthesizes a pseudo-version
	// ("v0.0.0-20260914102942-d66b195fdb22+dirty"), which reads as noise in
	// an operator-facing identity. The version segment is only ever the
	// ldflags-stamped value; the commit segment carries the rest.

	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == DefaultCommit && s.Value != "" {
				info.Commit = s.Value
			}
		case "vcs.time":
			if info.BuildTime == DefaultBuildTime && s.Value != "" {
				info.BuildTime = s.Value
			}
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}

	info.Commit = Shorten(info.Commit)
	return info
}

// String returns the canonical one-line identity:
//
//	v<version>-<commit>            e.g. v1.2.3-1a2b3c4d
//	v<version>-<commit>-dirty      working tree had uncommitted changes
//	vdev-1a2b3c4d                  no version stamped, commit known
//	vdev                           nothing stamped, no VCS metadata
//
// The version segment keeps a single leading "v" even when the injected
// version already carried the git-describe "-dirty" suffix, so a describe
// string and the vcs.modified flag never produce "-dirty-dirty".
func String() string { return Resolve().String() }

// String returns the canonical one-line identity (see the package-level
// String). Callers prefix it with the binary name: "crier v1.2.3-1a2b3c4d".
func (i Info) String() string {
	version := strings.TrimPrefix(i.Version, "v")
	versionDirty := strings.HasSuffix(version, "-dirty")
	out := "v" + version
	if i.Commit != "" && i.Commit != DefaultCommit {
		out += "-" + i.Commit
	}
	if i.Modified && !versionDirty {
		out += "-dirty"
	}
	return out
}

// Shorten trims a git revision to the commitLen characters an identity
// carries. Values already short enough, empty, or not a hex revision (an
// operator-supplied tag, say) are returned unchanged — a revision is the only
// thing worth shortening, and truncating a tag would corrupt it.
func Shorten(rev string) string {
	rev = strings.TrimSpace(rev)
	if len(rev) <= commitLen || !isHex(rev) {
		return rev
	}
	return rev[:commitLen]
}

// isHex reports whether s is a non-empty run of hex digits, i.e. a git object
// name rather than a tag or branch name.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
