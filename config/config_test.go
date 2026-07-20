package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/totalwindupflightsystems/crier/config"
)

// unsetAll clears every env var consumed by config.Load() so each test
// can set only what it cares about and rely on documented defaults.
func unsetAll(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CR_AUTH_TOKEN",
		"CR_LOG_LEVEL",
		"CR_LOG_FORMAT",
		"CRIER_PORT",
		"CR_DATABASE_URL",
		"DATABASE_URL",
		"CRIER_DATABASE_URL",
		"CR_DATABASE_MAX_CONNS",
		"CR_DATABASE_MIN_CONNS",
		"CR_DATABASE_MAX_CONN_LIFETIME",
		"CR_DATABASE_MAX_CONN_IDLE_TIME",
		"CR_DATABASE_CONNECT_TIMEOUT",
	} {
		t.Setenv(k, "")
	}
}

// ---------- Defaults ----------

func TestLoad_Defaults(t *testing.T) {
	unsetAll(t)

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, 8767, cfg.Port, "default Port")
	assert.Empty(t, cfg.AuthToken, "default AuthToken is empty (no env var)")
	assert.Equal(t, "info", cfg.LogLevel, "default LogLevel")
	assert.Equal(t, "text", cfg.LogFormat, "default LogFormat")

	assert.Empty(t, cfg.Database.URL, "default Database.URL is empty")
	assert.Equal(t, int32(4), cfg.Database.MaxConns, "default MaxConns")
	assert.Equal(t, int32(0), cfg.Database.MinConns, "default MinConns")
	assert.Equal(t, 30*time.Minute, cfg.Database.MaxConnLifetime, "default MaxConnLifetime")
	assert.Equal(t, 5*time.Minute, cfg.Database.MaxConnIdleTime, "default MaxConnIdleTime")
	assert.Equal(t, 10*time.Second, cfg.Database.ConnectTimeout, "default ConnectTimeout")
}

// ---------- LogLevel ----------

func TestLoad_LogLevel_Valid(t *testing.T) {
	unsetAll(t)

	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			t.Setenv("CR_LOG_LEVEL", level)
			cfg, err := config.Load()
			require.NoError(t, err)
			assert.Equal(t, level, cfg.LogLevel)
		})
	}
}

func TestLoad_LogLevel_Invalid(t *testing.T) {
	unsetAll(t)

	for _, level := range []string{"trace", "DEBUG", "Info", "fatal", "verbose", "0", "5"} {
		t.Run(level, func(t *testing.T) {
			t.Setenv("CR_LOG_LEVEL", level)
			cfg, err := config.Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "CR_LOG_LEVEL")
			assert.Equal(t, "info", cfg.LogLevel, "LogLevel should retain default on error")
		})
	}
}

// ---------- LogFormat ----------

func TestLoad_LogFormat_Valid(t *testing.T) {
	unsetAll(t)

	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			t.Setenv("CR_LOG_FORMAT", format)
			cfg, err := config.Load()
			require.NoError(t, err)
			assert.Equal(t, format, cfg.LogFormat)
		})
	}
}

func TestLoad_LogFormat_Invalid(t *testing.T) {
	unsetAll(t)

	for _, format := range []string{"yaml", "xml", "TEXT", "JSON", "plain"} {
		t.Run(format, func(t *testing.T) {
			t.Setenv("CR_LOG_FORMAT", format)
			cfg, err := config.Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "CR_LOG_FORMAT")
			assert.Equal(t, "text", cfg.LogFormat, "LogFormat should retain default on error")
		})
	}
}

// ---------- Port ----------

func TestLoad_Port_Valid(t *testing.T) {
	unsetAll(t)

	cases := []struct {
		name string
		port string
		want int
	}{
		{"low boundary", "1", 1},
		{"high boundary", "65535", 65535},
		{"common http-alt", "8080", 8080},
		{"https", "443", 443},
		{"low even", "2", 2},
		{"high even", "65534", 65534},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CRIER_PORT", tc.port)
			cfg, err := config.Load()
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.Port)
		})
	}
}

func TestLoad_Port_Invalid(t *testing.T) {
	unsetAll(t)

	cases := []struct {
		name string
		port string
	}{
		{"non-numeric", "abc"},
		{"empty string handled as default", ""}, // empty -> default, not an error
		{"zero", "0"},
		{"negative", "-1"},
		{"too high", "65536"},
		{"way too high", "99999"},
		{"float", "80.5"},
		{"hex", "0x50"},
		{"with spaces", " 8080"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CRIER_PORT", tc.port)
			cfg, err := config.Load()
			// Empty string is treated as "not set" -> default. Error otherwise.
			if tc.port == "" {
				require.NoError(t, err)
				assert.Equal(t, 8767, cfg.Port)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "CRIER_PORT")
			assert.Equal(t, 8767, cfg.Port, "Port should retain default on error")
		})
	}
}

// ---------- Database URL precedence ----------

func TestLoad_DBURL_CR_DATABASE_URL_Wins(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_DATABASE_URL", "postgres://cr")
	t.Setenv("DATABASE_URL", "postgres://legacy")
	t.Setenv("CRIER_DATABASE_URL", "postgres://crier")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://cr", cfg.Database.URL)
}

func TestLoad_DBURL_DefaultPrefixUsedWhenHigherUnset(t *testing.T) {
	unsetAll(t)
	t.Setenv("DATABASE_URL", "postgres://legacy")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://legacy", cfg.Database.URL)
}

func TestLoad_DBURL_CRIERUsedWhenOthersUnset(t *testing.T) {
	unsetAll(t)
	t.Setenv("CRIER_DATABASE_URL", "postgres://crier")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://crier", cfg.Database.URL)
}

func TestLoad_DBURL_EmptyByDefault(t *testing.T) {
	unsetAll(t)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.Database.URL)
}

func TestLoad_DBURL_AllEmptyDefaultsToEmpty(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.Database.URL)
}

// ---------- MaxConns / MinConns ----------

func TestLoad_MaxConns_Valid(t *testing.T) {
	unsetAll(t)

	cases := []string{"1", "2", "8", "100", "32767"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv("CR_DATABASE_MAX_CONNS", v)
			cfg, err := config.Load()
			require.NoError(t, err)
			n, err := parseInt32(v)
			require.NoError(t, err)
			assert.Equal(t, n, cfg.Database.MaxConns)
		})
	}
}

func TestLoad_MaxConns_Invalid(t *testing.T) {
	unsetAll(t)

	cases := []struct {
		name string
		val  string
	}{
		{"non-numeric", "abc"},
		{"zero", "0"},
		{"negative", "-5"},
		{"float", "4.5"},
		{"hex", "0x4"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CR_DATABASE_MAX_CONNS", tc.val)
			cfg, err := config.Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "CR_DATABASE_MAX_CONNS")
			assert.Equal(t, int32(4), cfg.Database.MaxConns, "MaxConns should retain default on error")
		})
	}
}

func TestLoad_MinConns_Valid(t *testing.T) {
	unsetAll(t)

	cases := []struct {
		val  string
		want int32
	}{
		{"0", 0},
		{"1", 1},
		{"2", 2},
		{"100", 100},
	}
	for _, tc := range cases {
		t.Run("val="+tc.val, func(t *testing.T) {
			t.Setenv("CR_DATABASE_MIN_CONNS", tc.val)
			cfg, err := config.Load()
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.Database.MinConns)
		})
	}
}

func TestLoad_MinConns_Invalid(t *testing.T) {
	unsetAll(t)

	cases := []struct {
		name string
		val  string
	}{
		{"non-numeric", "xyz"},
		{"negative", "-1"},
		{"float", "1.5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CR_DATABASE_MIN_CONNS", tc.val)
			cfg, err := config.Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "CR_DATABASE_MIN_CONNS")
		})
	}
}

func TestLoad_MinConnsGreaterThanMaxConns(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_DATABASE_MAX_CONNS", "2")
	t.Setenv("CR_DATABASE_MIN_CONNS", "5")

	cfg, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CR_DATABASE_MIN_CONNS")
	assert.Contains(t, err.Error(), "CR_DATABASE_MAX_CONNS")
	// Both fields retain their parsed values so the caller can see what was attempted.
	assert.Equal(t, int32(2), cfg.Database.MaxConns)
	assert.Equal(t, int32(5), cfg.Database.MinConns)
}

func TestLoad_MinConnsEqualMaxConns_OK(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_DATABASE_MAX_CONNS", "4")
	t.Setenv("CR_DATABASE_MIN_CONNS", "4")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, int32(4), cfg.Database.MinConns)
	assert.Equal(t, int32(4), cfg.Database.MaxConns)
}

// ---------- Durations ----------

func TestLoad_DurationFields_Valid(t *testing.T) {
	unsetAll(t)

	cases := []struct {
		name     string
		lifetime string
		idle     string
		timeout  string
		wantLife time.Duration
		wantIdle time.Duration
		wantTo   time.Duration
	}{
		{"seconds", "30s", "15s", "5s", 30 * time.Second, 15 * time.Second, 5 * time.Second},
		{"minutes", "10m", "2m", "30s", 10 * time.Minute, 2 * time.Minute, 30 * time.Second},
		{"hours", "1h", "30m", "10s", time.Hour, 30 * time.Minute, 10 * time.Second},
		{"mixed", "90s", "1m30s", "500ms", 90 * time.Second, 90 * time.Second, 500 * time.Millisecond},
		{"complex", "1h30m", "5m15s", "2s500ms", 90 * time.Minute, 5*time.Minute + 15*time.Second, 2*time.Second + 500*time.Millisecond},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CR_DATABASE_MAX_CONN_LIFETIME", tc.lifetime)
			t.Setenv("CR_DATABASE_MAX_CONN_IDLE_TIME", tc.idle)
			t.Setenv("CR_DATABASE_CONNECT_TIMEOUT", tc.timeout)

			cfg, err := config.Load()
			require.NoError(t, err)
			assert.Equal(t, tc.wantLife, cfg.Database.MaxConnLifetime)
			assert.Equal(t, tc.wantIdle, cfg.Database.MaxConnIdleTime)
			assert.Equal(t, tc.wantTo, cfg.Database.ConnectTimeout)
		})
	}
}

func TestLoad_DurationFields_Invalid(t *testing.T) {
	unsetAll(t)

	cases := []struct {
		name     string
		lifetime string
		idle     string
		timeout  string
		wantSub  string
	}{
		{"all unparseable", "abc", "xyz", "qqq", "invalid"},
		{"lifetime zero", "0s", "5m", "10s", "CR_DATABASE_MAX_CONN_LIFETIME"},
		{"lifetime negative", "-5m", "5m", "10s", "CR_DATABASE_MAX_CONN_LIFETIME"},
		{"idle zero", "30m", "0s", "10s", "CR_DATABASE_MAX_CONN_IDLE_TIME"},
		{"idle negative", "30m", "-1m", "10s", "CR_DATABASE_MAX_CONN_IDLE_TIME"},
		{"timeout zero", "30m", "5m", "0s", "CR_DATABASE_CONNECT_TIMEOUT"},
		{"timeout negative", "30m", "5m", "-1s", "CR_DATABASE_CONNECT_TIMEOUT"},
		{"lifetime no unit", "30", "5m", "10s", "CR_DATABASE_MAX_CONN_LIFETIME"},
		{"idle wrong unit", "30m", "5minutes", "10s", "CR_DATABASE_MAX_CONN_IDLE_TIME"},
		{"timeout wrong unit", "30m", "5m", "10seconds", "CR_DATABASE_CONNECT_TIMEOUT"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CR_DATABASE_MAX_CONN_LIFETIME", tc.lifetime)
			t.Setenv("CR_DATABASE_MAX_CONN_IDLE_TIME", tc.idle)
			t.Setenv("CR_DATABASE_CONNECT_TIMEOUT", tc.timeout)

			cfg, err := config.Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSub)
			// Untouched fields retain defaults.
			assert.Equal(t, 30*time.Minute, cfg.Database.MaxConnLifetime)
			assert.Equal(t, 5*time.Minute, cfg.Database.MaxConnIdleTime)
			assert.Equal(t, 10*time.Second, cfg.Database.ConnectTimeout)
		})
	}
}

// ---------- AuthToken ----------

func TestLoad_AuthToken(t *testing.T) {
	t.Run("empty by default", func(t *testing.T) {
		unsetAll(t)
		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Empty(t, cfg.AuthToken)
	})

	t.Run("picked up from env", func(t *testing.T) {
		unsetAll(t)
		t.Setenv("CR_AUTH_TOKEN", "super-secret-token-abc123")
		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Equal(t, "super-secret-token-abc123", cfg.AuthToken)
	})

	t.Run("empty string is not an error", func(t *testing.T) {
		unsetAll(t)
		t.Setenv("CR_AUTH_TOKEN", "")
		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Empty(t, cfg.AuthToken)
	})
}

// ---------- All env vars set together ----------

func TestLoad_AllEnvVarsSet(t *testing.T) {
	unsetAll(t)

	t.Setenv("CR_AUTH_TOKEN", "tok-123")
	t.Setenv("CR_LOG_LEVEL", "debug")
	t.Setenv("CR_LOG_FORMAT", "json")
	t.Setenv("CRIER_PORT", "9999")
	t.Setenv("CR_DATABASE_URL", "postgres://from-cr")
	t.Setenv("DATABASE_URL", "postgres://from-legacy")
	t.Setenv("CRIER_DATABASE_URL", "postgres://from-crier")
	t.Setenv("CR_DATABASE_MAX_CONNS", "20")
	t.Setenv("CR_DATABASE_MIN_CONNS", "2")
	t.Setenv("CR_DATABASE_MAX_CONN_LIFETIME", "45m")
	t.Setenv("CR_DATABASE_MAX_CONN_IDLE_TIME", "7m")
	t.Setenv("CR_DATABASE_CONNECT_TIMEOUT", "15s")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, "tok-123", cfg.AuthToken)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "json", cfg.LogFormat)
	assert.Equal(t, 9999, cfg.Port)
	assert.Equal(t, "postgres://from-cr", cfg.Database.URL, "CR_DATABASE_URL wins over DATABASE_URL and CRIER_DATABASE_URL")
	assert.Equal(t, int32(20), cfg.Database.MaxConns)
	assert.Equal(t, int32(2), cfg.Database.MinConns)
	assert.Equal(t, 45*time.Minute, cfg.Database.MaxConnLifetime)
	assert.Equal(t, 7*time.Minute, cfg.Database.MaxConnIdleTime)
	assert.Equal(t, 15*time.Second, cfg.Database.ConnectTimeout)
}

// ---------- Edge case: empty string is treated as "unset" for non-precedence fields ----------

func TestLoad_EmptyStringTreatedAsUnset(t *testing.T) {
	unsetAll(t)

	// Set values, then explicitly set them back to empty strings — these
	// should be treated as "not provided" and fall back to defaults, not as
	// values that should fail validation.
	t.Setenv("CR_LOG_LEVEL", "")
	t.Setenv("CR_LOG_FORMAT", "")
	t.Setenv("CRIER_PORT", "")
	t.Setenv("CR_DATABASE_MAX_CONNS", "")
	t.Setenv("CR_DATABASE_MIN_CONNS", "")
	t.Setenv("CR_DATABASE_MAX_CONN_LIFETIME", "")
	t.Setenv("CR_DATABASE_MAX_CONN_IDLE_TIME", "")
	t.Setenv("CR_DATABASE_CONNECT_TIMEOUT", "")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "text", cfg.LogFormat)
	assert.Equal(t, 8767, cfg.Port)
	assert.Equal(t, int32(4), cfg.Database.MaxConns)
	assert.Equal(t, int32(0), cfg.Database.MinConns)
	assert.Equal(t, 30*time.Minute, cfg.Database.MaxConnLifetime)
	assert.Equal(t, 5*time.Minute, cfg.Database.MaxConnIdleTime)
	assert.Equal(t, 10*time.Second, cfg.Database.ConnectTimeout)
}

// parseInt32 is a tiny helper for cross-checking MaxConns parsing.
func parseInt32(s string) (int32, error) {
	var n int64
	var err error
	// Mirror the production parser: base 10, bitSize 32.
	// We import strconv here too — but to keep this file's imports tight
	// we just delegate via Load when needed; instead, this helper does
	// the equivalent parsing inline.
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, assert.AnError
		}
		n = n*10 + int64(c-'0')
	}
	if err != nil {
		return 0, err
	}
	if n > 1<<31-1 {
		return 0, assert.AnError
	}
	return int32(n), nil
}