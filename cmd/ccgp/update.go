package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/daemon"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server"
)

// updateBase is the GitHub releases base URL. It is a variable so tests can
// point it at a local server.
var updateBase = "https://github.com/PHPCraftdream/cc-go-proxy/releases"

const (
	updateUserAgent = "ccgp-update"
	updateMaxMember = int64(200) << 20 // cap per extracted file
)

// updateCmd implements `ccgp --update [--check]`.
func updateCmd(args []string) error {
	check := false
	for _, a := range args {
		if a != "--check" {
			return fmt.Errorf("--update: unknown flag %q, see: ccgp --update --help", a)
		}
		check = true
	}
	lookup := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	download := &http.Client{Timeout: 5 * time.Minute}
	dir, err := installDir()
	if err != nil {
		return err
	}
	return updateFlow(check, updateBase, dir, server.ProxyVersion, lookup, download,
		defaultServerRunning, os.Stdout)
}

// defaultServerRunning reports whether a background server is running. The
// whole lookup (config load included) lives here so tests can stub it.
func defaultServerRunning() bool {
	host, port := listenAddr()
	_, err := daemon.Check(daemon.BaseURL(host, port))
	return err == nil
}

// updateFlow is the testable core: look up the latest release, compare, and
// (unless --check) install the binaries into dir.
func updateFlow(checkOnly bool, base, dir, curVersion string,
	lookup, download *http.Client, serverRunning func() bool, out io.Writer) error {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	latest, err := latestVersion(ctx, lookup, base)
	if err != nil {
		return err
	}
	switch compareVersions(curVersion, latest) {
	case 0:
		_, _ = fmt.Fprintf(out, "ccgp is up to date (%s)\n", curVersion)
		return nil
	case 1:
		_, _ = fmt.Fprintf(out, "this build (%s) is newer than the latest release (%s); nothing to do\n", curVersion, latest)
		return nil
	}
	if checkOnly {
		_, _ = fmt.Fprintf(out, "update available: %s -> %s\n", curVersion, latest)
		return nil
	}

	name, archive, zipArch, err := archiveName(latest, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	data, err := updateDownload(ctx, download, fmt.Sprintf("%s/download/v%s/%s", base, latest, archive))
	if err != nil {
		return err
	}
	// SHA256SUMS is small: 60 s, and redirects are followed like for the archive.
	sumsCtx, cancelSums := context.WithTimeout(ctx, 60*time.Second)
	defer cancelSums()
	sums, err := updateDownload(sumsCtx, download, fmt.Sprintf("%s/download/v%s/SHA256SUMS", base, latest))
	if err != nil {
		return err
	}
	// Verify before touching anything on disk.
	if err := verifyChecksum(sums, archive, data); err != nil {
		return err
	}

	install := []string{"ccgp" + exeExt(runtime.GOOS)}
	if dir != "" {
		if _, err := os.Stat(filepath.Join(dir, "ccgpy"+exeExt(runtime.GOOS))); err == nil {
			install = append(install, "ccgpy"+exeExt(runtime.GOOS))
		}
	}
	for _, bin := range install {
		content, err := extractMember(data, zipArch, name+"/"+bin, updateMaxMember)
		if err != nil {
			return err
		}
		if err := replaceBinary(dir, bin, content, runtime.GOOS); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(out, "updated ccgp %s -> %s\n", curVersion, latest)
	if serverRunning() {
		_, _ = fmt.Fprintln(out, "restart the background server to use it: ccgp --restart")
	}
	return nil
}

// latestVersion resolves <base>/latest without following the redirect and
// reads the version from the Location header (.../releases/tag/vX.Y.Z); the
// GitHub API (and its rate limit) is not used.
func latestVersion(ctx context.Context, client *http.Client, base string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", updateUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot look up the latest release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
		return "", fmt.Errorf("cannot find the latest release (HTTP %d from %s/latest)", resp.StatusCode, base)
	}
	tag := loc[strings.LastIndex(loc, "/")+1:]
	if !strings.HasPrefix(tag, "v") || len(tag) < 2 || tag[1] < '0' || tag[1] > '9' {
		return "", fmt.Errorf("cannot find the latest release (got %q)", loc)
	}
	return tag[1:], nil
}

// compareVersions compares two versions by their numeric X.Y.Z prefix; a
// pre-release/dev suffix is ignored. Returns -1, 0 or 1.
func compareVersions(a, b string) int {
	as, bs := versionParts(a), versionParts(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// versionParts parses the leading numeric components ("v0.2.1-dev" -> 0,2,1).
func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.Split(v, ".")
	var nums []int
	for _, p := range parts {
		digits := p
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				digits = p[:i]
				break
			}
		}
		if digits == "" {
			break
		}
		n := 0
		for i := 0; i < len(digits); i++ {
			n = n*10 + int(digits[i]-'0')
		}
		nums = append(nums, n)
	}
	return nums
}

// archiveName returns the archive's top-level directory and its file name:
// dir `ccgp_<ver>_<os>_<arch>`, file `<dir>.tar.gz` (`.zip` on Windows).
func archiveName(ver, goos, goarch string) (dir string, archive string, zipArch bool, err error) {
	switch goos {
	case "linux", "darwin", "windows":
	default:
		return "", "", false, fmt.Errorf("unsupported platform %s/%s: no release build", goos, goarch)
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", "", false, fmt.Errorf("unsupported platform %s/%s: no release build", goos, goarch)
	}
	dir = fmt.Sprintf("ccgp_%s_%s_%s", ver, goos, goarch)
	if goos == "windows" {
		return dir, dir + ".zip", true, nil
	}
	return dir, dir + ".tar.gz", false, nil
}

// verifyChecksum checks data against the archive's line in SHA256SUMS.
func verifyChecksum(sums []byte, name string, data []byte) error {
	h := sha256.Sum256(data)
	got := hex.EncodeToString(h[:])
	for _, line := range strings.Split(string(sums), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != name {
			continue
		}
		if !strings.EqualFold(fields[0], got) {
			return fmt.Errorf("checksum mismatch for %s (expected %s, got %s)", name, fields[0], got)
		}
		return nil
	}
	return fmt.Errorf("%s is not listed in SHA256SUMS", name)
}

// extractMember returns exactly the named archive member. Only fixed,
// verified names are accepted, so entry names from the archive never become
// file paths. Entries that are not regular files or exceed max are rejected.
func extractMember(data []byte, zipArch bool, member string, max int64) ([]byte, error) {
	if zipArch {
		return extractZipMember(data, member, max)
	}
	zr, err := gzipOrErr(data)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s is missing from the archive", member)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != member {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%s is not a regular file", member)
		}
		return readCapped(tr, member, max)
	}
}

func extractZipMember(data []byte, member string, max int64) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.Name != member {
			continue
		}
		if !f.FileInfo().Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", member)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		return readCapped(rc, member, max)
	}
	return nil, fmt.Errorf("%s is missing from the archive", member)
}

func readCapped(r io.Reader, name string, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s exceeds the size limit", name)
	}
	return data, nil
}

// replaceBinary installs content as dir/name, next to the running executable.
// On Unix the file is written as <name>.new and renamed over the target. On
// Windows a running exe cannot be overwritten but can be renamed away: the
// current file moves to <name>.old and the new file takes its place; a failed
// final rename rolls the old file back.
func replaceBinary(dir, name string, content []byte, goos string) error {
	return installBinary(dir, name, content, goos, os.Rename)
}

func installBinary(dir, name string, content []byte, goos string, rename func(from, to string) error) error {
	newPath := filepath.Join(dir, name+".new")
	oldPath := filepath.Join(dir, name+".old")
	// A leftover .new would keep its old permissions after WriteFile.
	if err := os.Remove(newPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cannot remove the stale %s: %w", newPath, err)
	}
	if err := os.WriteFile(newPath, content, 0o755); err != nil {
		return fmt.Errorf("cannot write %s: %w", newPath, err)
	}
	// WriteFile only applies the mode when it creates the file.
	if err := os.Chmod(newPath, 0o755); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("cannot make %s executable: %w", newPath, err)
	}
	if goos == "windows" {
		_ = os.Remove(oldPath)
		if err := rename(filepath.Join(dir, name), oldPath); err != nil {
			_ = os.Remove(newPath)
			return fmt.Errorf("cannot move the old binary aside: %w", err)
		}
		if err := rename(newPath, filepath.Join(dir, name)); err != nil {
			if backErr := rename(oldPath, filepath.Join(dir, name)); backErr != nil {
				return fmt.Errorf("install failed (%v) and the backup could not be restored (%v)", err, backErr)
			}
			_ = os.Remove(newPath)
			return fmt.Errorf("cannot replace %s: %w", name, err)
		}
		_ = os.Remove(oldPath)
		return nil
	}
	if err := rename(newPath, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("cannot replace %s: %w", name, err)
	}
	return nil
}

// installDir is the directory of the running executable, with symlinks
// resolved so the replacement lands next to the real binary.
func installDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// updateDownload fetches a release file into memory.
func updateDownload(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", updateUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, updateMaxMember+1))
	if err != nil {
		return nil, fmt.Errorf("download failed: %s: %w", url, err)
	}
	if int64(len(data)) > updateMaxMember {
		return nil, fmt.Errorf("download failed: %s exceeds the size limit", url)
	}
	return data, nil
}

// exeExt returns ".exe" on Windows, "" elsewhere.
func exeExt(goos string) string {
	if goos == "windows" {
		return ".exe"
	}
	return ""
}

func gzipOrErr(data []byte) (io.Reader, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("cannot unpack the archive: %w", err)
	}
	return zr, nil
}
