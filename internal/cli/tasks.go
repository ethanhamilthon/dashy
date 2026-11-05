package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"dashy/internal/agent"
)

const tasksUsage = "usage: dashy tasks run [-s ID] COMMAND... | list [-s ID] | status [-s ID] TASK | stop [-s ID] TASK"

func tasksCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(tasksUsage)
	}
	var session string
	set := flagSet("tasks", func(fs *flag.FlagSet) { fs.StringVar(&session, "s", "", "session ID") })
	var rest []string
	var err error
	if args[0] == "run" {
		err = set.Parse(args[1:])
		rest = set.Args()
	} else {
		rest, err = parseAll(set, args[1:])
	}
	if err != nil {
		return errors.New(tasksUsage)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	switch {
	case args[0] == "run" && len(rest) > 0:
		id, err := agent.RunTask(cwd, session, strings.Join(rest, " "))
		if err == nil {
			fmt.Fprintf(out, "%s started; its result arrives as <task-result id=%q> when it ends\n", id, id)
		}
		return err
	case args[0] == "stop" && len(rest) == 1:
		endpoint, err := agent.Endpoint(cwd, session)
		if err != nil {
			return err
		}
		_, err = agent.Send(endpoint, agent.Message{Task: &agent.TaskRequest{Op: "stop", ID: rest[0]}})
		if err == nil {
			fmt.Fprintf(out, "%s stopped\n", rest[0])
		}
		return err
	case (args[0] == "list" && len(rest) == 0) || (args[0] == "status" && len(rest) == 1):
		if session == "" {
			session = os.Getenv("DASHY_SESSION_ID")
		}
		if session == "" {
			return errors.New("outside a dashy shell, pass -s SESSION_ID")
		}
		home, err := agent.DashyHome(cwd)
		if err != nil {
			return err
		}
		list, err := agent.SessionTasks(home, session)
		if err != nil {
			return err
		}
		if args[0] == "list" {
			if len(list) == 0 {
				fmt.Fprintln(out, "no tasks")
			}
			for _, task := range list {
				fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", task.ID, taskState(task), taskTime(task), task.Command)
			}
			return nil
		}
		for _, task := range list {
			if task.ID == rest[0] {
				fmt.Fprintf(out, "%s %s · %s\n$ %s\n%s\n[full output: %s]\n", task.ID, taskState(task), taskTime(task), task.Command, agent.TaskOutput(task), task.Log)
				return nil
			}
		}
		return fmt.Errorf("no task %s in this session", rest[0])
	}
	return errors.New(tasksUsage)
}

func taskState(task agent.Task) string {
	if task.ExitCode != nil {
		return fmt.Sprintf("%s (exit %d)", task.Status, *task.ExitCode)
	}
	return task.Status
}

func taskTime(task agent.Task) string {
	end := task.Ended
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(task.Started).Round(time.Second).String()
}
