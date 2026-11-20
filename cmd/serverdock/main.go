// Command serverdock is the single ServerDock binary: server | agent | audit | admin | version.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "serverdock", version)
		return 0
	case "server":
		return serverMain(args[1:], stderr)
	case "agent":
		return agentMain(args[1:], stderr)
	case "audit":
		return auditMain(args[1:], stdout, stderr)
	case "admin":
		return adminResetMain(args[1:], os.Stdin, stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "serverdock: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: serverdock <server|agent|audit|admin|version> [flags]")
}
