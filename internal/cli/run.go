package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mattn/go-isatty"

	"dashy/internal/agent"
	"dashy/internal/shell"
	"dashy/internal/tui"
)

type ExitError struct {
	Code     int
	Err      error
	Reported bool
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return ""
}

func (e *ExitError) Unwrap() error {
	return e.Err
}

func (e *ExitError) ExitCode() int {
	return e.Code
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

func IsReported(err error) bool {
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Reported
	}
	return false
}

func isTerminal(r io.Reader) bool {
	if t, ok := r.(interface{ IsTerminal() bool }); ok {
		return t.IsTerminal()
	}
	if f, ok := r.(*os.File); ok {
		return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
	}
	return false
}

func Run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) (err error) {
	command := ""
	if len(args) > 0 && (args[0] == "notify" || args[0] == "view") {
		command, args = args[0], args[1:]
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if len(args) > 0 && command == "" {
		switch args[0] {
		case "version":
			fmt.Fprintln(out, strings.TrimPrefix(tui.Version, "v"))
			return nil
		}
	}

	pPassed := false
	normArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-p" {
			pPassed = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				normArgs = append(normArgs, arg, args[i+1])
				i++
			} else {
				normArgs = append(normArgs, "-p=")
			}
		} else if strings.HasPrefix(arg, "-p=") || strings.HasPrefix(arg, "--p=") {
			pPassed = true
			normArgs = append(normArgs, arg)
		} else {
			normArgs = append(normArgs, arg)
		}
	}

	flags := flag.NewFlagSet("dashy", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	prompt := flags.String("p", "", "run a single task and exit")
	sessionID := flags.String("s", "", "target session for notify/view (default: $DASHY_ENDPOINT)")
	resume := flags.String("resume", "", "resume a project session ID")
	configPath := flags.String("config", "", "base configuration file")
	flags.StringVar(configPath, "c", "", "alias for --config")
	noGlobal := flags.Bool("no-global", false, "skip inheriting from global configuration")
	if err := flags.Parse(normArgs); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if pPassed && *prompt == "" && flags.NArg() == 1 {
		*prompt = flags.Arg(0)
	} else if pPassed && *prompt != "" && flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	} else if pPassed && *prompt == "" && flags.NArg() > 1 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	} else if !pPassed && flags.NArg() != 0 && command == "" {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if command == "view" {
		return view(cwd, *sessionID, flags.Args())
	}
	if command == "notify" {
		text := strings.Join(flags.Args(), " ")
		if text == "" {
			data, e := io.ReadAll(io.LimitReader(in, 128*1024+1))
			if e != nil {
				return e
			}
			text = string(data)
		}
		return agent.Notify(cwd, *sessionID, text)
	}

	taskPrompt := *prompt
	if pPassed {
		if in != nil && !isTerminal(in) {
			data, e := io.ReadAll(in)
			if e != nil {
				return fmt.Errorf("reading stdin: %w", e)
			}
			stdinText := strings.TrimRight(string(data), "\r\n")
			if stdinText != "" {
				if taskPrompt != "" {
					taskPrompt = taskPrompt + "\n\n" + stdinText
				} else {
					taskPrompt = stdinText
				}
			}
		}
		if taskPrompt == "" {
			return fmt.Errorf("prompt is required")
		}
	}

	files := agent.ConfigFiles{Config: *configPath, NoGlobal: *noGlobal}
	if pPassed {
		files.Mode = "print"
	}
	a, err := agent.New(cwd, files, *resume)
	if err != nil {
		return err
	}
	defer a.Close()
	a.Start(tui.Version)
	defer func() {
		code := 0
		if err != nil {
			code = 1
		}
		a.Finish(code)
	}()
	if pPassed {
		if a.Settings().Provider == "" || a.Settings().Model == "" {
			return fmt.Errorf("no provider or model configured: run dashy to set up settings in /settings")
		}
		return runPrint(ctx, a, taskPrompt, out, diagnostics)
	}
	return tui.Run(a, cwd)
}

func runPrint(ctx context.Context, a *agent.Agent, prompt string, out, diagnostics io.Writer) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	interrupted := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			close(interrupted)
			cancel()
		case <-turnCtx.Done():
		}
	}()

	var lastAssistant strings.Builder
	emit := func(e agent.Event) {
		if e.Kind == "code" && e.Final && !e.Provisional {
			lastAssistant.Reset()
			cmd := strings.TrimRight(e.Text, "\r\n")
			fmt.Fprintf(diagnostics, "$ %s\n", cmd)
		} else if (e.Kind == "output" || e.Kind == "error") && e.Final {
			if s := shortResult(e.Text, agent.ShortResultLines); s != "" {
				fmt.Fprintln(diagnostics, s)
			}
		} else if e.Kind == "notice" {
			if strings.TrimSpace(e.Text) != "" {
				fmt.Fprintln(diagnostics, e.Text)
			}
		} else if e.Kind == "assistant" && e.Final {
			if lastAssistant.Len() > 0 && !strings.HasSuffix(lastAssistant.String(), "\n") {
				lastAssistant.WriteString("\n")
			}
			lastAssistant.WriteString(e.Text)
		}
	}

	err := a.RunInput(turnCtx, agent.Input{Text: prompt}, emit)

	var wasInterrupted bool
	select {
	case <-interrupted:
		wasInterrupted = true
	default:
	}

	sessionID := a.SessionID()
	if wasInterrupted {
		if sessionID != "" {
			fmt.Fprintf(diagnostics, "session: %s\n", sessionID)
		}
		return &ExitError{Code: 130, Reported: true}
	}

	if err != nil {
		fmt.Fprintf(diagnostics, "%v\n", err)
		if sessionID != "" {
			fmt.Fprintf(diagnostics, "session: %s\n", sessionID)
		}
		return &ExitError{Code: 1, Err: err, Reported: true}
	}

	finalText := lastAssistant.String()
	if finalText == "" {
		finalText = lastAssistantText(a.History)
	}
	if finalText != "" {
		if !strings.HasSuffix(finalText, "\n") {
			finalText += "\n"
		}
		fmt.Fprint(out, finalText)
	}
	if sessionID != "" {
		fmt.Fprintf(diagnostics, "session: %s\n", sessionID)
	}
	return nil
}

func lastAssistantText(history []json.RawMessage) string {
	for i := len(history) - 1; i >= 0; i-- {
		for _, r := range agent.RecordsForItem(history[i]) {
			if r.Kind == "assistant" && r.Text != "" {
				return r.Text
			}
		}
	}
	return ""
}

func shortResult(s string, maxLines int) string {
	s = strings.TrimRight(s, "\r\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		return strings.Join(lines[:maxLines], "\n") + "\n..."
	}
	return strings.Join(lines, "\n")
}

// view queues PNG/JPEG images for the session's next model request.
func view(cwd, id string, paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("usage: dashy view PATH [PATH ...]")
	}
	endpoint, err := agent.Endpoint(cwd, id)
	if err != nil {
		return err
	}
	home, err := agent.DashyHome(cwd)
	if err != nil {
		return err
	}
	for _, path := range paths {
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
