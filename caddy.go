package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Caddy site-file management.
//
// There is no database. The set of domains is exactly the set of
// <domain>.caddy files in CADDY_SITES_DIR, and the bucket each domain points at
// is read back from the "# bucket: <name>" comment on the first line of the
// file. Files whose name starts with "_" are panel-internal (currently only
// _s3api.caddy) and are never listed as project domains.
//
// Every write is transactional: write, reload, and on a failed reload put the
// previous content back and reload again so the running config is never left
// broken.

const (
	// bucketTagPrefix marks the single source of truth for domain -> bucket.
	bucketTagPrefix = "# bucket:"
	// allowTagPrefix marks one whitelist entry in _s3api.caddy.
	allowTagPrefix = "# allow:"
	// whitelistFileName is deliberately prefixed with "_" so it never collides
	// with a domain file.
	whitelistFileName = "_s3api.caddy"

	siteFileMode = 0o640
)

// CaddyManager owns CADDY_SITES_DIR.
type CaddyManager struct {
	dir       string
	webTarget string   // host:port of the Garage web endpoint (3902)
	s3Target  string   // host:port of the Garage S3 endpoint (3900)
	reloadCmd []string // e.g. ["sudo", "-n", "/bin/systemctl", "reload", "caddy"]

	mu sync.Mutex // serialises every mutation of the sites directory
}

// NewCaddyManager builds the manager.
func NewCaddyManager(dir, webTarget, s3Target string, reloadCmd []string) *CaddyManager {
	return &CaddyManager{
		dir:       dir,
		webTarget: webTarget,
		s3Target:  s3Target,
		reloadCmd: reloadCmd,
	}
}

// ReloadError describes a failed "systemctl reload caddy", including whatever
// Caddy said, so the panel can show it instead of a generic failure.
type ReloadError struct {
	Cmd         string
	Err         error
	Stderr      string
	Stdout      string
	Journal     string
	RolledBack  bool
	RollbackErr error
}

func (e *ReloadError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "reload Caddy gagal (%s)", e.Cmd)
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	for _, part := range []struct{ label, text string }{
		{"stderr", e.Stderr},
		{"stdout", e.Stdout},
		{"journal caddy", e.Journal},
	} {
		if t := strings.TrimSpace(part.text); t != "" {
			fmt.Fprintf(&b, "\n\n%s:\n%s", part.label, t)
		}
	}
	switch {
	case e.RollbackErr != nil:
		fmt.Fprintf(&b, "\n\nPERINGATAN: rollback gagal: %v — periksa %s secara manual.", e.RollbackErr, "direktori sites Caddy")
	case e.RolledBack:
		b.WriteString("\n\nPerubahan sudah dibatalkan (rollback), config Caddy kembali seperti semula.")
	}
	return b.String()
}

// Unwrap exposes the underlying exec error.
func (e *ReloadError) Unwrap() error { return e.Err }

// --- reading --------------------------------------------------------------

// Site is one domain file.
type Site struct {
	Domain  string // derived from the file name
	Bucket  string // from the "# bucket:" tag; empty when the file is not tagged
	File    string // absolute path
	Managed bool   // true when the file carries a "# bucket:" tag
	Problem string // human-readable reason the file is not usable
}

// ListSites reads every *.caddy file in the sites directory, skipping
// panel-internal files, and returns them sorted by domain.
func (c *CaddyManager) ListSites() ([]Site, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("direktori Caddy %s tidak ada — buat dulu dan pastikan bisa ditulis user panel", c.dir)
		}
		return nil, fmt.Errorf("tidak bisa membaca %s: %w", c.dir, err)
	}

	var sites []Site
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".caddy") || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		site := Site{
			Domain: strings.TrimSuffix(name, ".caddy"),
			File:   filepath.Join(c.dir, name),
		}
		if err := ValidateDomain(site.Domain); err != nil {
			site.Problem = "nama file bukan domain yang valid"
			sites = append(sites, site)
			continue
		}
		bucket, err := readBucketTag(site.File)
		switch {
		case err != nil:
			site.Problem = err.Error()
		case bucket == "":
			site.Problem = "file tidak punya baris \"" + bucketTagPrefix + " <bucket>\" — tidak dikelola panel"
		default:
			if err := ValidateBucketName(bucket); err != nil {
				site.Problem = "nama bucket pada tag tidak valid: " + err.Error()
			} else {
				site.Bucket = bucket
				site.Managed = true
			}
		}
		sites = append(sites, site)
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Domain < sites[j].Domain })
	return sites, nil
}

// readBucketTag reads the "# bucket: <name>" tag from the first non-empty line.
func readBucketTag(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("tidak bisa dibaca: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 8192), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			// First real directive reached without a tag.
			return "", nil
		}
		if rest, ok := cutPrefixFold(line, bucketTagPrefix); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("tidak bisa dibaca: %v", err)
	}
	return "", nil
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// DomainsForBucket returns the domains that point at a bucket, sorted.
func DomainsForBucket(sites []Site, bucket string) []string {
	var out []string
	for _, s := range sites {
		if s.Managed && s.Bucket == bucket {
			out = append(out, s.Domain)
		}
	}
	sort.Strings(out)
	return out
}

// DomainCounts maps bucket name -> number of domains pointing at it.
func DomainCounts(sites []Site) map[string]int {
	counts := map[string]int{}
	for _, s := range sites {
		if s.Managed {
			counts[s.Bucket]++
		}
	}
	return counts
}

// FirstDomainForBucket returns the alphabetically first domain of a bucket, or
// "" when the bucket has none. That domain is what the panel uses to build
// public URLs.
func FirstDomainForBucket(sites []Site, bucket string) string {
	domains := DomainsForBucket(sites, bucket)
	if len(domains) == 0 {
		return ""
	}
	return domains[0]
}

// --- writing --------------------------------------------------------------

// sitePath builds the absolute path of a domain file after validating that the
// domain cannot escape the sites directory.
func (c *CaddyManager) sitePath(domain string) (string, error) {
	if err := ValidateDomain(domain); err != nil {
		return "", err
	}
	name := domain + ".caddy"
	full := filepath.Join(c.dir, name)
	// Belt and braces: the joined path must still be a direct child of dir.
	if filepath.Dir(full) != filepath.Clean(c.dir) {
		return "", fmt.Errorf("domain %q menghasilkan path file yang tidak aman", domain)
	}
	return full, nil
}

// renderSite builds the Caddyfile for one domain. Both inputs are validated
// before this point, so no escaping is required (and none would be possible in
// Caddyfile syntax anyway).
func (c *CaddyManager) renderSite(domain, bucket string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s\n", bucketTagPrefix, bucket)
	fmt.Fprintf(&b, "%s {\n", domain)
	fmt.Fprintf(&b, "\treverse_proxy %s {\n", c.webTarget)
	// Garage's web endpoint selects the bucket from the Host header, so the
	// only rewrite needed is the Host. Paths are passed through untouched.
	fmt.Fprintf(&b, "\t\theader_up Host %s\n", bucket)
	b.WriteString("\t}\n")
	b.WriteString("\theader Cache-Control \"public, max-age=31536000, immutable\"\n")
	b.WriteString("\theader X-Content-Type-Options nosniff\n")
	b.WriteString("\tencode gzip zstd\n")
	b.WriteString("}\n")
	return b.Bytes()
}

// AddDomain writes <domain>.caddy and reloads Caddy, rolling back on failure.
func (c *CaddyManager) AddDomain(ctx context.Context, domain, bucket string) error {
	if err := ValidateDomain(domain); err != nil {
		return err
	}
	if err := ValidateBucketName(bucket); err != nil {
		return err
	}
	path, err := c.sitePath(domain)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("file %s sudah ada — hapus domain itu dulu kalau mau mengubah tujuannya", filepath.Base(path))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("tidak bisa memeriksa %s: %w", path, err)
	}

	return c.applyLocked(ctx, path, c.renderSite(domain, bucket))
}

// RemoveDomain deletes <domain>.caddy and reloads Caddy, restoring the file if
// the reload fails.
func (c *CaddyManager) RemoveDomain(ctx context.Context, domain string) error {
	path, err := c.sitePath(domain)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("file %s tidak ada", filepath.Base(path))
	}
	return c.applyLocked(ctx, path, nil)
}

// applyLocked writes newContent to path (nil means "delete"), reloads Caddy and
// restores the previous state when the reload fails. c.mu must be held.
func (c *CaddyManager) applyLocked(ctx context.Context, path string, newContent []byte) error {
	old, existed, err := readIfExists(path)
	if err != nil {
		return err
	}

	if err := writeOrRemove(path, newContent); err != nil {
		return err
	}

	reloadErr := c.reload(ctx)
	if reloadErr == nil {
		return nil
	}

	// Roll back to exactly what was there before and reload again so the
	// running config is never left in the broken state.
	var restore []byte
	if existed {
		restore = old
	}
	if err := writeOrRemove(path, restore); err != nil {
		reloadErr.RollbackErr = err
		return reloadErr
	}
	if err := c.reload(ctx); err != nil {
		reloadErr.RollbackErr = fmt.Errorf("reload setelah rollback juga gagal: %w", err)
		return reloadErr
	}
	reloadErr.RolledBack = true
	return reloadErr
}

func readIfExists(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("tidak bisa membaca %s: %w", path, err)
	}
	return data, true, nil
}

// writeOrRemove writes content atomically, or removes the file when content is
// nil. The temp file is created in the same directory with a leading dot so
// Caddy's "import sites/*.caddy" never picks up a half-written file.
func writeOrRemove(path string, content []byte) error {
	if content == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("tidak bisa menghapus %s: %w", path, err)
		}
		return nil
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*.caddy-part")
	if err != nil {
		return fmt.Errorf("tidak bisa menulis di %s (pastikan user panel punya akses tulis): %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("tidak bisa menulis %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(siteFileMode); err != nil {
		tmp.Close()
		return fmt.Errorf("tidak bisa set permission %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("tidak bisa flush %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("tidak bisa menutup %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("tidak bisa memindahkan file ke %s: %w", path, err)
	}
	return nil
}

// --- reload ---------------------------------------------------------------

// reload runs the configured reload command. It returns a *ReloadError on
// failure, never a bare error, so callers can surface Caddy's own output.
func (c *CaddyManager) reload(ctx context.Context) *ReloadError {
	if len(c.reloadCmd) == 0 {
		return &ReloadError{Cmd: "(kosong)", Err: errors.New("perintah reload tidak dikonfigurasi")}
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.reloadCmd[0], c.reloadCmd[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}

	re := &ReloadError{
		Cmd:    strings.Join(c.reloadCmd, " "),
		Err:    err,
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if strings.Contains(re.Stderr, "a password is required") || strings.Contains(re.Stderr, "no tty present") {
		re.Stderr += "\n(sudoers belum dipasang? lihat README bagian sudoers)"
	}
	// "systemctl reload" only prints a generic pointer to the journal, so pull
	// the actual Caddy error out of the journal when we are allowed to read it.
	re.Journal = readCaddyJournal(ctx)
	return re
}

// readCaddyJournal is a best-effort fetch of the last Caddy log lines. It needs
// no privileges beyond membership of the systemd-journal group; when that is
// not granted it simply returns "".
func readCaddyJournal(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "journalctl", "-u", "caddy.service", "-n", "25", "--no-pager", "--output=cat")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(out))
	if len(text) > 4000 {
		text = text[len(text)-4000:]
	}
	return text
}

// Reload exposes a plain reload (used after the whitelist file is removed).
func (c *CaddyManager) Reload(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.reload(ctx); err != nil {
		return err
	}
	return nil
}

// --- S3 API IP whitelist --------------------------------------------------

// WhitelistEntry is one "# allow: <value> | <label>" line.
type WhitelistEntry struct {
	Value string // IP or CIDR, canonical form
	Label string
}

// Whitelist is the parsed _s3api.caddy file.
type Whitelist struct {
	Exists  bool
	Path    string
	Domain  string // site address found in the file
	Entries []WhitelistEntry
}

// WhitelistPath is the absolute path of _s3api.caddy.
func (c *CaddyManager) WhitelistPath() string {
	return filepath.Join(c.dir, whitelistFileName)
}

// ReadWhitelist parses _s3api.caddy. A missing file is not an error.
func (c *CaddyManager) ReadWhitelist() (*Whitelist, error) {
	path := c.WhitelistPath()
	wl := &Whitelist{Path: path}

	data, existed, err := readIfExists(path)
	if err != nil {
		return nil, err
	}
	if !existed {
		return wl, nil
	}
	wl.Exists = true

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if rest, ok := cutPrefixFold(line, allowTagPrefix); ok {
			value, label, _ := strings.Cut(rest, "|")
			value = strings.TrimSpace(value)
			label = strings.TrimSpace(label)
			// Re-validate on read: the file is the database, and a hand-edited
			// line must not slip through unchecked.
			canonical, err := ValidateIPOrCIDR(value)
			if err != nil {
				continue
			}
			if ValidateLabel(label) != nil {
				label = ""
			}
			wl.Entries = append(wl.Entries, WhitelistEntry{Value: canonical, Label: label})
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if wl.Domain == "" {
			if addr, _, found := strings.Cut(line, "{"); found {
				wl.Domain = strings.TrimSpace(addr)
			}
		}
	}
	return wl, nil
}

// renderWhitelist builds _s3api.caddy. Read requests (GET/HEAD) always pass;
// every other method is refused unless the client IP is on the list.
func (c *CaddyManager) renderWhitelist(domain string, entries []WhitelistEntry) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		if e.Label != "" {
			fmt.Fprintf(&b, "%s %s | %s\n", allowTagPrefix, e.Value, e.Label)
		} else {
			fmt.Fprintf(&b, "%s %s\n", allowTagPrefix, e.Value)
		}
	}
	values := make([]string, 0, len(entries))
	for _, e := range entries {
		values = append(values, e.Value)
	}
	fmt.Fprintf(&b, "%s {\n", domain)
	b.WriteString("\t@write_denied {\n")
	b.WriteString("\t\tnot method GET HEAD\n")
	fmt.Fprintf(&b, "\t\tnot remote_ip %s\n", strings.Join(values, " "))
	b.WriteString("\t}\n")
	b.WriteString("\trespond @write_denied \"Forbidden: IP not whitelisted\" 403\n\n")
	fmt.Fprintf(&b, "\treverse_proxy %s\n", c.s3Target)
	b.WriteString("}\n")
	return b.Bytes()
}

// WriteWhitelist rewrites _s3api.caddy with the given entries and reloads
// Caddy, rolling back on failure. An empty entry list is refused: it would lock
// every writer out of the S3 API.
func (c *CaddyManager) WriteWhitelist(ctx context.Context, domain string, entries []WhitelistEntry) error {
	if err := ValidateDomain(domain); err != nil {
		return fmt.Errorf("S3_API_DOMAIN tidak valid: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("daftar IP tidak boleh kosong: itu akan memblokir semua operasi tulis. Tambah minimal satu IP, atau hapus file _s3api.caddy sekalian")
	}
	seen := map[string]bool{}
	clean := make([]WhitelistEntry, 0, len(entries))
	for _, e := range entries {
		value, err := ValidateIPOrCIDR(e.Value)
		if err != nil {
			return err
		}
		if e.Label != "" {
			if err := ValidateLabel(e.Label); err != nil {
				return err
			}
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		clean = append(clean, WhitelistEntry{Value: value, Label: e.Label})
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.applyLocked(ctx, c.WhitelistPath(), c.renderWhitelist(domain, clean))
}

// DeleteWhitelist removes _s3api.caddy entirely. After this the S3 API is no
// longer exposed through Caddy at all.
func (c *CaddyManager) DeleteWhitelist(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	path := c.WhitelistPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s memang tidak ada", whitelistFileName)
	}
	return c.applyLocked(ctx, path, nil)
}
