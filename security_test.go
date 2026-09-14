package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tes adversarial. Semuanya membuktikan klaim keamanan di README secara
// langsung, bukan lewat pembacaan kode.

// Nama domain dan bucket masuk mentah ke dalam Caddyfile. Kalau salah satunya
// bisa menyelipkan baris baru, penyerang bisa menulis blok site tambahan —
// misalnya mem-proxy domain orang lain, atau membuka direktori sistem.
func TestCaddyfileCannotBeInjectedThroughForms(t *testing.T) {
	payloads := []string{
		"a.com\n}\nevil.com {\n\troot * /etc\n\tfile_server\n}\n#",
		"a.com {\n\trespond \"pwned\"\n}",
		"a.com\r\n}\r\nevil.com {",
		"a.com\t}\tevil.com{",
		"a.com }",
		"a.com\x00evil.com",
		"../../etc/caddy/Caddyfile",
		"a.com/../../evil",
	}

	for _, p := range payloads {
		t.Run("domain", func(t *testing.T) {
			panel := newTestPanel(t)
			panel.garage.addBucket("media", 0, 0, true)
			resp := panel.post(t, "/domains/create", url.Values{"domain": {p}, "bucket": {"media"}})
			panel.followFlash(t, resp)

			entries, err := os.ReadDir(panel.sitesDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				names := []string{}
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("payload %q menghasilkan file: %v", p, names)
			}
		})
	}

	// Bucket dengan payload harus ditolak sebelum menyentuh file apa pun.
	for _, p := range []string{"media\n}\nevil.com {", "media\"", "media }", "../media"} {
		panel := newTestPanel(t)
		panel.garage.addBucket("media", 0, 0, true)
		resp := panel.post(t, "/domains/create", url.Values{"domain": {"ok.example.com"}, "bucket": {p}})
		panel.followFlash(t, resp)
		if _, err := os.Stat(filepath.Join(panel.sitesDir, "ok.example.com.caddy")); !os.IsNotExist(err) {
			t.Errorf("bucket payload %q tetap menulis file", p)
		}
	}
}

// File _s3api.caddy dibangun dari IP dan label. Keduanya harus tidak bisa
// menyelipkan direktif Caddy.
func TestWhitelistCannotBeInjected(t *testing.T) {
	p := newTestPanel(t)

	bad := []struct{ value, label string }{
		{"203.0.113.10\n}\nevil.com {", "x"},
		{"203.0.113.10 respond \"x\"", "x"},
		{"203.0.113.10", "label\n\trespond \"pwned\""},
		{"203.0.113.10", "label|palsu"},
		{"203.0.113.10", "label\"quote"},
		{"0.0.0.0/0 }", "x"},
	}
	for _, tc := range bad {
		resp := p.post(t, "/whitelist/add", url.Values{"value": {tc.value}, "label": {tc.label}})
		p.followFlash(t, resp)
	}
	if _, err := os.Stat(p.app.caddy.WhitelistPath()); !os.IsNotExist(err) {
		data, _ := os.ReadFile(p.app.caddy.WhitelistPath())
		t.Fatalf("payload berhasil membuat file whitelist:\n%s", data)
	}

	// Entri yang sah harus menghasilkan file yang bentuknya persis seperti
	// yang diharapkan — tidak ada baris liar.
	resp := p.post(t, "/whitelist/add", url.Values{"value": {"203.0.113.10"}, "label": {"kantor pusat"}})
	p.followFlash(t, resp)
	data, err := os.ReadFile(p.app.caddy.WhitelistPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "# allow:"),
			strings.HasPrefix(trimmed, "s3.example.com {"),
			trimmed == "@write_denied {",
			trimmed == "not method GET HEAD",
			strings.HasPrefix(trimmed, "not remote_ip "),
			trimmed == "}",
			strings.HasPrefix(trimmed, "respond @write_denied "),
			strings.HasPrefix(trimmed, "reverse_proxy "),
			trimmed == "":
		default:
			t.Errorf("baris tak terduga di whitelist: %q", trimmed)
		}
	}
}

// /preview membangun URL ke web endpoint dari object key. Key tidak boleh bisa
// mengubah tujuan request (SSRF) lewat "?", "#", atau URL absolut.
func TestPreviewKeyCannotRedirectTheUpstreamRequest(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)

	c := newTestS3(t, "http://127.0.0.1:3900")
	for _, key := range []string{
		"a.png?x=1",
		"a.png#frag",
		"http://evil.example.com/a.png",
		"//evil.example.com/a.png",
	} {
		u := *p.app.cfg.webURL
		u.Path = "/" + key
		u.RawQuery = ""
		parsed, err := url.Parse(u.String())
		if err != nil {
			t.Fatalf("%q: %v", key, err)
		}
		if parsed.Host != p.app.cfg.webURL.Host {
			t.Errorf("key %q mengubah host jadi %q", key, parsed.Host)
		}
		if parsed.RawQuery != "" {
			t.Errorf("key %q menyelipkan query %q", key, parsed.RawQuery)
		}
		if parsed.Fragment != "" {
			t.Errorf("key %q menyelipkan fragment %q", key, parsed.Fragment)
		}

		// Hal yang sama untuk jalur S3 bertanda tangan.
		req, err := c.newRequest(context.Background(), http.MethodGet, "media", key, nil, nil, "")
		if err != nil {
			t.Fatalf("%q: %v", key, err)
		}
		if req.URL.Host != "127.0.0.1:3900" {
			t.Errorf("key %q mengubah host S3 jadi %q", key, req.URL.Host)
		}
		if !strings.HasPrefix(req.URL.EscapedPath(), "/media/") {
			t.Errorf("key %q keluar dari prefix bucket: %q", key, req.URL.EscapedPath())
		}
	}
}

// Header Host pada request ke web endpoint diisi nama bucket. Nama bucket yang
// lolos validasi tidak boleh bisa menyelipkan header lain.
func TestBucketNameCannotInjectHeaders(t *testing.T) {
	for _, bad := range []string{
		"media\r\nX-Evil: 1",
		"media\nHost: evil.com",
		"media evil",
		"media:8080",
	} {
		if err := ValidateBucketName(bad); err == nil {
			t.Errorf("ValidateBucketName(%q) = nil, harusnya ditolak", bad)
		}
	}
}

// Isi file yang diunggah tidak menentukan Content-Type — ekstensi dari
// whitelist yang menentukan. Kalau tidak, file HTML bernama .png akan
// tersaji sebagai HTML di domain bucket.
func TestUploadContentTypeComesFromWhitelistNotClient(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)

	var gotType string
	p.app.s3 = newTestS3(t, p.app.s3.endpoint.String())

	res := p.upload(t, "media", "sebenarnya-html.png", []byte("<html><script>alert(1)</script></html>"))
	if !res.OK {
		t.Fatalf("upload gagal: %s", res.Error)
	}
	if !strings.HasSuffix(res.Key, ".png") {
		t.Errorf("key = %q, ekstensi harus dari nama file", res.Key)
	}
	gotType = ContentTypeForKey(res.Key)
	if gotType != "image/png" {
		t.Errorf("content type = %q, want image/png", gotType)
	}
}

// Direktori sites tidak boleh bisa ditembus lewat nama domain.
func TestSitesDirectoryIsNotEscapable(t *testing.T) {
	c := newTestCaddy(t, reloadOK)
	for _, bad := range []string{
		"../evil.com",
		"..%2Fevil.com",
		"sub/evil.com",
		`sub\evil.com`,
		"....//evil.com",
		".evil.com",
		"-evil.com",
	} {
		if _, err := c.sitePath(bad); err == nil {
			t.Errorf("sitePath(%q) diterima, harusnya ditolak", bad)
		}
	}

	// Yang sah harus tetap mendarat persis di dalam direktori itu.
	got, err := c.sitePath("cdn.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(got) != filepath.Clean(c.dir) {
		t.Errorf("path %q keluar dari %q", got, c.dir)
	}
}

// Rahasia tidak boleh bocor ke halaman mana pun selain sekali saat bucket
// dibuat.
func TestSecretsDoNotLeakIntoPages(t *testing.T) {
	p := newTestPanel(t, withLogin(t))
	p.garage.addBucket("media", 1, 10, true)
	p.login(t, "admin", testPassword)

	secrets := []string{
		p.app.cfg.AdminToken,
		p.app.cfg.S3SecretKey,
		p.app.cfg.PasswordHash,
		testPassword,
	}
	for _, path := range []string{"/buckets", "/domains", "/objects?bucket=media", "/whitelist"} {
		_, body := p.get(t, path)
		for _, s := range secrets {
			if s == "" {
				continue
			}
			if strings.Contains(body, s) {
				t.Errorf("%s membocorkan rahasia", path)
			}
		}
	}
}

// Session tidak boleh diterima kalau sudah dicabut, dan cookie asal-asalan
// tidak boleh lolos.
func TestForgedSessionCookieIsRejected(t *testing.T) {
	p := newTestPanel(t, withLogin(t))

	u, _ := url.Parse(p.srv.URL)
	for _, forged := range []string{
		strings.Repeat("a", 64),
		"",
		"../../etc/passwd",
	} {
		p.client.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookieName, Value: forged}})
		resp, _ := p.get(t, "/buckets")
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("cookie palsu %q diterima (status %d)", forged, resp.StatusCode)
		}
	}
}

// --- sync ------------------------------------------------------------------

// A remote's secret must never be readable back through the panel: not on any
// page, not in the polling JSON, not in the failed-key view, not in the job
// file, and not in the journal — even when the provider echoes the request's
// Authorization header into an error body.
func TestRemoteSecretNeverLeaksThroughSyncPagesFilesOrLog(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("arsip", 0, 0, false)
	p.addTestRemote(t)
	p.remote.put("backup", "in/a.txt", []byte("x"), "text/plain")

	logs := &safeLog{}
	log.SetOutput(logs)
	defer log.SetOutput(os.Stderr)

	// Start a job normally, then make the remote refuse everything with a
	// body that repeats a fake credential line.
	resp := p.post(t, "/sync/jobs/create", url.Values{
		"direction": {"import"}, "remote": {"wasabi"}, "remote_bucket": {"backup"}, "remote_prefix": {"in/"},
		"garage_bucket": {"arsip"}, "mode": {"overwrite"}, "transfers": {"1"},
	})
	p.followFlash(t, resp)
	jobs := p.waitJobs(t, func(js []jobView) bool { return len(js) == 1 && !js[0].Active })
	id := jobs[0].ID

	p.remote.deny = `<Error><Code>AccessDenied</Code><Message>denied for Credential=WASABIKEY/… secret ` + testRemoteSecret + `</Message></Error>`
	resp = p.post(t, "/sync/remotes/test", url.Values{"name": {"wasabi"}, "bucket": {"backup"}})
	flash := p.followFlash(t, resp)
	if !strings.Contains(flash, "AccessDenied") {
		t.Fatalf("provider error should be shown: %s", firstLines(flash))
	}

	_, syncPage := p.get(t, "/sync")
	_, failedPage := p.get(t, "/sync/jobs/failed?id="+id)
	jobFile, _ := os.ReadFile(filepath.Join(p.app.cfg.StateDir, "jobs", id+".json"))
	req, _ := http.NewRequest(http.MethodGet, p.srv.URL+"/sync/jobs.json", nil)
	req.Header.Set("Accept", "application/json")
	jr, err := p.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	jsonBody, _ := io.ReadAll(jr.Body)
	jr.Body.Close()

	p.app.jobs.Shutdown(t.Context())
	for name, blob := range map[string]string{
		"halaman /sync": syncPage,
		"halaman gagal": failedPage,
		"file job":      string(jobFile),
		"jobs.json":     string(jsonBody),
		"log":           logs.String(),
		"flash tes":     flash,
	} {
		if strings.Contains(blob, testRemoteSecret) {
			t.Errorf("%s memuat secret remote", name)
		}
		if strings.Contains(blob, testGarageSecret) {
			t.Errorf("%s memuat secret Garage", name)
		}
	}
	// The flash carries the provider's message — which here echoed the secret
	// — so the panel must have redacted it.
	if strings.Contains(flash, testRemoteSecret) {
		t.Error("pesan error penyedia yang menggemakan secret harus disamarkan")
	}
}

// The rclone child gets exactly the two remotes of its job and nothing from
// the panel's own environment.
func TestRcloneChildDoesNotInheritPanelSecrets(t *testing.T) {
	t.Setenv("GARAGE_ADMIN_TOKEN", "admin-token-must-stay-home")
	t.Setenv("PANEL_PASSWORD_HASH", "hash-must-stay-home")
	p := newTestPanel(t)
	p.garage.addBucket("arsip", 0, 0, false)
	p.addTestRemote(t)
	p.remote.put("backup", "a.txt", []byte("x"), "text/plain")
	p.post(t, "/sync/jobs/create", url.Values{
		"direction": {"import"}, "remote": {"wasabi"}, "remote_bucket": {"backup"},
		"garage_bucket": {"arsip"}, "mode": {"overwrite"}, "transfers": {"1"},
	})
	p.waitJobs(t, func(js []jobView) bool { return len(js) == 1 && !js[0].Active })

	recs := readFakeRcloneRecords(t, p.rcDir)
	if len(recs) != 1 {
		t.Fatalf("rclone runs = %d", len(recs))
	}
	env := strings.Join(recs[0].Env, "\n")
	for _, forbidden := range []string{"admin-token-must-stay-home", "hash-must-stay-home", "GARAGE_ADMIN_TOKEN", "PANEL_PASSWORD_HASH", "GARAGE_S3_SECRET_KEY"} {
		if strings.Contains(env, forbidden) {
			t.Errorf("child environment contains %s", forbidden)
		}
	}
	if !strings.Contains(env, "RCLONE_CONFIG_SRC_SECRET_ACCESS_KEY="+testRemoteSecret) || !strings.Contains(env, "RCLONE_CONFIG_DST_SECRET_ACCESS_KEY="+p.app.cfg.S3SecretKey) {
		t.Error("child must get exactly its two remotes")
	}
	if args := strings.Join(recs[0].Args, " "); strings.Contains(args, testRemoteSecret) || strings.Contains(args, p.app.cfg.S3SecretKey) {
		t.Error("secret in argv")
	}
}

// Extra rclone arguments are handed to exec as-is: no shell ever sees them.
func TestRcloneExtraArgsAreNotInterpretedByAShell(t *testing.T) {
	dir := t.TempDir()
	bin := fakeRcloneBin(t, dir)
	marker := filepath.Join(dir, "pwned")
	extra := []string{"--bwlimit", "1M;", "$(touch " + marker + ")", "`touch " + marker + "`", "a b"}
	r, err := detectRclone(bin, "", extra)
	if err != nil {
		t.Fatal(err)
	}
	chunk := filepath.Join(dir, "chunk.txt")
	if err := os.WriteFile(chunk, []byte("a.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.run(t.Context(), rcloneRun{Verb: "copy", ChunkPath: chunk, Src: "src:b/", Dst: "dst:c/"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("an extra argument was interpreted by a shell")
	}
	recs := readFakeRcloneRecords(t, dir)
	if len(recs) != 1 {
		t.Fatalf("runs = %d", len(recs))
	}
	got := strings.Join(recs[0].Args, "\x00")
	for _, want := range extra {
		if !strings.Contains(got, "\x00"+want+"\x00") {
			t.Errorf("argument %q not passed literally: %q", want, recs[0].Args)
		}
	}
}

// Job ids are the only user input that becomes a path under STATE_DIR.
func TestJobIDCannotEscapeStateDir(t *testing.T) {
	p := newTestPanel(t)
	victim := filepath.Join(p.app.cfg.StateDir, "remotes.json")
	if err := os.WriteFile(victim, []byte(`{"remotes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../remotes", "..%2Fremotes", "../../etc/passwd", "0123456789abcdef/../x", ""} {
		if resp, _ := p.get(t, "/sync/jobs/failed?id="+id); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("failed?id=%q: status %d, want 400", id, resp.StatusCode)
		}
		resp := p.post(t, "/sync/jobs/delete", url.Values{"id": {id}})
		if body := p.followFlash(t, resp); !strings.Contains(body, "tidak valid") {
			t.Errorf("delete id=%q accepted: %s", id, firstLines(body))
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Error("a file next to the jobs directory was touched")
	}
}

// Names typed into the file manager never reach a key as a path escape.
func TestFolderAndRenameNamesCannotTraverse(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)
	p.s3.put("media", "docs/a.pdf", []byte("A"), "application/pdf")

	for _, name := range []string{"..", "../x", "a/../b", `..\x`, "x/y"} {
		resp := p.post(t, "/objects/mkdir", url.Values{"bucket": {"media"}, "prefix": {"docs/"}, "name": {name}})
		if body := p.followFlash(t, resp); strings.Contains(body, "dibuat") {
			t.Errorf("mkdir %q accepted", name)
		}
		resp = p.post(t, "/objects/rename", url.Values{"bucket": {"media"}, "prefix": {"docs/"}, "key": {"docs/a.pdf"}, "name": {name + ".pdf"}})
		if body := p.followFlash(t, resp); strings.Contains(body, "diganti nama") {
			t.Errorf("rename to %q accepted", name)
		}
	}
	for _, dst := range []string{"../", "a/../", "/abs/"} {
		resp := p.post(t, "/objects/move", url.Values{"bucket": {"media"}, "prefix": {"docs/"}, "key": {"docs/a.pdf"}, "dst_prefix": {dst}})
		if body := p.followFlash(t, resp); strings.Contains(body, "dipindah") {
			t.Errorf("move to %q accepted", dst)
		}
	}
	if keys := p.s3.keys("media"); strings.Join(keys, ",") != "docs/a.pdf" {
		t.Errorf("bucket changed: %v", keys)
	}
}
