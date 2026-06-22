package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/ui"
)

// handleSessionSelect tells a running TUI to switch to the specified session via
// Unix socket. The TUI will detach from any current session and switch to the
// specified session.
func handleSessionSelect(profile string, args []string) {
	fs := flag.NewFlagSet("session select", flag.ExitOnError)

	fs.Usage = func() {
		fmt.Println("Usage: agent-deck session select <id|title>")
		fmt.Println()
		fmt.Println("Tell a running agent-deck TUI to switch to the specified session.")
		fmt.Println("If the TUI is currently attached to another session, it will")
		fmt.Println("detach first, then attach to the target session.")
		fmt.Println()
		fmt.Println("This command communicates with the running TUI via a Unix socket.")
		fmt.Println("If no TUI is running, the command will fail.")
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  agent-deck session select my-project")
		fmt.Println("  agent-deck session select abc123")
		fmt.Println("  agent-deck -p work session select my-project")
	}

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	identifier := fs.Arg(0)
	if identifier == "" {
		fs.Usage()
		os.Exit(1)
	}

	effectiveProfile := session.GetEffectiveProfile(profile)
	socketPath := ui.IPCSocketPath(effectiveProfile)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to running TUI (socket %s): %v\n", socketPath, err)
		fmt.Fprintln(os.Stderr, "Is agent-deck running?")
		os.Exit(1)
	}
	defer conn.Close()

	// Send the select command
	_, err = fmt.Fprintf(conn, "select %s\n", identifier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to send command: %v\n", err)
		os.Exit(1)
	}

	// Read response
	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		resp := scanner.Text()
		if resp == "ok" {
			fmt.Printf("Sent select command for: %s\n", identifier)
		} else {
			fmt.Fprintf(os.Stderr, "Error from TUI: %s\n", resp)
			os.Exit(1)
		}
	} else {
		fmt.Fprintln(os.Stderr, "Error: no response from TUI")
		os.Exit(1)
	}
}
