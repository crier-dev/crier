// Command mcp-capabilities generates the checked-in MCP capability report.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/crier-dev/crier/internal/mcp"
	"github.com/crier-dev/crier/internal/registry"
)

func main() {
	output := flag.String("output", "docs/mcp-capabilities.md", "report path")
	check := flag.Bool("check", false, "check the report instead of writing it")
	flag.Parse()

	server := mcp.New(registry.NewMemoryStore())
	want := mcp.CapabilityReportMarkdown(server.ToolNames())
	got, err := os.ReadFile(*output)
	if err != nil && !os.IsNotExist(err) {
		fatal(err)
	}
	if *check {
		if err != nil {
			fatal(err)
		}
		if string(got) != want {
			fatal(fmt.Errorf("%s is stale; run make generate", *output))
		}
		fmt.Printf("MCP capability report: PASS (%d tools, %s)\n", server.ToolCount(), *output)
		return
	}
	if err := os.WriteFile(*output, []byte(want), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("MCP capability report: generated %d tools at %s\n", server.ToolCount(), *output)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "mcp-capabilities: %v\n", err)
	os.Exit(1)
}
