package config

import (
	"fmt"
	"net/url"
	"strings"
)

// databaseScheme preserves PostgreSQL keyword/value DSNs (no URL scheme).
func databaseScheme(raw string) string {
	scheme, _, ok := strings.Cut(strings.TrimSpace(raw), ":")
	if !ok || strings.ContainsAny(scheme, "= /\t\n") {
		return ""
	}
	return strings.ToLower(scheme)
}

func databaseURLPath(raw, source string) (string, error) {
	scheme := databaseScheme(raw)
	switch scheme {
	case "", "postgres", "postgresql":
		return "", nil
	case "sqlite":
		u, err := url.Parse(raw)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host+u.Path == "" {
			return "", fmt.Errorf("DATABASE_URL_SCHEME: %s requires sqlite:///absolute-path or sqlite://relative-path (no credentials, query or fragment); CR_REGISTRY_SQLITE_PATH and CR_SQLITE_PATH override URL-derived paths", source)
		}
		return u.Host + u.Path, nil
	default:
		return "", fmt.Errorf("DATABASE_URL_SCHEME: %s has scheme %q (accepted: postgres, postgresql, sqlite); set %s to an accepted database URL", source, scheme, source)
	}
}

func (c Config) validateDatabaseBackend(source, variable, backend string) error {
	scheme := databaseScheme(c.Database.URL)
	wanted, alternative := "", ""
	if backend == "postgres" && scheme == "sqlite" {
		wanted, alternative = "postgres or postgresql", "sqlite"
	} else if backend == "sqlite" && (scheme == "postgres" || scheme == "postgresql") {
		wanted, alternative = "sqlite", "postgres"
	}
	if wanted == "" {
		return nil
	}
	return fmt.Errorf("DATABASE_BACKEND_MISMATCH: %s has scheme %q but %s=%s requires %s; set %s to a %s URL or set %s=%s; CR_REGISTRY_SQLITE_PATH and CR_SQLITE_PATH override URL-derived paths, not the URL scheme", source, scheme, variable, backend, wanted, source, wanted, variable, alternative)
}
