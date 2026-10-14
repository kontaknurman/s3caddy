package main

import (
	htmltemplate "html/template"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// htmlText is s the way html/template prints it as text ("+" → "&#43;").
func htmlText(s string) string {
	var b strings.Builder
	_ = htmltemplate.Must(htmltemplate.New("").Parse("{{.}}")).Execute(&b, s)
	return b.String()
}

func (p *testPanel) knob(t *testing.T, name, value string) {
	t.Helper()
	path := filepath.Join(p.rcDir, name)
	if value == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSyncPageAddsTestsAndUsesAnSFTPRemote(t *testing.T) {
	p := newTestPanel(t)
	ed := testEd25519Blob(t)
	ssh := newFakeSSHServer(t, ed)
	edKey, err := hostKeyFromBlob(ed)
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ssh.port())
	// The fake rclone checks the pinned key and the password like sshd.
	p.knob(t, "sftp-hostkey", edKey)
	p.knob(t, "sftp-password", testSFTPPassword)
	logs := &safeLog{}
	log.SetOutput(logs)
	defer log.SetOutput(os.Stderr)

	resp := p.post(t, "/sync/remotes/add", url.Values{
		"type": {"sftp"}, "name": {"vps-lama"}, "host": {"127.0.0.1"}, "port": {port},
		"user": {"root"}, "auth": {"password"}, "password": {testSFTPPassword + "\r\n"},
	})
	body := p.followFlash(t, resp)
	fp := hostKeyFingerprint(edKey)
	if !strings.Contains(body, "Remote &#34;vps-lama&#34; (VPS 127.0.0.1:"+port+") disimpan") ||
		!strings.Contains(body, htmlText("Host key server disematkan: ED25519 "+fp)) {
		t.Fatalf("remote not saved: %s", firstLines(body))
	}

	// The card: server, login kind and fingerprint, never the password.
	_, body = p.get(t, "/sync")
	for _, want := range []string{"root@127.0.0.1:" + port, htmlText(fp), "VPS lewat SSH (SFTP)", `name="path"`, `data-type="sftp"`, `name="remote_path"`} {
		if !strings.Contains(body, want) {
			t.Errorf("sync page lacks %q", want)
		}
	}
	if strings.Contains(body, htmlText(testSFTPPassword)) || strings.Contains(body, "host key belum disematkan") {
		t.Error("password shown, or host key reported unpinned")
	}
	stored, _ := p.app.remotes.Get("vps-lama")
	if stored.Password != testSFTPPassword || len(stored.HostKeys) != 1 || stored.HostKeys[0] != edKey {
		t.Errorf("stored = %q %v", stored.Password, stored.HostKeys)
	}
	kh := filepath.Join(p.app.cfg.StateDir, "known_hosts", "vps-lama")
	if raw, _ := os.ReadFile(kh); !strings.Contains(string(raw), "\n[*]:"+port+" "+edKey+"\n") {
		t.Errorf("known_hosts:\n%s", raw)
	}

	// Tes koneksi with a folder: login, list, write and delete a test file.
	root := filepath.Join(p.rcDir, "sftp", "127.0.0.1")
	uploads := filepath.Join(root, "var/www/uploads")
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		t.Fatal(err)
	}
	test := func(form url.Values) string {
		t.Helper()
		form.Set("name", "vps-lama")
		return p.followFlash(t, p.post(t, "/sync/remotes/test", form))
	}
	if body = test(url.Values{"path": {"/var/www/uploads"}}); !strings.Contains(body, "bisa login, folder /var/www/uploads ada, dan menulis lalu menghapus file uji di sana berhasil") {
		t.Errorf("probe: %s", firstLines(body))
	}
	if entries, _ := os.ReadDir(uploads); len(entries) != 0 {
		t.Errorf("the test file was left behind: %v", entries)
	}
	if body = test(url.Values{"path": {"/var/www/uploads"}, "write": {"0"}}); !strings.Contains(body, "bisa login dan folder /var/www/uploads ada") {
		t.Errorf("read probe: %s", firstLines(body))
	}
	if body = test(url.Values{"path": {"/srv/cadangan/baru"}}); !strings.Contains(body, "folder /srv/cadangan/baru belum ada, jadi dibuat") {
		t.Errorf("write probe of a new folder: %s", firstLines(body))
	}
	if info, err := os.Stat(filepath.Join(root, "srv/cadangan/baru")); err != nil || !info.IsDir() {
		t.Errorf("the destination folder was not created: %v", err)
	}
	if body = test(url.Values{"path": {"/srv/tidak-ada"}, "write": {"0"}}); !strings.Contains(body, "folder tidak ada di VPS") {
		t.Errorf("read probe of a missing folder: %s", firstLines(body))
	}
	if body = test(url.Values{"path": {"/etc/ssh"}}); !strings.Contains(body, "folder sistem") {
		t.Errorf("system folder: %s", firstLines(body))
	}
	if body = test(url.Values{"path": {""}}); !strings.Contains(body, "tidak boleh kosong") {
		t.Errorf("no folder: %s", firstLines(body))
	}
	p.knob(t, "sftp-password", "changed-on-the-server")
	if body = test(url.Values{"path": {"/var/www/uploads"}}); !strings.Contains(body, "unable to authenticate") || !strings.Contains(body, "PermitRootLogin prohibit-password") {
		t.Errorf("refused login: %s", firstLines(body))
	}
	p.knob(t, "sftp-password", testSFTPPassword)

	// A job from the form: on a VPS the remote side is a folder path; the
	// hidden bucket fields of the S3 half are ignored.
	writeTree(t, uploads, map[string]string{"a.jpg": "aaa", "sub/b.png": "bb"})
	resp = p.post(t, "/sync/jobs/create", url.Values{
		"direction": {"import"}, "remote": {"vps-lama"}, "remote_path": {"/var/www/uploads/"},
		"remote_bucket": {"ignored"}, "remote_prefix": {"ignored/"},
		"garage_bucket": {"media"}, "garage_prefix": {"lama/"}, "mode": {"skip-existing"}, "transfers": {"2"},
	})
	if body = p.followFlash(t, resp); !strings.Contains(body, "dimulai") || !strings.Contains(body, "vps-lama:/var/www/uploads/") {
		t.Fatalf("job not started: %s", firstLines(body))
	}
	jobs := p.waitJobs(t, func(js []jobView) bool { return len(js) == 1 && !js[0].Active })
	if j := jobs[0]; j.Status != JobDone || j.Counters.Transferred != 2 || j.SrcLabel != "vps-lama:/var/www/uploads/" || j.DstLabel != "garage:media/lama/" {
		t.Errorf("job = %s %+v %q → %q err %q", j.Status, j.Counters, j.SrcLabel, j.DstLabel, j.Error)
	}
	if got, _ := p.s3.get("media", "lama/sub/b.png"); string(got) != "bb" {
		t.Errorf("lama/sub/b.png = %q", got)
	}
	for _, bad := range []string{"etc", "/", "/home/budi"} {
		resp = p.post(t, "/sync/jobs/create", url.Values{
			"direction": {"export"}, "remote": {"vps-lama"}, "remote_path": {bad},
			"garage_bucket": {"media"}, "mode": {"skip-existing"}, "transfers": {"2"},
		})
		if body = p.followFlash(t, resp); !strings.Contains(body, "flash error") {
			t.Errorf("export to %q accepted: %s", bad, firstLines(body))
		}
	}
	// A schedule takes the same form.
	resp = p.post(t, "/sync/schedules/create", url.Values{
		"direction": {"export"}, "remote": {"vps-lama"}, "remote_path": {"/srv/backup/komik"},
		"garage_bucket": {"media"}, "mode": {"update-if-different"}, "transfers": {"2"}, "every": {"6h"},
	})
	if body = p.followFlash(t, resp); !strings.Contains(body, htmlText("Ekspor garage:media/ → vps-lama:/srv/backup/komik/")) {
		t.Errorf("schedule: %s", firstLines(body))
	}

	// Edit: switch to a key the panel generates. The card shows the command
	// that installs its public half; the private half never leaves the panel.
	// The server is the same, so its pinned host key is kept, not fetched.
	conns := ssh.connCount()
	resp = p.post(t, "/sync/remotes/update", url.Values{"name": {"vps-lama"}, "type": {"sftp"}, "host": {"127.0.0.1"}, "port": {port}, "user": {"root"}, "auth": {"key"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "Panel membuat kunci SSH baru") {
		t.Fatalf("edit to key: %s", firstLines(body))
	}
	if ssh.connCount() != conns {
		t.Error("an edit of the same server fetched the host key again instead of keeping the pinned one")
	}
	stored, _ = p.app.remotes.Get("vps-lama")
	if stored.PrivateKey == "" || stored.Password != "" || !strings.HasPrefix(stored.PublicKey, "ssh-ed25519 ") ||
		len(stored.HostKeys) != 1 || stored.HostKeys[0] != edKey {
		t.Fatalf("after edit: key %v pw %q pub %q hostkeys %v", stored.PrivateKey != "", stored.Password, stored.PublicKey, stored.HostKeys)
	}
	_, body = p.get(t, "/sync")
	if !strings.Contains(body, htmlText(sftpInstallCommand(stored.PublicKey))) || !strings.Contains(body, "kunci SSH (dibuat panel)") {
		t.Error("install command or login kind missing from the card")
	}
	pemBody := strings.Split(stored.PrivateKey, "\n")[1]
	if strings.Contains(body, pemBody) || strings.Contains(body, htmlText(pemBody)) {
		t.Error("the private key is on the page")
	}
	// Until the key is installed the server refuses it, with a hint.
	if body = test(url.Values{"path": {"/var/www/uploads"}}); !strings.Contains(body, htmlText("authorized_keys milik user root")) {
		t.Errorf("key not yet installed: %s", firstLines(body))
	}
	p.knob(t, "sftp-accept-keys", "1")
	if body = test(url.Values{"path": {"/var/www/uploads"}}); !strings.Contains(body, "bisa login") {
		t.Errorf("key installed: %s", firstLines(body))
	}
	// Saving again without touching the key keeps it.
	before := stored.PrivateKey
	p.post(t, "/sync/remotes/update", url.Values{"name": {"vps-lama"}, "type": {"sftp"}, "host": {"127.0.0.1"}, "port": {port}, "user": {"root"}, "auth": {"key"}})
	if stored, _ = p.app.remotes.Get("vps-lama"); stored.PrivateKey != before {
		t.Error("an edit without a new key replaced the key")
	}

	// The VPS was reinstalled: the new host key is refused until the
	// operator asks for it to be fetched again.
	ed2 := testEd25519Blob(t)
	ssh2 := newFakeSSHServer(t, ed2)
	key2, _ := hostKeyFromBlob(ed2)
	p.knob(t, "sftp-hostkey", key2)
	if body = test(url.Values{"path": {"/var/www/uploads"}}); !strings.Contains(body, "TIDAK SAMA") {
		t.Errorf("changed host key: %s", firstLines(body))
	}
	port2 := strconv.Itoa(ssh2.port())
	resp = p.post(t, "/sync/remotes/update", url.Values{"name": {"vps-lama"}, "type": {"sftp"}, "host": {"127.0.0.1"}, "port": {port2}, "user": {"root"}, "auth": {"key"}, "refresh_hostkey": {"1"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, htmlText(hostKeyFingerprint(key2))) {
		t.Errorf("re-pin: %s", firstLines(body))
	}
	if raw, _ := os.ReadFile(kh); !strings.Contains(string(raw), "[*]:"+port2+" "+key2) || strings.Contains(string(raw), edKey) {
		t.Errorf("known_hosts after re-pin:\n%s", raw)
	}
	// Pasted host keys (from /etc/ssh/ssh_host_*_key.pub) are taken as is.
	resp = p.post(t, "/sync/remotes/update", url.Values{"name": {"vps-lama"}, "type": {"sftp"}, "host": {"127.0.0.1"}, "port": {port2}, "user": {"root"}, "auth": {"key"},
		"host_keys": {edKey + " root@vps\n"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "Host key yang ditempel disematkan") {
		t.Errorf("pasted host key: %s", firstLines(body))
	}
	if stored, _ = p.app.remotes.Get("vps-lama"); len(stored.HostKeys) != 1 || stored.HostKeys[0] != edKey {
		t.Errorf("pasted host key not pinned: %v", stored.HostKeys)
	}
	resp = p.post(t, "/sync/remotes/update", url.Values{"name": {"vps-lama"}, "type": {"sftp"}, "host": {"127.0.0.1"}, "port": {port2}, "user": {"root"}, "auth": {"key"},
		"host_keys": {"ssh-dss AAAA"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "remote tidak diubah") {
		t.Errorf("bad host key accepted: %s", firstLines(body))
	}

	// Nothing secret in the log, the job files or the pages.
	if strings.Contains(logs.String(), testSFTPPassword) || strings.Contains(logs.String(), pemBody) {
		t.Error("a secret reached the log")
	}
	entries, _ := os.ReadDir(filepath.Join(p.app.cfg.StateDir, "jobs"))
	for _, e := range entries {
		if raw, err := os.ReadFile(filepath.Join(p.app.cfg.StateDir, "jobs", e.Name())); err == nil && strings.Contains(string(raw), testSFTPPassword) {
			t.Errorf("job file %s holds the password", e.Name())
		}
	}

	// Deleting the remote removes its known_hosts file. (The schedule above
	// uses it, so the schedule goes first.)
	for _, s := range p.app.schedules.List() {
		p.post(t, "/sync/schedules/delete", url.Values{"id": {s.ID}})
	}
	resp = p.post(t, "/sync/remotes/delete", url.Values{"name": {"vps-lama"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "dihapus") {
		t.Errorf("delete: %s", firstLines(body))
	}
	if _, err := os.Stat(kh); !os.IsNotExist(err) {
		t.Errorf("known_hosts file left: %v", err)
	}
}

func TestSFTPRemotePinsTheHostKeyOnFirstSuccessfulTest(t *testing.T) {
	p := newTestPanel(t)
	// Saved while the server was unreachable: stored, but not usable yet.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()

	resp := p.post(t, "/sync/remotes/add", url.Values{
		"type": {"sftp"}, "name": {"vps"}, "host": {"127.0.0.1"}, "port": {port},
		"user": {"deploy"}, "auth": {"password"}, "password": {"pw"},
	})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, "disimpan") || !strings.Contains(body, "Host key server belum bisa diambil") {
		t.Fatalf("add while down: %s", firstLines(body))
	}
	_, body = p.get(t, "/sync")
	if !strings.Contains(body, "host key belum disematkan") {
		t.Error("the card must say the server is not verified yet")
	}
	// A job cannot use it yet.
	resp = p.post(t, "/sync/jobs/create", url.Values{
		"direction": {"import"}, "remote": {"vps"}, "remote_path": {"/srv/data"},
		"garage_bucket": {"media"}, "mode": {"skip-existing"}, "transfers": {"1"},
	})
	if body = p.followFlash(t, resp); !strings.Contains(body, "belum disematkan") {
		t.Errorf("job on an unpinned remote: %s", firstLines(body))
	}
	// Tes koneksi while still down says so.
	resp = p.post(t, "/sync/remotes/test", url.Values{"name": {"vps"}, "path": {"/srv/data"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "tidak bisa menghubungi") {
		t.Errorf("test while down: %s", firstLines(body))
	}

	// The server comes up; the first Tes koneksi pins what it presents.
	ed := testEd25519Blob(t)
	newFakeSSHServerAt(t, addr, ed)
	key, _ := hostKeyFromBlob(ed)
	p.knob(t, "sftp-password", "pw")
	if err := os.MkdirAll(filepath.Join(p.rcDir, "sftp", "127.0.0.1", "srv", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	resp = p.post(t, "/sync/remotes/test", url.Values{"name": {"vps"}, "path": {"/srv/data"}})
	body = p.followFlash(t, resp)
	if !strings.Contains(body, "bisa login") || !strings.Contains(body, htmlText("Host key server disematkan: ED25519 "+hostKeyFingerprint(key))) {
		t.Errorf("first test: %s", firstLines(body))
	}
	if stored, _ := p.app.remotes.Get("vps"); len(stored.HostKeys) != 1 || stored.HostKeys[0] != key || stored.HostKeysAt.IsZero() {
		t.Errorf("not pinned: %+v", stored.HostKeys)
	}
	// From now on that key is required.
	p.knob(t, "sftp-hostkey", testHostKey(t))
	resp = p.post(t, "/sync/remotes/test", url.Values{"name": {"vps"}, "path": {"/srv/data"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "TIDAK SAMA") {
		t.Errorf("impostor accepted: %s", firstLines(body))
	}
}

func TestSFTPEditReusesThePasswordOnlyForTheSameLogin(t *testing.T) {
	p := newTestPanel(t)
	key := testHostKey(t)
	form := func(port, user, password string) url.Values {
		return url.Values{"type": {"sftp"}, "name": {"vps"}, "host": {"127.0.0.1"}, "port": {port}, "user": {user},
			"auth": {"password"}, "password": {password}, "host_keys": {key}}
	}
	if body := p.followFlash(t, p.post(t, "/sync/remotes/add", form("22", "root", "rahasia-lama"))); !strings.Contains(body, "disimpan") {
		t.Fatalf("add: %s", firstLines(body))
	}
	// Same login, blank password: kept.
	if body := p.followFlash(t, p.post(t, "/sync/remotes/update", form("22", "root", ""))); !strings.Contains(body, "diperbarui") {
		t.Errorf("same login: %s", firstLines(body))
	}
	if r, _ := p.app.remotes.Get("vps"); r.Password != "rahasia-lama" {
		t.Errorf("password = %q", r.Password)
	}
	// Another port or user with a blank password: refused, nothing changes,
	// so the stored password never goes to a server it was not typed for.
	for _, f := range []url.Values{form("2222", "root", ""), form("22", "deploy", "")} {
		body := p.followFlash(t, p.post(t, "/sync/remotes/update", f))
		if !strings.Contains(body, "remote tidak diubah") || !strings.Contains(body, "isi password-nya lagi") {
			t.Errorf("changed login kept the password: %s", firstLines(body))
		}
	}
	if r, _ := p.app.remotes.Get("vps"); r.Port != 22 || r.User != "root" || r.Password != "rahasia-lama" {
		t.Errorf("refused edit changed the remote: %d %s %q", r.Port, r.User, r.Password)
	}
	// Typed again: fine.
	if body := p.followFlash(t, p.post(t, "/sync/remotes/update", form("2222", "deploy", "rahasia-baru"))); !strings.Contains(body, "diperbarui") {
		t.Errorf("changed login with password: %s", firstLines(body))
	}
	if r, _ := p.app.remotes.Get("vps"); r.Port != 2222 || r.User != "deploy" || r.Password != "rahasia-baru" {
		t.Errorf("after edit: %d %s %q", r.Port, r.User, r.Password)
	}
}

func TestSFTPRemoteFormRefusesBadInput(t *testing.T) {
	p := newTestPanel(t)
	base := url.Values{"type": {"sftp"}, "name": {"vps"}, "host": {"127.0.0.1"}, "port": {"22"}, "user": {"root"}, "auth": {"password"}, "password": {"pw"},
		"host_keys": {testHostKey(t)}}
	for name, c := range map[string]struct {
		field, value, want string
	}{
		"user in host":       {"host", "root@127.0.0.1", "user@"},
		"url as host":        {"host", "sftp://127.0.0.1", "sftp://"},
		"metadata address":   {"host", "169.254.169.254", "tidak boleh dipakai"},
		"port in host":       {"host", "127.0.0.1:22", "port diisi"},
		"port not a number":  {"port", "ssh", "harus angka"},
		"port out of range":  {"port", "70000", "tidak valid"},
		"option as user":     {"user", "-oProxyCommand=sh", "tidak valid"},
		"no password":        {"password", "", "password harus diisi"},
		"no login method":    {"auth", "agent", "pilih cara login"},
		"encrypted key":      {"auth", "key", "passphrase"},
		"garbage host key":   {"host_keys", "hello world", "tidak dikenali"},
		"name of the garage": {"name", "garage", "dipakai untuk Garage"},
	} {
		form := url.Values{}
		for k, v := range base {
			form[k] = append([]string{}, v...)
		}
		form.Set(c.field, c.value)
		if name == "encrypted key" {
			form.Set("private_key", "-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY-----\n")
		}
		body := p.followFlash(t, p.post(t, "/sync/remotes/add", form))
		if !strings.Contains(body, "remote tidak disimpan") || !strings.Contains(body, htmlText(c.want)) {
			t.Errorf("%s: %s", name, firstLines(body))
		}
	}
	if n := len(p.app.remotes.List()); n != 0 {
		t.Errorf("%d remotes saved from bad input", n)
	}
	// A valid one with a pasted host key needs no connection at all.
	body := p.followFlash(t, p.post(t, "/sync/remotes/add", base))
	if !strings.Contains(body, "Host key yang ditempel disematkan") {
		t.Errorf("pasted host key: %s", firstLines(body))
	}
	// The kind of a remote cannot be switched by the edit form.
	body = p.followFlash(t, p.post(t, "/sync/remotes/update", url.Values{"name": {"vps"}, "type": {"s3"}, "provider": {"Wasabi"},
		"endpoint": {"https://s3.wasabisys.com"}, "region": {"us-east-1"}, "access_key": {"AK"}, "secret_key": {"SK"}}))
	if stored, _ := p.app.remotes.Get("vps"); !stored.IsSFTP() || stored.Endpoint != "" {
		t.Errorf("edit turned the remote into %+v: %s", stored.Type, firstLines(body))
	}
}
