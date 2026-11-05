// Command studioctl is the operator CLI.
//
// M0 subcommands: version, config check. Database, backup and restore
// subcommands arrive with their milestones.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/config"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/version"
)

const usage = `studioctl: Dataset Studio operator CLI

Usage:
  studioctl version          print build information as JSON
  studioctl config check     validate STUDIO_* environment variables and
                             print the effective configuration (secrets redacted)
  studioctl help             show this help
`

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

func run(args []string, lookup config.Lookup, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		return printJSON(stdout, stderr, version.Get())
	case "config":
		if len(args) != 2 || args[1] != "check" {
			_, _ = fmt.Fprint(stderr, usage)
			return 2
		}
		cfg, err := config.Load(lookup)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "configuration invalid:\n%v\n", err)
			return 1
		}
		return printSummary(stdout, cfg.Summary())
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func printJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func printSummary(w io.Writer, m map[string]string) int {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(w, "%-18s %s\n", k, m[k])
	}
	return 0
}
