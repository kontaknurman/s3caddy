package main

import (
	"context"
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
