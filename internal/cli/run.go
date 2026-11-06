package cli

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mattn/go-isatty"

	"dashy/internal/acp"
	"dashy/internal/agent"
	"dashy/internal/shell"
	"dashy/internal/tui"
)

//go:embed help.txt
var helpText string

type ExitError struct {
	Code     int
	Err      error
	Reported bool
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

func ExitCode(err error) int {
	var exit *ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.Code
	}
	return 1
}

func IsReported(err error) bool {
	var exit *ExitError
	return errors.As(err, &exit) && exit.Reported
}

var cancelled = &ExitError{Code: 130, Reported: true}

func isTerminal(v any) bool {
	if w, ok := v.(*failedWriter); ok {
		v = w.w
	}
	f, ok := v.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}

func IsHelper(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "version", "help", "--help", "-h", "tasks", "view", "read", "write", "patch", "grep":
		return true
	}
	return false
}

type failedWriter struct {
	w   io.Writer
	err error
}

func (f *failedWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil && f.err == nil {
		f.err = err
	}
	return n, err
}

var Version = "dev"

func Run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) error {
	if len(args) > 0 && args[0] == "acp" {
		var opts acp.Options
		fs := flagSet("acp", func(fs *flag.FlagSet) {
			fs.StringVar(&opts.Config, "config", "", "project instructions")
			fs.BoolVar(&opts.HideTools, "hide-tools", false, "omit tool calls")
		})
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() > 0 {
			return errors.New("usage: dashy acp [--config FILE] [--hide-tools]")
		}
		opts.Version = Version
		return cancelledOr(ctx, acp.Run(ctx, in, out, opts))
	}
	w := &failedWriter{w: out}
	err := run(ctx, args, in, w, diagnostics)
	if err == nil && w.err != nil {
		err = fmt.Errorf("write output: %w", w.err)
	}
	return err
}

func run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "version", "help", "--help", "-h":
			if len(args) > 1 {
				return fmt.Errorf("%s takes no arguments", args[0])
			}
			if args[0] == "version" {
				fmt.Fprintln(out, strings.TrimPrefix(Version, "v"))
			} else {
				fmt.Fprint(out, helpText)
			}
			return nil
		case "tasks":
			return tasksCommand(args[1:], out)
		case "view":
			return viewCommand(args[1:])
		case "models":
			return modelsCommand(ctx, args[1:], out, diagnostics)
		case "read":
			return readCommand(args[1:], out)
		case "write":
			return writeCommand(args[1:], in, out)
		case "patch":
			return patchCommand(args[1:], in, out)
		case "grep":
			return grepCommand(args[1:], out, diagnostics)
		}
	}
	return sessionCommand(ctx, args, in, out, diagnostics)
}

func flagSet(name string, define func(*flag.FlagSet)) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	define(fs)
	return fs
}

func viewCommand(args []string) error {
	var session string
	fs := flagSet("view", func(fs *flag.FlagSet) { fs.StringVar(&session, "s", "", "session ID") })
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("view: %w", err)
	}
	if fs.NArg() == 0 {
		return errors.New("usage: dashy view [-s ID] PATH [PATH ...]")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	endpoint, err := agent.Endpoint(cwd, session)
	if err != nil {
		return err
	}
	home, err := agent.DashyHome(cwd)
	if err != nil {
		return err
	}
	for _, path := range fs.Args() {
		image, err := shell.CaptureImageCopy(path, filepath.Join(home, "attachments"))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if _, err = agent.Send(endpoint, agent.Message{Image: &image}); err != nil {
			_ = os.Remove(image.Path)
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

func modelsCommand(ctx context.Context, args []string, out, diagnostics io.Writer) error {
	var provider string
	fs := flagSet("models", func(fs *flag.FlagSet) { fs.StringVar(&provider, "provider", "", "provider name") })
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errors.New("usage: dashy models [--provider NAME]")
	}
	cwd, _ := os.Getwd() // only resolves a relative DASHY_HOME, which then reports its own error
	home, err := agent.DashyHome(cwd)
	if err != nil {
		return err
	}
	settings, err := agent.LoadSettings(home)
	if err != nil {
		return err
	}
	providers := settings.Providers
	if provider != "" {
		p, ok := providers[provider]
		if !ok {
			return fmt.Errorf("provider %q is not configured", provider)
		}
		providers = map[string]agent.Provider{provider: p}
	}
	if len(providers) == 0 {
		return errors.New("no providers configured: run dashy and add one with /connect")
	}
	choices, errs := agent.AllModels(ctx, providers)
	if ctx.Err() != nil {
		return cancelled
	}
	for _, c := range choices {
		fmt.Fprintf(out, "%s\t%s\n", c.Provider, c.Model)
	}
	for _, name := range slices.Sorted(maps.Keys(errs)) {
		fmt.Fprintf(diagnostics, "%s: %v\n", name, errs[name])
	}
	if len(errs) > 0 {
		return &ExitError{Code: 1, Reported: true}
	}
	return nil
}

func sessionCommand(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) error {
	var config, resume string
	fs := flagSet("dashy", func(fs *flag.FlagSet) {
		fs.StringVar(&config, "config", "", "project instructions")
		fs.StringVar(&config, "c", "", "alias for --config")
		fs.StringVar(&resume, "resume", "", "resume a project session")
	})
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w (see dashy --help)", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if !isTerminal(in) || !isTerminal(out) {
		return errors.New("the interactive TUI needs a terminal; editors should use dashy acp")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	a, err := agent.New(cwd, agent.Options{Config: config, Resume: resume})
	if err != nil {
		return err
	}
	defer a.Close()
	return cancelledOr(ctx, tui.Run(ctx, a, cwd, Version))
}

func cancelledOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return cancelled
	}
	return err
}
