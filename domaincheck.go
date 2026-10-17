package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Checking a domain's HTTPS from the panel.
//
// When a new domain "does not work", Caddy has not died: it keeps serving the
// other sites and keeps asking for the missing certificate in the background,
// with growing pauses. What visitors see comes from one of three places, and
// this check tells them apart:
//
//   - DNS that does not point here (yet): no certificate can be issued.
//   - Caddy without a certificate for the domain yet. Issuance takes seconds
//     when DNS and port 80 are right; Caddy logs why when they are not.
//   - A Cloudflare proxy in SSL/TLS mode Flexible. Cloudflare then talks
//     plain HTTP to Caddy, Caddy redirects HTTP to HTTPS, and the visitor
//     goes round in circles (ERR_TOO_MANY_REDIRECTS). The mode has to be
//     Full (strict).

var (
	// certWait is how long a check waits for Caddy's certificate after a
	// reload. Variables so tests can shorten them.
	certWait = 45 * time.Second
	certPoll = 2 * time.Second
	// retryCooldown spaces out the reloads that ask for a certificate now:
	// Let's Encrypt allows five failed validations per domain an hour.
	retryCooldown = time.Minute
)

// cloudflareRanges are the addresses Cloudflare's proxy answers from, as
// published at https://www.cloudflare.com/ips/.
var cloudflareRanges = parseCIDRs(
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
)

func parseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err) // the list is a constant
		}
		out = append(out, n)
	}
	return out
}

func isCloudflareIP(ip net.IP) bool {
	for _, n := range cloudflareRanges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// CertStatus is the certificate Caddy serves for a domain.
type CertStatus struct {
	OK       bool
	Issuer   string
	NotAfter time.Time
	// Trusted: the chain verifies against the system's CAs, which browsers
	// and Cloudflare's Full (strict) mode need.
	Trusted bool
	Err     string // why there is none
}

// probeCertificate asks Caddy's HTTPS port for the domain the way a browser
// does (SNI), and checks that what comes back is a current certificate for
// that name. Only Caddy on this machine is asked: the address in DNS may be
// another server, or Cloudflare, with a certificate of its own.
func (c *CaddyManager) probeCertificate(ctx context.Context, domain string) CertStatus {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// The chain is checked by hand below: a certificate that is there but
	// not publicly trusted is a different answer from none at all.
	d := tls.Dialer{Config: &tls.Config{ServerName: domain, InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", c.httpsAddr)
	if err != nil {
		return CertStatus{Err: tlsProbeError(err)}
	}
	defer conn.Close()
	state := conn.(*tls.Conn).ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return CertStatus{Err: "Caddy tidak mengirim sertifikat"}
	}
	leaf := state.PeerCertificates[0]
	if err := leaf.VerifyHostname(domain); err != nil {
		return CertStatus{Err: "sertifikat yang dikirim Caddy bukan untuk " + domain}
	}
	if time.Now().After(leaf.NotAfter) {
		return CertStatus{Err: "sertifikat " + domain + " kedaluwarsa sejak " + leaf.NotAfter.Format("2006-01-02")}
	}
	st := CertStatus{OK: true, Issuer: certIssuer(leaf), NotAfter: leaf.NotAfter}
	pool := x509.NewCertPool()
	for _, ic := range state.PeerCertificates[1:] {
		pool.AddCert(ic)
	}
	_, verr := leaf.Verify(x509.VerifyOptions{DNSName: domain, Intermediates: pool})
	st.Trusted = verr == nil
	return st
}

func certIssuer(c *x509.Certificate) string {
	if len(c.Issuer.Organization) > 0 {
		return c.Issuer.Organization[0]
	}
	return c.Issuer.CommonName
}

// tlsProbeError puts a failed handshake with Caddy into words. Caddy answers
// a name it has no certificate for with the TLS alert "internal error".
func tlsProbeError(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return "tidak ada yang mendengarkan di port HTTPS server ini (Caddy tidak jalan?)"
	case strings.Contains(s, "internal error"), strings.Contains(s, "unrecognized name"):
		return "Caddy belum punya sertifikat untuk domain ini"
	}
	return firstLine(s)
}

// readCaddyLogSince returns Caddy's journal lines logged since the time. Like
// readCaddyJournal it needs the systemd-journal group and returns nothing
// without it.
func readCaddyLogSince(ctx context.Context, since time.Time) []string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "journalctl", "-u", "caddy.service",
		"--since", "@"+strconv.FormatInt(since.Unix(), 10), "--no-pager", "--output=cat")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) > 2000 {
		lines = lines[len(lines)-2000:]
	}
	return lines
}

// caddyCertFailure looks through Caddy's JSON log lines for a failed attempt
// at the domain's certificate since the time. Caddy logs the challenge that
// failed with the CA's reason, then "will retry". Caddy 2.6 puts the reason in
// "error", 2.7 and later in "problem.detail".
func caddyCertFailure(lines []string, domain string, since time.Time) (failed bool, reason string) {
	from := float64(since.UnixNano()) / 1e9
	var detail, fallback string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			TS         json.RawMessage `json:"ts"`
			Msg        string          `json:"msg"`
			Identifier string          `json:"identifier"`
			Error      string          `json:"error"`
			Problem    struct {
				Detail string `json:"detail"`
			} `json:"problem"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		// A log with its own time format keeps every line; journalctl
		// --since has already cut at the second.
		if ts, err := strconv.ParseFloat(string(ev.TS), 64); err == nil && ts < from {
			continue
		}
		if ev.Identifier != domain && !strings.Contains(ev.Error, "["+domain+"]") {
			continue
		}
		switch ev.Msg {
		case "challenge failed", "validating authorization":
			if detail == "" {
				detail = ev.Problem.Detail
				if detail == "" {
					detail = ev.Error
				}
			}
		case "could not get certificate from issuer":
			if fallback == "" {
				fallback = ev.Error
			}
		case "will retry":
			failed = true
		}
	}
	if !failed {
		return false, ""
	}
	if detail == "" {
		detail = fallback
	}
	return true, detail
}

// certReasonHint turns the usual reasons a CA gives into a next step.
func certReasonHint(reason string) string {
	low := strings.ToLower(reason)
	switch {
	case strings.Contains(low, "nxdomain"), strings.Contains(low, "no valid a records"):
		return "Record DNS domain ini belum ada atau belum menyebar. Buat record A ke IP server ini."
	case strings.Contains(low, "ratelimited"), strings.Contains(low, "too many"):
		return "Kena batas Let's Encrypt. Tunggu sekitar satu jam sebelum mencoba lagi."
	case strings.Contains(low, "timeout"), strings.Contains(low, "firewall"):
		return "Let's Encrypt tidak bisa menjangkau port 80 server ini. Buka port 80 dan 443 di firewall."
	case strings.Contains(low, "connection refused"):
		return "Port 80 server ini menolak koneksi. Pastikan Caddy mendengarkan di port 80."
	case strings.Contains(low, "unauthorized"), strings.Contains(low, "invalid response"):
		return "Let's Encrypt sampai ke server lain, bukan Caddy ini. Periksa record A dan AAAA domain itu, termasuk AAAA lama."
	}
	return ""
}

// EdgeStatus is what https://<domain>/ answers through Cloudflare.
type EdgeStatus struct {
	Status   int
	Location string
	// Loop: redirected to the very URL asked, which is what Caddy's HTTP to
	// HTTPS redirect looks like through SSL/TLS mode Flexible.
	Loop bool
	Err  string
}

// newEdgeClient asks a URL once, without following redirects.
func newEdgeClient() *http.Client {
	return &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// checkEdge requests https://<domain>/ through whatever DNS points at.
func (c *CaddyManager) checkEdge(ctx context.Context, domain string) EdgeStatus {
	target := "https://" + domain + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return EdgeStatus{Err: err.Error()}
	}
	req.Header.Set("User-Agent", "garagepanel-domain-check")
	resp, err := c.edgeClient.Do(req)
	if err != nil {
		return EdgeStatus{Err: firstLine(err.Error())}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	st := EdgeStatus{Status: resp.StatusCode, Location: resp.Header.Get("Location")}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && st.Location != "" {
		if u, err := req.URL.Parse(st.Location); err == nil && u.String() == target {
			st.Loop = true
		}
	}
	return st
}

// DomainReport is what the panel found out about one domain.
type DomainReport struct {
	Domain     string
	Addrs      []string
	NoDNS      bool   // DNS has no A or AAAA record
	DNSErr     string // the lookup failed otherwise
	Cloudflare bool   // DNS points at Cloudflare's proxy
	Cert       CertStatus
	CertReason string // the CA's reason, when Caddy's journal is readable
	Edge       *EdgeStatus
	Retried    bool // Caddy was reloaded to ask for the certificate now
	Cooldown   bool // it was asked for less than retryCooldown ago
}

// CheckDomain looks the domain up and waits up to wait for Caddy's
// certificate, giving up early when Caddy logs a failed attempt since the
// time. With retry, a missing certificate is asked for now (see
// RetryCertificate). A domain behind Cloudflare is also asked for through
// Cloudflare, where SSL/TLS mode Flexible shows as a redirect loop.
func (c *CaddyManager) CheckDomain(ctx context.Context, domain string, since time.Time, wait time.Duration, retry bool) (DomainReport, error) {
	rep := DomainReport{Domain: domain}
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	addrs, err := c.lookupHost(lctx, domain)
	cancel()
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		rep.NoDNS = true
	case err != nil:
		rep.DNSErr = firstLine(err.Error())
	}
	rep.Addrs = addrs
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && c.isCloudflare(ip) {
			rep.Cloudflare = true
		}
	}
	if rep.NoDNS {
		// No certificate can come; asking would only count a failure.
		wait, retry = 0, false
	}
	if retry {
		if st := c.checkCert(ctx, domain); !st.OK {
			since = time.Now()
			retried, err := c.RetryCertificate(ctx, domain)
			if err != nil {
				return rep, err
			}
			rep.Retried, rep.Cooldown = retried, !retried
			if retried && wait < certWait {
				wait = certWait
			}
		}
	}
	rep.Cert, rep.CertReason = c.waitForCertificate(ctx, domain, since, wait)
	if rep.Cloudflare {
		e := c.checkEdge(ctx, domain)
		rep.Edge = &e
	}
	return rep, nil
}

// waitForCertificate polls Caddy for the certificate until it is there, Caddy
// logs a failed attempt, or wait is over.
func (c *CaddyManager) waitForCertificate(ctx context.Context, domain string, since time.Time, wait time.Duration) (CertStatus, string) {
	deadline := time.Now().Add(wait)
	for {
		st := c.checkCert(ctx, domain)
		if st.OK {
			return st, ""
		}
		if failed, why := caddyCertFailure(c.readLog(ctx, since), domain, since); failed {
			return st, why
		}
		if !time.Now().Before(deadline) {
			return st, ""
		}
		select {
		case <-ctx.Done():
			return st, ""
		case <-time.After(certPoll):
		}
	}
}

// RetryCertificate reloads Caddy so it asks for the domain's certificate now
// rather than after its growing pause. It does nothing (and says so) when the
// domain was asked for less than retryCooldown ago.
func (c *CaddyManager) RetryCertificate(ctx context.Context, domain string) (bool, error) {
	c.mu.Lock()
	if t, ok := c.lastRetry[domain]; ok && time.Since(t) < retryCooldown {
		c.mu.Unlock()
		return false, nil
	}
	c.lastRetry[domain] = time.Now()
	c.mu.Unlock()
	if err := c.Reload(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// Lines puts the report into words for the flash message. ok is false when
// something needs the user's attention.
func (r DomainReport) Lines() (ok bool, lines []string) {
	ok = true
	switch {
	case r.NoDNS:
		ok = false
		lines = append(lines, fmt.Sprintf("DNS %s belum ada (tidak ada record A atau AAAA). Arahkan ke IP server ini; sebelum itu sertifikat tidak bisa terbit.", r.Domain))
	case r.DNSErr != "":
		lines = append(lines, fmt.Sprintf("DNS %s tidak bisa diperiksa dari server ini: %s", r.Domain, r.DNSErr))
	case r.Cloudflare:
		lines = append(lines, fmt.Sprintf("DNS %s lewat proxy Cloudflare (%s).", r.Domain, strings.Join(r.Addrs, ", ")))
	default:
		lines = append(lines, fmt.Sprintf("DNS %s mengarah ke %s.", r.Domain, strings.Join(r.Addrs, ", ")))
	}
	switch {
	case r.Retried:
		lines = append(lines, "Sertifikat diminta ulang: Caddy di-reload.")
	case r.Cooldown:
		lines = append(lines, "Sertifikat baru saja diminta ulang. Tunggu satu menit sebelum meminta lagi: Let's Encrypt hanya mengizinkan 5 kegagalan per domain per jam.")
	}
	if r.Cert.OK {
		l := fmt.Sprintf("Caddy punya sertifikat dari %s, berlaku sampai %s.", r.Cert.Issuer, r.Cert.NotAfter.Format("2006-01-02"))
		if !r.Cert.Trusted {
			l += " Sertifikat ini tidak dipercaya browser, jadi mode Full (strict) di Cloudflare akan menolaknya."
		}
		lines = append(lines, l)
	} else if !r.NoDNS {
		ok = false
		l := "Caddy belum punya sertifikat untuk " + r.Domain
		switch {
		case r.CertReason != "":
			l += ". Alasan dari Let's Encrypt: " + truncate(r.CertReason, 300)
			if h := certReasonHint(r.CertReason); h != "" {
				l += "\n" + h
			}
		case r.Cert.Err != "":
			l += ": " + r.Cert.Err + ". Caddy terus mencoba di belakang layar; klik Periksa HTTPS lagi nanti."
		}
		lines = append(lines, l)
	}
	if r.Edge != nil {
		l, good := edgeLine(r.Domain, *r.Edge)
		ok = ok && good
		lines = append(lines, l)
	}
	return ok, lines
}

// edgeLine explains what Cloudflare answered.
func edgeLine(domain string, e EdgeStatus) (string, bool) {
	switch {
	case e.Err != "":
		return fmt.Sprintf("https://%s/ tidak bisa dibuka lewat Cloudflare dari server ini: %s", domain, e.Err), false
	case e.Loop:
		return fmt.Sprintf("Lewat Cloudflare, https://%s/ dialihkan ke dirinya sendiri tanpa akhir: SSL/TLS encryption mode di Cloudflare masih Flexible. "+
			"Cloudflare lalu menghubungi server ini lewat HTTP, dan Caddy selalu mengalihkan HTTP ke HTTPS. "+
			"Ganti ke Full (strict) di dashboard Cloudflare, menu SSL/TLS.", domain), false
	case e.Status == 525:
		return "Cloudflare gagal handshake SSL dengan server ini (525): Caddy belum punya sertifikat untuk domain ini.", false
	case e.Status == 526:
		return "Cloudflare menolak sertifikat server ini (526): mode Full (strict) butuh sertifikat dari CA publik, seperti yang diterbitkan Caddy.", false
	case e.Status == 521:
		return "Cloudflare tidak bisa terhubung ke server ini (521): Caddy tidak jalan, atau port 443 tertutup untuk IP Cloudflare.", false
	case e.Status == 522, e.Status == 523, e.Status == 524:
		return fmt.Sprintf("Cloudflare tidak bisa menjangkau server ini (%d): periksa IP di DNS Cloudflare dan firewall untuk IP Cloudflare.", e.Status), false
	case e.Status >= 500:
		return fmt.Sprintf("Lewat Cloudflare, https://%s/ menjawab %d.", domain, e.Status), false
	}
	return fmt.Sprintf("Lewat Cloudflare, https://%s/ sampai ke server ini (HTTP %d): SSL/TLS mode di Cloudflare sudah benar.", domain, e.Status), true
}
