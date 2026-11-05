package filelist

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type Options struct {
	Directories bool
	LinkFiles   bool
	Hidden      bool // include hidden entries in the filesystem fallback; git controls its own exclusions
	Max, Depth  int
	Ignore      func(string) bool
}

func List(ctx context.Context, root string, o Options) (paths, failures []string, err error) {
	seen := map[string]bool{}
	add := func(p string) bool {
		if p == "" || seen[p] || (o.Depth > 0 && strings.Count(p, "/") > o.Depth) || (o.Ignore != nil && o.Ignore(p)) {
			return false
		}
		if o.Max > 0 && len(paths) >= o.Max {
			return true
		}
		seen[p] = true
		paths = append(paths, p)
		return o.Max > 0 && len(paths) >= o.Max
	}
	gitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(gitCtx, "git", "ls-files", "-z", "-c", "--others", "--exclude-standard")
	cmd.Dir = root
	pipe, e := cmd.StdoutPipe()
	if e == nil {
		e = cmd.Start()
	}
	if e == nil {
		scan := bufio.NewScanner(pipe)
		scan.Buffer(make([]byte, 4096), 1<<20)
		scan.Split(func(data []byte, eof bool) (int, []byte, error) {
			if i := bytes.IndexByte(data, 0); i >= 0 {
				return i + 1, data[:i], nil
			}
			if eof && len(data) > 0 {
				return len(data), data, nil
			}
			return 0, nil, nil
		})
		limited := false
		for scan.Scan() {
			p := scan.Text()
			if o.Directories {
				for d := filepath.ToSlash(filepath.Dir(p)); d != "."; d = filepath.ToSlash(filepath.Dir(d)) {
					if add(d + "/") {
						limited = true
						break
					}
				}
			}
			if limited || add(p) {
				limited = true
				cancel()
				break
			}
		}
		scanErr := scan.Err()
		if scanErr != nil {
			cancel()
		}
		waitErr := cmd.Wait()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if scanErr != nil {
			return nil, nil, scanErr
		}
		if limited || waitErr == nil {
			slices.Sort(paths)
			return paths, nil, nil
		}
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	paths = nil
	clear(seen)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if o.Max > 0 && len(paths) >= o.Max {
			return fs.SkipAll
		}
		if e != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", p, e))
			return nil
		}
		if p == root {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if (!o.Hidden && strings.HasPrefix(d.Name(), ".")) || d.Name() == ".git" || d.Name() == "node_modules" || (o.Ignore != nil && o.Ignore(rel)) || (o.Depth > 0 && strings.Count(rel, "/") > o.Depth) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if o.Directories {
				add(rel + "/")
			}
		} else if d.Type().IsRegular() {
			add(rel)
		} else if o.LinkFiles && d.Type()&fs.ModeSymlink != 0 {
			if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
				add(rel)
			}
		}
		return nil
	})
	slices.Sort(paths)
	return paths, failures, err
}
