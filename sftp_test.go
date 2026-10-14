package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testSFTPPassword = "rahasia VPS 'root' $1"

// testHostKey is a pinned ed25519 host key line ("ssh-ed25519 AAAA…").
func testHostKey(t *testing.T) string {
	t.Helper()
	k, err := hostKeyFromBlob(testEd25519Blob(t))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sftpTestRemote(t *testing.T, name string) Remote {
	return Remote{Name: name, Type: RemoteTypeSFTP, Host: "203.0.113.10", Port: 22, User: "root",
		Password: testSFTPPassword, HostKeys: []string{testHostKey(t)}}
}

// envValue returns the value of key in a newline-joined environment.
func envValue(env, key string) string {
	for _, line := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return v
		}
	}
	return ""
}

func TestRcloneObscureMatchesRclone(t *testing.T) {
	for _, pw := range []string{"p", testSFTPPassword, "pässwörd", strings.Repeat("x", maxSFTPPassword)} {
		a, b := rcloneObscure(pw), rcloneObscure(pw)
		if a == b {
			t.Errorf("obscure(%q) is deterministic; rclone uses a random IV", pw)
		}
		if len(pw) >= 8 && strings.Contains(a, pw) { // a one-letter password can appear in base64 by chance
			t.Errorf("obscured value contains the password")
		}
		for _, ob := range []string{a, b} {
			if got, err := rcloneReveal(ob); err != nil || got != pw {
				t.Errorf("reveal(obscure(%q)) = %q, %v", pw, got, err)
			}
		}
	}
	for _, bad := range []string{"", "!!!", "AAAA"} {
		if _, err := rcloneReveal(bad); err == nil {
			t.Errorf("reveal(%q) accepted", bad)
		}
	}

	bin := realRcloneBin()
	if bin == "" {
		t.Skip("no rclone binary for the cross-check (set GARAGEPANEL_TEST_RCLONE)")
	}
	out, err := exec.Command(bin, "reveal", rcloneObscure(testSFTPPassword), "--config", "").Output()
	if err != nil || strings.TrimSuffix(string(out), "\n") != testSFTPPassword {
		t.Errorf("rclone reveal of the panel's value = %q, %v", out, err)
	}
	out, err = exec.Command(bin, "obscure", testSFTPPassword, "--config", "").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rcloneReveal(strings.TrimSpace(string(out))); err != nil || got != testSFTPPassword {
		t.Errorf("panel reveal of rclone's value = %q, %v", got, err)
	}
}

func TestPemOneLineIsWhatRcloneWants(t *testing.T) {
	got := pemOneLine("-----BEGIN PRIVATE KEY-----\r\nAAAA\r\nBBBB\r\n-----END PRIVATE KEY-----\r\n\r\n")
	if want := `-----BEGIN PRIVATE KEY-----\nAAAA\nBBBB\n-----END PRIVATE KEY-----\n`; got != want {
		t.Errorf("pemOneLine = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Error("a real newline makes rclone fail with: pem key not formatted properly")
	}
}

func TestPrivateKeysGeneratedAndPasted(t *testing.T) {
	priv, pub, err := generateSFTPKey("garagepanel-vps@panel")
	if err != nil {
		t.Fatal(err)
	}
	// What the panel generates is accepted, and the same key pasted from
	// Windows with stray blank lines normalises to the same text.
	got, err := validatePrivateKey("\n  " + strings.ReplaceAll(priv, "\n", "\r\n") + "\r\n\r\n")
	if err != nil || got != priv {
		t.Fatalf("generated key: %q, %v", got, err)
	}
	// The authorized_keys line belongs to that private key.
	block, _ := pem.Decode([]byte(priv))
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	edPub := k.(ed25519.PrivateKey).Public().(ed25519.PublicKey)
	blob := append(sshString([]byte("ssh-ed25519")), sshString(edPub)...)
	if want := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " garagepanel-vps@panel"; pub != want {
		t.Errorf("public key = %q, want %q", pub, want)
	}
	if cmd := sftpInstallCommand(pub); !strings.Contains(cmd, "echo '"+pub+"' >> ~/.ssh/authorized_keys") || !strings.HasPrefix(cmd, "mkdir -p ~/.ssh && chmod 700 ~/.ssh") {
		t.Errorf("install command: %s", cmd)
	}
	// The comment ends up inside that copy-paste command, so it stays tame.
	if c := sftpKeyComment("vps-lama"); !strings.HasPrefix(c, "garagepanel-vps-lama@") || strings.ContainsAny(c, "' ;$`\"\\\n") {
		t.Errorf("key comment %q", c)
	}

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(typ string, b []byte, headers map[string]string) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: typ, Headers: headers, Bytes: b}))
	}
	openssh := func(cipher string) string {
		return encode("OPENSSH PRIVATE KEY", append([]byte("openssh-key-v1\x00"), sshString([]byte(cipher))...), nil)
	}
	for name, text := range map[string]string{
		"pkcs1 rsa":       encode("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaKey), nil),
		"sec1 ec":         encode("EC PRIVATE KEY", ecDER, nil),
		"openssh no pass": openssh("none"),
	} {
		if _, err := validatePrivateKey(text); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	for name, c := range map[string]struct{ text, want string }{
		"empty":                   {"  \n", "kosong"},
		"public key pasted":       {pub, "bukan format PEM"},
		"openssh with passphrase": {openssh("aes256-ctr"), "passphrase"},
		"legacy encrypted pem": {encode("RSA PRIVATE KEY", []byte("x"),
			map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00112233445566778899AABBCCDDEEFF"}), "passphrase"},
		"pkcs8 encrypted": {encode("ENCRYPTED PRIVATE KEY", []byte("x"), nil), "passphrase"},
		"two keys":        {priv + priv, "satu kunci"},
		"certificate":     {encode("CERTIFICATE", []byte("x"), nil), "tidak didukung"},
		"broken pkcs8":    {encode("PRIVATE KEY", []byte("not der"), nil), "PKCS#8"},
		"broken openssh":  {encode("OPENSSH PRIVATE KEY", []byte("nope"), nil), "rusak"},
		"too long":        {encode("PRIVATE KEY", make([]byte, maxPrivateKeyLen), nil), "terlalu panjang"},
	} {
		if _, err := validatePrivateKey(c.text); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestSSHHostUserAndPortRules(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.10":      "203.0.113.10",
		" VPS.Example.COM ": "vps.example.com",
		"[2001:db8::1]":     "2001:db8::1",
		"2001:DB8::1":       "2001:db8::1",
		"127.0.0.1":         "127.0.0.1", // a folder on the panel's own server
		"localhost":         "localhost",
	} {
		if got, err := ValidateSSHHost(in); err != nil || got != want {
			t.Errorf("ValidateSSHHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "root@203.0.113.10", "sftp://vps.example.com", "203.0.113.10:22", "vps.example.com:22",
		"169.254.169.254", "fe80::1", "0.0.0.0", "::", "224.0.0.1", "-vps.example.com", "vps..example.com",
		"vps_1.example.com", "vps example", "vps;reboot", strings.Repeat("a", 64) + ".com"} {
		if _, err := ValidateSSHHost(bad); err == nil {
			t.Errorf("ValidateSSHHost(%q) accepted", bad)
		}
	}
	for _, ok := range []string{"root", "deploy", "www-data", "first.last", "_svc", "User1"} {
		if err := ValidateSSHUser(ok); err != nil {
			t.Errorf("user %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-oProxyCommand=sh", ".hidden", "a b", "a;b", "a@b", "a/b", "a:b", strings.Repeat("u", 65)} {
		if ValidateSSHUser(bad) == nil {
			t.Errorf("user %q accepted", bad)
		}
	}
	for _, p := range []int{0, -1, 65536} {
		if ValidateSSHPort(p) == nil {
			t.Errorf("port %d accepted", p)
		}
	}
	if ValidateSSHPort(22) != nil || ValidateSSHPort(2222) != nil {
		t.Error("valid ports refused")
	}
}

func TestSFTPFolderRules(t *testing.T) {
	for in, want := range map[string]string{
		"/var/www/uploads":       "var/www/uploads/",
		"/var/www/uploads/":      "var/www/uploads/",
		"  /srv/data  ":          "srv/data/",
		"/var/www":               "var/www/",
		"/home/budi/public_html": "home/budi/public_html/",
		"/root/backup":           "root/backup/",
		"/mnt/hdd/komik 2026":    "mnt/hdd/komik 2026/",
		"/data":                  "data/",
	} {
		got, err := NormalizeSFTPFolder(in)
		if err != nil || got != want {
			t.Errorf("NormalizeSFTPFolder(%q) = %q, %v; want %q", in, got, err, want)
			continue
		}
		if err := ValidateSFTPFolder(got); err != nil {
			t.Errorf("stored form %q refused: %v", got, err)
		}
	}
	for in, want := range map[string]string{
		"":                 "tidak boleh kosong",
		"var/www":          "path absolut",
		"/":                `tidak boleh "/"`,
		"//":               `tidak boleh "/"`,
		"/var//www":        `"//"`,
		"/etc":             "folder sistem",
		"/etc/nginx":       "folder sistem",
		"/usr/local/share": "folder sistem",
		"/boot/grub":       "folder sistem",
		"/proc/1":          "folder sistem",
		"/root":            "terlalu umum",
		"/home":            "terlalu umum",
		"/var":             "terlalu umum",
		"/home/budi":       "home directory",
		"/home/budi/.ssh":  "kunci rahasia",
		"/srv/x/.ssh/keys": "kunci rahasia",
		"/root/.gnupg":     "kunci rahasia",
		"/var/lib":         "state layanan",
		"/var/lib/mysql":   "state layanan",
		"/var/spool/cron":  "state layanan",
		"/var/backups":     "shadow",
		"/srv/../etc":      `".."`,
		"/srv/./data":      `"."`,
		"/srv/a\nb":        "karakter kontrol",
	} {
		if _, err := NormalizeSFTPFolder(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("NormalizeSFTPFolder(%q) err = %v, want %q", in, err, want)
		}
	}
	// The stored form is checked again when a job is created or resumed.
	for _, bad := range []string{"", "/srv/", "srv", "etc/", "home/budi/", "srv/.ssh/"} {
		if ValidateSFTPFolder(bad) == nil {
			t.Errorf("ValidateSFTPFolder(%q) accepted", bad)
		}
	}
}

func TestEndpointFolderSemantics(t *testing.T) {
	folder := Endpoint{Remote: "vps", Prefix: "var/www/uploads/"}
	if !folder.IsFolder() || folder.String() != "vps:/var/www/uploads/" || folder.rclonePath() != "/var/www/uploads/" {
		t.Errorf("folder side: %v %q %q", folder.IsFolder(), folder.String(), folder.rclonePath())
	}
	bucket := Endpoint{Remote: "wasabi", Bucket: "backup", Prefix: "in/"}
	if bucket.IsFolder() || bucket.rclonePath() != "backup/in/" || bucket.String() != "wasabi:backup/in/" {
		t.Errorf("bucket side: %q %q", bucket.rclonePath(), bucket.String())
	}
	garage := Endpoint{Bucket: "media"}
	if garage.IsFolder() || garage.empty() || !(Endpoint{}).empty() || folder.empty() {
		t.Error("empty/IsFolder wrong")
	}
	for _, c := range []struct {
		a, b Endpoint
		want bool
	}{
		{folder, Endpoint{Remote: "vps", Prefix: "var/www/"}, true},              // parent
		{folder, Endpoint{Remote: "vps", Prefix: "var/www/uploads/2026/"}, true}, // child
		{folder, folder, true}, // same
		{folder, Endpoint{Remote: "vps", Prefix: "var/www/uploads-old/"}, false}, // sibling sharing a name prefix
		{folder, Endpoint{Remote: "vps-2", Prefix: "var/www/uploads/"}, false},   // other server
		{folder, garage, false}, // a bucket
		{bucket, Endpoint{Remote: "wasabi", Bucket: "backup", Prefix: "x/"}, true}, // same bucket (as before)
		{garage, Endpoint{Bucket: "media", Prefix: "a/"}, true},
	} {
		if got := c.a.conflicts(c.b); got != c.want {
			t.Errorf("%s vs %s: conflicts = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := c.b.conflicts(c.a); got != c.want {
			t.Errorf("%s vs %s (reversed): conflicts = %v, want %v", c.b, c.a, got, c.want)
		}
	}
	if folder.conflictLabel() != "folder vps:/var/www/uploads/" || !strings.HasPrefix(garage.conflictLabel(), "bucket ") {
		t.Errorf("labels: %q %q", folder.conflictLabel(), garage.conflictLabel())
	}
}

func TestSortedListingMergesRunsInByteOrder(t *testing.T) {
	old := sortRunEntries
	sortRunEntries = 3 // force several runs on disk
	t.Cleanup(func() { sortRunEntries = old })

	dir := t.TempDir()
	w := newSortedListingWriter(dir, "listing-src")
	// The order rclone lists a tree in: directory by directory.
	in := []string{"b/2.jpg", "a.txt", "Z.txt", "b/1.jpg", "a-b.txt", "a/b.txt", "é.txt", "b/1.jpg", "c.txt", "a b.txt"}
	for i, rel := range in {
		if err := w.add(rel, int64(100+i)); err != nil {
			t.Fatal(err)
		}
	}
	path, err := w.finish()
	if err != nil {
		t.Fatal(err)
	}
	if runs, _ := filepath.Glob(filepath.Join(dir, "listing-src.run*")); len(runs) != 0 {
		t.Errorf("run files left behind: %v", runs)
	}
	collect := func(startAfter string) ([]string, map[string]int64) {
		it, err := openFileIterator(path, "pre/", startAfter)
		if err != nil {
			t.Fatal(err)
		}
		defer it.Close()
		var keys []string
		sizes := map[string]int64{}
		for {
			e, err := it.peek(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if e == nil {
				return keys, sizes
			}
			keys = append(keys, e.Key)
			sizes[e.Key] = e.Size
			it.advance()
		}
	}
	keys, sizes := collect("")
	// Byte order, like an S3 listing; the duplicate appears once.
	want := []string{"pre/Z.txt", "pre/a b.txt", "pre/a-b.txt", "pre/a.txt", "pre/a/b.txt", "pre/b/1.jpg", "pre/b/2.jpg", "pre/c.txt", "pre/é.txt"}
	if strings.Join(keys, "|") != strings.Join(want, "|") {
		t.Errorf("keys = %q\nwant   %q", keys, want)
	}
	if sizes["pre/a.txt"] != 101 || sizes["pre/c.txt"] != 108 {
		t.Errorf("sizes lost: %v", sizes)
	}
	// A checkpoint skips everything up to and including it.
	if keys, _ := collect("pre/a.txt"); len(keys) == 0 || keys[0] != "pre/a/b.txt" || len(keys) != 5 {
		t.Errorf("after checkpoint: %q", keys)
	}

	// One run: renamed, still without duplicates.
	sortRunEntries = 100
	w = newSortedListingWriter(dir, "listing-one")
	for _, rel := range []string{"b", "a", "b"} {
		_ = w.add(rel, 1)
	}
	one, err := w.finish()
	if err != nil {
		t.Fatal(err)
	}
	r, err := openListingReader(one)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		e, ok, err := r.next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got = append(got, e.rel)
	}
	r.Close()
	if strings.Join(got, ",") != "a,b" {
		t.Errorf("single run = %v", got)
	}

	// Nothing listed: an empty file, an iterator that is done at once.
	empty, err := newSortedListingWriter(dir, "listing-dst").finish()
	if err != nil {
		t.Fatal(err)
	}
	it, err := openFileIterator(empty, "x/", "")
	if err != nil {
		t.Fatal(err)
	}
	if e, err := it.peek(t.Context()); e != nil || err != nil {
		t.Errorf("empty listing: %v %v", e, err)
	}
	it.Close()

	// A truncated file is an error, not a short listing.
	if err := os.WriteFile(filepath.Join(dir, "bad"), []byte{5, 'a', 'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	it, _ = openFileIterator(filepath.Join(dir, "bad"), "", "")
	if _, err := it.peek(t.Context()); err == nil {
		t.Error("truncated listing accepted")
	}
	it.Close()
	// A cancelled job stops walking.
	it, _ = openFileIterator(path, "pre/", "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := it.peek(ctx); err == nil {
		t.Error("peek ignored a cancelled context")
	}
	it.Close()
}

func TestSFTPRemoteRendersForRcloneWithoutPlainSecrets(t *testing.T) {
	key := testHostKey(t)
	r := Remote{Name: "vps", Type: RemoteTypeSFTP, Host: "203.0.113.10", Port: 2222, User: "root",
		Password: testSFTPPassword, HostKeys: []string{key}, knownHostsPath: "/state/known_hosts/vps"}
	env := strings.Join(r.rcloneEnv("src"), "\n")
	for key, want := range map[string]string{
		"RCLONE_CONFIG_SRC_TYPE":                "sftp",
		"RCLONE_CONFIG_SRC_HOST":                "203.0.113.10",
		"RCLONE_CONFIG_SRC_PORT":                "2222",
		"RCLONE_CONFIG_SRC_USER":                "root",
		"RCLONE_CONFIG_SRC_KNOWN_HOSTS_FILE":    "/state/known_hosts/vps",
		"RCLONE_CONFIG_SRC_KEY_USE_AGENT":       "false",
		"RCLONE_CONFIG_SRC_HOST_KEY_ALGORITHMS": "ssh-ed25519",
	} {
		if got := envValue(env, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if strings.Contains(env, testSFTPPassword) || strings.Contains(env, "KEY_PEM") {
		t.Errorf("plain password or key in env:\n%s", env)
	}
	if got, err := rcloneReveal(envValue(env, "RCLONE_CONFIG_SRC_PASS")); err != nil || got != testSFTPPassword {
		t.Errorf("PASS does not reveal to the password: %q %v", got, err)
	}

	// Key login: the key travels as one line, and there is no password.
	priv, _, err := generateSFTPKey("x")
	if err != nil {
		t.Fatal(err)
	}
	r.Password, r.PrivateKey = "", priv
	env = strings.Join(r.rcloneEnv("dst"), "\n")
	if got := envValue(env, "RCLONE_CONFIG_DST_KEY_PEM"); got != pemOneLine(priv) || strings.Contains(env, "_PASS=") {
		t.Errorf("key env:\n%s", env)
	}

	// No known_hosts file and no pinned keys: rclone is pointed at a file
	// that does not exist and refuses the connection instead of trusting it.
	r.knownHostsPath, r.HostKeys = "", nil
	env = strings.Join(r.rcloneEnv("my-alias"), "\n")
	if !strings.HasPrefix(envValue(env, "RCLONE_CONFIG_MY_ALIAS_KNOWN_HOSTS_FILE"), "/nonexistent/") || strings.Contains(env, "HOST_KEY_ALGORITHMS") {
		t.Errorf("fail-closed env:\n%s", env)
	}
}

func TestSFTPRemoteValidation(t *testing.T) {
	base := sftpTestRemote(t, "vps")
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	priv, _, err := generateSFTPKey("x")
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Remote){
		"password and key":    func(r *Remote) { r.PrivateKey = priv },
		"no credential":       func(r *Remote) { r.Password = "" },
		"newline in password": func(r *Remote) { r.Password = "a\nb" },
		"NUL in password":     func(r *Remote) { r.Password = "a\x00b" },
		"long password":       func(r *Remote) { r.Password = strings.Repeat("p", maxSFTPPassword+1) },
		"bad host key":        func(r *Remote) { r.HostKeys = []string{"ssh-ed25519 AAAA"} },
		"too many host keys": func(r *Remote) {
			for len(r.HostKeys) <= maxPinnedHostKeys {
				r.HostKeys = append(r.HostKeys, r.HostKeys[0])
			}
		},
		"s3 endpoint":  func(r *Remote) { r.Endpoint = "https://s3.example.com" },
		"s3 key":       func(r *Remote) { r.AccessKey = "AKIA" },
		"bad host":     func(r *Remote) { r.Host = "root@vps" },
		"bad port":     func(r *Remote) { r.Port = 0 },
		"bad user":     func(r *Remote) { r.User = "-o" },
		"bad name":     func(r *Remote) { r.Name = "VPS!" },
		"unknown type": func(r *Remote) { r.Type = "ftp" },
	} {
		r := base
		r.HostKeys = append([]string{}, base.HostKeys...)
		mutate(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// An S3 remote never carries SSH credentials.
	s3 := testRemote("wasabi")
	s3.Password = "x"
	if err := s3.Validate(); err == nil {
		t.Error("S3 remote with a password accepted")
	}

	// normaliseRemote fills the defaults and tidies pasted input.
	in := Remote{Name: " vps ", Type: RemoteTypeSFTP, Host: "VPS.Example.COM", User: " root ", Password: "pw\r\n",
		Provider: "Wasabi", Region: "x", Addressing: "path", PublicKey: "stale"}
	got, err := normaliseRemote(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "vps" || got.Host != "vps.example.com" || got.Port != 22 || got.User != "root" || got.Password != "pw" ||
		got.Provider != "" || got.Region != "" || got.Addressing != "" || got.PublicKey != "" {
		t.Errorf("normalised = %+v", got)
	}
	in.Name = garageRemoteName
	if _, err := normaliseRemote(in); err == nil {
		t.Error("an SFTP remote may not take the name of the panel's own Garage")
	}
}

func TestRcloneProblemAndSFTPHints(t *testing.T) {
	// Real rclone v1.75 output, captured against `rclone serve sftp`.
	auth := rcloneLogTail("2026/10/14 13:08:30 CRITICAL: Failed to create file system for \"probe:/var/www/\": NewFs: couldn't connect SSH: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none password], no supported methods remain\n")
	if got := rcloneProblem(auth); got != "ssh: handshake failed: ssh: unable to authenticate, attempted methods [none password], no supported methods remain" {
		t.Errorf("auth problem = %q", got)
	}
	missing := rcloneLogTail("2026/10/14 13:08:29 ERROR : error listing: directory not found\n2026/10/14 13:08:29 NOTICE: Failed to lsjson with 2 errors: last error was: error in ListJSON: directory not found\n")
	if got := rcloneProblem(missing); got != "error listing: directory not found" {
		t.Errorf("missing problem = %q", got)
	}
	if rcloneProblem(nil) == "" {
		t.Error("no lines must still explain something")
	}

	root := Remote{User: "root", Password: "x", Port: 2222}
	withKey := Remote{User: "deploy", PrivateKey: "k", Port: 22}
	user := Remote{User: "deploy", Password: "x", Port: 22}
	for _, c := range []struct {
		msg  string
		r    Remote
		want string
	}{
		{"ssh: unable to authenticate", root, "PermitRootLogin prohibit-password"},
		{"ssh: unable to authenticate", withKey, "authorized_keys milik user deploy"},
		{"ssh: unable to authenticate", user, "password salah"},
		{"ssh: handshake failed: knownhosts: key mismatch", user, "TIDAK SAMA"},
		{"ssh: handshake failed: knownhosts: key is unknown", user, "belum disematkan"},
		{"dial tcp 203.0.113.10:22: connect: connection refused", user, "Port SSH tertutup"},
		{"dial tcp: lookup vps.example.com: no such host", user, "DNS"},
		{"dial tcp 203.0.113.10:2222: i/o timeout", root, "port 2222"},
		{"error listing: directory not found", user, "Folder tidak ada"},
		{"private: error listing: permission denied", user, "tidak punya izin"},
	} {
		if got := sftpHint(c.msg, c.r); !strings.Contains(got, c.want) {
			t.Errorf("hint for %q = %q, want %q", c.msg, got, c.want)
		}
	}
	if got := sftpHint("something else entirely", user); got != "" {
		t.Errorf("unexpected hint %q", got)
	}
	err := sftpError(root, "vps:/srv/data/", auth)
	if !strings.HasPrefix(err.Error(), "vps:/srv/data/: ssh: handshake failed") || !strings.Contains(err.Error(), "\nLogin ditolak") {
		t.Errorf("sftpError = %q", err)
	}
}

func TestRemoteStorePinsHostKeysInPrivateKnownHostsFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remotes.json")
	store, err := NewRemoteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	r := sftpTestRemote(t, "vps")
	r.Port = 2222
	if err := store.Add(r); err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(dir, "known_hosts", "vps")
	raw, err := os.ReadFile(kh)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\n[*]:2222 "+r.HostKeys[0]+"\n") {
		t.Errorf("known_hosts:\n%s", raw)
	}
	if info, _ := os.Stat(kh); info.Mode().Perm() != 0o600 {
		t.Errorf("known_hosts mode = %o", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Dir(kh)); info.Mode().Perm() != 0o700 {
		t.Errorf("known_hosts dir mode = %o", info.Mode().Perm())
	}
	got, _ := store.Get("vps")
	if got.knownHostsPath != kh || envValue(strings.Join(got.rcloneEnv("src"), "\n"), "RCLONE_CONFIG_SRC_KNOWN_HOSTS_FILE") != kh {
		t.Errorf("rclone is not pointed at %s", kh)
	}

	// The view shows fingerprints and the server, never the password.
	v := got.View()
	if !v.IsSFTP || v.Type != RemoteTypeSFTP || v.Endpoint != "sftp://root@203.0.113.10:2222" || len(v.HostKeys) != 1 ||
		v.HostKeys[0].Type != "ED25519" || v.HostKeys[0].Fingerprint != hostKeyFingerprint(r.HostKeys[0]) || v.InstallCmd != "" || v.UsesKey {
		t.Errorf("view = %+v", v)
	}
	if b, _ := json.Marshal(v); strings.Contains(string(b), testSFTPPassword) {
		t.Error("the view carries the password")
	}

	// known_hosts files are derived data, rewritten from remotes.json.
	if err := os.Remove(kh); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRemoteStore(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kh); err != nil {
		t.Errorf("known_hosts not rewritten on start: %v", err)
	}

	// A remote keeps its kind.
	if err := store.Update("vps", testRemote("vps")); err == nil {
		t.Error("an SFTP remote was turned into an S3 one")
	}
	if err := store.Add(testRemote("wasabi")); err != nil {
		t.Fatal(err)
	}
	if err := store.Update("wasabi", sftpTestRemote(t, "wasabi")); err == nil {
		t.Error("an S3 remote was turned into an SFTP one")
	}

	// Moving to port 22 rewrites the host pattern; creation time stays.
	upd := got
	upd.Port = 22
	if err := store.Update("vps", upd); err != nil {
		t.Fatal(err)
	}
	if raw, _ = os.ReadFile(kh); !strings.Contains(string(raw), "\n* "+r.HostKeys[0]+"\n") {
		t.Errorf("known_hosts after port change:\n%s", raw)
	}
	if after, _ := store.Get("vps"); !after.CreatedAt.Equal(got.CreatedAt) || after.UpdatedAt.IsZero() {
		t.Errorf("timestamps: created %v→%v updated %v", got.CreatedAt, after.CreatedAt, after.UpdatedAt)
	}

	// Without pinned keys the file holds no key: rclone refuses the server.
	upd.HostKeys = nil
	if err := store.Update("vps", upd); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(kh)
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			t.Errorf("unpinned remote has a known_hosts entry: %q", line)
		}
	}

	// First contact (Tes koneksi) pins what the server presented…
	other := testHostKey(t)
	if pinned, err := store.pinHostKeys("vps", upd.Host, upd.Port, []string{other}); err != nil || !pinned {
		t.Fatalf("first pin: %v %v", pinned, err)
	}
	if raw, _ = os.ReadFile(kh); !strings.Contains(string(raw), "\n* "+other+"\n") || strings.Contains(string(raw), r.HostKeys[0]) {
		t.Errorf("known_hosts after pin:\n%s", raw)
	}
	if after, _ := store.Get("vps"); after.HostKeysAt.IsZero() || len(after.HostKeys) != 1 || after.HostKeys[0] != other {
		t.Errorf("pin not recorded: %+v", after.HostKeys)
	}
	// …but a slower, concurrent test never replaces keys pinned meanwhile,
	// and keys fetched from a server the remote no longer points at (it was
	// edited during the test) are refused.
	if pinned, err := store.pinHostKeys("vps", upd.Host, upd.Port, []string{r.HostKeys[0]}); err != nil || pinned {
		t.Errorf("a second pin replaced the first: %v %v", pinned, err)
	}
	if _, err := store.pinHostKeys("vps", "198.51.100.7", upd.Port, []string{r.HostKeys[0]}); err == nil {
		t.Error("keys fetched from another server were pinned")
	}
	if after, _ := store.Get("vps"); len(after.HostKeys) != 1 || after.HostKeys[0] != other {
		t.Errorf("pinned keys changed: %v", after.HostKeys)
	}

	// Deleting the remote deletes its file.
	if err := store.Delete("vps"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kh); !os.IsNotExist(err) {
		t.Errorf("known_hosts left after delete: %v", err)
	}
}
