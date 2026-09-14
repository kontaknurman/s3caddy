package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// A Remote is another S3 endpoint (Wasabi, AWS, MinIO, a second Garage) the
// panel can sync buckets with. Remotes live in STATE_DIR/remotes.json, mode
// 0600, because they carry a secret key. The panel reads them to list and
// probe buckets itself, and hands them to rclone — through the child's
// environment, never through argv or a file — for the transfers.

const (
	remotesFileName = "remotes.json"
	remotesFileMode = 0o600
)

// Remote is one saved endpoint. The JSON tags are the on-disk format.
type Remote struct {
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	// Addressing is auto (provider default, other style tried on failure),
	// path or virtual. The probe rewrites auto to whatever worked.
	Addressing string    `json:"addressing,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt,omitempty"`
}

// RemoteView is what templates and JSON answers see. It has no secret field,
// so a template cannot print one by accident.
type RemoteView struct {
	Name            string
	Provider        string
	ProviderLabel   string
	Endpoint        string
	Region          string
	AccessKeyMasked string
	Addressing      string // auto | path | virtual, as stored
	AddressingLabel string // the style actually used, in words
	Insecure        bool   // plain http: objects travel unencrypted
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Validate checks every field before the remote is saved or used.
func (r Remote) Validate() error {
	if err := ValidateRemoteName(r.Name); err != nil {
		return err
	}
	if err := ValidateProvider(r.Provider); err != nil {
		return err
	}
	if _, err := ValidateRemoteEndpoint(r.Endpoint); err != nil {
		return err
	}
	if err := ValidateRegion(r.Region); err != nil {
		return err
	}
	if err := ValidateAddressing(r.Addressing); err != nil {
		return err
	}
	if r.AccessKey == "" || r.SecretKey == "" {
		return errors.New("access key dan secret key tidak boleh kosong")
	}
	if strings.ContainsAny(r.AccessKey+r.SecretKey, " \t\r\n\x00") {
		return errors.New("access key / secret key mengandung spasi atau baris baru")
	}
	if len(r.AccessKey) > 256 || len(r.SecretKey) > 256 {
		return errors.New("access key / secret key terlalu panjang")
	}
	return nil
}

// Insecure reports whether objects would travel over plain http.
func (r Remote) Insecure() bool { return strings.HasPrefix(r.Endpoint, "http://") }

// virtualHost resolves the addressing style: an explicit choice, else the
// provider's default.
func (r Remote) virtualHost() bool {
	switch r.Addressing {
	case AddressingPath:
		return false
	case AddressingVirtual:
		return true
	}
	if p, ok := providerByName(r.Provider); ok {
		return p.Virtual
	}
	return false
}

// View strips the secret.
func (r Remote) View() RemoteView {
	label := r.Provider
	if p, ok := providerByName(r.Provider); ok {
		label = p.Label
	}
	addressing := r.Addressing
	if addressing == "" {
		addressing = AddressingAuto
	}
	return RemoteView{
		Name:            r.Name,
		Provider:        r.Provider,
		ProviderLabel:   label,
		Endpoint:        r.Endpoint,
		Region:          r.Region,
		AccessKeyMasked: maskAccessKey(r.AccessKey),
		Addressing:      addressing,
		AddressingLabel: addressingLabel(r.virtualHost()),
		Insecure:        r.Insecure(),
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
}

// maskAccessKey keeps just enough of an access key to recognise it.
func maskAccessKey(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("•", len(s))
	}
	return s[:4] + "…" + s[len(s)-4:]
}

// Client builds the panel's own S3 client for the remote (listing, probing)
// in the remote's addressing style.
func (r Remote) Client() (*S3, error) {
	return r.clientWith(r.virtualHost())
}

// s3DialOverride lets tests route <bucket>.127.0.0.1 to a fake server.
var s3DialOverride = map[string]string{}

func (r Remote) clientWith(virtual bool) (*S3, error) {
	opts := S3Options{Label: r.Name, Garage: r.Name == garageRemoteName, Provider: r.Provider, VirtualHost: virtual}
	if addr, ok := s3DialOverride[r.Endpoint]; ok {
		opts.dialAddr = addr
	}
	c, err := NewS3With(r.Endpoint, r.AccessKey, r.SecretKey, r.Region, opts)
	if err != nil {
		return nil, fmt.Errorf("remote %s: endpoint %w", r.Name, err)
	}
	return c, nil
}

// garageRemoteName is the alias of the panel's own Garage on the rclone side.
const garageRemoteName = "garage"

// garageRemote describes the panel's own Garage as a Remote so both sides of
// a job are handled by the same code.
func garageRemote(cfg *Config) Remote {
	return Remote{
		Name:       garageRemoteName,
		Provider:   "Other",
		Endpoint:   cfg.S3URL,
		Region:     cfg.S3Region,
		AccessKey:  cfg.S3AccessKey,
		SecretKey:  cfg.S3SecretKey,
		Addressing: AddressingPath,
	}
}

// rcloneEnv renders the remote as rclone environment variables under alias
// ("src", "dst" or "garage"), which is how a remote is defined without a
// config file: RCLONE_CONFIG_<ALIAS>_<OPTION>.
func (r Remote) rcloneEnv(alias string) []string {
	prefix := "RCLONE_CONFIG_" + strings.ToUpper(strings.ReplaceAll(alias, "-", "_")) + "_"
	rcloneProvider := r.Provider
	if p, ok := providerByName(r.Provider); ok {
		rcloneProvider = p.Rclone
	}
	pathStyle := "true"
	if r.virtualHost() {
		pathStyle = "false"
	}
	return []string{
		prefix + "TYPE=s3",
		prefix + "PROVIDER=" + rcloneProvider,
		prefix + "ENDPOINT=" + r.Endpoint,
		prefix + "REGION=" + r.Region,
		prefix + "ACCESS_KEY_ID=" + r.AccessKey,
		prefix + "SECRET_ACCESS_KEY=" + r.SecretKey,
		prefix + "ENV_AUTH=false",
		prefix + "FORCE_PATH_STYLE=" + pathStyle,
		// The panel's key may not be allowed to create buckets, and the
		// bucket must already exist anyway.
		prefix + "NO_CHECK_BUCKET=true",
	}
}

// RemoteStore is the on-disk list of remotes.
type RemoteStore struct {
	path    string
	mu      sync.Mutex
	remotes []Remote
}

type remotesFile struct {
	Remotes []Remote `json:"remotes"`
}

// NewRemoteStore loads STATE_DIR/remotes.json. A missing file is an empty
// list; a corrupt one is an error, never silently discarded.
func NewRemoteStore(path string) (*RemoteStore, error) {
	s := &RemoteStore{path: path}
	data, exists, err := readIfExists(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return s, nil
	}
	var doc remotesFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s rusak: %w", path, err)
	}
	seen := map[string]bool{}
	for _, r := range doc.Remotes {
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("%s: remote %q tidak valid: %w", path, r.Name, err)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("%s: remote %q tercantum dua kali", path, r.Name)
		}
		seen[r.Name] = true
	}
	s.remotes = doc.Remotes
	return s, nil
}

// List returns the remotes without secrets, sorted by name.
func (s *RemoteStore) List() []RemoteView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RemoteView, 0, len(s.remotes))
	for _, r := range s.remotes {
		out = append(out, r.View())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns a remote with its secret, for building clients.
func (s *RemoteStore) Get(name string) (Remote, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.remotes {
		if r.Name == name {
			return r, true
		}
	}
	return Remote{}, false
}

// Add validates, normalises and saves a remote. Credentials are trimmed the
// same way secretEnv trims the panel's own: a trailing carriage return from a
// pasted value is the classic cause of "signature does not match".
func (s *RemoteStore) Add(r Remote) error {
	r, err := normaliseRemote(r)
	if err != nil {
		return err
	}
	r.CreatedAt = time.Now().UTC().Truncate(time.Second)

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.remotes {
		if existing.Name == r.Name {
			return fmt.Errorf("remote %q sudah ada; pakai tombol Edit untuk mengubahnya", r.Name)
		}
	}
	updated := append(append([]Remote{}, s.remotes...), r)
	if err := s.saveLocked(updated); err != nil {
		return err
	}
	s.remotes = updated
	return nil
}

// normaliseRemote trims, normalises and validates user input for a remote.
func normaliseRemote(r Remote) (Remote, error) {
	r.Name = strings.TrimSpace(r.Name)
	r.Provider = strings.TrimSpace(r.Provider)
	r.Region = strings.TrimSpace(r.Region)
	r.Addressing = strings.TrimSpace(r.Addressing)
	if r.Addressing == "" {
		r.Addressing = AddressingAuto
	}
	r.AccessKey = strings.Trim(r.AccessKey, " \t\r\n")
	r.SecretKey = strings.Trim(r.SecretKey, " \t\r\n")
	endpoint, err := ValidateRemoteEndpoint(r.Endpoint)
	if err != nil {
		return r, err
	}
	r.Endpoint = endpoint
	if err := r.Validate(); err != nil {
		return r, err
	}
	if r.Name == garageRemoteName {
		return r, fmt.Errorf("nama %q dipakai untuk Garage panel sendiri; pilih nama lain", garageRemoteName)
	}
	return r, nil
}

// Update replaces a remote's settings. Empty key fields keep the stored
// pair, so the form can be submitted without retyping credentials; a changed
// access key requires the secret too, because the two only work together.
// The name cannot change.
func (s *RemoteStore) Update(name string, r Remote) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, existing := range s.remotes {
		if existing.Name == name {
			idx = i
		}
	}
	if idx < 0 {
		return fmt.Errorf("remote %q tidak ada", name)
	}
	old := s.remotes[idx]
	r.Name = name
	r.AccessKey = strings.Trim(r.AccessKey, " \t\r\n")
	r.SecretKey = strings.Trim(r.SecretKey, " \t\r\n")
	if r.AccessKey == "" {
		r.AccessKey = old.AccessKey
	}
	if r.SecretKey == "" {
		if r.AccessKey != old.AccessKey {
			return errors.New("access key diganti, jadi secret key-nya harus diisi juga")
		}
		r.SecretKey = old.SecretKey
	}
	r, err := normaliseRemote(r)
	if err != nil {
		return err
	}
	r.CreatedAt = old.CreatedAt
	r.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	updated := append([]Remote{}, s.remotes...)
	updated[idx] = r
	if err := s.saveLocked(updated); err != nil {
		return err
	}
	s.remotes = updated
	return nil
}

// setAddressing records the addressing style a probe found to work.
func (s *RemoteStore) setAddressing(name, style string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	updated := append([]Remote{}, s.remotes...)
	for i := range updated {
		if updated[i].Name == name {
			if updated[i].Addressing == style {
				return nil
			}
			updated[i].Addressing = style
			if err := s.saveLocked(updated); err != nil {
				return err
			}
			s.remotes = updated
			return nil
		}
	}
	return fmt.Errorf("remote %q tidak ada", name)
}

// Delete removes a remote. Callers check first that no job still uses it.
func (s *RemoteStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []Remote
	found := false
	for _, r := range s.remotes {
		if r.Name == name {
			found = true
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		return fmt.Errorf("remote %q tidak ada", name)
	}
	if err := s.saveLocked(kept); err != nil {
		return err
	}
	s.remotes = kept
	return nil
}

func (s *RemoteStore) saveLocked(remotes []Remote) error {
	if remotes == nil {
		remotes = []Remote{}
	}
	data, err := json.MarshalIndent(remotesFile{Remotes: remotes}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.path, append(data, '\n'), remotesFileMode); err != nil {
		return err
	}
	// The temp file was created with the umask; make sure the final file is
	// private even where the umask is permissive.
	return os.Chmod(s.path, remotesFileMode)
}

// ProbeReport is what a connection test found out.
type ProbeReport struct {
	Virtual  bool   // addressing style that worked
	Switched bool   // the other style than configured had to be used
	WriteOK  bool   // a test object could be written and deleted
	Leftover string // key of a test object that could not be deleted
}

// Probe checks a bucket on the remote: read (list one key), and when write is
// set, write + delete a tiny test object. If the configured addressing style
// fails and the other one works, the remote is switched to it and the report
// says so. The error carries the provider's own message plus the panel's hint.
func (s *RemoteStore) Probe(ctx context.Context, name, bucket string, write bool) (ProbeReport, error) {
	r, ok := s.Get(name)
	if !ok {
		return ProbeReport{}, fmt.Errorf("remote %q tidak ada", name)
	}
	if err := ValidateRemoteBucketName(bucket); err != nil {
		return ProbeReport{}, err
	}
	report, err := probeRemote(ctx, r, bucket, write)
	if err != nil {
		return report, err
	}
	if report.Switched {
		style := AddressingPath
		if report.Virtual {
			style = AddressingVirtual
		}
		if serr := s.setAddressing(name, style); serr != nil {
			return report, serr
		}
	}
	return report, nil
}

// probeRemote is Probe for a Remote value (used for the Garage side too).
// The addressing style is settled first with a listing; the write test then
// uses whatever worked.
func probeRemote(ctx context.Context, r Remote, bucket string, write bool) (ProbeReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	preferred := r.virtualHost()
	styles := []bool{preferred}
	if r.Name != garageRemoteName {
		styles = append(styles, !preferred)
	}
	var firstErr error
	for i, virtual := range styles {
		c, err := r.clientWith(virtual)
		if err != nil {
			return ProbeReport{}, err
		}
		if _, err := c.ListObjectsV2(ctx, bucket, ListOptions{MaxKeys: 1}); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if ctx.Err() != nil {
				break
			}
			continue
		}
		report := ProbeReport{Virtual: virtual, Switched: i > 0}
		if write {
			if err := probeWrite(ctx, c, bucket, &report); err != nil {
				return report, fmt.Errorf("remote %s, bucket %s: %w", r.Name, bucket, err)
			}
		}
		return report, nil
	}
	return ProbeReport{}, fmt.Errorf("remote %s, bucket %s: %w", r.Name, bucket, firstErr)
}

// probeWrite writes and deletes a tiny object.
func probeWrite(ctx context.Context, c *S3, bucket string, report *ProbeReport) error {
	key := ".garagepanel-probe-" + randomHex(4)
	if err := c.PutObject(ctx, bucket, key, []byte("garagepanel probe\n"), "text/plain"); err != nil {
		var s3err *S3Error
		if errors.As(err, &s3err) && s3err.StatusCode == http.StatusForbidden {
			s3err.Hint = c.writeDeniedHint()
		}
		return fmt.Errorf("bisa membaca, tapi menulis objek uji ditolak: %w", err)
	}
	report.WriteOK = true
	if err := c.DeleteObject(ctx, bucket, key); err != nil {
		report.Leftover = key
		return fmt.Errorf("objek uji %s berhasil ditulis tapi tidak bisa dihapus (key tanpa izin hapus?): %w", key, err)
	}
	return nil
}
