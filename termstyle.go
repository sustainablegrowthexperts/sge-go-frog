package main

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// ANSI SGR fragments for stdout when attached to a TTY and NO_COLOR is unset.
var (
	styleReset   string
	styleTitle   string // banner
	styleHeading string // section titles
	styleLabel   string // primary prompt lines
	styleNote    string // secondary hints
	styleDim     string // muted / caret
	styleWarn    string // validation retries
	styleOK      string // success accent
	styleErr     string // errors
)

func init() {
	initStyles()
}

func initStyles() {
	if os.Getenv("NO_COLOR") != "" {
		return
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return
	}
	const esc = "\x1b["
	styleReset = esc + "0m"
	styleTitle = esc + "1;36m"   // bold cyan
	styleHeading = esc + "1;35m" // bold magenta
	styleLabel = esc + "1m"      // bold (default foreground)
	styleNote = esc + "2;33m"    // dim yellow
	styleDim = esc + "2m"
	styleWarn = esc + "33m"
	styleOK = esc + "1;32m"
	styleErr = esc + "1;31m"
}

func printPromptCaret() {
	if styleDim != "" {
		fmt.Print(styleDim + "> " + styleReset)
		return
	}
	fmt.Print("> ")
}
