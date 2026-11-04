package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/build"
)

// harnessProbeTimeout bounds `crier harness list` / `probe` as a whole; each
// adapter's own version probe has a shorter budget.
const harnessProbeTimeout = 30 * time.Second

// runHarness dispatches `crier harness <subcommand>` (CRIER-HARNESS §4.4).
// It never starts the server, and it reads and probes only: there is no
// delivery verb, because delivery is an ingress action, not an agent action.
func runHarness(args []string, out, errOut io.Writer, getenv config.Getenv) int {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	cfg := harness.LoadConfig(getenv)
	reg := build.Registry(cfg)

	if len(args) == 0 {
		harnessUsage(errOut)
		return 2
	}
	switch args[0] {
	case "list":
		return harnessList(reg, out, errOut)
	case "probe":
		if len(args) < 2 {
			_, _ = fmt.Fprintln(errOut, "crier harness probe: needs an adapter name")
			harnessUsage(errOut)
			return 2
		}
		return harnessProbe(reg, args[1], out, errOut)
	case "help", "-help", "--help", "-h":
		harnessUsage(out)
		return 0
	default:
		_, _ = fmt.Fprintf(errOut, "crier harness: unknown subcommand %q\n", args[0])
		harnessUsage(errOut)
		return 2
	}
}

// harnessList prints the capability table: name, available, version, features,
// reason. An unavailable adapter never changes the exit code — listing is
// reporting, not gating.
func harnessList(reg *harness.Registry, out, errOut io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), harnessProbeTimeout)
	defer cancel()
	caps := reg.Capabilities(ctx)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tAVAILABLE\tVERSION\tFEATURES\tREASON")
	for _, name := range sortedNames(caps) {
		writeCapabilityRow(w, name, caps[name])
	}
	if err := w.Flush(); err != nil {
		_, _ = fmt.Fprintf(errOut, "crier harness list: %v\n", err)
		return 1
	}
	return 0
}

// harnessProbe prints one capability row and exits 1 when it is unavailable, so
// a script can gate on it.
func harnessProbe(reg *harness.Registry, name string, out, errOut io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), harnessProbeTimeout)
	defer cancel()
	cap, err := reg.Probe(ctx, name)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "crier harness probe: %s is not a registered adapter (known: %s)\n",
			name, strings.Join(reg.Names(), ", "))
		return 1
	}
	// The machine-readable form is one JSON object; the CLI's human form is
	// the same table row `list` prints. Both are emitted: a script reads the
	// JSON, an operator reads the row.
	encoded, mErr := json.Marshal(cap)
	if mErr != nil {
		_, _ = fmt.Fprintf(errOut, "crier harness probe: encode capability: %v\n", mErr)
		return 1
	}
	_, _ = fmt.Fprintln(out, string(encoded))
	if !cap.Available {
		return 1
	}
	return 0
}

func writeCapabilityRow(w io.Writer, name string, cap harness.Capability) {
	_, _ = fmt.Fprintf(w, "%s\t%t\t%s\t%s\t%s\n",
		name, cap.Available, cap.Version, featureList(cap.Features), cap.Reason)
}

// featureList renders the enabled features sorted, so the column is stable
// across runs and diffable in a test.
func featureList(features map[string]bool) string {
	var on []string
	for name, enabled := range features {
		if enabled {
			on = append(on, name)
		}
	}
	sort.Strings(on)
	return strings.Join(on, ",")
}

func sortedNames(caps map[string]harness.Capability) []string {
	names := make([]string, 0, len(caps))
	for name := range caps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func harnessUsage(out io.Writer) {
	_, _ = fmt.Fprintln(out, "usage: crier harness list | crier harness probe <name>")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "  list         probe every configured adapter (name, available, version, features, reason)")
	_, _ = fmt.Fprintln(out, "  probe NAME   probe one adapter; exit 1 when it is unavailable")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Configuration: harness.timeout_seconds, max_concurrent and adapters.* in the")
	_, _ = fmt.Fprintln(out, "relay config (CR_HARNESS_* env overrides; CR_HARNESS_CONFIG names the file).")
}
