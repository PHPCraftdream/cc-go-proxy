package ops

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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

// A .old that cannot be removed (a process of an earlier update holds it open;
// here a non-empty directory stands in) must not block the update: the current
// file goes aside under another name, and leftovers of earlier updates go.
func TestReplaceBinaryWindowsPathBusyOld(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "ccgp"), "old")
	busy := filepath.Join(dir, "ccgp.old")
	if err := os.Mkdir(busy, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExe(t, filepath.Join(busy, "held"), "held")
	stale := filepath.Join(dir, "ccgp.old-20200101-000000.000")
	writeExe(t, stale, "stale")
	other := filepath.Join(dir, "ccgpy.old-20200101-000000.000")
	writeExe(t, other, "other binary's leftover")

	if err := replaceBinary(dir, "ccgp", []byte("new"), "windows"); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "ccgp"))
	if err != nil || string(got) != "new" {
		t.Errorf("ccgp = %q, %v; want \"new\"", got, err)
	}
	if _, err := os.Stat(filepath.Join(busy, "held")); err != nil {
		t.Errorf("the busy .old was touched: %v", err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("the stale ccgp.old-* was not removed")
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another binary's leftover was removed: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "ccgp.old-*")); len(left) != 0 {
		t.Errorf("aside file left behind: %v", left)
	}
}

// With a busy .old a failed final rename still rolls the old file back.
func TestReplaceBinaryWindowsPathBusyOldRollback(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "ccgp"), "old")
	busy := filepath.Join(dir, "ccgp.old")
	if err := os.Mkdir(busy, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExe(t, filepath.Join(busy, "held"), "held")
	calls := 0
	fail := func(from, to string) error {
		calls++
		if calls == 2 {
			return fmt.Errorf("rename blocked")
		}
		return os.Rename(from, to)
	}
	if err := installBinary(dir, "ccgp", []byte("new"), "windows", fail); err == nil {
		t.Fatal("want error from failing rename")
	}
	got, err := os.ReadFile(filepath.Join(dir, "ccgp"))
	if err != nil || string(got) != "old" {
		t.Errorf("ccgp = %q, %v; want rollback to \"old\"", got, err)
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
	if err := UpdateCmd([]string{"--bogus"}); err == nil {
		t.Error("unknown flag accepted")
	}
}

// A member that cannot be extracted aborts the update before the first
// binary is replaced: the install never holds two different versions.
func TestUpdateFlowBrokenMemberLeavesTheInstallAlone(t *testing.T) {
	dir := t.TempDir()
	ccgpExe, ccgpyExe := "ccgp"+exeExt(runtime.GOOS), "ccgpy"+exeExt(runtime.GOOS)
	writeExe(t, filepath.Join(dir, ccgpExe), "old-ccgp")
	writeExe(t, filepath.Join(dir, ccgpyExe), "old-ccgpy")

	ver := "9.9.9"
	name, archive, zipArch, err := archiveName(ver, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	ccgpMember, ccgpyMember := name+"/ccgp"+exeExt(runtime.GOOS), name+"/ccgpy"+exeExt(runtime.GOOS)
	var arch []byte
	if zipArch {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, err := zw.Create(ccgpMember)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("new-ccgp")); err != nil {
			t.Fatal(err)
		}
		fh := &zip.FileHeader{Name: ccgpyMember, Method: zip.Deflate}
		fh.SetMode(os.ModeSymlink)
		w, err = zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("C:/Windows/System32")); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		arch = buf.Bytes()
	} else {
		arch = buildTarGzHeaders(t, []*tar.Header{
			{Name: ccgpMember, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len("new-ccgp"))},
			{Name: ccgpyMember, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		}, map[string][]byte{ccgpMember: []byte("new-ccgp")})
	}
	sums := fmt.Sprintf("%s  %s\n", sum(arch), archive)

	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/releases/tag/v"+ver)
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/download/v"+ver+"/"+archive, func(w http.ResponseWriter, r *http.Request) { w.Write(arch) })
	mux.HandleFunc("/download/v"+ver+"/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sums)) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	lookup, download := updateClients()
	var out bytes.Buffer
	err = updateFlow(false, srv.URL, dir, "0.1.0", lookup, download, func() bool { return false }, &out)
	if err == nil || !strings.Contains(err.Error(), ccgpyExe) {
		t.Fatalf("want the ccgpy member to be rejected, got %v", err)
	}
	for bin, want := range map[string]string{ccgpExe: "old-ccgp", ccgpyExe: "old-ccgpy"} {
		got, err := os.ReadFile(filepath.Join(dir, bin))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", bin, got, err, want)
		}
	}
	for _, pat := range []string{"*.new", "*.old"} {
		if got, _ := filepath.Glob(filepath.Join(dir, pat)); len(got) != 0 {
			t.Errorf("%s left behind: %v", pat, got)
		}
	}
}
