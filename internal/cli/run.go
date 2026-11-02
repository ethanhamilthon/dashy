package cli

import (
	"context"
	_ "embed"
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

//go:embed help.txt
var helpText string

type runFlags struct {
	prompt     *string
	sessionID  *string
	resume     *string
	configPath *string
	noGlobal   *bool
}

func setupFlags(flags *flag.FlagSet) *runFlags {
	rf := &runFlags{
		prompt:     flags.String("p", "", "run a single task and exit"),
		sessionID:  flags.String("s", "", "target session for notify/view (default: $DASHY_ENDPOINT)"),
		resume:     flags.String("resume", "", "resume a project session ID"),
		configPath: flags.String("config", "", "base configuration file"),
		noGlobal:   flags.Bool("no-global", false, "skip inheriting from global configuration"),
	}
	flags.StringVar(rf.configPath, "c", "", "alias for --config")
	return rf
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
		case "help", "--help", "-h":
			fmt.Fprint(out, helpText)
			return nil
		}
	}
	flags := flag.NewFlagSet("dashy", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	flags.Usage = func() {}
	rf := setupFlags(flags)
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			fmt.Fprint(out, helpText)
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
		return view(cwd, *rf.sessionID, flags.Args())
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
		return agent.Notify(cwd, *rf.sessionID, text)
	}
	files := agent.ConfigFiles{Config: *rf.configPath, NoGlobal: *rf.noGlobal}
	if *rf.prompt != "" {
		files.Mode = "print"
	}
	a, err := agent.New(cwd, files, *rf.resume)
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
	if *rf.prompt != "" {
		if a.Settings().Provider == "" || a.Settings().Model == "" {
			return fmt.Errorf("no provider or model configured: run dashy to set up settings in /settings")
		}
	}
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
