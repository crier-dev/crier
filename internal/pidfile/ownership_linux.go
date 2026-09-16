//go:build linux

package pidfile

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// procExe returns the executable path of pid as the kernel sees it
// (readlink /proc/<pid>/exe). It lives in its own file so a build tag is
// possible; crier targets linux. A missing process yields ErrNotAlive;
// anything else (e.g. EACCES on a foreign user's process) is returned
// verbatim so SafeToSignal can fail closed with the cause.
func procExe(pid int) (string, error) {
	link := fmt.Sprintf("/proc/%d/exe", pid)
	live, err := os.Readlink(link)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return "", ErrNotAlive
		}
		return "", err
	}
	// The kernel appends " (deleted)" when the original binary was
	// unlinked (typically a rebuild). The path still identifies the
	// process this pidfile recorded, so compare the path itself.
	return strings.TrimSuffix(live, " (deleted)"), nil
}
