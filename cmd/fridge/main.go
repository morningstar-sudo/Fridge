package main

import (
	"fmt"
	"os"
)

func main() {
	// Task 13 adds `server` and `healthcheck`.
	if len(os.Args) > 1 && os.Args[1] == "server" {
		fmt.Fprintln(os.Stderr, "not implemented yet")
		os.Exit(2)
	}
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}
