package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"dashy/internal/cli"
	"dashy/internal/tui"
)

//go:embed VERSION
var version string

func main() {
	tui.Version = strings.TrimSpace(version)
	ctx, cancel := context.Background(), func() {}
	if !cli.IsHelper(os.Args[1:]) {
		// Sessions save and clean up on a signal; helpers just end, even when blocked reading.
		ctx, cancel = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	}
	defer cancel()
	if err := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		code := cli.ExitCode(err)
		if !cli.IsReported(err) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(code)
	}
}
