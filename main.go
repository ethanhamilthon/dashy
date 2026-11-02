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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		code := cli.ExitCode(err)
		if !cli.IsReported(err) && code != 130 {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(code)
	}
}
