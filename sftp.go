package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// An SFTP remote is a folder on another server reached over SSH — typically
// the old web server whose images move into Garage, or a VPS that should get
// a copy of a bucket. rclone does the transfers with its sftp backend; the
// panel lists the folder through rclone, sorts the listing on disk and feeds
// it into the same merge-join, chunks and checkpoints as an S3 remote.
//
// Credentials follow the S3 rules: stored in remotes.json (0600), handed to
// rclone through the child's environment only, never shown again. The
// server's host key is pinned on first contact and checked on every
// connection, so a man-in-the-middle cannot collect a password later.

const (
	RemoteTypeS3   = "s3"
	RemoteTypeSFTP = "sftp"

	sftpDefaultPort   = 22
	maxSFTPPassword   = 1024
	maxPrivateKeyLen  = 16 << 10
	knownHostsDirName = "known_hosts"
	sftpProbeTimeout  = 90 * time.Second

	// rclone 1.63 is the oldest that can be told which host key types to
	// accept (host_key_algorithms). Older ones pick ECDSA when they can, so a
	// pinned ed25519 key alone fails as "key mismatch". 1.63 also uploads to
	// a temporary name and renames, so a reader never sees half a file and a
	// symlink in the way is replaced instead of written through.
	rcloneSFTPMinMajor = 1
	rcloneSFTPMinMinor = 63
)

// sftpParentBudget bounds the parent listing of a probe: a parent with
// millions of entries falls back to asking the folder itself. A variable so
// tests can shorten it.
var sftpParentBudget = 20 * time.Second

// sftpSupport says why this rclone cannot serve SFTP remotes, or nil.
func (r *rcloneRunner) sftpSupport() error {
	if r == nil {
		return errors.New("rclone tidak tersedia, jadi remote SFTP tidak bisa dihubungi")
	}
	parts := strings.SplitN(r.version, ".", 3)
	major, _ := strconv.Atoi(parts[0])
	minor := 0
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	if major > rcloneSFTPMinMajor || (major == rcloneSFTPMinMajor && minor >= rcloneSFTPMinMinor) {
		return nil
	}
	return fmt.Errorf("remote VPS (SFTP) butuh rclone v%d.%d atau lebih baru; yang terpasang v%s (paket apt Ubuntu 24.04 masih 1.60). Pasang versi resmi: curl -fsSL https://rclone.org/install.sh | sudo bash, lalu restart panel",
		rcloneSFTPMinMajor, rcloneSFTPMinMinor, r.version)
}

// rcloneObscureKey is rclone's fixed key for obscured config values. It is no
// secret — rclone documents obscuring as a guard against casual glances only
// — but rclone refuses a plain password, so the panel obscures it exactly
// the way `rclone obscure` does.
var rcloneObscureKey = []byte{
	0x9c, 0x93, 0x5b, 0x48, 0x73, 0x0a, 0x55, 0x4d,
	0x6b, 0xfd, 0x7c, 0x63, 0xc8, 0x86, 0xa9, 0x2b,
	0xd3, 0x90, 0x19, 0x8e, 0xb8, 0x12, 0x8a, 0xfb,
	0xf4, 0xde, 0x16, 0x2b, 0x8b, 0x95, 0xf6, 0x38,
}

// rcloneObscure is `rclone obscure`: AES-256-CTR with a random IV, base64url.
func rcloneObscure(s string) string {
	block, err := aes.NewCipher(rcloneObscureKey)
	if err != nil {
		panic(err) // the key is a constant of the right length
	}
	buf := make([]byte, aes.BlockSize+len(s))
	if _, err := rand.Read(buf[:aes.BlockSize]); err != nil {
		panic(err)
	}
	cipher.NewCTR(block, buf[:aes.BlockSize]).XORKeyStream(buf[aes.BlockSize:], []byte(s))
	return base64.RawURLEncoding.EncodeToString(buf)
}

// rcloneReveal undoes rcloneObscure (tests and the fake rclone use it).
func rcloneReveal(s string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) < aes.BlockSize {
		return "", errors.New("bukan nilai obscure rclone")
	}
	block, err := aes.NewCipher(rcloneObscureKey)
	if err != nil {
		return "", err
	}
	out := make([]byte, len(b)-aes.BlockSize)
	cipher.NewCTR(block, b[:aes.BlockSize]).XORKeyStream(out, b[aes.BlockSize:])
	return string(out), nil
}

// pemOneLine is the form rclone wants for key_pem: one line with "\n"
// escapes (it unquotes the value; a real newline is a syntax error).
func pemOneLine(pemText string) string {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(pemText, "\r\n", "\n")), "\n")
	return strings.Join(lines, `\n`) + `\n`
}

// validatePrivateKey checks a pasted private key and returns it normalised
// (LF line endings, one trailing newline). Encrypted keys are refused: the
// passphrase would have to be stored right next to the key, which protects
// nothing.
func validatePrivateKey(text string) (string, error) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return "", errors.New("kunci privat kosong")
	}
	if len(text) > maxPrivateKeyLen {
		return "", errors.New("kunci privat terlalu panjang")
	}
	block, rest := pem.Decode([]byte(text))
	if block == nil {
		return "", errors.New("kunci privat bukan format PEM (harus diawali -----BEGIN … PRIVATE KEY-----)")
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return "", errors.New("tempel satu kunci privat saja")
	}
	if strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED") || block.Type == "ENCRYPTED PRIVATE KEY" {
		return "", errors.New("kunci privat ini terenkripsi passphrase. Buat kunci khusus tanpa passphrase (ssh-keygen -t ed25519 -N '' -f garagepanel-vps), atau biarkan kolom kosong supaya panel membuatkannya")
	}
	switch block.Type {
	case "OPENSSH PRIVATE KEY":
		if !bytes.HasPrefix(block.Bytes, []byte("openssh-key-v1\x00")) {
			return "", errors.New("kunci OPENSSH rusak")
		}
		cipherName, _, ok := readSSHString(block.Bytes[len("openssh-key-v1\x00"):])
		if !ok {
			return "", errors.New("kunci OPENSSH rusak")
		}
		if string(cipherName) != "none" {
			return "", errors.New("kunci privat ini terenkripsi passphrase. Buat kunci khusus tanpa passphrase (ssh-keygen -t ed25519 -N '' -f garagepanel-vps), atau biarkan kolom kosong supaya panel membuatkannya")
		}
	case "PRIVATE KEY":
		if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
			return "", fmt.Errorf("kunci PKCS#8 tidak bisa dibaca: %v", err)
		}
	case "RSA PRIVATE KEY":
		if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			return "", fmt.Errorf("kunci RSA tidak bisa dibaca: %v", err)
		}
	case "EC PRIVATE KEY":
		if _, err := x509.ParseECPrivateKey(block.Bytes); err != nil {
			return "", fmt.Errorf("kunci EC tidak bisa dibaca: %v", err)
		}
	default:
		return "", fmt.Errorf("jenis kunci %q tidak didukung", block.Type)
	}
	return text + "\n", nil
}

// generateSFTPKey makes an ed25519 key pair for one remote: the private half
// as PKCS#8 PEM (which rclone's SSH library reads), the public half as an
// authorized_keys line.
func generateSFTPKey(comment string) (privatePEM, authorizedKey string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	privatePEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	blob := append(sshString([]byte("ssh-ed25519")), sshString(pub)...)
	authorizedKey = "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " " + comment
	return privatePEM, authorizedKey, nil
}

var keyCommentUnsafe = regexp.MustCompile(`[^A-Za-z0-9._@-]+`)

// sftpKeyComment names a generated key so it can be recognised (and removed)
// in authorized_keys on the server. It only ever contains shell-safe
// characters, because it goes into a copy-paste command.
func sftpKeyComment(remoteName string) string {
	host, _ := os.Hostname()
	host = keyCommentUnsafe.ReplaceAllString(host, "")
	if host == "" {
		host = "panel"
	}
	return keyCommentUnsafe.ReplaceAllString("garagepanel-"+remoteName+"@"+host, "")
}

// sftpInstallCommand is what the operator runs on the server, as the SSH
// user, to accept the panel's generated key.
func sftpInstallCommand(authorizedKey string) string {
	return "mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '" + authorizedKey + "' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys"
}

// validateSFTP checks an SFTP remote.
func (r Remote) validateSFTP() error {
	if err := ValidateRemoteName(r.Name); err != nil {
		return err
	}
	if _, err := ValidateSSHHost(r.Host); err != nil {
		return err
	}
	if err := ValidateSSHPort(r.Port); err != nil {
		return err
	}
	if err := ValidateSSHUser(r.User); err != nil {
		return err
	}
	switch {
	case r.Password != "" && r.PrivateKey != "":
		return errors.New("isi password atau kunci SSH, bukan keduanya")
	case r.Password == "" && r.PrivateKey == "":
		return errors.New("password atau kunci SSH harus diisi")
	case r.Password != "":
		if len(r.Password) > maxSFTPPassword {
			return errors.New("password terlalu panjang")
		}
		if strings.ContainsAny(r.Password, "\x00\r\n") {
			return errors.New("password mengandung karakter yang tidak boleh (baris baru atau NUL)")
		}
	default:
		if _, err := validatePrivateKey(r.PrivateKey); err != nil {
			return err
		}
	}
	if len(r.HostKeys) > maxPinnedHostKeys {
		return fmt.Errorf("maksimal %d host key", maxPinnedHostKeys)
	}
	for _, k := range r.HostKeys {
		if _, err := validateHostKey(k); err != nil {
			return err
		}
	}
	if r.Endpoint != "" || r.AccessKey != "" || r.SecretKey != "" {
		return errors.New("remote SFTP tidak memakai endpoint/access key S3")
	}
	return nil
}

// normaliseSFTP trims and normalises an SFTP remote before validation.
func normaliseSFTP(r Remote) (Remote, error) {
	r.Name = strings.TrimSpace(r.Name)
	r.Type = RemoteTypeSFTP
	r.Provider = ""
	r.Region = ""
	r.Addressing = ""
	host, err := ValidateSSHHost(r.Host)
	if err != nil {
		return r, err
	}
	r.Host = host
	if r.Port == 0 {
		r.Port = sftpDefaultPort
	}
	r.User = strings.TrimSpace(r.User)
	r.Password = strings.Trim(r.Password, "\r\n")
	if r.PrivateKey != "" {
		key, err := validatePrivateKey(r.PrivateKey)
		if err != nil {
			return r, err
		}
		r.PrivateKey = key
	} else {
		r.PublicKey = ""
	}
	if err := r.Validate(); err != nil {
		return r, err
	}
	return r, nil
}

// sftpEnv renders the remote for rclone. The password is obscured, the key
// flattened to the one-line form, and the host key file is always set: with
// no pinned key it is empty, and rclone then refuses the server instead of
// skipping verification.
//
// Listings skip symlinks (and FIFOs, sockets, devices). Followed, a link
// back up the tree copies the folder again at every level until the kernel
// gives up, a link to /etc or ~/.ssh smuggles exactly the files the folder
// rules keep out into a bucket, and a FIFO blocks the read forever. A
// folder that is itself a link still works: only entries inside are
// skipped.
func (r Remote) sftpEnv(prefix string) []string {
	known := r.knownHostsPath
	if known == "" {
		known = "/nonexistent/garagepanel-known-hosts" // fail closed
	}
	host := r.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // rclone appends ":port" to the host as is
	}
	env := []string{
		prefix + "TYPE=sftp",
		prefix + "HOST=" + host,
		prefix + "PORT=" + strconv.Itoa(r.Port),
		prefix + "USER=" + r.User,
		prefix + "KNOWN_HOSTS_FILE=" + known,
		prefix + "KEY_USE_AGENT=false",
		prefix + "SKIP_LINKS=true",
	}
	if algos := hostKeyAlgorithms(r.HostKeys); algos != "" {
		env = append(env, prefix+"HOST_KEY_ALGORITHMS="+algos)
	}
	if r.PrivateKey != "" {
		env = append(env, prefix+"KEY_PEM="+pemOneLine(r.PrivateKey))
	} else {
		env = append(env, prefix+"PASS="+rcloneObscure(r.Password))
	}
	return env
}

// --- talking to the server through rclone ------------------------------------

// command runs a short rclone command — list one folder, write a probe file —
// and returns its stdout, the last log lines and the exit code.
func (r *rcloneRunner) command(ctx context.Context, env []string, stdin io.Reader, verb string, args ...string) ([]byte, []string, int, error) {
	full := append([]string{verb,
		"--config", "",
		"--contimeout", "15s",
		"--timeout", "60s",
		"--retries", "1",
		"--low-level-retries", "2",
	}, args...)
	full = append(full, r.sharedArgs()...)
	cmd := exec.CommandContext(ctx, r.bin, full...)
	cmd.Env = append(minimalEnv(), env...)
	cmd.Stdin = stdin
	var stdout, stderr cappedBuffer
	stdout.max, stderr.max = 4<<20, 256<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	lines := rcloneLogTail(stderr.String())
	if ctx.Err() != nil {
		return nil, lines, -1, ctx.Err()
	}
	if err == nil {
		return stdout.Bytes(), lines, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.Bytes(), lines, exitErr.ExitCode(), nil
	}
	return nil, lines, -1, fmt.Errorf("rclone gagal dijalankan: %w", err)
}

// sharedArgs are what every SFTP command gets like a copy run does: the
// panel's cache directory and RCLONE_EXTRA_ARGS, so a flag an old SSH server
// needs (say --sftp-use-insecure-cipher) also applies to tests and listings.
func (r *rcloneRunner) sharedArgs() []string {
	var out []string
	if r.cacheDir != "" {
		out = append(out, "--cache-dir", r.cacheDir)
	}
	return append(out, r.extraArgs...)
}

// cappedBuffer keeps at most max bytes.
type cappedBuffer struct {
	bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

var rcloneLogStamp = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// rcloneLogTail returns the last plain-text log lines, without timestamps.
func rcloneLogTail(stderr string) []string {
	var out []string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(rcloneLogStamp.ReplaceAllString(strings.TrimSpace(line), ""))
		if line == "" || rcloneEchoesConfig(line) {
			continue
		}
		out = append(out, truncate(line, rcloneMaxLineLen))
	}
	if len(out) > rcloneLastLines {
		out = out[len(out)-rcloneLastLines:]
	}
	return out
}

// rcloneProblem picks the line that explains a failure (the last CRITICAL or
// ERROR one) and strips rclone's wrapping, which repeats the remote path.
func rcloneProblem(lines []string) string {
	pick := ""
	for _, l := range lines {
		if strings.HasPrefix(l, "CRITICAL:") || strings.HasPrefix(l, "ERROR") || strings.HasPrefix(l, "Failed") {
			pick = l
		}
	}
	if pick == "" && len(lines) > 0 {
		pick = lines[len(lines)-1]
	}
	if pick == "" {
		return "rclone tidak menulis alasan"
	}
	for _, p := range []string{"CRITICAL:", "ERROR :", "ERROR:"} {
		pick = strings.TrimSpace(strings.TrimPrefix(pick, p))
	}
	if i := strings.Index(pick, `": `); strings.HasPrefix(pick, "Failed to create file system for") && i >= 0 {
		pick = pick[i+3:]
	}
	for _, p := range []string{"NewFs: ", "couldn't connect SSH: "} {
		pick = strings.TrimPrefix(pick, p)
	}
	return pick
}

// sftpHint turns the usual SSH failures into a next step.
func sftpHint(msg string, r Remote) string {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "unable to authenticate"):
		if r.PrivateKey == "" && r.User == "root" {
			return "Login ditolak. Ubuntu dan Debian bawaannya menolak login root dengan password (PermitRootLogin prohibit-password).\n" +
				"Pakai kunci SSH: klik Edit → \"Kunci SSH\" dengan kolom kunci kosong, panel membuatkan kunci dan menampilkan perintah untuk memasangnya di VPS. Atau periksa password-nya."
		}
		if r.PrivateKey != "" {
			return "Login ditolak. Pastikan kunci publik remote ini sudah ada di ~/.ssh/authorized_keys milik user " + r.User + " di VPS (perintah pemasangannya ada di kartu remote)."
		}
		return "Login ditolak: user atau password salah, atau server tidak menerima login password."
	case strings.Contains(low, "knownhosts: key mismatch"):
		return "Host key server TIDAK SAMA dengan yang disematkan saat remote dibuat. Kalau VPS memang baru diinstal ulang, klik Edit dan centang \"Ambil ulang host key\".\n" +
			"Kalau tidak ada instal ulang, jangan lanjutkan: koneksinya mungkin disadap."
	case strings.Contains(low, "knownhosts: key is unknown"), strings.Contains(low, "known_hosts"):
		return "Host key server belum disematkan. Klik Tes koneksi supaya panel mengambil dan menyimpannya."
	case strings.Contains(low, "connection refused"):
		return "Port SSH tertutup atau salah. Periksa nomor port, firewall, dan bahwa sshd berjalan di VPS."
	case strings.Contains(low, "no such host"):
		return "Nama host tidak ditemukan di DNS. Periksa ejaannya atau pakai alamat IP."
	case strings.Contains(low, "i/o timeout"), strings.Contains(low, "timed out"), strings.Contains(low, "no route to host"), strings.Contains(low, "network is unreachable"):
		return "VPS tidak terjangkau. Periksa alamat, firewall (port " + strconv.Itoa(r.Port) + " harus terbuka untuk server panel), dan bahwa VPS menyala."
	case strings.Contains(low, "directory not found"), strings.Contains(low, "file does not exist"), strings.Contains(low, "no such file"):
		return "Folder tidak ada di VPS. Periksa path-nya (harus absolut, contoh /var/www/uploads)."
	case strings.Contains(low, "permission denied"):
		return "User " + r.User + " tidak punya izin di folder itu. Periksa pemilik dan izin folder di VPS."
	}
	return ""
}

// sftpError builds the error shown for a failed rclone call on an SFTP remote.
func sftpError(r Remote, what string, lines []string) error {
	msg := rcloneProblem(lines)
	if hint := sftpHint(msg, r); hint != "" {
		return fmt.Errorf("%s: %s\n%s", what, msg, hint)
	}
	return fmt.Errorf("%s: %s", what, msg)
}

// probeSFTP checks a folder on an SFTP remote: that it exists, and when write
// is set, creating it if needed and writing then deleting a tiny file. Unlike
// an S3 bucket, a missing destination folder is fine — rclone creates it.
// The folder's content is not listed here (see sftpFolderExists); a folder
// the user may not read shows up when the job lists it.
func probeSFTP(ctx context.Context, rc *rcloneRunner, r Remote, folder string, write bool) (ProbeReport, error) {
	var report ProbeReport
	if err := rc.sftpSupport(); err != nil {
		return report, err
	}
	if len(r.HostKeys) == 0 {
		return report, fmt.Errorf("remote %s: host key server belum disematkan. Klik Tes koneksi di kartu remote dulu", r.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, sftpProbeTimeout)
	defer cancel()
	env := r.rcloneEnv("probe")
	target := "probe:/" + folder
	display := r.Name + ":/" + folder
	exists, err := sftpFolderExists(ctx, rc, env, r, folder)
	if errors.Is(err, context.DeadlineExceeded) {
		return report, fmt.Errorf("%s: pemeriksaan melewati %v; folder dan folder induknya berisi sangat banyak file, atau koneksi ke VPS lambat", display, sftpProbeTimeout)
	}
	if err != nil {
		return report, err
	}
	if !write {
		if !exists {
			return report, fmt.Errorf("%s: folder tidak ada di VPS", display)
		}
		return report, nil
	}
	if !exists {
		if _, lines, code, err := rc.command(ctx, env, nil, "mkdir", target); err != nil {
			return report, err
		} else if code != 0 {
			return report, sftpError(r, display+" (membuat folder)", lines)
		}
		report.Created = true
	}
	name := ".garagepanel-probe-" + randomHex(4)
	if _, lines, code, err := rc.command(ctx, env, strings.NewReader("garagepanel probe\n"), "rcat", target+name); err != nil {
		return report, err
	} else if code != 0 {
		return report, sftpError(r, display+" bisa dibaca, tapi menulis file uji ditolak", lines)
	}
	report.WriteOK = true
	if _, lines, code, err := rc.command(ctx, env, nil, "deletefile", target+name); err != nil {
		return report, err
	} else if code != 0 {
		report.Leftover = "/" + folder + name
		return report, sftpError(r, "file uji /"+folder+name+" tertulis tapi tidak bisa dihapus", lines)
	}
	return report, nil
}

// sftpFolderExists looks the folder up in its parent's listing: one level,
// directories only, symlinks followed (a folder may be a link to another
// disk). Listing the folder itself would cost one round trip per ~100 names,
// minutes for a flat folder of millions of files, and the probe runs inside
// an HTTP request. The parent listing also proves the login and the pinned
// host key. When the parent cannot be read (mode 711) or is itself huge
// (still listing after sftpParentBudget), the folder is asked directly.
func sftpFolderExists(ctx context.Context, rc *rcloneRunner, env []string, r Remote, folder string) (bool, error) {
	parent, base := path.Split(strings.TrimSuffix(folder, "/"))
	found := false
	pctx, cancel := context.WithTimeout(ctx, sftpParentBudget)
	code, lines, err := rc.streamLsjson(pctx, env, probeLsjsonArgs("probe:/"+parent, "--max-depth", "1", "--dirs-only", "--sftp-skip-links=false"),
		func(it lsjsonItem) error {
			if it.IsDir && it.Path == base {
				found = true
			}
			return nil
		})
	cancel()
	switch {
	case err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		// A huge parent: ask the folder itself.
	case err != nil:
		return false, err
	case code == 0:
		return found, nil
	case code == 3:
		return false, nil // the parent does not exist either
	case !strings.Contains(strings.ToLower(rcloneProblem(lines)), "permission denied"):
		return false, sftpError(r, r.Name+":/"+folder, lines)
	}
	return sftpFolderListable(ctx, rc, env, r, folder)
}

// sftpFolderListable is the fallback of sftpFolderExists: list the folder
// itself, one level. The path ends in "/", so a file of that name is
// "directory not found" (exit 3), like a missing folder.
func sftpFolderListable(ctx context.Context, rc *rcloneRunner, env []string, r Remote, folder string) (bool, error) {
	code, lines, err := rc.streamLsjson(ctx, env, probeLsjsonArgs("probe:/"+folder, "--max-depth", "1"),
		func(lsjsonItem) error { return nil })
	switch {
	case err != nil:
		return false, err
	case code == 0 || code == 3:
		return code == 0, nil
	}
	return false, sftpError(r, r.Name+":/"+folder, lines)
}

// probeLsjsonArgs is an lsjson command with the short timeouts of a probe.
func probeLsjsonArgs(remotePath string, extra ...string) []string {
	return append([]string{"lsjson", remotePath,
		"--no-mimetype", "--no-modtime",
		"--config", "",
		"--contimeout", "15s",
		"--timeout", "60s",
		"--retries", "1",
		"--low-level-retries", "2",
	}, extra...)
}
