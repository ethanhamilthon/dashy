package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubEndpoints points the package at a local release server for the test's duration.
func stubEndpoints(t *testing.T, server *httptest.Server) {
	t.Helper()
	api, base := apiURL, releaseBase
	apiURL, releaseBase = server.URL+"/latest", server.URL+"/download/"
	t.Cleanup(func() { apiURL, releaseBase = api, base })
}

// shrink lowers a size limit for the test's duration.
func shrink(t *testing.T, limit *int64, value int64) {
	t.Helper()
	old := *limit
	*limit = value
	t.Cleanup(func() { *limit = old })
}

// stubExecutable makes Install replace target instead of the running test binary.
func stubExecutable(t *testing.T, target string) {
	t.Helper()
	exe := executablePath
	executablePath = func() (string, error) { return target, nil }
	t.Cleanup(func() { executablePath = exe })
}

// release serves /latest plus one platform archive and its checksums.txt.
type release struct {
	tag, asset, sums string
	archive          []byte
	downloads        atomic.Int32
}

func (r *release) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/latest":
		json.NewEncoder(w).Encode(map[string]string{"tag_name": r.tag})
		return
	case "/download/" + r.tag + "/checksums.txt":
		fmt.Fprint(w, r.sums)
	case "/download/" + r.tag + "/" + r.asset:
		w.Write(r.archive)
	default:
		http.NotFound(w, req)
		return
	}
	r.downloads.Add(1)
}

// testAsset returns the archive name for this platform, skipping unsupported hosts.
func testAsset(t *testing.T) string {
	t.Helper()
	if !(runtime.GOOS == "darwin" || runtime.GOOS == "linux") || !(runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") {
		t.Skip("no release asset for this platform")
	}
	return fmt.Sprintf("dashy_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
}

type entry struct {
	name, body string
	typ        byte
}

func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o755}
		if e.typ == tar.TypeSymlink {
			hdr.Linkname = e.body
		} else {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeSymlink {
			continue
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// setupInstall starts a release server and an existing "old build" binary to replace.
func setupInstall(t *testing.T, r *release) (target string) {
	t.Helper()
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	stubEndpoints(t, server)
	target = filepath.Join(t.TempDir(), "dashy")
	if err := os.WriteFile(target, []byte("old build"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubExecutable(t, target)
	return target
}

func requireUnchanged(t *testing.T, target string) {
	t.Helper()
	if data, err := os.ReadFile(target); err != nil || string(data) != "old build" {
		t.Fatalf("binary changed: %q, %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil || len(entries) != 1 {
		t.Fatalf("leftover files in install dir: %v, %v", entries, err)
	}
}

func TestInstallReplacesBinary(t *testing.T) {
	asset := testAsset(t)
	archive := tarball(t, entry{"./README", "docs", tar.TypeReg}, entry{"./dashy", "new build", tar.TypeReg})
	r := &release{tag: "v9.9.9", asset: asset, archive: archive, sums: digest(archive) + " *" + asset + "\n"}
	target := setupInstall(t, r)
	var out bytes.Buffer
	if err := Install(context.Background(), "0.2.1", &out); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "new build" {
		t.Fatalf("binary = %q, %v", data, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, %v", info.Mode(), err)
	}
	if want := "Updated dashy 0.2.1 -> v9.9.9. Restart dashy to use it.\n"; out.String() != want {
		t.Fatalf("output = %q", out.String())
	}
	if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestInstallFailuresLeaveBinaryUntouched(t *testing.T) {
	asset := testAsset(t)
	good := tarball(t, entry{"dashy", "new build", tar.TypeReg})
	traversal := tarball(t, entry{"../dashy", "evil", tar.TypeReg})
	noDashy := tarball(t, entry{"README", "docs", tar.TypeReg})
	duplicate := tarball(t, entry{"dashy", "one", tar.TypeReg}, entry{"dashy", "two", tar.TypeReg})
	symlink := tarball(t, entry{"dashy", "/etc/passwd", tar.TypeSymlink})
	backslash := tarball(t, entry{`sub\dashy`, "evil", tar.TypeReg})
	corrupt := append([]byte(nil), good...)
	corrupt[len(corrupt)-1] ^= 0xff // flips the gzip trailer's length field
	cases := []struct {
		name    string
		archive []byte
		sums    string
		want    string
	}{
		{"bad checksum", good, strings.Repeat("0", 64) + "  " + asset + "\n", "checksum mismatch"},
		{"asset not listed", good, digest(good) + "  other.tar.gz\n", "does not list " + asset},
		{"path traversal", traversal, digest(traversal) + "  " + asset + "\n", "unsafe archive entry"},
		{"no dashy entry", noDashy, digest(noDashy) + "  " + asset + "\n", "no dashy binary"},
		{"duplicate dashy entry", duplicate, digest(duplicate) + "  " + asset + "\n", "more than one dashy binary"},
		{"symlink named dashy", symlink, digest(symlink) + "  " + asset + "\n", "not a regular file"},
		{"backslash in name", backslash, digest(backslash) + "  " + asset + "\n", "unsafe archive entry"},
		{"corrupt gzip trailer", corrupt, digest(corrupt) + "  " + asset + "\n", "gzip: invalid checksum"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &release{tag: "v9.9.9", asset: asset, archive: c.archive, sums: c.sums}
			target := setupInstall(t, r)
			var out bytes.Buffer
			err := Install(context.Background(), "0.2.1", &out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Install error = %v, want %q", err, c.want)
			}
			requireUnchanged(t, target)
		})
	}
}

func TestInstallRejectsOversizedChecksums(t *testing.T) {
	asset := testAsset(t)
	shrink(t, &maxChecksums, 64)
	r := &release{tag: "v9.9.9", asset: asset, archive: []byte("x"), sums: strings.Repeat("a", 65)}
	target := setupInstall(t, r)
	var out bytes.Buffer
	err := Install(context.Background(), "0.2.1", &out)
	if err == nil || !strings.Contains(err.Error(), "exceeds 64 bytes") {
		t.Fatalf("Install error = %v, want exceeds 64 bytes", err)
	}
	requireUnchanged(t, target)
}

func TestInstallRejectsOversizedArchive(t *testing.T) {
	asset := testAsset(t)
	shrink(t, &maxArchive, 1<<10)
	archive := bytes.Repeat([]byte("x"), 1<<10+1)
	r := &release{tag: "v9.9.9", asset: asset, archive: archive, sums: digest(archive) + "  " + asset + "\n"}
	target := setupInstall(t, r)
	var out bytes.Buffer
	err := Install(context.Background(), "0.2.1", &out)
	if err == nil || !strings.Contains(err.Error(), "response exceeds 1024 bytes") {
		t.Fatalf("Install error = %v, want response exceeds 1024 bytes", err)
	}
	requireUnchanged(t, target)
}

func TestInstallRejectsDecompressedBomb(t *testing.T) {
	asset := testAsset(t)
	shrink(t, &maxArchive, 1<<10)
	// Zeros compress to far below the limit, but the extracted binary does not.
	archive := tarball(t, entry{"dashy", strings.Repeat("\x00", 1<<10+1), tar.TypeReg})
	if int64(len(archive)) > maxArchive {
		t.Fatalf("test archive is %d bytes, want under %d", len(archive), maxArchive)
	}
	r := &release{tag: "v9.9.9", asset: asset, archive: archive, sums: digest(archive) + "  " + asset + "\n"}
	target := setupInstall(t, r)
	var out bytes.Buffer
	err := Install(context.Background(), "0.2.1", &out)
	if err == nil || !strings.Contains(err.Error(), "decompressed archive exceeds 1024 bytes") {
		t.Fatalf("Install error = %v, want decompressed archive exceeds 1024 bytes", err)
	}
	requireUnchanged(t, target)
}

func TestFetchLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "12345")
	}))
	defer server.Close()
	ctx := context.Background()
	if data, err := fetch(ctx, server.URL, 5); err != nil || string(data) != "12345" {
		t.Fatalf("at limit: %q, %v", data, err)
	}
	if _, err := fetch(ctx, server.URL, 4); err == nil || !strings.Contains(err.Error(), "response exceeds 4 bytes") {
		t.Fatalf("over limit: %v", err)
	}
}

func TestLatestRejectsInvalidTag(t *testing.T) {
	for _, tag := range []string{"../", "../../evil", "v1.0.0/../x", "v1.0.0?x", "v1.0.0#x", "v1.0.0\\x", " v1.0.0", "latest"} {
		t.Run(tag, func(t *testing.T) {
			server := httptest.NewServer(&release{tag: tag})
			defer server.Close()
			stubEndpoints(t, server)
			if got, err := Latest(context.Background()); err == nil {
				t.Fatalf("Latest(%q) = %q, want error", tag, got)
			}
		})
	}
}

func TestInstallRejectsInvalidLatestTag(t *testing.T) {
	r := &release{tag: "../", asset: "unused"}
	target := setupInstall(t, r)
	var out bytes.Buffer
	err := Install(context.Background(), "0.2.1", &out)
	if err == nil || !strings.Contains(err.Error(), "invalid tag") {
		t.Fatalf("Install error = %v, want invalid tag", err)
	}
	if out.Len() != 0 || r.downloads.Load() != 0 {
		t.Fatalf("output = %q, downloads = %d", out.String(), r.downloads.Load())
	}
	requireUnchanged(t, target)
}

func TestInstallDevBuildInstallsValidTag(t *testing.T) {
	asset := testAsset(t)
	archive := tarball(t, entry{"dashy", "new build", tar.TypeReg})
	r := &release{tag: "v9.9.9", asset: asset, archive: archive, sums: digest(archive) + "  " + asset + "\n"}
	target := setupInstall(t, r)
	var out bytes.Buffer
	if err := Install(context.Background(), "dev", &out); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "new build" {
		t.Fatalf("binary = %q, %v", data, err)
	}
}

func TestChecksumFor(t *testing.T) {
	sums := []byte("aa  dashy_linux_amd64.tar.gz\nbb *dashy_darwin_arm64.tar.gz\nCC  other\n")
	if got, err := checksumFor(sums, "dashy_linux_amd64.tar.gz"); err != nil || got != "aa" {
		t.Fatalf("text mode = %q, %v", got, err)
	}
	if got, err := checksumFor(sums, "dashy_darwin_arm64.tar.gz"); err != nil || got != "bb" {
		t.Fatalf("binary mode = %q, %v", got, err)
	}
	if _, err := checksumFor(sums, "dashy_linux_arm64.tar.gz"); err == nil {
		t.Fatal("missing asset accepted")
	}
}

func TestInstallSkipsCurrentRelease(t *testing.T) {
	r := &release{tag: "v0.2.1", asset: "unused"}
	target := setupInstall(t, r)
	var out bytes.Buffer
	if err := Install(context.Background(), "0.2.1", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "dashy 0.2.1 is the latest release.\n" {
		t.Fatalf("output = %q", out.String())
	}
	if n := r.downloads.Load(); n != 0 {
		t.Fatalf("downloads = %d, want 0", n)
	}
	requireUnchanged(t, target)
}

func TestRunCheckReportsLatest(t *testing.T) {
	r := &release{tag: "v9.9.9"}
	server := httptest.NewServer(r)
	defer server.Close()
	stubEndpoints(t, server)
	var out bytes.Buffer
	if err := Run(context.Background(), "0.2.1", []string{"-check"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "latest v9.9.9") {
		t.Fatalf("check output = %q", out.String())
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.2.1", "v0.2.2", true},
		{"v0.2.1", "0.2.1", false},
		{"0.9.9", "1.0.0", true},
		{"1.0.0", "0.9.9", false},
		{"dev", "v1.0.0", false},
		{"0.2.1", "garbage", false},
	}
	for _, c := range cases {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Fatalf("Newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestAvailableUsesCacheAndFetchesOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.3.0"})
	}))
	defer server.Close()
	stubEndpoints(t, server)

	if got := Available(context.Background(), "0.2.1"); got != "v0.3.0" {
		t.Fatalf("Available = %q", got)
	}
	if got := Available(context.Background(), "0.2.1"); got != "v0.3.0" {
		t.Fatalf("cached Available = %q", got)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if got := Available(context.Background(), "0.3.0"); got != "" {
		t.Fatalf("up-to-date Available = %q", got)
	}
	if got := Available(context.Background(), "dev"); got != "" {
		t.Fatalf("dev Available = %q", got)
	}
}

func TestCacheFileHonorsRelativeHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DASHY_HOME", "relative-home")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolved, "relative-home", "update.json")
	if got := cacheFile(); got != want {
		t.Fatalf("cacheFile = %q, want %q", got, want)
	}
	var cached checkCache
	if err := json.Unmarshal([]byte(`{"checked":"2000-01-01T00:00:00Z","latest":"v1.0.0"}`), &cached); err != nil {
		t.Fatal(err)
	}
	if time.Since(cached.Checked) < cacheTTL {
		t.Fatal("stale cache should expire")
	}
}
