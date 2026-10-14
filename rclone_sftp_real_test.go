package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startRcloneSFTPServer runs `rclone serve sftp` over root on a free port,
// with the given host key, user "u" + testSFTPPassword, and the keys in
// authorizedKeys. It returns the port.
func startRcloneSFTPServer(t *testing.T, bin, root, hostKey, authorizedKeys, cacheDir string) int {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "serve", "sftp", root, "--addr", "127.0.0.1:0",
		"--user", "u", "--pass", testSFTPPassword, "--authorized-keys", authorizedKeys,
		"--key", hostKey, "--config", "", "--cache-dir", cacheDir)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	listening := regexp.MustCompile(`SFTP server listening on 127\.0\.0\.1:(\d+)`)
	portCh := make(chan int, 1)
	go func() {
		sent := false
		sc := bufio.NewScanner(stderr)
		for sc.Scan() { // keep draining so the server never blocks on its log
			if m := listening.FindStringSubmatch(sc.Text()); m != nil && !sent {
				p, _ := strconv.Atoi(m[1])
				portCh <- p
				sent = true
			}
		}
	}()
	select {
	case p := <-portCh:
		return p
	case <-time.After(20 * time.Second):
		t.Fatal("rclone serve sftp did not start")
		return 0
	}
}

// The fake rclone mirrors the sftp backend's contract; this runs the real
// one against `rclone serve sftp`: host key pinning with the panel's own SSH
// client, password and key logins, probes, an import and an export.
func TestRealRcloneSFTPImportExport(t *testing.T) {
	bin := realRcloneBin()
	if bin == "" {
		t.Skip("no rclone binary (set GARAGEPANEL_TEST_RCLONE)")
	}
	e := newJobTestEnv(t)
	rc, err := detectRclone(bin, filepath.Join(e.stateDir, "cache"), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.remotes.SetRclone(rc)
	mgr, err := NewJobManager(filepath.Join(e.stateDir, "jobs"), garageRemote(e.cfg), e.remotes, rc)
	if err != nil {
		t.Fatal(err)
	}
	mgr.chunkKeys = 100

	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	// The server's host key and a client key, both made the way the panel
	// makes keys (ed25519, PKCS#8).
	hostPriv, hostPub, err := generateSFTPKey("host")
	if err != nil {
		t.Fatal(err)
	}
	hostKeyFile := filepath.Join(dir, "ssh_host_ed25519_key")
	clientPriv, clientPub, err := generateSFTPKey(sftpKeyComment("vps-key"))
	if err != nil {
		t.Fatal(err)
	}
	authorized := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(hostKeyFile, []byte(hostPriv), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authorized, []byte(clientPub+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := startRcloneSFTPServer(t, bin, root, hostKeyFile, authorized, filepath.Join(dir, "cache"))

	big := make([]byte, 3<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	writeTree(t, filepath.Join(root, "srv/komik"), map[string]string{
		"seri a/001.jpg":  "jpeg-1",
		"seri a/002.jpg":  "jpeg-2",
		"cover.png":       "png",
		"judul ä+b/x.txt": "teks",
	})
	if err := os.WriteFile(filepath.Join(root, "srv/komik/big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	// The first Tes koneksi pins exactly the key the server holds.
	if err := e.remotes.Add(Remote{Name: "vps", Type: RemoteTypeSFTP, Host: "127.0.0.1", Port: port, User: "u", Password: testSFTPPassword}); err != nil {
		t.Fatal(err)
	}
	report, err := e.remotes.Probe(t.Context(), "vps", "/srv/komik", true)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := strings.Join(strings.Fields(hostPub)[:2], " ")
	if !report.WriteOK || len(report.PinnedKeys) != 1 || report.PinnedKeys[0] != wantKey {
		t.Fatalf("probe = %+v, want pinned %s", report, wantKey)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "srv/komik/.garagepanel-probe-*")); len(left) != 0 {
		t.Errorf("probe file left: %v", left)
	}

	// Import: the folder tree into a bucket.
	importSpec := JobSpec{Kind: JobSync, Mode: "skip-existing", Transfers: 4,
		Src: Endpoint{Remote: "vps", Prefix: "srv/komik/"}, Dst: Endpoint{Bucket: "arsip", Prefix: "komik/"}}
	st, err := mgr.Create(t.Context(), importSpec)
	if err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, mgr, st.ID, terminal)
	if s.Status != JobDone || s.Counters.Transferred != 5 || s.Counters.Failed != 0 {
		t.Fatalf("import: status=%s counters=%+v err=%q log=%v", s.Status, s.Counters, s.Error, s.LastLog)
	}
	for rel, want := range map[string]string{"seri a/001.jpg": "jpeg-1", "cover.png": "png", "judul ä+b/x.txt": "teks"} {
		if got, _ := e.garage.get("arsip", "komik/"+rel); string(got) != want {
			t.Errorf("komik/%s = %q", rel, got)
		}
	}
	if got, _ := e.garage.get("arsip", "komik/big.bin"); !bytes.Equal(got, big) {
		t.Errorf("big.bin differs (len %d)", len(got))
	}
	if ct := e.garage.contentType("arsip", "komik/seri a/001.jpg"); ct != "image/jpeg" {
		t.Errorf("content type = %q", ct)
	}
	st, err = mgr.Create(t.Context(), importSpec)
	if err != nil {
		t.Fatal(err)
	}
	if s = waitJob(t, mgr, st.ID, terminal); s.Status != JobDone || s.Counters.Transferred != 0 || s.Counters.Skipped != 5 {
		t.Errorf("import rerun: status=%s counters=%+v", s.Status, s.Counters)
	}

	// Export: a bucket into a folder that does not exist yet.
	e.garage.put("media", "seri/001.webp", []byte("webp-1"), "image/webp")
	e.garage.put("media", "seri/002.webp", []byte("webp-2"), "image/webp")
	e.garage.put("media", "index.html", []byte("<h1>"), "text/html")
	exportSpec := JobSpec{Kind: JobSync, Mode: "update-if-different", Transfers: 2,
		Src: Endpoint{Bucket: "media"}, Dst: Endpoint{Remote: "vps", Prefix: "srv/export/media/"}}
	st, err = mgr.Create(t.Context(), exportSpec)
	if err != nil {
		t.Fatal(err)
	}
	if s = waitJob(t, mgr, st.ID, terminal); s.Status != JobDone || s.Counters.Transferred != 3 || s.Counters.Failed != 0 {
		t.Fatalf("export: status=%s counters=%+v err=%q log=%v", s.Status, s.Counters, s.Error, s.LastLog)
	}
	if got, err := os.ReadFile(filepath.Join(root, "srv/export/media/seri/002.webp")); err != nil || string(got) != "webp-2" {
		t.Errorf("exported file = %q, %v", got, err)
	}
	e.garage.put("media", "index.html", []byte("<h1>baru</h1>"), "text/html")
	st, err = mgr.Create(t.Context(), exportSpec)
	if err != nil {
		t.Fatal(err)
	}
	if s = waitJob(t, mgr, st.ID, terminal); s.Status != JobDone || s.Counters.Transferred != 1 || s.Counters.Skipped != 2 {
		t.Errorf("export refresh: status=%s counters=%+v err=%q", s.Status, s.Counters, s.Error)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "srv/export/media/index.html")); string(got) != "<h1>baru</h1>" {
		t.Errorf("index.html = %q", got)
	}

	// Key login with a panel-generated key that is in authorized_keys.
	keys := []string{wantKey}
	add := func(r Remote) {
		t.Helper()
		r.Type, r.Host, r.Port = RemoteTypeSFTP, "127.0.0.1", port
		if r.HostKeys == nil {
			r.HostKeys = keys
		}
		if err := e.remotes.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	add(Remote{Name: "vps-key", User: "root", PrivateKey: clientPriv})
	if _, err := e.remotes.Probe(t.Context(), "vps-key", "/srv/komik", false); err != nil {
		t.Errorf("key login: %v", err)
	}
	// A key the server does not know, a wrong password, a wrong host key:
	// rclone's words plus the panel's next step.
	otherPriv, _, err := generateSFTPKey("x")
	if err != nil {
		t.Fatal(err)
	}
	add(Remote{Name: "vps-unknown-key", User: "deploy", PrivateKey: otherPriv})
	add(Remote{Name: "vps-bad-pw", User: "root", Password: "salah"})
	add(Remote{Name: "vps-mitm", User: "u", Password: testSFTPPassword, HostKeys: []string{testHostKey(t)}})
	for name, want := range map[string][]string{
		"vps-unknown-key": {"unable to authenticate", "authorized_keys milik user deploy"},
		"vps-bad-pw":      {"unable to authenticate", "PermitRootLogin prohibit-password"},
		"vps-mitm":        {"key mismatch", "TIDAK SAMA"},
	} {
		_, err := e.remotes.Probe(t.Context(), name, "/srv/komik", false)
		for _, w := range want {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Errorf("%s: err = %v, want %q", name, err, w)
			}
		}
	}
	// A folder that does not exist, read-only.
	if _, err := e.remotes.Probe(t.Context(), "vps", "/srv/tidak-ada", false); err == nil || !strings.Contains(err.Error(), "folder tidak ada di VPS") {
		t.Errorf("missing folder: %v", err)
	}
}
