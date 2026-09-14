package main

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Test vector RFC-style untuk PBKDF2-HMAC-SHA256: password "password",
// salt "salt", 4096 iterasi, panjang 32 byte. Ini mengunci pustaka standar yang
// dipakai, bukan kode kita — kalau nilai ini berubah, ada yang salah besar.
func TestPBKDF2StdlibVector(t *testing.T) {
	key, err := pbkdf2.Key(sha256.New, "password", []byte("salt"), 4096, 32)
	if err != nil {
		t.Fatal(err)
	}
	want := "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"
	if got := hex.EncodeToString(key); got != want {
		t.Errorf("PBKDF2-HMAC-SHA256 =\n got %s\nwant %s", got, want)
	}
}

func TestHashAndVerifyPassword(t *testing.T) {
	const pass = "rahasia-panel-2026"

	hash, err := HashPassword(pass)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, hashPrefix+".") {
		t.Errorf("hash tidak memakai prefix yang diharapkan: %q", hash)
	}
	// Hash ini ditempel ke EnvironmentFile dan ke shell, jadi tidak boleh
	// mengandung karakter yang ditelan ekspansi variabel atau quoting.
	if strings.ContainsAny(hash, "$ \t\"'\\`") {
		t.Errorf("hash mengandung karakter yang berbahaya untuk shell/systemd: %q", hash)
	}

	if !VerifyPassword(hash, pass) {
		t.Error("password yang benar ditolak")
	}
	for _, wrong := range []string{"", "salah", pass + "x", strings.ToUpper(pass)} {
		if VerifyPassword(hash, wrong) {
			t.Errorf("password salah %q diterima", wrong)
		}
	}

	// Salt acak: dua hash dari password sama harus berbeda, tapi keduanya sah.
	hash2, err := HashPassword(pass)
	if err != nil {
		t.Fatal(err)
	}
	if hash2 == hash {
		t.Error("dua hash dari password yang sama identik — salt tidak acak")
	}
	if !VerifyPassword(hash2, pass) {
		t.Error("hash kedua tidak bisa diverifikasi")
	}
}

func TestHashPasswordRejectsShort(t *testing.T) {
	if _, err := HashPassword(strings.Repeat("a", minPasswordLen-1)); err == nil {
		t.Error("password pendek harus ditolak")
	}
	if _, err := HashPassword(strings.Repeat("a", minPasswordLen)); err != nil {
		t.Errorf("password dengan panjang minimum ditolak: %v", err)
	}
}

func TestParsePasswordHashRejectsGarbage(t *testing.T) {
	bad := []string{
		"",
		"bukan-hash",
		"pbkdf2-sha256.600000.onlythree",
		"bcrypt.600000.c2FsdA.a2V5",
		"pbkdf2-sha256.nol.c2FsdA.a2V5",
		"pbkdf2-sha256.0.c2FsdA.a2V5",
		"pbkdf2-sha256.600000..a2V5",
		"pbkdf2-sha256.600000.!!!.a2V5",
	}
	for _, s := range bad {
		if _, err := ParsePasswordHash(s); err == nil {
			t.Errorf("ParsePasswordHash(%q) = nil, harusnya error", s)
		}
		if VerifyPassword(s, "apa pun") {
			t.Errorf("VerifyPassword dengan hash rusak %q mengembalikan true", s)
		}
	}
}

func TestSessionStore(t *testing.T) {
	s := newSessionStore()

	id := s.create("admin", "127.0.0.1")
	if id == "" {
		t.Fatal("session id kosong")
	}
	sess, ok := s.get(id)
	if !ok || sess.user != "admin" {
		t.Fatalf("session tidak ditemukan: %+v %v", sess, ok)
	}

	if _, ok := s.get("tidak-ada"); ok {
		t.Error("session palsu diterima")
	}
	if _, ok := s.get(""); ok {
		t.Error("session id kosong diterima")
	}

	s.delete(id)
	if _, ok := s.get(id); ok {
		t.Error("session masih ada setelah dihapus")
	}
	if s.count() != 0 {
		t.Errorf("masih ada %d session", s.count())
	}
}

func TestSessionExpiry(t *testing.T) {
	s := newSessionStore()
	id := s.create("admin", "127.0.0.1")

	// Lewat batas idle.
	s.mu.Lock()
	sess := s.m[id]
	sess.expires = time.Now().Add(-time.Minute)
	s.m[id] = sess
	s.mu.Unlock()

	if _, ok := s.get(id); ok {
		t.Error("session kedaluwarsa masih diterima")
	}

	// Batas mutlak tetap berlaku walau idle-nya terus diperpanjang.
	id = s.create("admin", "127.0.0.1")
	s.mu.Lock()
	sess = s.m[id]
	sess.created = time.Now().Add(-sessionMaxTTL - time.Hour)
	sess.expires = time.Now().Add(time.Hour)
	s.m[id] = sess
	s.mu.Unlock()

	if _, ok := s.get(id); ok {
		t.Error("session melewati batas mutlak masih diterima")
	}
}

func TestSessionSlidesExpiry(t *testing.T) {
	s := newSessionStore()
	id := s.create("admin", "127.0.0.1")

	s.mu.Lock()
	s.m[id] = session{user: "admin", created: time.Now(), expires: time.Now().Add(time.Minute)}
	s.mu.Unlock()

	if _, ok := s.get(id); !ok {
		t.Fatal("session harusnya masih berlaku")
	}
	s.mu.Lock()
	left := time.Until(s.m[id].expires)
	s.mu.Unlock()
	if left < sessionIdleTTL-time.Minute {
		t.Errorf("batas idle tidak diperpanjang, sisa %s", left)
	}
}

func TestLoginLimiter(t *testing.T) {
	l := newLoginLimiter()

	if ok, _ := l.allowed("1.2.3.4"); !ok {
		t.Fatal("percobaan pertama harus diizinkan")
	}
	for i := 0; i < loginMaxFailures-1; i++ {
		l.fail("1.2.3.4")
		if ok, _ := l.allowed("1.2.3.4"); !ok {
			t.Fatalf("terkunci terlalu cepat setelah %d kegagalan", i+1)
		}
	}
	l.fail("1.2.3.4")

	ok, wait := l.allowed("1.2.3.4")
	if ok {
		t.Errorf("harus terkunci setelah %d kegagalan", loginMaxFailures)
	}
	if wait <= 0 || wait > loginLockout {
		t.Errorf("sisa waktu tunggu aneh: %s", wait)
	}

	// Penguncian hanya berlaku untuk key yang gagal.
	if ok, _ := l.allowed("5.6.7.8"); !ok {
		t.Error("alamat lain ikut terkunci")
	}

	// Login berhasil menghapus catatannya.
	l.reset("1.2.3.4")
	if ok, _ := l.allowed("1.2.3.4"); !ok {
		t.Error("reset tidak membuka kunci")
	}
}

func TestLoginKeyTrustsForwardedForOnlyFromLoopback(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{"tanpa proxy", "203.0.113.9:5000", "", "203.0.113.9"},
		{"XFF dari peer non-loopback diabaikan", "203.0.113.9:5000", "1.1.1.1", "203.0.113.9"},
		{"XFF dari loopback dipakai", "127.0.0.1:5000", "198.51.100.7", "198.51.100.7"},
		// Caddy menambahkan IP asli di akhir; entri sebelumnya bisa dipalsukan.
		{"ambil entri terakhir", "127.0.0.1:5000", "1.1.1.1, 198.51.100.7", "198.51.100.7"},
		{"XFF sampah diabaikan", "127.0.0.1:5000", "bukan-ip", "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/login", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := loginKey(r); got != tc.want {
				t.Errorf("loginKey = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSafeNextBlocksOpenRedirect(t *testing.T) {
	safe := map[string]string{
		"/objects?bucket=media": "/objects?bucket=media",
		"/domains":              "/domains",
	}
	for in, want := range safe {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
	unsafe := []string{
		"",
		"https://evil.example.com",
		"//evil.example.com",
		"/\\evil.example.com",
		"http://evil.example.com/x",
		"/login",
		"/login?next=/x",
		"/x\r\nSet-Cookie: a=b",
		"javascript:alert(1)",
	}
	for _, in := range unsafe {
		if got := safeNext(in); got != "/buckets" {
			t.Errorf("safeNext(%q) = %q, harusnya jatuh ke /buckets", in, got)
		}
	}
}

func TestIsSecureRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if isSecureRequest(r) {
		t.Error("http biasa tidak boleh dianggap secure")
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if !isSecureRequest(r) {
		t.Error("X-Forwarded-Proto: https harus dianggap secure")
	}
}
