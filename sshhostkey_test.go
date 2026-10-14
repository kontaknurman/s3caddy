package main

import (
	"bufio"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeSSHServer speaks the start of RFC 4253 like an SSH server with the
// given host keys: version line, KEXINIT, ECDH reply carrying the host key.
// It never completes a key exchange — neither does the panel's client.
type fakeSSHServer struct {
	ln    net.Listener
	blobs [][]byte // public key blobs, one per type
	mu    sync.Mutex
	conns int
}

func newFakeSSHServer(t *testing.T, blobs ...[]byte) *fakeSSHServer {
	t.Helper()
	return newFakeSSHServerAt(t, "127.0.0.1:0", blobs...)
}

// newFakeSSHServerAt listens on a given address, e.g. a port a remote was
// saved with while nothing listened there.
func newFakeSSHServerAt(t *testing.T, addr string, blobs ...[]byte) *fakeSSHServer {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSSHServer{ln: ln, blobs: blobs}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			s.mu.Unlock()
			go s.handle(c)
		}
	}()
	return s
}

func (s *fakeSSHServer) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// connCount is how many connections the server accepted so far.
func (s *fakeSSHServer) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

// keyLines are the "type base64" forms of the server's keys.
func (s *fakeSSHServer) keyLines() []string {
	var out []string
	for _, b := range s.blobs {
		k, _ := hostKeyFromBlob(b)
		out = append(out, k)
	}
	return out
}

func blobType(b []byte) string {
	typ, _, _ := readSSHString(b)
	return string(typ)
}

// algorithmsFor are the host key algorithm names a key type is offered as.
func algorithmsFor(typ string) []string {
	if typ == "ssh-rsa" {
		return []string{"rsa-sha2-512", "rsa-sha2-256", "ssh-rsa"}
	}
	return []string{typ}
}

func (s *fakeSSHServer) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	// A banner line before the identification is allowed by the RFC.
	io.WriteString(c, "Welcome to the fake\r\nSSH-2.0-FakeSSH_1.0\r\n")
	if _, err := readSSHLine(r); err != nil {
		return
	}
	clientInit, err := readSSHPacket(r)
	if err != nil || clientInit[0] != sshMsgKexInit {
		return
	}
	lists, err := parseKexInit(clientInit)
	if err != nil {
		return
	}
	var offered []string
	for _, b := range s.blobs {
		offered = append(offered, algorithmsFor(blobType(b))...)
	}
	// A message the client must skip, then our KEXINIT.
	writeSSHPacket(c, []byte{sshMsgIgnore, 0, 0, 0, 0})
	init := []byte{sshMsgKexInit}
	init = append(init, make([]byte, 16)...)
	for _, l := range []string{"curve25519-sha256,diffie-hellman-group14-sha256", strings.Join(offered, ","), "aes128-ctr", "aes128-ctr", "hmac-sha2-256", "hmac-sha2-256", "none", "none", "", ""} {
		init = append(init, sshString([]byte(l))...)
	}
	init = append(init, 0, 0, 0, 0, 0)
	writeSSHPacket(c, init)
	chosen := chooseSSHAlgorithm(strings.Split(lists[1], ","), strings.Join(offered, ","))
	if chosen == "" {
		return // like OpenSSH: no matching host key type, hang up
	}
	ecdhInit, err := readSSHPacket(r)
	if err != nil || ecdhInit[0] != sshMsgKexECDHInit {
		return
	}
	var blob []byte
	for _, b := range s.blobs {
		for _, a := range algorithmsFor(blobType(b)) {
			if a == chosen {
				blob = b
			}
		}
	}
	reply := []byte{sshMsgKexECDHReply}
	reply = append(reply, sshString(blob)...)
	reply = append(reply, sshString(make([]byte, 32))...)
	reply = append(reply, sshString([]byte("signature-not-checked"))...)
	writeSSHPacket(c, reply)
}

func testEd25519Blob(t *testing.T) []byte {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return append(sshString([]byte("ssh-ed25519")), sshString(pub)...)
}

func testECDSABlob(t *testing.T) []byte {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b := sshString([]byte("ecdsa-sha2-nistp256"))
	b = append(b, sshString([]byte("nistp256"))...)
	return append(b, sshString(k.PublicKey().Bytes())...)
}

func TestFetchHostKeysGetsEveryType(t *testing.T) {
	ed, ec := testEd25519Blob(t), testECDSABlob(t)
	srv := newFakeSSHServer(t, ed, ec)
	keys, err := fetchHostKeys(t.Context(), "127.0.0.1", srv.port())
	if err != nil {
		t.Fatal(err)
	}
	want := srv.keyLines()
	if len(keys) != 2 || keys[0] != want[0] || keys[1] != want[1] {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	// One connection per key family, like ssh-keyscan.
	srv.mu.Lock()
	conns := srv.conns
	srv.mu.Unlock()
	if conns != len(hostKeyFamilies) {
		t.Errorf("connections = %d, want %d", conns, len(hostKeyFamilies))
	}
	if fp := hostKeyFingerprint(keys[0]); !strings.HasPrefix(fp, "SHA256:") || len(fp) != len("SHA256:")+43 {
		t.Errorf("fingerprint %q", fp)
	}
	if hostKeyLabel(keys[0]) != "ED25519" || hostKeyLabel(keys[1]) != "ECDSA" {
		t.Errorf("labels %s %s", hostKeyLabel(keys[0]), hostKeyLabel(keys[1]))
	}
}

func TestFetchHostKeysErrors(t *testing.T) {
	// Nothing listening.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if _, err := fetchHostKeys(t.Context(), "127.0.0.1", port); err == nil || !strings.Contains(err.Error(), "tidak bisa menghubungi") {
		t.Errorf("closed port: %v", err)
	}
	// Not an SSH server.
	notSSH, _ := net.Listen("tcp", "127.0.0.1:0")
	defer notSSH.Close()
	go func() {
		for {
			c, err := notSSH.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
			c.Close()
		}
	}()
	if _, err := fetchHostKeys(t.Context(), "127.0.0.1", notSSH.Addr().(*net.TCPAddr).Port); err == nil {
		t.Error("an HTTP server was accepted as SSH")
	}
}

func TestHostKeyParsingAndKnownHosts(t *testing.T) {
	ed := testEd25519Blob(t)
	line := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(ed)
	// .pub file content, ssh-keyscan output with a comment, duplicates.
	keys, err := parseHostKeys(line + " root@vps\r\n# 203.0.113.5:22 SSH-2.0-OpenSSH_9.6\n[203.0.113.5]:2222 " + line + "\n\n" + line + "\n")
	if err != nil || len(keys) != 1 || keys[0] != line {
		t.Fatalf("parse = %v %v", keys, err)
	}
	for _, bad := range []string{
		"ssh-dss AAAAB3NzaC1kc3MAAACBAP",        // unsupported type
		"ssh-ed25519 !!!notbase64",              // bad base64
		"ssh-rsa " + line[len("ssh-ed25519 "):], // blob type does not match
		"@cert-authority *.example.com " + line, // CA lines
		"just some text",                        // nonsense
	} {
		if _, err := parseHostKeys(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if got := knownHostsContent(keys, 22); !strings.Contains(got, "\n* "+line+"\n") {
		t.Errorf("port 22 known_hosts:\n%s", got)
	}
	if got := knownHostsContent(keys, 2222); !strings.Contains(got, "\n[*]:2222 "+line+"\n") {
		t.Errorf("port 2222 known_hosts:\n%s", got)
	}
	rsa := "ssh-rsa " + base64.StdEncoding.EncodeToString(sshString([]byte("ssh-rsa")))
	if got := hostKeyAlgorithms([]string{rsa, line}); got != "ssh-ed25519 rsa-sha2-512 rsa-sha2-256 ssh-rsa" {
		t.Errorf("algorithms = %q", got)
	}
}

func TestSSHPacketFraming(t *testing.T) {
	for _, n := range []int{0, 1, 3, 7, 8, 100} {
		payload := []byte(strings.Repeat("x", n+1))
		var buf strings.Builder
		if err := writeSSHPacket(&buf, payload); err != nil {
			t.Fatal(err)
		}
		if len(buf.String())%8 != 0 {
			t.Errorf("packet of %d not padded to 8: %d", n, len(buf.String()))
		}
		got, err := readSSHPacket(strings.NewReader(buf.String()))
		if err != nil || string(got) != string(payload) {
			t.Errorf("round trip %d: %q %v", n, got, err)
		}
	}
	if _, err := readSSHPacket(strings.NewReader("\x00\x00\x00\x01\x00")); err == nil {
		t.Error("too short a packet accepted")
	}
	if _, _, ok := readSSHString([]byte{0, 0, 0, 9, 'a'}); ok {
		t.Error("string longer than its buffer accepted")
	}
	if strconv.Itoa(len(sshKexAlgorithms)) == "0" {
		t.Error("no key exchanges offered")
	}
}
