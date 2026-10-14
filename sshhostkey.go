package main

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// The panel pins an SFTP server's host key the first time it talks to it
// (trust on first use) and from then on hands rclone a known_hosts file, so
// a later man-in-the-middle cannot collect the password or the data. Go's
// standard library has no SSH client and rclone only logs a fingerprint, so
// this file speaks just enough of RFC 4253 to receive the key: version
// exchange, KEXINIT, and an ECDH key exchange whose reply carries the host
// key. The connection is dropped right after that. No session is set up and
// no credential is ever sent.

const (
	sshMsgDisconnect    = 1
	sshMsgIgnore        = 2
	sshMsgUnimplemented = 3
	sshMsgDebug         = 4
	sshMsgKexInit       = 20
	sshMsgKexECDHInit   = 30
	sshMsgKexECDHReply  = 31

	sshClientVersion = "SSH-2.0-garagepanel"
	sshMaxPacket     = 256 << 10
	// hostKeyTimeout bounds one connection.
	hostKeyTimeout = 15 * time.Second
	// maxPinnedHostKeys bounds what a remote may carry.
	maxPinnedHostKeys = 16
)

// hostKeyFamilies are asked for one connection each, like ssh-keyscan does: a
// server presents a single host key per key exchange, so every type it has
// must be fetched separately. All of them are pinned, because the SSH client
// inside rclone picks the type by its own preference.
var hostKeyFamilies = [][]string{
	{"ssh-ed25519"},
	{"ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521"},
	{"rsa-sha2-512", "rsa-sha2-256", "ssh-rsa"},
}

// sshKexAlgorithms are the key exchanges offered, all available in crypto/ecdh.
var sshKexAlgorithms = []string{
	"curve25519-sha256", "curve25519-sha256@libssh.org",
	"ecdh-sha2-nistp256", "ecdh-sha2-nistp384", "ecdh-sha2-nistp521",
}

// Ciphers and MACs are only offered so the server agrees to the key exchange;
// the connection never gets far enough to use them.
const (
	sshCiphers = "chacha20-poly1305@openssh.com,aes128-gcm@openssh.com,aes256-gcm@openssh.com,aes128-ctr,aes192-ctr,aes256-ctr"
	sshMACs    = "hmac-sha2-256-etm@openssh.com,hmac-sha2-512-etm@openssh.com,hmac-sha2-256,hmac-sha2-512,hmac-sha1"
)

// hostKeyTypes are the key types the panel pins, as written in known_hosts.
var hostKeyTypes = map[string]bool{
	"ssh-ed25519": true, "ecdsa-sha2-nistp256": true, "ecdsa-sha2-nistp384": true,
	"ecdsa-sha2-nistp521": true, "ssh-rsa": true,
}

// errNoMatchingHostKey means the server has no key of the requested family.
var errNoMatchingHostKey = errors.New("server tidak punya host key jenis ini")

// hostKeyDialError marks a failure to reach the server at all.
type hostKeyDialError struct{ err error }

func (e *hostKeyDialError) Error() string { return e.err.Error() }
func (e *hostKeyDialError) Unwrap() error { return e.err }

// fetchHostKeys returns every host key the SSH server at host:port presents,
// each as "type base64" (the second and third field of a known_hosts line).
func fetchHostKeys(ctx context.Context, host string, port int) ([]string, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	var keys []string
	var firstErr error
	for _, family := range hostKeyFamilies {
		key, err := fetchHostKey(ctx, addr, family)
		if err != nil {
			var dialErr *hostKeyDialError
			if errors.As(err, &dialErr) {
				return nil, fmt.Errorf("tidak bisa menghubungi %s: %w", addr, dialErr.err)
			}
			if firstErr == nil && !errors.Is(err, errNoMatchingHostKey) {
				firstErr = err
			}
			continue
		}
		if !containsString(keys, key) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		if firstErr == nil {
			firstErr = errors.New("server tidak menawarkan host key ed25519, ecdsa, maupun rsa")
		}
		return nil, fmt.Errorf("host key %s tidak bisa diambil: %w", addr, firstErr)
	}
	return keys, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// fetchHostKey runs one key exchange far enough to receive the host key of
// one of the given algorithms.
func fetchHostKey(ctx context.Context, addr string, hostKeyAlgs []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, hostKeyTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", &hostKeyDialError{err}
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	r := bufio.NewReader(conn)
	if _, err := io.WriteString(conn, sshClientVersion+"\r\n"); err != nil {
		return "", &hostKeyDialError{err}
	}
	if _, err := readSSHVersion(r); err != nil {
		return "", &hostKeyDialError{err}
	}
	if err := writeSSHPacket(conn, buildKexInit(hostKeyAlgs)); err != nil {
		return "", err
	}

	var serverKexInit []byte
	for serverKexInit == nil {
		p, err := readSSHPacket(r)
		if err != nil {
			// A server without a key of this family usually just hangs up.
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return "", errNoMatchingHostKey
			}
			return "", err
		}
		switch p[0] {
		case sshMsgKexInit:
			serverKexInit = p
		case sshMsgIgnore, sshMsgDebug, sshMsgUnimplemented:
		case sshMsgDisconnect:
			return "", disconnectError(p)
		default:
			return "", fmt.Errorf("pesan SSH %d datang sebelum KEXINIT", p[0])
		}
	}
	lists, err := parseKexInit(serverKexInit)
	if err != nil {
		return "", err
	}
	if chooseSSHAlgorithm(hostKeyAlgs, lists[1]) == "" {
		return "", errNoMatchingHostKey
	}
	kex := chooseSSHAlgorithm(sshKexAlgorithms, lists[0])
	var curve ecdh.Curve
	switch kex {
	case "curve25519-sha256", "curve25519-sha256@libssh.org":
		curve = ecdh.X25519()
	case "ecdh-sha2-nistp256":
		curve = ecdh.P256()
	case "ecdh-sha2-nistp384":
		curve = ecdh.P384()
	case "ecdh-sha2-nistp521":
		curve = ecdh.P521()
	default:
		return "", fmt.Errorf("server hanya menawarkan pertukaran kunci %q; panel butuh curve25519 atau ecdh-sha2 (OpenSSH sejak 2011 punya keduanya) — tempel host key manual", lists[0])
	}
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	init := append([]byte{sshMsgKexECDHInit}, sshString(priv.PublicKey().Bytes())...)
	if err := writeSSHPacket(conn, init); err != nil {
		return "", err
	}
	for {
		p, err := readSSHPacket(r)
		if err != nil {
			return "", fmt.Errorf("server memutus sebelum mengirim host key: %w", err)
		}
		switch p[0] {
		case sshMsgKexECDHReply:
			blob, _, ok := readSSHString(p[1:])
			if !ok {
				return "", errors.New("balasan pertukaran kunci rusak")
			}
			key, err := hostKeyFromBlob(blob)
			if err != nil {
				return "", err
			}
			return key, nil
		case sshMsgIgnore, sshMsgDebug, sshMsgUnimplemented:
		case sshMsgDisconnect:
			return "", disconnectError(p)
		default:
			return "", fmt.Errorf("pesan SSH %d tidak terduga saat pertukaran kunci", p[0])
		}
	}
}

// hostKeyFromBlob renders a public key blob as "type base64".
func hostKeyFromBlob(blob []byte) (string, error) {
	typ, _, ok := readSSHString(blob)
	if !ok || !hostKeyTypes[string(typ)] {
		return "", fmt.Errorf("jenis host key %q tidak didukung", truncate(string(typ), 40))
	}
	if len(blob) > 4096 {
		return "", errors.New("host key terlalu panjang")
	}
	return string(typ) + " " + base64.StdEncoding.EncodeToString(blob), nil
}

// readSSHVersion reads the server's identification line. RFC 4253 allows
// other lines before it.
func readSSHVersion(r *bufio.Reader) (string, error) {
	for i := 0; i < 50; i++ {
		line, err := readSSHLine(r)
		if err != nil {
			return "", fmt.Errorf("server tidak mengirim identitas SSH: %w", err)
		}
		if strings.HasPrefix(line, "SSH-") {
			if !strings.HasPrefix(line, "SSH-2.0-") && !strings.HasPrefix(line, "SSH-1.99-") {
				return "", fmt.Errorf("versi protokol %q tidak didukung", truncate(line, 60))
			}
			return line, nil
		}
	}
	return "", errors.New("server tidak mengirim identitas SSH (bukan server SSH?)")
}

// readSSHLine reads one CR LF (or LF) terminated line of at most 255 bytes.
func readSSHLine(r *bufio.Reader) (string, error) {
	var b []byte
	for {
		c, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if c == '\n' {
			return strings.TrimSuffix(string(b), "\r"), nil
		}
		b = append(b, c)
		if len(b) > 255 {
			return "", errors.New("baris identitas terlalu panjang (bukan server SSH?)")
		}
	}
}

// buildKexInit is SSH_MSG_KEXINIT with the given host key algorithms.
func buildKexInit(hostKeyAlgs []string) []byte {
	b := []byte{sshMsgKexInit}
	cookie := make([]byte, 16)
	_, _ = rand.Read(cookie)
	b = append(b, cookie...)
	for _, list := range []string{
		strings.Join(sshKexAlgorithms, ","), strings.Join(hostKeyAlgs, ","),
		sshCiphers, sshCiphers, sshMACs, sshMACs, "none", "none", "", "",
	} {
		b = append(b, sshString([]byte(list))...)
	}
	b = append(b, 0)          // first_kex_packet_follows
	b = append(b, 0, 0, 0, 0) // reserved
	return b
}

// parseKexInit returns the ten name-lists of a KEXINIT payload.
func parseKexInit(p []byte) ([10]string, error) {
	var lists [10]string
	if len(p) < 17 {
		return lists, errors.New("KEXINIT server terlalu pendek")
	}
	rest := p[17:]
	for i := range lists {
		s, r, ok := readSSHString(rest)
		if !ok {
			return lists, errors.New("KEXINIT server rusak")
		}
		lists[i] = string(s)
		rest = r
	}
	return lists, nil
}

// chooseSSHAlgorithm is SSH negotiation: the first of the client's
// algorithms the server also lists.
func chooseSSHAlgorithm(client []string, server string) string {
	offered := strings.Split(server, ",")
	for _, c := range client {
		if containsString(offered, c) {
			return c
		}
	}
	return ""
}

func disconnectError(p []byte) error {
	if len(p) >= 5 {
		if msg, _, ok := readSSHString(p[5:]); ok && len(msg) > 0 {
			return fmt.Errorf("server memutus koneksi: %s", truncate(string(msg), 200))
		}
	}
	return errors.New("server memutus koneksi")
}

// writeSSHPacket frames an unencrypted packet (block size 8, no MAC).
func writeSSHPacket(w io.Writer, payload []byte) error {
	pad := 8 - (5+len(payload))%8
	if pad < 4 {
		pad += 8
	}
	buf := make([]byte, 5+len(payload)+pad)
	binary.BigEndian.PutUint32(buf, uint32(1+len(payload)+pad))
	buf[4] = byte(pad)
	copy(buf[5:], payload)
	_, _ = rand.Read(buf[5+len(payload):])
	_, err := w.Write(buf)
	return err
}

// readSSHPacket reads one unencrypted packet and returns its payload.
func readSSHPacket(r io.Reader) ([]byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(hdr[:4])
	pad := uint32(hdr[4])
	if length < 2 || length > sshMaxPacket || pad+1 >= length {
		return nil, errors.New("paket SSH tidak valid")
	}
	rest := make([]byte, length-1)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, err
	}
	return rest[:length-1-pad], nil
}

// sshString encodes a string the SSH way: uint32 length, then the bytes.
func sshString(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}

// readSSHString decodes one SSH string from the front of b.
func readSSHString(b []byte) (s, rest []byte, ok bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, false
	}
	return b[4 : 4+n], b[4+n:], true
}

// hostKeyFingerprint is OpenSSH's SHA256 fingerprint of a "type base64" key,
// the same string `ssh-keygen -lf` prints.
func hostKeyFingerprint(key string) string {
	_, b64, _ := strings.Cut(key, " ")
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "?"
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// hostKeyLabel is the short type name people see in ssh-keygen output.
func hostKeyLabel(key string) string {
	typ, _, _ := strings.Cut(key, " ")
	switch {
	case typ == "ssh-ed25519":
		return "ED25519"
	case strings.HasPrefix(typ, "ecdsa-"):
		return "ECDSA"
	case typ == "ssh-rsa":
		return "RSA"
	}
	return typ
}

// parseHostKeys accepts what people paste: the content of a host's
// /etc/ssh/ssh_host_*_key.pub ("type base64 comment") or ssh-keyscan /
// known_hosts lines ("host type base64"). Comments and blank lines are
// skipped. Each key is checked to be well-formed.
func parseHostKeys(text string) ([]string, error) {
	var keys []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "@") {
			return nil, errors.New("baris @cert-authority / @revoked tidak didukung; tempel host key biasa")
		}
		f := strings.Fields(line)
		var typ, b64 string
		switch {
		case len(f) >= 2 && hostKeyTypes[f[0]]:
			typ, b64 = f[0], f[1]
		case len(f) >= 3 && hostKeyTypes[f[1]]:
			typ, b64 = f[1], f[2]
		default:
			return nil, fmt.Errorf("baris host key tidak dikenali: %q (contoh: ssh-ed25519 AAAAC3…)", truncate(line, 60))
		}
		key, err := validateHostKey(typ + " " + b64)
		if err != nil {
			return nil, err
		}
		if !containsString(keys, key) {
			keys = append(keys, key)
		}
	}
	if len(keys) > maxPinnedHostKeys {
		return nil, fmt.Errorf("maksimal %d host key", maxPinnedHostKeys)
	}
	return keys, nil
}

// validateHostKey checks one "type base64" key: known type, decodable, and
// the blob's own type field matching.
func validateHostKey(key string) (string, error) {
	typ, b64, ok := strings.Cut(strings.TrimSpace(key), " ")
	if !ok || !hostKeyTypes[typ] {
		return "", fmt.Errorf("host key %q: jenis tidak didukung (ssh-ed25519, ecdsa-sha2-nistp256/384/521, ssh-rsa)", truncate(key, 40))
	}
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("host key %s: base64 tidak valid", typ)
	}
	inner, _, ok := readSSHString(blob)
	if !ok || string(inner) != typ {
		return "", fmt.Errorf("host key %s: isi kunci tidak cocok dengan jenisnya", typ)
	}
	if len(blob) > 4096 {
		return "", errors.New("host key terlalu panjang")
	}
	return typ + " " + b64, nil
}

// hostKeyAlgorithms lists the SSH host key algorithms that match the pinned
// keys, most preferred first, so rclone never negotiates a type that was not
// pinned (which would fail as "key mismatch").
func hostKeyAlgorithms(keys []string) string {
	var out []string
	add := func(names ...string) {
		for _, n := range names {
			if !containsString(out, n) {
				out = append(out, n)
			}
		}
	}
	for _, want := range []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "ssh-rsa"} {
		for _, k := range keys {
			if typ, _, _ := strings.Cut(k, " "); typ == want {
				if want == "ssh-rsa" {
					add("rsa-sha2-512", "rsa-sha2-256", "ssh-rsa")
				} else {
					add(want)
				}
			}
		}
	}
	return strings.Join(out, " ")
}

// knownHostsContent renders pinned keys as a known_hosts file for one remote.
// The host pattern is a wildcard: the file belongs to this remote alone, and
// rclone's SSH library checks both the configured name and the IP it
// resolved to, which a single name could not cover.
func knownHostsContent(keys []string, port int) string {
	pattern := "*"
	if port != 22 {
		pattern = "[*]:" + strconv.Itoa(port)
	}
	var b strings.Builder
	b.WriteString("# Dikelola garagepanel. Host key yang disematkan untuk satu remote SFTP.\n")
	for _, k := range keys {
		b.WriteString(pattern + " " + k + "\n")
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
