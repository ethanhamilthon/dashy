package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"dashy/internal/agent"
	"dashy/internal/shell"
	"dashy/internal/tui"
)

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
	flags := flag.NewFlagSet("dashy", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	prompt := flags.String("p", "", "run a single task and exit")
	sessionID := flags.String("s", "", "target session for notify/view (default: $DASHY_ENDPOINT)")
	resume := flags.String("resume", "", "resume a project session ID")
	configPath := flags.String("config", "", "base configuration file")
	flags.StringVar(configPath, "c", "", "alias for --config")
	noGlobal := flags.Bool("no-global", false, "skip inheriting from global configuration")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 && command == "" {
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
	files := agent.ConfigFiles{Config: *configPath, NoGlobal: *noGlobal}
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
	_ = prompt
	return tui.Run(a, cwd)
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
