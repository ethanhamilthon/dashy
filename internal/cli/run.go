package cli

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattn/go-isatty"

	"dashy/internal/agent"
	"dashy/internal/shell"
	"dashy/internal/tui"
)

//go:embed help.txt
var helpText string

// ExitError ends the process with Code. Reported means the message was already written to stderr.
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

// ExitCode is the process exit code for err: 0 for nil, the ExitError code, else 1.
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

// IsReported reports whether err was already printed.
func IsReported(err error) bool {
	var exit *ExitError
	return errors.As(err, &exit) && exit.Reported
}

// cancelled is the exit of an interrupted run: SIGINT or SIGTERM ends it with 130.
var cancelled = &ExitError{Code: 130, Reported: true}

func isTerminal(r io.Reader) bool {
	if f, ok := r.(*os.File); ok {
		return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
	}
	t, ok := r.(interface{ IsTerminal() bool })
	return ok && t.IsTerminal()
}

// Run executes one command line. stdout receives results; diagnostics receives
// progress and errors. ctx ends everything: a cancelled run exits 130.
func Run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "version", "help", "--help", "-h":
			if len(args) > 1 {
				return fmt.Errorf("%s takes no arguments", args[0])
			}
			if args[0] == "version" {
				fmt.Fprintln(out, strings.TrimPrefix(tui.Version, "v"))
			} else {
				fmt.Fprint(out, helpText)
			}
			return nil
		case "notify":
			return notifyCommand(ctx, args[1:], in)
		case "view":
			return viewCommand(args[1:])
		}
	}
	return sessionCommand(ctx, args, in, out, diagnostics)
}

// flagSet parses args with a fixed set of flags; positional arguments are returned.
func flagSet(name string, define func(*flag.FlagSet)) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	define(fs)
	return fs
}

func notifyCommand(ctx context.Context, args []string, in io.Reader) error {
	var session string
	fs := flagSet("notify", func(fs *flag.FlagSet) { fs.StringVar(&session, "s", "", "session ID") })
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	text := strings.Join(fs.Args(), " ")
	if len(fs.Args()) == 0 {
		data, err := readStdin(ctx, in, 128*1024+1)
		if err != nil {
			return cancelledOr(ctx, err)
		}
		text = data
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	return agent.Notify(cwd, session, text)
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
		if err = agent.Send(endpoint, agent.Message{Image: &image}); err != nil {
			_ = os.Remove(image.Path)
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

// sessionCommand is the TUI (no -p) or one-shot print mode (-p).
func sessionCommand(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) error {
	var prompt, config, resume string
	var noGlobal bool
	printMode := false
	fs := flagSet("dashy", func(fs *flag.FlagSet) {
		fs.StringVar(&config, "config", "", "AGENTS.md file to use instead of ./AGENTS.md")
		fs.StringVar(&config, "c", "", "alias for --config")
		fs.BoolVar(&noGlobal, "no-global", false, "do not read ~/.dashy/AGENTS.md")
		fs.StringVar(&resume, "resume", "", "resume a session ID of this project")
		fs.Func("p", "run one task and print the final answer", func(v string) error {
			printMode, prompt = true, v
			return nil
		})
	})
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, helpText)
			return nil
		}
		return fmt.Errorf("%w (see dashy --help)", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s (see dashy --help)", strings.Join(fs.Args(), " "))
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	opts := agent.Options{Config: config, NoGlobal: noGlobal, Resume: resume, Print: printMode}
	if !printMode {
		a, err := agent.New(cwd, opts)
		if err != nil {
			return err
		}
		defer a.Close()
		if err := tui.Run(ctx, a, cwd); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return cancelled
		}
		return nil
	}
	if in != nil && !isTerminal(in) {
		data, err := readStdin(ctx, in, 0)
		if err != nil {
			return cancelledOr(ctx, err)
		}
		if text := strings.TrimRight(data, "\r\n"); text != "" {
			if prompt != "" {
				prompt += "\n\n"
			}
			prompt += text
		}
	}
	if prompt == "" {
		return errors.New("prompt is required")
	}
	if ctx.Err() != nil {
		return cancelled
	}
	a, err := agent.New(cwd, opts)
	if err != nil {
		return err
	}
	defer a.Close()
	if a.Settings().Provider == "" || a.Settings().Model == "" {
		return errors.New("no provider or model configured: run dashy and choose one in /settings")
	}
	return runPrint(ctx, a, prompt, out, diagnostics)
}

// readStdin reads all of in (or at most limit bytes when limit > 0), giving up when ctx ends.
func readStdin(ctx context.Context, in io.Reader, limit int64) (string, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var r io.Reader = in
		if limit > 0 {
			r = io.LimitReader(in, limit)
		}
		data, err := io.ReadAll(r)
		done <- result{data, err}
	}()
	select {
	case r := <-done:
		return string(r.data), r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// cancelledOr maps an interrupted wait to the cancelled exit; other errors pass through.
func cancelledOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return cancelled
	}
	return err
}

// runPrint runs one turn without a TUI. Only the final answer goes to out; progress goes to diagnostics.
func runPrint(ctx context.Context, a *agent.Agent, prompt string, out, diagnostics io.Writer) error {
	emit := func(e agent.Event) {
		switch {
		case e.Kind == "code" && e.Final && !e.Provisional:
			fmt.Fprintf(diagnostics, "$ %s\n", strings.TrimRight(e.Text, "\r\n"))
		case (e.Kind == "output" || e.Kind == "error") && e.Final:
			if s := shortResult(e.Text, 4); s != "" {
				fmt.Fprintln(diagnostics, s)
			}
		case e.Kind == "notice" && strings.TrimSpace(e.Text) != "":
			fmt.Fprintln(diagnostics, e.Text)
		}
	}
	err := a.Run(ctx, prompt, emit)
	if ctx.Err() != nil {
		fmt.Fprintf(diagnostics, "session: %s\n", a.SessionID())
		return cancelled
	}
	if err != nil {
		fmt.Fprintln(diagnostics, err)
		fmt.Fprintf(diagnostics, "session: %s\n", a.SessionID())
		return &ExitError{Code: 1, Err: err, Reported: true}
	}
	if final := a.FinalText(); final != "" {
		if !strings.HasSuffix(final, "\n") {
			final += "\n"
		}
		fmt.Fprint(out, final)
	}
	fmt.Fprintf(diagnostics, "session: %s\n", a.SessionID())
	return nil
}

// shortResult keeps the first lines of a tool result for progress output.
func shortResult(s string, maxLines int) string {
	s = strings.TrimRight(s, "\r\n")
	lines := strings.Split(s, "\n")
	if s == "" {
		return ""
	}
	if len(lines) > maxLines {
		return strings.Join(lines[:maxLines], "\n") + "\n..."
	}
	return s
}
