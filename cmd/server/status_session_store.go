package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/crier-dev/crier/config"
)

// sessionStoreLocation reports local storage evidence, NOT a durability promise.
// mountinfo cannot distinguish a Docker named volume from a bind mount, prove
// that the host will retain either, or inspect a PostgreSQL server's storage.
func sessionStoreLocation(cfg config.Config) (*string, string) {
	var path string
	switch cfg.ResolvedSessionBackend() {
	case config.SessionBackendPostgres:
		return nil, "external"
	case config.SessionBackendSQLite:
		path = cfg.Session.SQLitePath
		if path == "" {
			path = config.DefaultSQLitePath
		}
		if strings.HasPrefix(path, "file:") {
			u, err := url.Parse(path)
			if err != nil || u.Host != "" {
				return nil, "unknown"
			}
			if u.Query().Get("mode") == "memory" {
				path = ":memory:"
			} else if u.Opaque != "" {
				path, err = url.PathUnescape(u.Opaque)
				if err != nil {
					return nil, "unknown"
				}
			} else {
				path = u.Path
			}
		}
		if path == "" {
			return nil, "unknown" // unnamed SQLite URI: no local filename to report
		}
		if path == ":memory:" {
			return &path, "memory"
		}
	case config.SessionBackendJSONL:
		path = cfg.Session.LogRoot
		if path == "" {
			path = config.DefaultSessionLogRoot
		}
	default:
		return nil, "unknown"
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "unknown"
	}
	// The store has already opened the file/directory in production. Resolve
	// symlinks before comparing mountpoints; a /data link can target the layer.
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return &absolute, "unknown"
	}
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return &resolved, "unknown"
	}
	return &resolved, sessionMountKind(resolved, string(mountinfo))
}

// registryStoreLocation reports the registry path actually in use, or nil when
// the active backend keeps no local file: the in-memory store has none, and
// PostgreSQL's location is not observable from this process (and its URL is
// configuration, never posture). The path is reported only for the backend that
// opens it, so /status cannot name a file the store never touches (DF-CRIER-428).
func registryStoreLocation(cfg config.Config) *string {
	if cfg.ResolvedRegistryBackend() != "sqlite" || cfg.Registry.SQLitePath == "" {
		return nil
	}
	// Registry SQLite accepts file paths only — never :memory: or a URI — so
	// there is no DSN to unwrap here (see buildStatusResponse's durability note).
	absolute, err := filepath.Abs(cfg.Registry.SQLitePath)
	if err != nil {
		return nil
	}
	// The store has already opened the file in production; resolving symlinks
	// reports the file the process is really writing.
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return &resolved
	}
	return &absolute
}

// sessionMountKind selects the longest path-component mount prefix, not a
// string prefix (/data is not /database). A root disk mount is deliberately
// unknown: it may be bare metal OR a container without an overlay root.
func sessionMountKind(path, mountinfo string) string {
	best, kind := -1, "unknown"
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(mountinfo, "\n") {
		before, after, ok := strings.Cut(line, " - ")
		left, right := strings.Fields(before), strings.Fields(after)
		if !ok || len(left) < 6 || len(right) < 3 {
			continue
		}
		mount := unescape.Replace(left[4])
		if !filepath.IsAbs(mount) || (path != mount && !strings.HasPrefix(path, strings.TrimSuffix(mount, "/")+"/")) {
			continue
		}
		if len(mount) < best {
			continue
		}
		if len(mount) == best {
			// Stacked mounts require additional topology evidence. Do not
			// guess which filesystem is exposed from their listing order.
			kind = "unknown"
			continue
		}
		best = len(mount)
		switch right[0] {
		case "tmpfs", "ramfs":
			kind = "memory"
		case "overlay", "fuse.overlayfs":
			// A bind of a host overlay directory can itself be a mount;
			// only the namespace's overlay root is evidence of the layer.
			if mount == "/" {
				kind = "overlay-layer"
			} else {
				kind = "mounted-filesystem"
			}
		default:
			if mount == "/" {
				kind = "unknown"
			} else {
				kind = "mounted-filesystem"
			}
		}
	}
	return kind
}
