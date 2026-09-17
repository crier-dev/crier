package buildinfo

import (
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"
)

// stubVars replaces the linker-injected identity for one test and restores it
// afterwards. These are package variables, so tests using this helper must not
// run in parallel.
func stubVars(t *testing.T, version, commit, buildTime string) {
	t.Helper()
	origVersion, origCommit, origBuildTime := Version, Commit, BuildTime
	Version, Commit, BuildTime = version, commit, buildTime
	t.Cleanup(func() {
		Version, Commit, BuildTime = origVersion, origCommit, origBuildTime
	})
}

// stubBuildInfo points the readBuildInfo seam at synthetic build settings for
// one test. ok=false models a binary with no usable build info at all.
func stubBuildInfo(t *testing.T, bi *debug.BuildInfo, ok bool) {
	t.Helper()
	orig := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) { return bi, ok }
	t.Cleanup(func() { readBuildInfo = orig })
}

// vcsBuildInfo builds the settings the Go toolchain stamps into a main package
// compiled inside a git checkout.
func vcsBuildInfo(revision, commitTime string, modified bool) *debug.BuildInfo {
	modifiedValue := "false"
	if modified {
		modifiedValue = "true"
	}
	return &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/crier-dev/crier", Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "-buildmode", Value: "exe"},
			{Key: "vcs.revision", Value: revision},
			{Key: "vcs.time", Value: commitTime},
			{Key: "vcs.modified", Value: modifiedValue},
		},
	}
}

const (
	fullRevision = "3f8a1c9d4e5b6a7c8d9e0f1a2b3c4d5e6f7a8b9c"
	shortPrefix  = "3f8a1c9d"
	commitTime   = "2026-09-14T06:05:59Z"
)

// TestResolveFallsBackToVCS is the load-bearing behaviour of the DF-CRIER-127
// cluster: a binary built with no ldflags at all (sentinels everywhere) must
// still report the commit it was built from.
func TestResolveFallsBackToVCS(t *testing.T) {
	stubVars(t, DefaultVersion, DefaultCommit, DefaultBuildTime)
	stubBuildInfo(t, vcsBuildInfo(fullRevision, commitTime, false), true)

	got := Resolve()
	if got.Version != DefaultVersion {
		t.Errorf("Version = %q, want %q (no version was stamped)", got.Version, DefaultVersion)
	}
	if got.Commit != shortPrefix {
		t.Errorf("Commit = %q, want %q (shortened vcs.revision)", got.Commit, shortPrefix)
	}
	if got.BuildTime != commitTime {
		t.Errorf("BuildTime = %q, want %q (vcs.time verbatim, RFC 3339)", got.BuildTime, commitTime)
	}
	if got.Modified {
		t.Error("Modified = true, want false (vcs.modified=false)")
	}

	want := "dev-" + shortPrefix
	if got.String() != want {
		t.Errorf("String() = %q, want %q", got.String(), want)
	}
	if String() != want {
		t.Errorf("package String() = %q, want %q", String(), want)
	}
	if strings.Contains(got.String(), "vdev") {
		t.Errorf("String() = %q carries the sentinel glued to a \"v\" — the commit segment is the identity in an unstamped build (DF-CRIER-171)", got.String())
	}
}

// TestResolveInjectedWinsOverVCS pins the priority order: ldflags-stamped
// values must never be overwritten by the build info the toolchain added.
func TestResolveInjectedWinsOverVCS(t *testing.T) {
	stubVars(t, "1.2.3", "abc1234", "2026-01-02T03:04:05Z")
	stubBuildInfo(t, vcsBuildInfo(fullRevision, commitTime, true), true)

	got := Resolve()
	if got.Version != "1.2.3" {
		t.Errorf("Version = %q, want %q", got.Version, "1.2.3")
	}
	if got.Commit != "abc1234" {
		t.Errorf("Commit = %q, want %q", got.Commit, "abc1234")
	}
	if got.BuildTime != "2026-01-02T03:04:05Z" {
		t.Errorf("BuildTime = %q, want %q", got.BuildTime, "2026-01-02T03:04:05Z")
	}
	// vcs.modified is not an injectable value: it always describes the tree
	// the binary was actually built from.
	if !got.Modified {
		t.Error("Modified = false, want true (vcs.modified=true)")
	}
	if want := "v1.2.3-abc1234-dirty"; got.String() != want {
		t.Errorf("String() = %q, want %q", got.String(), want)
	}
}

// TestResolveInjectedCommitIsShortened: a full 40-char sha passed via
// -ldflags must come out in the same 8-char shape as the VCS fallback, so the
// canonical string is identical whichever source supplied it.
func TestResolveInjectedCommitIsShortened(t *testing.T) {
	stubVars(t, "1.2.3", fullRevision, "2026-01-02T03:04:05Z")
	stubBuildInfo(t, vcsBuildInfo("0000000000000000000000000000000000000000", commitTime, false), true)

	got := Resolve()
	if got.Commit != shortPrefix {
		t.Errorf("Commit = %q, want %q", got.Commit, shortPrefix)
	}
	if want := "v1.2.3-" + shortPrefix; got.String() != want {
		t.Errorf("String() = %q, want %q", got.String(), want)
	}
}

// TestResolveWithoutBuildInfoReturnsSentinels: a tarball build (no VCS
// metadata) reports the placeholders instead of inventing an identity.
func TestResolveWithoutBuildInfoReturnsSentinels(t *testing.T) {
	stubVars(t, "", "", "")

	for _, tc := range []struct {
		name string
		bi   *debug.BuildInfo
		ok   bool
	}{
		{name: "no build info at all", bi: nil, ok: false},
		{name: "empty build info", bi: &debug.BuildInfo{}, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubBuildInfo(t, tc.bi, tc.ok)

			got := Resolve()
			if got.Version != DefaultVersion {
				t.Errorf("Version = %q, want %q", got.Version, DefaultVersion)
			}
			if got.Commit != DefaultCommit {
				t.Errorf("Commit = %q, want %q", got.Commit, DefaultCommit)
			}
			if got.BuildTime != DefaultBuildTime {
				t.Errorf("BuildTime = %q, want %q", got.BuildTime, DefaultBuildTime)
			}
			// No commit segment when there is no commit: "dev", not
			// "dev-unknown" — and no "v" glued to the sentinel.
			if want := DefaultVersion; got.String() != want {
				t.Errorf("String() = %q, want %q", got.String(), want)
			}
		})
	}
}

// TestResolveIgnoresModulePseudoVersion: for a working copy the toolchain
// synthesizes a pseudo-version ("v0.0.0-<ts>-<sha>+dirty"). It must NOT be
// adopted as the version — the identity would read as noise — so an unstamped
// build reports DefaultVersion no matter what Main.Version holds.
func TestResolveIgnoresModulePseudoVersion(t *testing.T) {
	stubVars(t, DefaultVersion, DefaultCommit, DefaultBuildTime)

	bi := vcsBuildInfo(fullRevision, commitTime, true)
	bi.Main.Version = "v0.0.0-20260914102942-d66b195fdb22+dirty"
	stubBuildInfo(t, bi, true)

	got := Resolve()
	if got.Version != DefaultVersion {
		t.Errorf("Version = %q, want %q (Main.Version must not leak in)", got.Version, DefaultVersion)
	}
	if want := "dev-" + shortPrefix + "-dirty"; got.String() != want {
		t.Errorf("String() = %q, want %q", got.String(), want)
	}

	t.Run("devel Main.Version is not a version either", func(t *testing.T) {
		bi := vcsBuildInfo(fullRevision, commitTime, false)
		bi.Main.Version = "(devel)"
		stubBuildInfo(t, bi, true)

		if got := Resolve(); got.Version != DefaultVersion {
			t.Errorf("Version = %q, want %q", got.Version, DefaultVersion)
		}
	})
}

// TestStringDirtyIsNotDoubled: `git describe --dirty` already puts "-dirty" in
// the version, and vcs.modified independently reports a dirty tree — the
// canonical string must carry the marker once.
func TestStringDirtyIsNotDoubled(t *testing.T) {
	stubVars(t, "e9eb006-dirty", DefaultCommit, DefaultBuildTime)
	stubBuildInfo(t, vcsBuildInfo(fullRevision, commitTime, true), true)

	got := Resolve()
	want := "ve9eb006-dirty-" + shortPrefix
	if got.String() != want {
		t.Errorf("String() = %q, want %q (single -dirty marker)", got.String(), want)
	}
}

// TestStringCanonicalFormat covers the formats the CLI and GET /version
// present, including the leading-"v" normalisation of a version that already
// carries one.
func TestStringCanonicalFormat(t *testing.T) {
	for _, tc := range []struct {
		name string
		info Info
		want string
	}{
		{name: "tagged release", info: Info{Version: "1.2.3", Commit: "1a2b3c4d"}, want: "v1.2.3-1a2b3c4d"},
		{name: "dirty release", info: Info{Version: "1.2.3", Commit: "1a2b3c4d", Modified: true}, want: "v1.2.3-1a2b3c4d-dirty"},
		{name: "untagged commit", info: Info{Version: "dev", Commit: "1a2b3c4d"}, want: "dev-1a2b3c4d"},
		{name: "no version no commit", info: Info{Version: "dev", Commit: "unknown"}, want: "dev"},
		{name: "no version dirty", info: Info{Version: "dev", Commit: "unknown", Modified: true}, want: "dev-dirty"},
		{name: "describe string with dirty", info: Info{Version: "e9eb006-dirty", Commit: "1a2b3c4d"}, want: "ve9eb006-dirty-1a2b3c4d"},
		{name: "version already prefixed", info: Info{Version: "v1.2.3", Commit: "1a2b3c4d"}, want: "v1.2.3-1a2b3c4d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStringSentinelNeverGluesV is the DF-CRIER-171 format clause: the "v"
// prefix belongs to a real stamped version. The DefaultVersion sentinel is
// rendered bare, so no identity an unstamped build prints can read "vdev" —
// the send-off form ("dev-<commit>") names the commit, and the only zero-commit
// form is the bare "dev" / "dev-dirty" pair.
func TestStringSentinelNeverGluesV(t *testing.T) {
	for _, tc := range []struct {
		name string
		info Info
		want string
	}{
		{name: "sentinel with commit", info: Info{Version: DefaultVersion, Commit: "1a2b3c4d"}, want: "dev-1a2b3c4d"},
		{name: "sentinel with commit, dirty tree", info: Info{Version: DefaultVersion, Commit: "1a2b3c4d", Modified: true}, want: "dev-1a2b3c4d-dirty"},
		{name: "sentinel with sentinel commit", info: Info{Version: DefaultVersion, Commit: DefaultCommit}, want: "dev"},
		{name: "sentinel with sentinel commit, dirty tree", info: Info{Version: DefaultVersion, Commit: DefaultCommit, Modified: true}, want: "dev-dirty"},
		// A sentinel stamped WITH a leading "v" (an operator passing
		// VERSION=vdev, say) is normalised to the same bare form.
		{name: "sentinel stamped with a v", info: Info{Version: "v" + DefaultVersion, Commit: "1a2b3c4d"}, want: "dev-1a2b3c4d"},
		// Real versions are untouched: the prefix still belongs to them.
		{name: "tagged", info: Info{Version: "1.2.3", Commit: "1a2b3c4d"}, want: "v1.2.3-1a2b3c4d"},
		{name: "describe string", info: Info{Version: "740ec81-dirty", Commit: "740ec816"}, want: "v740ec81-dirty-740ec816"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.info.String()
			if got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "vdev") {
				t.Errorf("String() = %q contains \"vdev\" — the sentinel must never carry the version prefix (DF-CRIER-171)", got)
			}
		})
	}
}

// TestBareBuildIdentityIsCommitBearing is the acceptance proof for the bare
// `go build` artifact: no ldflags, no version — the identity the binary prints
// must still name the commit the Go toolchain recorded for a checkout
// (vcs.revision) and must not glue a "v" onto the sentinel. Measured at HEAD
// before the fix: `go build -o /tmp/bare ./cmd/server && /tmp/bare -version`
// -> "crier vdev-740ec816-dirty".
func TestBareBuildIdentityIsCommitBearing(t *testing.T) {
	stubVars(t, DefaultVersion, DefaultCommit, DefaultBuildTime)
	stubBuildInfo(t, vcsBuildInfo(fullRevision, commitTime, true), true)

	got := String()
	if !strings.Contains(got, shortPrefix) {
		t.Errorf("bare-build identity %q does not name the commit %q", got, shortPrefix)
	}
	if strings.Contains(got, "vdev") {
		t.Errorf("bare-build identity %q contains \"vdev\"", got)
	}
	if want := "dev-" + shortPrefix + "-dirty"; got != want {
		t.Errorf("bare-build identity = %q, want %q", got, want)
	}
}

// TestVersionSegment pins the accessor the MCP handshake serves as
// serverInfo.version (DF-CRIER-171): the version half of the one identity,
// bare, so it is a slice of buildinfo.String() rather than a second format.
func TestVersionSegment(t *testing.T) {
	// readBuildInfo is stubbed out so the version can only come from the
	// linker-injected variables.
	stubBuildInfo(t, nil, false)

	for _, tc := range []struct {
		name            string
		version, commit string
		want            string
	}{
		{name: "stamped version", version: "1.2.3", commit: "1a2b3c4d", want: "1.2.3"},
		{name: "stamped version with v", version: "v1.2.3", commit: "1a2b3c4d", want: "1.2.3"},
		{name: "describe string", version: "740ec81-dirty", commit: "740ec816", want: "740ec81-dirty"},
		{name: "sentinel", version: DefaultVersion, commit: "1a2b3c4d", want: DefaultVersion},
		{name: "empty stamp", version: "", commit: "", want: DefaultVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubVars(t, tc.version, tc.commit, DefaultBuildTime)
			if got := VersionSegment(); got != tc.want {
				t.Errorf("VersionSegment() = %q, want %q", got, tc.want)
			}
			// The segment must appear verbatim inside the canonical
			// identity — that is what makes the two surfaces derivable
			// from one another.
			if identity := String(); !strings.Contains(identity, VersionSegment()) {
				t.Errorf("identity %q does not carry the version segment %q", identity, VersionSegment())
			}
		})
	}
}

// TestResolveStripsLeadingV: a version stamped as "v1.2.3" must not render as
// "vv1.2.3" — the canonical string owns the prefix.
func TestResolveStripsLeadingV(t *testing.T) {
	stubVars(t, "v1.2.3", "abc1234", "2026-01-02T03:04:05Z")
	stubBuildInfo(t, vcsBuildInfo(fullRevision, commitTime, false), true)

	got := Resolve()
	if got.Version != "1.2.3" {
		t.Errorf("Version = %q, want %q", got.Version, "1.2.3")
	}
	if want := "v1.2.3-abc1234"; got.String() != want {
		t.Errorf("String() = %q, want %q", got.String(), want)
	}
}

// TestShorten documents exactly which revisions are shortened: hex object
// names only, so an operator-supplied tag is never truncated.
func TestShorten(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "full sha", in: fullRevision, want: shortPrefix},
		{name: "short sha unchanged", in: "1a2b3c4", want: "1a2b3c4"},
		{name: "exactly commitLen", in: shortPrefix, want: shortPrefix},
		{name: "non-hex long value kept whole", in: "release-candidate-1", want: "release-candidate-1"},
		{name: "uppercase hex shortened", in: "ABCDEF0123456789", want: "ABCDEF01"},
		{name: "sentinel", in: DefaultCommit, want: DefaultCommit},
		{name: "empty", in: "", want: ""},
		{name: "whitespace trimmed", in: "  " + fullRevision + "  ", want: shortPrefix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Shorten(tc.in); got != tc.want {
				t.Errorf("Shorten(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestInfoJSONContract pins the wire shape of GET /version, which the server
// handler serves straight from Resolve().
func TestInfoJSONContract(t *testing.T) {
	stubVars(t, "1.2.3", "1a2b3c4d", "2026-01-02T03:04:05Z")
	stubBuildInfo(t, vcsBuildInfo(fullRevision, commitTime, false), true)

	raw, err := json.Marshal(Resolve())
	if err != nil {
		t.Fatalf("marshal Info: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal Info JSON: %v", err)
	}

	want := map[string]any{
		"version":    "1.2.3",
		"commit":     "1a2b3c4d",
		"build_time": "2026-01-02T03:04:05Z",
		"modified":   false,
	}
	if len(decoded) != len(want) {
		t.Fatalf("JSON keys = %v, want exactly %d keys %v", decoded, len(want), want)
	}
	for key, wantValue := range want {
		if got := decoded[key]; got != wantValue {
			t.Errorf("JSON %q = %#v, want %#v", key, got, wantValue)
		}
	}
}
