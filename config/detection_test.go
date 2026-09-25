package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/detect"
)

// CR-FEAT-030: the detection layer's env surface. Two properties matter more
// than the parsing itself — the feature is OFF by default (an existing
// deployment is unaffected), and a declared-but-invalid setting is a startup
// ERROR rather than a silently ignored one.
func TestLoad_DetectionDefaultsOff(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_DETECT_ENABLED", "")
	t.Setenv("CR_DETECT_LOG", "")
	t.Setenv("CR_DETECT_KEY", "")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.False(t, cfg.Detection.Enabled, "detection must be opt-in")
	assert.Empty(t, cfg.Detection.LogPath, "no log path by default — no file is written")
	assert.Equal(t, detect.DefaultQuietStartHour, cfg.Detection.QuietStartHour, "documented quiet-window start")
	assert.Equal(t, detect.DefaultQuietEndHour, cfg.Detection.QuietEndHour, "documented quiet-window end")
	// The thresholds keep their zero value, so the ONE place they are defined
	// stays internal/detect's constants (never a second copy in config).
	assert.Zero(t, cfg.Detection.FanoutMinTargets)
	assert.Zero(t, cfg.Detection.NewPeerMinTargets)
}

func TestLoad_DetectionFullSurface(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_DETECT_ENABLED", "true")
	t.Setenv("CR_DETECT_LOG", "/var/lib/crier/delivery.jsonl")
	t.Setenv("CR_DETECT_KEY", "/var/lib/crier/delivery.key")
	t.Setenv("CR_DETECT_FANOUT_WINDOW_S", "30")
	t.Setenv("CR_DETECT_FANOUT_MIN_TARGETS", "7")
	t.Setenv("CR_DETECT_NEWPEER_WINDOW_S", "45")
	t.Setenv("CR_DETECT_NEWPEER_MIN_TARGETS", "4")
	t.Setenv("CR_DETECT_QUIET_HOURS", "22-6")
	t.Setenv("CR_DETECT_QUIET_MIN_MESSAGES", "9")
	t.Setenv("CR_CANARY_TOKENS", "tok-a, tok-b")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.True(t, cfg.Detection.Enabled)
	assert.Equal(t, "/var/lib/crier/delivery.jsonl", cfg.Detection.LogPath)
	assert.Equal(t, "/var/lib/crier/delivery.key", cfg.Detection.KeyPath)
	assert.Equal(t, 30*time.Second, cfg.Detection.FanoutWindow)
	assert.Equal(t, 7, cfg.Detection.FanoutMinTargets)
	assert.Equal(t, 45*time.Second, cfg.Detection.NewPeerWindow)
	assert.Equal(t, 4, cfg.Detection.NewPeerMinTargets)
	assert.Equal(t, 22, cfg.Detection.QuietStartHour)
	assert.Equal(t, 6, cfg.Detection.QuietEndHour)
	assert.Equal(t, 9, cfg.Detection.QuietMinMessages)
	assert.Equal(t, []string{"tok-a", "tok-b"}, cfg.Detection.CanaryTokens)
}

// TestLoad_DetectionKeyPathDefaults: a log without a key path is signed with
// <log>.key. The alternative — an unsigned log — would make every record
// unverifiable, which is the one thing this log exists to prevent.
func TestLoad_DetectionKeyPathDefaults(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_DETECT_ENABLED", "true")
	t.Setenv("CR_DETECT_LOG", "/tmp/delivery.jsonl")
	t.Setenv("CR_DETECT_KEY", "")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "/tmp/delivery.jsonl.key", cfg.Detection.KeyPath)
}

// TestLoad_DetectionQuietHoursZeroZeroDisables pins the documented escape
// hatch: an explicit equal pair turns the odd-hour signal off, which is a
// different thing from leaving it unset.
func TestLoad_DetectionQuietHoursZeroZeroDisables(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_DETECT_QUIET_HOURS", "0-0")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, cfg.Detection.QuietStartHour, cfg.Detection.QuietEndHour,
		"0-0 must be a disabled window, not the default one")
}

func TestLoad_DetectionInvalidValuesAreStartupErrors(t *testing.T) {
	cases := []struct{ name, env, value string }{
		{"enable", "CR_DETECT_ENABLED", "sometimes"},
		{"fanout window", "CR_DETECT_FANOUT_WINDOW_S", "0"},
		{"fanout targets", "CR_DETECT_FANOUT_MIN_TARGETS", "-1"},
		{"newpeer window", "CR_DETECT_NEWPEER_WINDOW_S", "soon"},
		{"newpeer targets", "CR_DETECT_NEWPEER_MIN_TARGETS", "0"},
		{"quiet hours shape", "CR_DETECT_QUIET_HOURS", "night"},
		{"quiet hours bound", "CR_DETECT_QUIET_HOURS", "25-3"},
		{"quiet hours end bound", "CR_DETECT_QUIET_HOURS", "1-24"},
		{"quiet messages", "CR_DETECT_QUIET_MIN_MESSAGES", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unsetAll(t)
			t.Setenv("CR_DETECT_ENABLED", "true")
			t.Setenv(tc.env, tc.value)
			_, err := config.Load()
			require.Error(t, err, "%s=%q must be refused at startup", tc.env, tc.value)
			assert.Contains(t, err.Error(), tc.env, "the error must name the variable")
		})
	}
}
