package cli

import (
	"os"

	"golang.org/x/term"
)

// isInteractive reports whether stdin is connected to a terminal.
func isInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// isTerminalOutput reports whether stdout is connected to a terminal. Progress that rewrites a
// line in place is only legible there; piped or redirected output should be written once.
func isTerminalOutput() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}
