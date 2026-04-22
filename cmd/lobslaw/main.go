package main

import (
	"fmt"
	"os"
)

// Version and Commit are injected at build time via -ldflags.
var (
	Version = "dev"
	Commit  = "none"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("lobslaw %s (%s)\n", Version, Commit)
		return
	}
	fmt.Fprintln(os.Stderr, "lobslaw: not yet implemented — see PLAN.md")
	os.Exit(1)
}
