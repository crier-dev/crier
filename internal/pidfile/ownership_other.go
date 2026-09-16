//go:build !linux

package pidfile

import "errors"

// procExe is the non-linux fallback: /proc/<pid>/exe does not exist, so
// ownership can never be verified. SafeToSignal therefore fails closed on
// every platform but linux — refusing to signal is always safe.
func procExe(pid int) (string, error) {
	return "", errors.New("pidfile: ownership check unsupported on this platform")
}
