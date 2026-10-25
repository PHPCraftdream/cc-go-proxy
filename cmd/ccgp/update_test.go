package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"0.1.1", "0.1.10", -1},
		{"0.1.10", "0.1.1", 1},
		{"v0.2.0", "0.2.0", 0},
		{"0.2.0", "v0.2.0", 0},
		{"0.2.0-dev", "0.2.0", 0},
		{"v0.3.0-rc1", "v0.2.9", 1},
		{"0.10.0", "0.9.9", 1},
		{"1.0", "1.0.0", 0},
		{"", "0.1.0", -1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestArchiveName(t *testing.T) {
	cases := []struct {
		ver, goos, goarch string
		dir, archive      string
		zipArch           bool
		wantErr           bool
	}{
		{"0.1.2", "linux", "amd64", "ccgp_0.1.2_linux_amd64", "ccgp_0.1.2_linux_amd64.tar.gz", false, false},
		{"0.1.2", "darwin", "arm64", "ccgp_0.1.2_darwin_arm64", "ccgp_0.1.2_darwin_arm64.tar.gz", false, false},
		{"0.1.2", "windows", "amd64", "ccgp_0.1.2_windows_amd64", "ccgp_0.1.2_windows_amd64.zip", true, false},
		{"0.1.2", "windows", "arm64", "ccgp_0.1.2_windows_arm64", "ccgp_0.1.2_windows_arm64.zip", true, false},
		{"0.1.2", "freebsd", "amd64", "", "", false, true},
		{"0.1.2", "linux", "386", "", "", false, true},
	}
	for _, c := range cases {
		dir, archive, zipArch, err := archiveName(c.ver, c.goos, c.goarch)
		if c.wantErr {
			if err == nil {
				t.Errorf("archiveName(%s, %s, %s): want error, got %q", c.ver, c.goos, c.goarch, archive)
			}
			continue
		}
		if err != nil || dir != c.dir || archive != c.archive || zipArch != c.zipArch {
			t.Errorf("archiveName(%s, %s, %s) = %q, %q, %v, %v; want %q, %q, %v",
				c.ver, c.goos, c.goarch, dir, archive, zipArch, err, c.dir, c.archive, c.zipArch)
		}
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("archive bytes")
	sums := fmt.Sprintf("%s  other.tar.gz\n%s  ccgp_0.1.0_linux_amd64.tar.gz\n", sum([]byte("x")), sum(data))
	if err := verifyChecksum([]byte(sums), "ccgp_0.1.0_linux_amd64.tar.gz", data); err != nil {
		t.Errorf("valid checksum rejected: %v", err)
	}
	if err := verifyChecksum([]byte(sums), "other.tar.gz", data); err == nil {
		t.Error("mismatching checksum accepted")
	}
	if err := verifyChecksum([]byte(sums), "missing.tar.gz", data); err == nil {
		t.Error("archive missing from SHA256SUMS accepted")
	}
}

// buildTarGz packs name -> content pairs as regular files.
func buildTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	headers := make([]*tar.Header, 0, len(files))
	bodies := make(map[string][]byte, len(files))
	for name, content := range files {
		headers = append(headers, &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(content))})
		bodies[name] = content
	}
	return buildTarGzHeaders(t, headers, bodies)
}

// buildTarGzHeaders writes the given headers with their bodies verbatim, so
// tests can add non-regular entries such as symlinks.
func buildTarGzHeaders(t *testing.T, headers []*tar.Header, bodies map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, hdr := range headers {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		// Header-only entries (symlinks) take no body.
		if body := bodies[hdr.Name]; len(body) > 0 {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func buildZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildZipSymlink packs one symlink entry at name.
func buildZipSymlink(t *testing.T, name, target string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fh := &zip.FileHeader{Name: name, Method: zip.Deflate}
	fh.SetMode(os.ModeSymlink)
	w, err := zw.CreateHeader(fh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(target)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractMemberTarGz(t *testing.T) {
	dir := "ccgp_9.9.9_linux_amd64"
	ccgp, ccgpy := []byte("ccgp-binary"), []byte("ccgpy-binary")
	arch := buildTarGz(t, map[string][]byte{
		dir + "/ccgp":          ccgp,
		dir + "/ccgpy":         ccgpy,
		"README":               []byte("nope"),
		"../" + dir + "/ccgp":  []byte("evil"),
		dir + "/../../evil.sh": []byte("evil"),
	})
	got, err := extractMember(arch, false, dir+"/ccgpy", 1<<20)
	if err != nil {
		t.Fatalf("extractMember: %v", err)
	}
	if !bytes.Equal(got, ccgpy) {
		t.Errorf("got %q, want %q", got, ccgpy)
	}
	if _, err := extractMember(arch, false, dir+"/nope", 1<<20); err == nil {
		t.Error("missing member accepted")
	}
	if _, err := extractMember([]byte("not a gzip"), false, dir+"/ccgp", 1<<20); err == nil {
		t.Error("broken archive accepted")
	}
}

func TestExtractMemberTarGzSymlinkRejected(t *testing.T) {
	member := "ccgp_9.9.9_linux_amd64/ccgp"
	hdr := &tar.Header{Name: member, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}
	arch := buildTarGzHeaders(t, []*tar.Header{hdr}, nil)
	got, err := extractMember(arch, false, member, 1<<20)
	if err == nil {
		t.Errorf("symlink member accepted, got %q", got)
	}
}

func TestExtractMemberZip(t *testing.T) {
	dir := "ccgp_9.9.9_windows_amd64"
	ccgp := []byte("ccgp.exe-bytes")
	arch := buildZip(t, map[string][]byte{
		dir + "/ccgp.exe":  ccgp,
		dir + "/ccgpy.exe": []byte("ccgpy"),
		"../../evil.exe":   []byte("evil"),
	})
	got, err := extractMember(arch, true, dir+"/ccgp.exe", 1<<20)
	if err != nil {
		t.Fatalf("extractMember: %v", err)
	}
	if !bytes.Equal(got, ccgp) {
		t.Errorf("got %q, want %q", got, ccgp)
	}
	if _, err := extractMember(arch, true, dir+"/evil.exe", 1<<20); err == nil {
		t.Error("traversal member accepted")
	}
}

func TestExtractMemberZipSymlinkRejected(t *testing.T) {
	member := "ccgp_9.9.9_windows_amd64/ccgp.exe"
	arch := buildZipSymlink(t, member, "C:/Windows/System32")
	if got, err := extractMember(arch, true, member, 1<<20); err == nil {
		t.Errorf("symlink member accepted, got %q", got)
	}
}

func TestExtractMemberOversize(t *testing.T) {
	arch := buildTarGz(t, map[string][]byte{"d/ccgp": bytes.Repeat([]byte("a"), 4096)})
	if _, err := extractMember(arch, false, "d/ccgp", 1024); err == nil {
		t.Error("oversize member accepted")
	}
	zarch := buildZip(t, map[string][]byte{"d/ccgp.exe": bytes.Repeat([]byte("a"), 4096)})
	if _, err := extractMember(zarch, true, "d/ccgp.exe", 1024); err == nil {
		t.Error("oversize zip member accepted")
	}
}

func writeExe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceBinaryUnixPath(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "ccgp"), "old")
	if err := replaceBinary(dir, "ccgp", []byte("new"), "linux"); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "ccgp"))
	if err != nil || string(got) != "new" {
		t.Errorf("ccgp = %q, %v; want \"new\"", got, err)
	}
	for _, leftover := range []string{"ccgp.new", "ccgp.old"} {
		if _, err := os.Stat(filepath.Join(dir, leftover)); err == nil {
			t.Errorf("%s left behind", leftover)
		}
	}
}

func TestReplaceBinaryStaleNonExecutableNew(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS))
	writeExe(t, target, "old")
	if err := os.WriteFile(target+".new", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(dir, "ccgp"+exeExt(runtime.GOOS), []byte("new"), runtime.GOOS); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "new" {
		t.Fatalf("ccgp = %q, %v; want \"new\"", got, err)
	}
	if runtime.GOOS == "windows" {
		return // POSIX mode bits are not reliable on Windows
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", fi.Mode().Perm())
	}
}

func TestReplaceBinaryWindowsPathRollback(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "ccgp"), "old")
	calls := 0
	fail := func(from, to string) error {
		calls++
		if calls == 2 { // the final rename of the new file into place
			return fmt.Errorf("rename blocked")
		}
		return os.Rename(from, to)
	}
	err := installBinary(dir, "ccgp", []byte("new"), "windows", fail)
	if err == nil {
		t.Fatal("want error from failing rename")
	}
	got, err2 := os.ReadFile(filepath.Join(dir, "ccgp"))
	if err2 != nil || string(got) != "old" {
		t.Errorf("ccgp = %q, %v; want rollback to \"old\"", got, err2)
	}
}

func TestReplaceBinaryWindowsPath(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "ccgp"), "old")
	// A stale .old must be removed before the current file moves aside.
	writeExe(t, filepath.Join(dir, "ccgp.old"), "stale")
	if err := replaceBinary(dir, "ccgp", []byte("new"), "windows"); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "ccgp"))
	if err != nil || string(got) != "new" {
		t.Errorf("ccgp = %q, %v; want \"new\"", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ccgp.old")); err == nil {
		t.Error("ccgp.old left behind")
	}
}

// updateServer serves /latest (302), the archive and SHA256SUMS for ver,
// built for the platform the tests run on (production uses runtime values).
func updateServer(t *testing.T, ver string, ccgp, ccgpy []byte, sumsOverride string) *httptest.Server {
	t.Helper()
	name, archive, zipArch, err := archiveName(ver, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	ext := exeExt(runtime.GOOS)
	files := map[string][]byte{name + "/ccgp" + ext: ccgp, name + "/ccgpy" + ext: ccgpy}
	var arch []byte
	if zipArch {
		arch = buildZip(t, files)
	} else {
		arch = buildTarGz(t, files)
	}
	sums := fmt.Sprintf("%s  %s\n", sum(arch), archive)
	if sumsOverride != "" {
		sums = sumsOverride
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/releases/tag/v"+ver)
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/download/v"+ver+"/"+archive, func(w http.ResponseWriter, r *http.Request) {
		w.Write(arch)
	})
	mux.HandleFunc("/download/v"+ver+"/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sums))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func updateClients() (lookup, download *http.Client) {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		&http.Client{}
}

func TestUpdateFlowInstalls(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS)), "old-ccgp")
	writeExe(t, filepath.Join(dir, "ccgpy"+exeExt(runtime.GOOS)), "old-ccgpy")
	srv := updateServer(t, "9.9.9", []byte("new-ccgp"), []byte("new-ccgpy"), "")
	lookup, download := updateClients()
	var out bytes.Buffer
	err := updateFlow(false, srv.URL, dir, "0.1.0", lookup, download,
		func() bool { return false }, &out)
	if err != nil {
		t.Fatalf("updateFlow: %v", err)
	}
	if !strings.Contains(out.String(), "updated ccgp 0.1.0 -> 9.9.9") {
		t.Errorf("output %q lacks the updated message", out.String())
	}
	for bin, want := range map[string]string{"ccgp": "new-ccgp", "ccgpy": "new-ccgpy"} {
		got, err := os.ReadFile(filepath.Join(dir, bin+exeExt(runtime.GOOS)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", bin, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS)+".new")); err == nil {
		t.Error("ccgp.new left behind")
	}
}

func TestUpdateFlowSkipsCcgpyWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS)), "old")
	srv := updateServer(t, "9.9.9", []byte("new-ccgp"), []byte("new-ccgpy"), "")
	lookup, download := updateClients()
	var out bytes.Buffer
	if err := updateFlow(false, srv.URL, dir, "0.1.0", lookup, download,
		func() bool { return false }, &out); err != nil {
		t.Fatalf("updateFlow: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ccgpy"+exeExt(runtime.GOOS))); err == nil {
		t.Error("ccgpy installed although it was absent")
	}
}

func TestUpdateFlowCheckDoesNotInstall(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS)), "old")
	srv := updateServer(t, "9.9.9", []byte("new"), []byte("new"), "")
	lookup, download := updateClients()
	var out bytes.Buffer
	if err := updateFlow(true, srv.URL, dir, "0.1.0", lookup, download,
		func() bool { return false }, &out); err != nil {
		t.Fatalf("updateFlow: %v", err)
	}
	if !strings.Contains(out.String(), "update available: 0.1.0 -> 9.9.9") {
		t.Errorf("output %q lacks the update-available message", out.String())
	}
	got, _ := os.ReadFile(filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS)))
	if string(got) != "old" {
		t.Errorf("--check installed the update: ccgp = %q", got)
	}
}

func TestUpdateFlowUpToDate(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS)), "old")
	srv := updateServer(t, "0.1.0", []byte("new"), []byte("new"), "")
	lookup, download := updateClients()
	var out bytes.Buffer
	if err := updateFlow(true, srv.URL, dir, "0.1.0", lookup, download,
		func() bool { return false }, &out); err != nil {
		t.Fatalf("updateFlow: %v", err)
	}
	if !strings.Contains(out.String(), "ccgp is up to date (0.1.0)") {
		t.Errorf("output %q lacks the up-to-date message", out.String())
	}
	got, _ := os.ReadFile(filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS)))
	if string(got) != "old" {
		t.Errorf("up-to-date installed the update: ccgp = %q", got)
	}
}

func TestUpdateFlowBadChecksumLeavesFiles(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS)), "old")
	_, archive, _, err := archiveName("9.9.9", runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	srv := updateServer(t, "9.9.9", []byte("new"), []byte("new"),
		"deadbeef  "+archive+"\n")
	lookup, download := updateClients()
	var out bytes.Buffer
	err = updateFlow(false, srv.URL, dir, "0.1.0", lookup, download,
		func() bool { return false }, &out)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch error, got %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS)))
	if string(got) != "old" {
		t.Errorf("bad checksum changed ccgp to %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS)+".new")); err == nil {
		t.Error("ccgp.new left behind after failed verification")
	}
}

func TestUpdateFlowServerHint(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS)), "old")
	srv := updateServer(t, "9.9.9", []byte("new"), []byte("new"), "")
	lookup, download := updateClients()
	var out bytes.Buffer
	if err := updateFlow(false, srv.URL, dir, "0.1.0", lookup, download,
		func() bool { return true }, &out); err != nil {
		t.Fatalf("updateFlow: %v", err)
	}
	if !strings.Contains(out.String(), "ccgp --restart") {
		t.Errorf("output %q lacks the restart hint", out.String())
	}
}

func TestUpdateFlowNewerLocal(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS)), "old")
	srv := updateServer(t, "0.0.9", []byte("new"), []byte("new"), "")
	lookup, download := updateClients()
	var out bytes.Buffer
	if err := updateFlow(false, srv.URL, dir, "0.1.0", lookup, download,
		func() bool { return false }, &out); err != nil {
		t.Fatalf("updateFlow: %v", err)
	}
	if !strings.Contains(out.String(), "newer than the latest release") {
		t.Errorf("output %q lacks the newer-than-latest message", out.String())
	}
	got, _ := os.ReadFile(filepath.Join(dir, "ccgp"+exeExt(runtime.GOOS)))
	if string(got) != "old" {
		t.Error("newer local build got downgraded")
	}
}

func TestUpdateCmdFlags(t *testing.T) {
	if err := updateCmd([]string{"--bogus"}); err == nil {
		t.Error("unknown flag accepted")
	}
}
