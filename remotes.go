package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	Name string `json:"name"`
	// Type is "s3" (also when empty, in files written before SFTP existed) or
	// "sftp".
	Type      string `json:"type,omitempty"`
	Provider  string `json:"provider"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	// Addressing is auto (provider default, other style tried on failure),
	// path or virtual. The probe rewrites auto to whatever worked.
	Addressing string `json:"addressing,omitempty"`

	// SFTP: a server reached over SSH. Exactly one of Password and
	// PrivateKey is set.
	Host       string `json:"host,omitempty"`
	Port       int    `json:"port,omitempty"`
	User       string `json:"user,omitempty"`
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"`
	// PublicKey is the authorized_keys line of a key the panel generated.
	PublicKey string `json:"publicKey,omitempty"`
	// HostKeys are the server's pinned host keys, "type base64" each.
	HostKeys   []string  `json:"hostKeys,omitempty"`
	HostKeysAt time.Time `json:"hostKeysAt,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`

	// knownHostsPath is where RemoteStore wrote HostKeys for rclone.
	knownHostsPath string
}

// IsSFTP reports whether the remote is a folder on an SSH server.
func (r Remote) IsSFTP() bool { return r.Type == RemoteTypeSFTP }

// RemoteView is what templates and JSON answers see. It has no secret field,
// so a template cannot print one by accident.
type RemoteView struct {
	Name            string
	Type            string // s3 | sftp
	IsSFTP          bool
	Provider        string
	ProviderLabel   string
	Endpoint        string
	Region          string
	AccessKeyMasked string
	Addressing      string // auto | path | virtual, as stored
	AddressingLabel string // the style actually used, in words
	Insecure        bool   // plain http: objects travel unencrypted

	// SFTP
	Host          string
	Port          int
	User          string
	UsesKey       bool
	PublicKey     string // authorized_keys line of a generated key
	InstallCmd    string // command that adds PublicKey on the server
	HostKeys      []HostKeyView
	HostKeysAt    time.Time
	HostKeyPinned bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// HostKeyView is one pinned host key, the way ssh-keygen -lf shows it.
type HostKeyView struct {
	Type        string
	Fingerprint string
}

// Validate checks every field before the remote is saved or used.
func (r Remote) Validate() error {
	switch r.Type {
	case RemoteTypeSFTP:
		return r.validateSFTP()
	case "", RemoteTypeS3:
	default:
		return fmt.Errorf("jenis remote %q tidak dikenal", r.Type)
	}
	if r.Host != "" || r.Password != "" || r.PrivateKey != "" {
		return errors.New("remote S3 tidak memakai host/password/kunci SSH")
	}
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
	if r.IsSFTP() {
		v := RemoteView{
			Name:          r.Name,
			Type:          RemoteTypeSFTP,
			IsSFTP:        true,
			ProviderLabel: "VPS lewat SSH (SFTP)",
			Endpoint:      "sftp://" + r.User + "@" + sshHostPort(r.Host, r.Port),
			Host:          r.Host,
			Port:          r.Port,
			User:          r.User,
			UsesKey:       r.PrivateKey != "",
			PublicKey:     r.PublicKey,
			HostKeysAt:    r.HostKeysAt,
			HostKeyPinned: len(r.HostKeys) > 0,
			CreatedAt:     r.CreatedAt,
			UpdatedAt:     r.UpdatedAt,
		}
		if r.PublicKey != "" {
			v.InstallCmd = sftpInstallCommand(r.PublicKey)
		}
		for _, k := range r.HostKeys {
			v.HostKeys = append(v.HostKeys, HostKeyView{Type: hostKeyLabel(k), Fingerprint: hostKeyFingerprint(k)})
		}
		return v
	}
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
		Type:            RemoteTypeS3,
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

// sshHostPort writes host:port the way ssh users read it (port 22 implied).
func sshHostPort(host string, port int) string {
	h := host
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if port == 0 || port == 22 {
		return h
	}
	return h + ":" + strconv.Itoa(port)
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
	if r.IsSFTP() {
		return r.sftpEnv(prefix)
	}
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
	// rclone talks to SFTP remotes for probes; set once rclone was found.
	rclone *rcloneRunner
}

// SetRclone gives the store the rclone it needs to probe SFTP remotes.
func (s *RemoteStore) SetRclone(rc *rcloneRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rclone = rc
}

// knownHostsDir holds one known_hosts file per SFTP remote, next to
// remotes.json.
func (s *RemoteStore) knownHostsDir() string {
	return filepath.Join(filepath.Dir(s.path), knownHostsDirName)
}

func (s *RemoteStore) knownHostsFile(name string) string {
	return filepath.Join(s.knownHostsDir(), name)
}

// writeKnownHosts writes the pinned host keys of an SFTP remote where rclone
// will read them. A remote without pinned keys gets an empty file, which
// makes rclone refuse the server rather than skip the check.
func (s *RemoteStore) writeKnownHosts(r Remote) error {
	if !r.IsSFTP() {
		return nil
	}
	if err := os.MkdirAll(s.knownHostsDir(), 0o700); err != nil {
		return fmt.Errorf("direktori known_hosts: %w", err)
	}
	path := s.knownHostsFile(r.Name)
	if err := writeFileAtomic(path, []byte(knownHostsContent(r.HostKeys, r.Port)), remotesFileMode); err != nil {
		return err
	}
	return os.Chmod(path, remotesFileMode)
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
		// The known_hosts files are derived data: rewrite them from the
		// source of truth on every start.
		if err := s.writeKnownHosts(r); err != nil {
			return nil, err
		}
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
			if r.IsSFTP() {
				r.knownHostsPath = s.knownHostsFile(r.Name)
			}
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
	if err := s.writeKnownHosts(r); err != nil {
		return err
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
	if r.Type == RemoteTypeSFTP {
		r, err := normaliseSFTP(r)
		if err != nil {
			return r, err
		}
		if r.Name == garageRemoteName {
			return r, fmt.Errorf("nama %q dipakai untuk Garage panel sendiri; pilih nama lain", garageRemoteName)
		}
		return r, nil
	}
	r.Type = RemoteTypeS3
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
	if r.IsSFTP() || old.IsSFTP() {
		// The handler resolved "keep the stored password/key" already;
		// the type of a remote cannot change, its jobs depend on it.
		if r.Type != old.Type {
			return errors.New("jenis remote tidak bisa diubah; buat remote baru")
		}
		r, err := normaliseRemote(r)
		if err != nil {
			return err
		}
		r.CreatedAt = old.CreatedAt
		r.UpdatedAt = time.Now().UTC().Truncate(time.Second)
		if err := s.writeKnownHosts(r); err != nil {
			return err
		}
		updated := append([]Remote{}, s.remotes...)
		updated[idx] = r
		if err := s.saveLocked(updated); err != nil {
			return err
		}
		s.remotes = updated
		return nil
	}
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
	if err := os.Remove(s.knownHostsFile(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// setHostKeys pins the host keys a probe fetched.
func (s *RemoteStore) setHostKeys(name string, keys []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	updated := append([]Remote{}, s.remotes...)
	for i := range updated {
		if updated[i].Name == name {
			updated[i].HostKeys = keys
			updated[i].HostKeysAt = time.Now().UTC().Truncate(time.Second)
			if err := s.writeKnownHosts(updated[i]); err != nil {
				return err
			}
			if err := s.saveLocked(updated); err != nil {
				return err
			}
			s.remotes = updated
			return nil
		}
	}
	return fmt.Errorf("remote %q tidak ada", name)
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
	// PinnedKeys are the SFTP host keys this probe fetched and pinned.
	PinnedKeys []string
}

// Probe checks a bucket on the remote: read (list one key), and when write is
// set, write + delete a tiny test object. If the configured addressing style
// fails and the other one works, the remote is switched to it and the report
// says so. The error carries the provider's own message plus the panel's hint.
//
// For an SFTP remote, bucket is the folder ("/var/www/uploads"), and a remote
// without pinned host keys gets them now (trust on first use).
func (s *RemoteStore) Probe(ctx context.Context, name, bucket string, write bool) (ProbeReport, error) {
	r, ok := s.Get(name)
	if !ok {
		return ProbeReport{}, fmt.Errorf("remote %q tidak ada", name)
	}
	if r.IsSFTP() {
		folder, err := NormalizeSFTPFolder(bucket)
		if err != nil {
			return ProbeReport{}, err
		}
		var pinned []string
		if len(r.HostKeys) == 0 {
			keys, err := fetchHostKeys(ctx, r.Host, r.Port)
			if err != nil {
				return ProbeReport{}, fmt.Errorf("remote %s: %w", r.Name, err)
			}
			if err := s.setHostKeys(name, keys); err != nil {
				return ProbeReport{}, err
			}
			r, _ = s.Get(name)
			pinned = keys
		}
		s.mu.Lock()
		rc := s.rclone
		s.mu.Unlock()
		report, err := probeSFTP(ctx, rc, r, folder, write)
		report.PinnedKeys = pinned
		return report, err
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
