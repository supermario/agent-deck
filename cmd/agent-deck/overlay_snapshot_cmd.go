package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/asheshgoplani/agent-deck/internal/web"
)

// handleOverlaySnapshot prints this machine's overlay rows as JSON.
//
// A controller runs this over SSH to pull another machine's sessions into one
// overlay. Read-only, and it does not need a TUI running: the rows come from
// stored session state plus this machine's own panes and transcripts, which is
// exactly why the controller cannot derive them itself.
func handleOverlaySnapshot(profile string, args []string) {
	fs := flag.NewFlagSet("overlay-snapshot", flag.ExitOnError)
	jsonOutput := fs.Bool("json", true, "Output as JSON (the only supported format)")
	explain := fs.Bool("explain", false, "Also report, on stderr, what was kept and dropped")
	maxIdle := fs.Duration("max-idle", 0, "Include sessions idle up to this long (default: the tool's cache TTL plus an hour)")
	includeRemotes := fs.Bool("include-remotes", false, "Also pull every configured remote, showing what the overlay receives")

	fs.Usage = func() {
		fmt.Println("Usage: agent-deck overlay-snapshot [--json]")
		fmt.Println()
		fmt.Println("Print this machine's overlay rows (sessions with status, countdown,")
		fmt.Println("shell/agent counts and PR refs) as JSON.")
		fmt.Println()
		fmt.Println("Intended for a controller pulling this deck into a single overlay:")
		fmt.Println("  agent-deck remote add work mario@workbox")
		fmt.Println("  # the controller then runs this over SSH every few seconds")
	}

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	_ = jsonOutput

	build := web.BuildOverlaySnapshotJSON
	if *includeRemotes {
		build = web.BuildOverlayFleetJSON
	}
	out, err := build(profile, *maxIdle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
	if *explain {
		fmt.Fprintln(os.Stderr, web.ExplainOverlaySnapshot(profile, *maxIdle))
	}
}
