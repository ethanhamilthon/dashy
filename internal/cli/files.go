package cli

import (
	"bytes"
	"context"
	"dashy/internal/filelist"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func parseAll(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
			return positional, err
		}
		if used := len(args) - fs.NArg(); used > 0 && args[used-1] == "--" {
			return append(positional, fs.Args()...), nil
		}
		positional, args = append(positional, fs.Arg(0)), fs.Args()[1:]
	}
}

const maxTextFile = 32 << 20

var (
	errBinary     = errors.New("binary file")
	errNotRegular = errors.New("not a regular file")
)

func textLines(path string) ([]string, error) {
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular():
		return nil, errNotRegular
	case info.Size() > maxTextFile:
		return nil, fmt.Errorf("file too large (%d MB, limit %d MB)", info.Size()>>20, maxTextFile>>20)
	}
	data, err := os.ReadFile(path)
	if err == nil && bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		err = errBinary
	}
	if err != nil || len(data) == 0 {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), "\n"), nil
}

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

func writeFile(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
	}
	mode := fs.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errNotRegular
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".dashy-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = errors.Join(file.Chmod(mode), file.Sync(), file.Close()); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

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
	blocks, err := parseBlocks(string(data))
	if err != nil {
		return fmt.Errorf("patch: %w; nothing changed", err)
	}
	text := string(old)
	for i, b := range blocks {
		if text, err = applyBlock(text, b[0], b[1]); err != nil {
			return fmt.Errorf("patch %s: block %d: %w; nothing changed (dashy read the file and copy the text exactly, with more context if it repeats)", args[0], i+1, err)
		}
	}
	if err = writeFile(args[0], []byte(text)); err != nil {
		return fmt.Errorf("patch %s: %w", args[0], err)
	}
	fmt.Fprintf(out, "patched %s: %d blocks\n", args[0], len(blocks))
	return nil
}

func grepCommand(args []string, out, diagnostics io.Writer) error {
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
		rest = append([]string{""}, rest...)
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
	files, failures, err := searchFiles(paths)
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
		lines, err := textLines(file)
		if err != nil && !errors.Is(err, errBinary) && !errors.Is(err, errNotRegular) && !errors.Is(err, fs.ErrNotExist) {
			failures = append(failures, fmt.Sprintf("%s: %v", file, err))
		}
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
	for _, f := range failures {
		fmt.Fprintln(diagnostics, "grep: "+f)
	}
	switch {
	case len(failures) > 0:
		return &ExitError{Code: 2, Reported: true} // like grep: an incomplete search is exit 2
	case matches == 0:
		return &ExitError{Code: 1, Reported: true} // like grep: no match is exit 1
	}
	return nil
}

func searchFiles(paths []string) (files, failures []string, err error) {
	for _, root := range paths {
		if info, err := os.Stat(root); err != nil {
			return nil, nil, err
		} else if !info.IsDir() {
			files = append(files, root)
			continue
		}
		listed, failed, err := filelist.List(context.Background(), root, filelist.Options{})
		if err != nil {
			return nil, nil, err
		}
		for _, p := range listed {
			files = append(files, filepath.Join(root, p))
		}
		failures = append(failures, failed...)
	}
	return files, failures, nil
}

func parseBlocks(input string) (blocks [][2]string, err error) {
	state, search := 0, ""
	var cur []string
	for i, line := range strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n") {
		switch {
		case line == "<<<<<<< SEARCH":
			if state != 0 {
				return nil, fmt.Errorf("line %d: <<<<<<< SEARCH inside block %d", i+1, len(blocks)+1)
			}
			state, cur = 1, nil
		case line == "=======" && state == 1:
			state, search, cur = 2, strings.Join(cur, "\n"), nil
		case line == ">>>>>>> REPLACE":
			if state != 2 {
				return nil, fmt.Errorf("line %d: >>>>>>> REPLACE without its SEARCH and =======", i+1)
			}
			blocks, state = append(blocks, [2]string{search, strings.Join(cur, "\n")}), 0
		case state != 0:
			cur = append(cur, line)
		}
	}
	if state != 0 {
		return nil, fmt.Errorf("block %d is not closed with >>>>>>> REPLACE", len(blocks)+1)
	}
	if len(blocks) == 0 {
		return nil, errors.New("no <<<<<<< SEARCH / ======= / >>>>>>> REPLACE blocks on stdin")
	}
	return blocks, nil
}

func applyBlock(text, search, replace string) (string, error) {
	if search == "" {
		return "", errors.New("empty search text")
	}
	var eols []string
	if strings.Contains(search, "\n") {
		eols = []string{"\n", "\r\n"}
	} else if i := strings.Index(text, search); i >= 0 {
		eols = []string{lineBreakAt(text, i+len(search))}
	} else {
		eols = []string{"\n"}
	}
	total, from, to := 0, "", ""
	for _, eol := range eols {
		s, r := strings.ReplaceAll(search, "\n", eol), strings.ReplaceAll(replace, "\n", eol)
		if r == "" && strings.Count(text, s+eol) == 1 {
			s += eol
		}
		if n := strings.Count(text, s); n > 0 {
			total, from, to = total+n, s, r
		}
	}
	if total != 1 {
		return "", fmt.Errorf("search text found %d times, need exactly 1", total)
	}
	return strings.Replace(text, from, to, 1), nil
}

func lineBreakAt(text string, i int) string {
	if j := strings.IndexByte(text[i:], '\n'); j >= 0 {
		if j > 0 && text[i+j-1] == '\r' || j == 0 && i > 0 && text[i-1] == '\r' {
			return "\r\n"
		}
		return "\n"
	}
	if strings.Contains(text, "\r\n") {
		return "\r\n"
	}
	return "\n"
}
