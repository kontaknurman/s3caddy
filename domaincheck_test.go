package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// offlineDomainChecks keeps a test's domain checks off the network: DNS knows
// no domain, and Caddy has no certificate.
func offlineDomainChecks(c *CaddyManager) {
	c.lookupHost = func(_ context.Context, host string) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	c.checkCert = func(context.Context, string) CertStatus {
		return CertStatus{Err: "Caddy belum punya sertifikat untuk domain ini"}
	}
	c.readLog = func(context.Context, time.Time) []string { return nil }
}

// fastDomainChecks shortens the waits of a domain check for one test.
func fastDomainChecks(t *testing.T) {
	t.Helper()
	oldWait, oldPoll, oldCool := certWait, certPoll, retryCooldown
	certWait, certPoll = 2*time.Second, 10*time.Millisecond
	t.Cleanup(func() { certWait, certPoll, retryCooldown = oldWait, oldPoll, oldCool })
}

// testCertificate is a self-signed certificate for names.
func testCertificate(t *testing.T, notAfter time.Time, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: names[0], Organization: []string{"Uji CA"}},
		DNSNames:              names,
		NotBefore:             time.Now().Add(-48 * time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// startTLSOrigin stands in for Caddy's HTTPS port: a name without a
// certificate gets the TLS alert Caddy sends ("internal error").
func startTLSOrigin(t *testing.T, certs map[string]tls.Certificate) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if c, ok := certs[hello.ServerName]; ok {
				return &c, nil
			}
			return nil, fmt.Errorf("no certificate available for '%s'", hello.ServerName)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = conn.(*tls.Conn).Handshake()
				conn.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// startEdge stands in for Cloudflare's edge in front of domain: every
// request for it reaches handler, whatever DNS says.
func startEdge(t *testing.T, domain string, handler http.HandlerFunc) *http.Client {
	t.Helper()
	cert := testCertificate(t, time.Now().Add(90*24*time.Hour), domain)
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	client := newEdgeClient()
	client.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}
	return client
}

// flashOf is the flash message of a page.
func flashOf(body string) string {
	i := strings.Index(body, `<div class="flash`)
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], "</div>")
	if j < 0 {
		return body[i:]
	}
	return body[i : i+j]
}

func TestCloudflareRangesAreTheProxyAddresses(t *testing.T) {
	for ip, want := range map[string]bool{
		"104.16.132.229": true, "172.67.1.1": true, "188.114.97.3": true, "2606:4700::6810:84e5": true,
		"203.0.113.5": false, "127.0.0.1": false, "10.0.0.1": false, "2001:db8::1": false,
	} {
		if got := isCloudflareIP(net.ParseIP(ip)); got != want {
			t.Errorf("isCloudflareIP(%s) = %v", ip, got)
		}
	}
}

// Lines as Caddy 2.11.7 and 2.6.2 log a certificate for a domain without DNS.
const (
	logDomain  = "gagal-ssl.garagepanel-uji-tidak-ada.com"
	log2_11_ok = `{"level":"info","ts":1792229505.62,"logger":"tls.obtain","msg":"obtaining certificate","identifier":"gagal-ssl.garagepanel-uji-tidak-ada.com"}`
	log2_11_ch = `{"level":"error","ts":1792229507.0535495,"logger":"http.acme_client","msg":"challenge failed","identifier":"gagal-ssl.garagepanel-uji-tidak-ada.com","challenge_type":"http-01","problem":{"type":"urn:ietf:params:acme:error:dns","title":"","detail":"DNS problem: NXDOMAIN looking up A for gagal-ssl.garagepanel-uji-tidak-ada.com - check that a DNS record exists for this domain","instance":"","subproblems":null}}`
	log2_11_wr = `{"level":"error","ts":1792229508.717098,"logger":"tls.obtain","msg":"will retry","error":"[gagal-ssl.garagepanel-uji-tidak-ada.com] Obtain: [gagal-ssl.garagepanel-uji-tidak-ada.com] solving challenge: gagal-ssl.garagepanel-uji-tidak-ada.com: [gagal-ssl.garagepanel-uji-tidak-ada.com] authorization failed: HTTP 400 urn:ietf:params:acme:error:dns - DNS problem: NXDOMAIN looking up A for gagal-ssl.garagepanel-uji-tidak-ada.com (ca=https://acme-staging-v02.api.letsencrypt.org/directory)","attempt":1,"retrying_in":60,"elapsed":3.203394508,"max_duration":2592000}`
	log2_6_ch  = `{"level":"error","ts":1792229549.4560866,"logger":"http.acme_client","msg":"challenge failed","identifier":"gagal-ssl.garagepanel-uji-tidak-ada.com","challenge_type":"tls-alpn-01","status_code":400,"problem_type":"urn:ietf:params:acme:error:dns","error":"DNS problem: NXDOMAIN looking up A for gagal-ssl.garagepanel-uji-tidak-ada.com - check that a DNS record exists for this domain"}`
	log2_6_is  = `{"level":"error","ts":1792229552.4278874,"logger":"tls.obtain","msg":"could not get certificate from issuer","identifier":"gagal-ssl.garagepanel-uji-tidak-ada.com","issuer":"acme-staging-v02.api.letsencrypt.org-directory","error":"[gagal-ssl.garagepanel-uji-tidak-ada.com] solving challenges: gagal-ssl.garagepanel-uji-tidak-ada.com: no solvers available for remaining challenges (configured=[http-01 tls-alpn-01] offered=[tls-alpn-01 http-01 dns-01 dns-persist-01] remaining=[dns-01 dns-persist-01])"}`
	log2_6_wr  = `{"level":"error","ts":1792229552.4279475,"logger":"tls.obtain","msg":"will retry","error":"[gagal-ssl.garagepanel-uji-tidak-ada.com] Obtain: [gagal-ssl.garagepanel-uji-tidak-ada.com] solving challenges: gagal-ssl.garagepanel-uji-tidak-ada.com: no solvers available for remaining challenges","attempt":1,"retrying_in":60,"elapsed":4.514736118,"max_duration":2592000}`
)

func TestCaddyCertFailureReadsCaddy26And211Logs(t *testing.T) {
	since := time.Unix(1792229500, 0)
	for name, lines := range map[string][]string{
		"2.11": {log2_11_ok, log2_11_ch, log2_11_wr},
		"2.6":  {log2_6_ch, log2_6_is, log2_6_wr},
	} {
		failed, reason := caddyCertFailure(lines, logDomain, since)
		if !failed || !strings.HasPrefix(reason, "DNS problem: NXDOMAIN") {
			t.Errorf("%s: failed=%v reason=%q", name, failed, reason)
		}
		if h := certReasonHint(reason); !strings.Contains(h, "record A") {
			t.Errorf("%s: hint = %q", name, h)
		}
		// An attempt still running has no "will retry" yet.
		if failed, _ := caddyCertFailure(lines[:len(lines)-1], logDomain, since); failed {
			t.Errorf("%s: failure reported before Caddy gave up", name)
		}
		// Lines from before the reload, or about another domain, do not count.
		if failed, _ := caddyCertFailure(lines, logDomain, time.Unix(1792229600, 0)); failed {
			t.Errorf("%s: an older attempt was counted", name)
		}
		if failed, _ := caddyCertFailure(lines, "cdn.example.com", since); failed {
			t.Errorf("%s: another domain's failure was counted", name)
		}
	}
	// Garbage and console-format lines are skipped.
	if failed, _ := caddyCertFailure([]string{"", "not json", "2026/10/17 ERROR something"}, logDomain, time.Time{}); failed {
		t.Error("non-JSON lines counted")
	}
	for reason, want := range map[string]string{
		"urn:ietf:params:acme:error:rateLimited - too many failed authorizations (5) for \"x\"": "Tunggu sekitar satu jam",
		"Timeout during connect (likely firewall problem)":                                      "firewall",
		"Invalid response from http://x/.well-known/acme-challenge/abc: 404":                    "server lain",
	} {
		if h := certReasonHint(reason); !strings.Contains(h, want) {
			t.Errorf("hint(%q) = %q", reason, h)
		}
	}
}

func TestProbeCertificateAsksCaddyByName(t *testing.T) {
	c := NewCaddyManager(t.TempDir(), "127.0.0.1:3902", "127.0.0.1:3900", reloadOK)
	good := testCertificate(t, time.Now().Add(60*24*time.Hour), "cdn.example.com")
	c.httpsAddr = startTLSOrigin(t, map[string]tls.Certificate{
		"cdn.example.com":   good,
		"lama.example.com":  testCertificate(t, time.Now().Add(-time.Hour), "lama.example.com"),
		"salah.example.com": good,
	})
	ctx := t.Context()
	st := c.probeCertificate(ctx, "cdn.example.com")
	if !st.OK || st.Issuer != "Uji CA" || st.Trusted || !st.NotAfter.Equal(good.Leaf.NotAfter) {
		t.Errorf("good certificate: %+v", st)
	}
	for domain, want := range map[string]string{
		"baru.example.com":  "belum punya sertifikat",
		"lama.example.com":  "kedaluwarsa",
		"salah.example.com": "bukan untuk salah.example.com",
	} {
		if st := c.probeCertificate(ctx, domain); st.OK || !strings.Contains(st.Err, want) {
			t.Errorf("%s: %+v", domain, st)
		}
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	c.httpsAddr = ln.Addr().String()
	ln.Close()
	if st := c.probeCertificate(ctx, "cdn.example.com"); st.OK || !strings.Contains(st.Err, "Caddy tidak jalan") {
		t.Errorf("nothing listening: %+v", st)
	}
}

func TestCheckEdgeTellsFlexibleFromFull(t *testing.T) {
	c := NewCaddyManager(t.TempDir(), "127.0.0.1:3902", "127.0.0.1:3900", reloadOK)
	cases := map[string]struct {
		handler http.HandlerFunc
		loop    bool
		status  int
		ok      bool
		say     string
	}{
		"flexible": {func(w http.ResponseWriter, r *http.Request) {
			// Caddy's HTTP to HTTPS redirect, passed on by Cloudflare.
			http.Redirect(w, r, "https://cdn.example.com/", http.StatusPermanentRedirect)
		}, true, 308, false, "Flexible"},
		"no certificate at the origin": {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(525) }, false, 525, false, "(525)"},
		"origin down":                  {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(521) }, false, 521, false, "(521)"},
		"full":                         {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }, false, 404, true, "sudah benar"},
		"another redirect": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://cdn.example.com/index.html", http.StatusFound)
		}, false, 302, true, "sudah benar"},
	}
	for name, tc := range cases {
		c.edgeClient = startEdge(t, "cdn.example.com", tc.handler)
		e := c.checkEdge(t.Context(), "cdn.example.com")
		line, ok := edgeLine("cdn.example.com", e)
		if e.Loop != tc.loop || e.Status != tc.status || ok != tc.ok || !strings.Contains(line, tc.say) {
			t.Errorf("%s: %+v ok=%v %q", name, e, ok, line)
		}
	}
}

// After a domain is added the panel waits for the certificate and says what
// Cloudflare does with it.
func TestAddingADomainBehindFlexibleCloudflareIsExplained(t *testing.T) {
	fastDomainChecks(t)
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)
	var probes atomic.Int32
	p.app.caddy.lookupHost = func(context.Context, string) ([]string, error) { return []string{"104.16.132.229"}, nil }
	p.app.caddy.checkCert = func(context.Context, string) CertStatus {
		if probes.Add(1) < 3 { // issued after a few seconds
			return CertStatus{Err: "Caddy belum punya sertifikat untuk domain ini"}
		}
		return CertStatus{OK: true, Issuer: "Let's Encrypt", NotAfter: time.Now().Add(89 * 24 * time.Hour), Trusted: true}
	}
	p.app.caddy.edgeClient = startEdge(t, "cdn.example.com", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://cdn.example.com/", http.StatusPermanentRedirect)
	})
	resp := p.post(t, "/domains/create", url.Values{"domain": {"cdn.example.com"}, "bucket": {"media"}})
	body := p.followFlash(t, resp)
	for _, want := range []string{`class="flash warn"`, "lewat proxy Cloudflare", "Caddy punya sertifikat dari Let", "Flexible", "Full (strict)"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in: %s", want, firstLines(body))
		}
	}
	if probes.Load() < 3 {
		t.Errorf("the panel did not wait for the certificate (%d probes)", probes.Load())
	}

	// Once the mode is Full (strict), the same check is all good.
	p.app.caddy.edgeClient = startEdge(t, "cdn.example.com", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	resp = p.post(t, "/domains/check", url.Values{"domain": {"cdn.example.com"}})
	body = p.followFlash(t, resp)
	if !strings.Contains(body, `class="flash ok"`) || !strings.Contains(body, "sudah benar") {
		t.Errorf("full mode: %s", firstLines(body))
	}
}

// Caddy logs a failed attempt: the panel stops waiting and passes the CA's
// reason on, with the next step.
func TestAddingADomainWhoseCertificateFailsSaysWhy(t *testing.T) {
	fastDomainChecks(t)
	certWait = 20 * time.Second // the log, not the timeout, must end the wait
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)
	const domain = "gagal-ssl.garagepanel-uji-tidak-ada.com"
	p.app.caddy.lookupHost = func(context.Context, string) ([]string, error) { return []string{"203.0.113.5"}, nil }
	p.app.caddy.readLog = func(_ context.Context, since time.Time) []string {
		ts := fmt.Sprintf("%.3f", float64(since.UnixNano())/1e9+1)
		return []string{
			strings.Replace(log2_11_ch, "1792229507.0535495", ts, 1),
			strings.Replace(log2_11_wr, "1792229508.717098", ts, 1),
		}
	}
	start := time.Now()
	resp := p.post(t, "/domains/create", url.Values{"domain": {domain}, "bucket": {"media"}})
	body := p.followFlash(t, resp)
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("waited %v; the failed attempt in the log should end the wait", d)
	}
	for _, want := range []string{`class="flash warn"`, "mengarah ke 203.0.113.5", "Caddy belum punya sertifikat", "NXDOMAIN", "Buat record A"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in: %s", want, firstLines(body))
		}
	}
	if _, err := os.Stat(filepath.Join(p.sitesDir, domain+".caddy")); err != nil {
		t.Error("the domain must stay; Caddy keeps trying in the background")
	}
}

// "Periksa HTTPS" asks for a missing certificate again by reloading Caddy,
// at most once a minute per domain.
func TestCheckButtonAsksForAMissingCertificateAgain(t *testing.T) {
	fastDomainChecks(t)
	reloads := filepath.Join(t.TempDir(), "reloads")
	p := newTestPanel(t, func(c *Config) { c.ReloadCmd = []string{"sh", "-c", "echo x >> " + reloads} })
	p.garage.addBucket("media", 0, 0, true)
	if err := p.app.caddy.AddDomain(t.Context(), "cdn.example.com", "media"); err != nil {
		t.Fatal(err)
	}
	reloadCount := func() int {
		b, _ := os.ReadFile(reloads)
		return strings.Count(string(b), "x")
	}
	before := reloadCount()
	p.app.caddy.lookupHost = func(context.Context, string) ([]string, error) { return []string{"203.0.113.5"}, nil }
	p.app.caddy.checkCert = func(context.Context, string) CertStatus {
		if reloadCount() > before { // Caddy got it right after the reload
			return CertStatus{OK: true, Issuer: "Let's Encrypt", NotAfter: time.Now().Add(89 * 24 * time.Hour), Trusted: true}
		}
		return CertStatus{Err: "Caddy belum punya sertifikat untuk domain ini"}
	}
	resp := p.post(t, "/domains/check", url.Values{"domain": {"cdn.example.com"}})
	body := p.followFlash(t, resp)
	if f := flashOf(body); !strings.Contains(f, `class="flash ok"`) || !strings.Contains(f, "diminta ulang") || !strings.Contains(f, "Caddy punya sertifikat") {
		t.Errorf("retry: %s", firstLines(body))
	}
	if n := reloadCount() - before; n != 1 {
		t.Errorf("reloads = %d, want 1", n)
	}
	// With the certificate there, a check does not reload.
	p.post(t, "/domains/check", url.Values{"domain": {"cdn.example.com"}})
	if n := reloadCount() - before; n != 1 {
		t.Errorf("a domain with a certificate was asked for again (%d reloads)", n)
	}

	// Still no certificate: one reload, then a minute of rest.
	p.app.caddy.checkCert = func(context.Context, string) CertStatus {
		return CertStatus{Err: "Caddy belum punya sertifikat untuk domain ini"}
	}
	if err := p.app.caddy.AddDomain(t.Context(), "img.example.com", "media"); err != nil {
		t.Fatal(err)
	}
	before = reloadCount()
	certWait = 50 * time.Millisecond
	p.post(t, "/domains/check", url.Values{"domain": {"img.example.com"}})
	resp = p.post(t, "/domains/check", url.Values{"domain": {"img.example.com"}})
	body = p.followFlash(t, resp)
	if n := reloadCount() - before; n != 1 {
		t.Errorf("reloads = %d, want 1 within the cooldown", n)
	}
	if !strings.Contains(body, `class="flash warn"`) || !strings.Contains(body, "Tunggu satu menit") {
		t.Errorf("cooldown: %s", firstLines(body))
	}
}

func TestCheckButtonDoesNotAskWithoutDNSOrForUnknownDomains(t *testing.T) {
	fastDomainChecks(t)
	reloads := filepath.Join(t.TempDir(), "reloads")
	p := newTestPanel(t, func(c *Config) { c.ReloadCmd = []string{"sh", "-c", "echo x >> " + reloads} })
	p.garage.addBucket("media", 0, 0, true)
	resp := p.post(t, "/domains/create", url.Values{"domain": {"cdn.example.com"}, "bucket": {"media"}})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, `class="flash warn"`) || !strings.Contains(body, "DNS cdn.example.com belum ada") {
		t.Errorf("add without DNS: %s", firstLines(body))
	}
	if !strings.Contains(body, "Periksa HTTPS") || !strings.Contains(body, "Full (strict)") {
		t.Error("the domains page lacks the check button or the Cloudflare note")
	}
	before, _ := os.ReadFile(reloads)
	resp = p.post(t, "/domains/check", url.Values{"domain": {"cdn.example.com"}})
	body = p.followFlash(t, resp)
	if after, _ := os.ReadFile(reloads); len(after) != len(before) {
		t.Error("a domain without DNS was asked for: Let's Encrypt would count a failure")
	}
	if f := flashOf(body); !strings.Contains(f, "belum ada") || strings.Contains(f, "diminta ulang") {
		t.Errorf("check without DNS: %s", flashOf(body))
	}
	resp = p.post(t, "/domains/check", url.Values{"domain": {"lain.example.com"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "tidak terdaftar") {
		t.Errorf("unknown domain: %s", firstLines(body))
	}
}
