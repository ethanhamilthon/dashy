// Package selfupdate checks GitHub releases and installs the dashy binary.
package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Repo is the GitHub repository that publishes dashy releases.
const Repo = "ethanhamilthon/dashy"

const cacheTTL = 6 * time.Hour

// Download size limits are variables so tests can shrink them.
var (
	maxChecksums int64 = 1 << 20
	maxArchive   int64 = 100 << 20
)

// Release endpoints and the executable path are variables so tests can redirect them.
var (
	apiURL         = "https://api.github.com/" + "repos/" + Repo + "/releases/latest"
	releaseBase    = "https://github.com/" + Repo + "/releases/download/"
	executablePath = os.Executable
)

var client = &http.Client{Timeout: 5 * time.Minute}

type checkCache struct {
	Checked time.Time `json:"checked"`
	Latest  string    `json:"latest"`
}

// capReader reads from r and fails, instead of truncating, once more than limit bytes arrive.
type capReader struct {
	r    io.Reader
	what string // names the stream in the error, such as "response"
	max  int64
	left int64
}

func newCapReader(r io.Reader, what string, limit int64) *capReader {
	return &capReader{r: r, what: what, max: limit, left: limit}
}

func (c *capReader) Read(p []byte) (int, error) {
	// Ask for one byte past the remaining budget so an overflow is visible.
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	if int64(n) > c.left {
		return 0, fmt.Errorf("%s exceeds %d bytes", c.what, c.max)
	}
	c.left -= int64(n)
	return n, err
}

func fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "dashy-selfupdate")
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, response.Status)
	}
	data, err := io.ReadAll(newCapReader(response.Body, "response", limit))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	return data, nil
}

// Latest returns the newest released version tag, such as v0.2.2.
func Latest(ctx context.Context) (string, error) {
	data, err := fetch(ctx, apiURL, 1<<20)
	if err != nil {
		return "", err
	}
	var release struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return "", err
	}
	if release.Tag == "" {
		return "", errors.New("release has no tag")
	}
	if !validTag(release.Tag) {
		return "", fmt.Errorf("release has invalid tag %q", release.Tag)
	}
	return release.Tag, nil
}

// validTag reports whether tag is a semantic version that is safe to place in a download URL.
func validTag(tag string) bool {
	return canonical(tag) != "" && tag == strings.TrimSpace(tag) &&
		!strings.ContainsAny(tag, "/\\?#") && !strings.Contains(tag, "..")
}

// canonical returns value as a "vX.Y.Z" semantic version, or "" when invalid.
func canonical(value string) string {
	if value = strings.TrimSpace(value); !strings.HasPrefix(value, "v") {
		value = "v" + value
	}
	if !semver.IsValid(value) {
		return ""
	}
	return value
}

// Newer reports whether latest is a later semantic version than current.
func Newer(current, latest string) bool {
	have, want := canonical(current), canonical(latest)
	return have != "" && want != "" && semver.Compare(want, have) > 0
}

func cacheFile() string {
	home := os.Getenv("DASHY_HOME")
	if home == "" {
		dir, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(dir, ".dashy")
	}
	path, err := filepath.Abs(filepath.Join(home, "update.json"))
	if err != nil {
		return ""
	}
	return path
}

// Available returns the newer version tag, using a cached answer to keep startup cheap.
// It returns an empty string when the installation is current, unknown, or the check is off.
func Available(ctx context.Context, current string) string {
	if canonical(current) == "" || os.Getenv("DASHY_NO_UPDATE_CHECK") != "" {
		return ""
	}
	path := cacheFile()
	var cached checkCache
	if data, err := os.ReadFile(path); err != nil || json.Unmarshal(data, &cached) != nil ||
		cached.Latest == "" || time.Since(cached.Checked) >= cacheTTL {
		latest, err := Latest(ctx)
		if err != nil {
			return ""
		}
		cached = checkCache{Checked: time.Now(), Latest: latest}
		if data, err := json.Marshal(cached); err == nil {
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
			_ = os.WriteFile(path, append(data, '\n'), 0o644)
		}
	}
	if Newer(current, cached.Latest) {
		return cached.Latest
	}
	return ""
}

// Install downloads the latest release for this platform, verifies its checksum, and replaces the binary.
func Install(ctx context.Context, current string, out io.Writer) error {
	target, err := executablePath()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	latest, err := Latest(ctx)
	if err != nil {
		return err
	}
	if canonical(current) != "" && !Newer(current, latest) {
		fmt.Fprintf(out, "dashy %s is the latest release.\n", current)
		return nil
	}
	if !slices.Contains([]string{"darwin", "linux"}, runtime.GOOS) || !slices.Contains([]string{"amd64", "arm64"}, runtime.GOARCH) {
		return fmt.Errorf("no dashy release for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	asset := fmt.Sprintf("dashy_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	base := releaseBase + latest + "/"
	sums, err := fetch(ctx, base+"checksums.txt", maxChecksums)
	if err != nil {
		return err
	}
	want, err := checksumFor(sums, asset)
	if err != nil {
		return err
	}
	archive, err := fetch(ctx, base+asset, maxArchive)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(archive); hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("checksum mismatch for %s", asset)
	}
	if err := replaceBinary(target, archive); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated dashy %s -> %s. Restart dashy to use it.\n", strings.TrimPrefix(current, "v"), latest)
	return nil
}

// checksumFor returns the sha256 listed for asset; lines are "HASH  name" or "HASH *name".
func checksumFor(sums []byte, asset string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt does not list %s", asset)
}

// replaceBinary writes the extracted binary to a temp file beside target, then renames it over target.
func replaceBinary(target string, archive []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), "dashy-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // harmless once the file has been renamed
	if err := writeBinary(tmp, archive); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

// writeBinary extracts the dashy binary into f, makes it executable, and flushes it to disk.
func writeBinary(f *os.File, archive []byte) error {
	if err := extractBinary(archive, f); err != nil {
		return err
	}
	if err := f.Chmod(0o755); err != nil {
		return err
	}
	return f.Sync()
}

// unsafeName reports whether an archive entry name could escape the install directory.
func unsafeName(name string) bool {
	return strings.Contains(name, "\\") || path.IsAbs(name) || slices.Contains(strings.Split(name, "/"), "..")
}

// extractBinary copies the regular file "dashy" from a gzipped tar into w.
// It scans the whole archive, rejects unsafe or duplicate entries, and verifies the gzip trailer.
func extractBinary(archive []byte, w io.Writer) (err error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	defer func() {
		if cerr := gz.Close(); err == nil {
			err = cerr
		}
	}()
	body := newCapReader(gz, "decompressed archive", maxArchive) // bounds the decompressed size
	tr := tar.NewReader(body)
	found := false
	for {
		hdr, herr := tr.Next()
		if errors.Is(herr, io.EOF) {
			break
		}
		if herr != nil {
			return herr
		}
		if unsafeName(hdr.Name) {
			return fmt.Errorf("unsafe archive entry %q", hdr.Name)
		}
		if path.Clean(hdr.Name) != "dashy" {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("archive entry %q is not a regular file", hdr.Name)
		}
		if found {
			return errors.New("archive has more than one dashy binary")
		}
		found = true
		if _, err := io.Copy(w, tr); err != nil {
			return err
		}
	}
	// The tar reader stops at its end marker; drain the rest so the gzip checksum and length are verified.
	if _, err := io.Copy(io.Discard, body); err != nil {
		return err
	}
	if !found {
		return errors.New("archive has no dashy binary")
	}
	return nil
}

// Run implements the dashy update command. With check only, it reports without installing.
func Run(ctx context.Context, current string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	flags.SetOutput(out)
	check := flags.Bool("check", false, "only report whether a newer release exists")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if !*check {
		return Install(ctx, current, out)
	}
	latest, err := Latest(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "installed v%s, latest %s\n", strings.TrimPrefix(current, "v"), latest)
	if Newer(current, latest) {
		fmt.Fprintln(out, "run dashy update to install it")
	}
	return nil
}
