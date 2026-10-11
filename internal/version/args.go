package version

import (
	"fmt"
	"io"
	"os"
)

// HandleArgs answers --version and --help, and refuses any other argument,
// exiting in every case. It returns only when there are no arguments. The
// daemons are configured entirely through env vars, so an argument never means
// "start", and a stray flag must not bring up a second daemon (sair#73).
// Call it before anything else in main, including the auto-updater.
func HandleArgs(usage string) {
	if code, done := handleArgs(os.Args[1:], usage, os.Stdout, os.Stderr); done {
		os.Exit(code)
	}
}

func handleArgs(args []string, usage string, stdout, stderr io.Writer) (code int, done bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "--version", "-v":
		fmt.Fprintln(stdout, Version)
		return 0, true
	case "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0, true
	}
	fmt.Fprintf(stderr, "unknown argument: %s\n%s", args[0], usage)
	return 2, true
}
