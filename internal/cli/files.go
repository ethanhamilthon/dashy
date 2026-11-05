package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// parseAll parses flags wherever they appear among the positional arguments, which it returns.
func parseAll(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
			return positional, err
		}
		positional, args = append(positional, fs.Arg(0)), fs.Args()[1:]
	}
}

// textLines reads a text file as lines without their endings; binary files are refused.
func textLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err == nil && bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		err = errors.New("binary file")
	}
	if err != nil || len(data) == 0 {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), "\n"), nil
}

// readCommand prints numbered lines of a file: dashy read [-o LINE] [-n COUNT] FILE.
func readCommand(args []string, out io.Writer) error {
	var offset, count int
	set := flagSet("read", func(fs *flag.FlagSet) { fs.IntVar(&offset, "o", 1, ""); fs.IntVar(&count, "n", 2000, "") })
	paths, err := parseAll(set, args)
	if err != nil || len(paths) != 1 {
		return errors.New("usage: dashy read [-o LINE] [-n COUNT] FILE")
	}
	lines, err := textLines(paths[0])
	if err != nil {
		return fmt.Errorf("read %s: %w", paths[0], err)
	}
	start, end := max(offset, 1)-1, min(len(lines), max(offset, 1)-1+max(count, 0))
	for i := start; i < end; i++ {
		fmt.Fprintf(out, "%6d\t%s\n", i+1, cut(lines[i], 2000))
	}
	if end < len(lines) {
		fmt.Fprintf(out, "... %d more lines; continue with -o %d\n", len(lines)-end, end+1)
	}
	return nil
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// writeCommand replaces or creates a file with stdin: dashy write FILE.
func writeCommand(args []string, in io.Reader, out io.Writer) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: dashy write FILE < CONTENT")
	}
	data, err := io.ReadAll(in)
	if err == nil {
		err = writeFile(args[0], data)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", args[0], err)
	}
	fmt.Fprintf(out, "wrote %s: %d lines\n", args[0], bytes.Count(data, []byte("\n")))
	return nil
}

// writeFile keeps the mode of an existing file and creates missing directories.
func writeFile(path string, data []byte) error {
	mode := fs.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

// patchCommand applies SEARCH/REPLACE blocks from stdin; each search text must occur exactly once.
func patchCommand(args []string, in io.Reader, out io.Writer) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: dashy patch FILE < BLOCKS (<<<<<<< SEARCH / ======= / >>>>>>> REPLACE)")
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("patch: %w", err)
	}
	crlf, text := bytes.Contains(old, []byte("\r\n")), strings.ReplaceAll(string(old), "\r\n", "\n")
	blocks := regexp.MustCompile(`(?s)<<<<<<< SEARCH\n(.*?)\n?=======\n(.*?)\n?>>>>>>> REPLACE`).FindAllStringSubmatch(strings.ReplaceAll(string(data), "\r\n", "\n"), -1)
	if len(blocks) == 0 {
		return errors.New("patch: no <<<<<<< SEARCH / ======= / >>>>>>> REPLACE blocks on stdin")
	}
	for i, b := range blocks {
		search, replace := b[1], b[2]
		if replace == "" && strings.Count(text, search+"\n") == 1 {
			search += "\n" // deleting whole lines takes their line break too
		}
		if n := strings.Count(text, search); n != 1 || search == "" {
			return fmt.Errorf("patch %s: block %d: search text found %d times, need exactly 1; nothing changed (dashy read the file and copy the text exactly, with more context if it repeats)", args[0], i+1, n)
		}
		text = strings.Replace(text, search, replace, 1)
	}
	if crlf {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	if err = writeFile(args[0], []byte(text)); err != nil {
		return fmt.Errorf("patch %s: %w", args[0], err)
	}
	fmt.Fprintf(out, "patched %s: %d blocks\n", args[0], len(blocks))
	return nil
}

// grepCommand searches files for a Go regexp: dashy grep [-i] [-l] [-g GLOB] PATTERN [PATH...];
// with --files it lists the files instead. Directories are searched recursively without ignored (git) and hidden files.
func grepCommand(args []string, out io.Writer) error {
	var ignoreCase, names, list bool
	var glob string
	set := flagSet("grep", func(fs *flag.FlagSet) {
		fs.BoolVar(&ignoreCase, "i", false, "")
		fs.BoolVar(&names, "l", false, "")
		fs.StringVar(&glob, "g", "", "")
		fs.BoolVar(&list, "files", false, "")
	})
	rest, err := parseAll(set, args)
	if list {
		rest = append([]string{""}, rest...) // no pattern
	}
	if err != nil || len(rest) == 0 {
		return errors.New("usage: dashy grep [-i] [-l] [-g GLOB] PATTERN [PATH...] | dashy grep --files [-g GLOB] [PATH...]")
	}
	if ignoreCase {
		rest[0] = "(?i)" + rest[0]
	}
	re, err := regexp.Compile(rest[0])
	if err != nil {
		return fmt.Errorf("grep: %w", err)
	}
	paths := rest[1:]
	if len(paths) == 0 {
		paths = []string{"."}
	}
	files, err := searchFiles(paths)
	if err != nil {
		return fmt.Errorf("grep: %w", err)
	}
	matches := 0
	for _, file := range files {
		if ok, _ := filepath.Match(glob, filepath.Base(file)); glob != "" && !ok {
			continue
		}
		if list {
			if matches++; matches > 500 {
				fmt.Fprintln(out, "... more than 500 files; narrow the glob or the paths")
				return nil
			}
			fmt.Fprintln(out, file)
			continue
		}
		lines, _ := textLines(file)
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			if matches++; matches > 200 {
				fmt.Fprintln(out, "... more than 200 matches; narrow the pattern or the paths")
				return nil
			}
			if names {
				fmt.Fprintln(out, file)
				break
			}
			fmt.Fprintf(out, "%s:%d:%s\n", file, i+1, cut(strings.TrimSpace(line), 300))
		}
	}
	if matches == 0 {
		return &ExitError{Code: 1, Reported: true} // like grep: no match is exit 1
	}
	return nil
}

// searchFiles expands directories to their files: git's view when it has one, else a walk without hidden entries.
func searchFiles(paths []string) ([]string, error) {
	var files []string
	for _, root := range paths {
		if info, err := os.Stat(root); err != nil {
			return nil, err
		} else if !info.IsDir() {
			files = append(files, root)
			continue
		}
		if listed, err := exec.Command("git", append([]string{"-C", root}, strings.Fields("ls-files -z -c --others --exclude-standard")...)...).Output(); err == nil {
			for _, f := range strings.Split(strings.TrimSuffix(string(listed), "\x00"), "\x00") {
				if f != "" {
					files = append(files, filepath.Join(root, f))
				}
			}
			continue
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return nil // unreadable entries are skipped
			case p != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules"):
				if d.IsDir() {
					return filepath.SkipDir
				}
			case d.Type().IsRegular():
				files = append(files, p)
			}
			return nil
		})
	}
	return files, nil
}
