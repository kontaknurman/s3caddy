package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Login bawaan panel.
//
// Password di-hash dengan PBKDF2-HMAC-SHA256 dari pustaka standar
// (crypto/pbkdf2, Go 1.24+), jadi tidak ada dependency eksternal seperti bcrypt
// atau argon2. Jumlah iterasinya mengikuti anjuran OWASP untuk PBKDF2-SHA256.
//
// Session disimpan di memori: restart panel = semua orang logout. Itu memang
// diinginkan untuk panel sekecil ini — tidak ada state yang perlu dipersistensi.

const (
	// pbkdf2Iterations mengikuti anjuran OWASP Password Storage Cheat Sheet
	// untuk PBKDF2-HMAC-SHA256.
	pbkdf2Iterations = 600_000
	pbkdf2SaltLen    = 16
	pbkdf2KeyLen     = 32

	// hashPrefix memakai titik sebagai pemisah, bukan "$" seperti format PHC.
	// Hash ini ditempel ke EnvironmentFile systemd dan ke perintah shell, dan
	// "$600000" akan hilang ditelan ekspansi variabel shell.
	hashPrefix = "pbkdf2-sha256"

	minPasswordLen = 12

	sessionCookieName = "garagepanel_session"
	// Session diperpanjang setiap request sampai batas idle ini,
	// dengan batas mutlak sejak login.
	sessionIdleTTL = 12 * time.Hour
	sessionMaxTTL  = 7 * 24 * time.Hour

	loginMaxFailures = 5
	loginLockout     = 15 * time.Minute
)

var b64 = base64.RawURLEncoding

// --- hashing --------------------------------------------------------------

// HashPassword menghasilkan string hash yang siap dipasang ke
// PANEL_PASSWORD_HASH.
func HashPassword(password string) (string, error) {
	if utf8.RuneCountInString(password) < minPasswordLen {
		return "", fmt.Errorf("password minimal %d karakter", minPasswordLen)
	}
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("tidak bisa membuat salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyLen)
	if err != nil {
		return "", fmt.Errorf("tidak bisa menghitung hash: %w", err)
	}
	return strings.Join([]string{
		hashPrefix,
		strconv.Itoa(pbkdf2Iterations),
		b64.EncodeToString(salt),
		b64.EncodeToString(key),
	}, "."), nil
}

type parsedHash struct {
	iterations int
	salt       []byte
	key        []byte
}

// ParsePasswordHash memeriksa bentuk hash tanpa memverifikasi password apa pun,
// supaya panel bisa menolak start saat PANEL_PASSWORD_HASH salah bentuk —
// bukan baru ketahuan waktu ada yang mencoba login.
func ParsePasswordHash(encoded string) (*parsedHash, error) {
	parts := strings.Split(strings.TrimSpace(encoded), ".")
	if len(parts) != 4 || parts[0] != hashPrefix {
		return nil, fmt.Errorf("format hash tidak dikenal, harus %s.<iterasi>.<salt>.<key> — buat ulang dengan: garagepanel -hash-password", hashPrefix)
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 {
		return nil, errors.New("jumlah iterasi pada hash tidak valid")
	}
	salt, err := b64.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return nil, errors.New("salt pada hash tidak valid")
	}
	key, err := b64.DecodeString(parts[3])
	if err != nil || len(key) == 0 {
		return nil, errors.New("key pada hash tidak valid")
	}
	return &parsedHash{iterations: iterations, salt: salt, key: key}, nil
}

// VerifyPassword membandingkan password dengan hash tersimpan dalam waktu
// konstan.
func VerifyPassword(encoded, password string) bool {
	parsed, err := ParsePasswordHash(encoded)
	if err != nil {
		return false
	}
	key, err := pbkdf2.Key(sha256.New, password, parsed.salt, parsed.iterations, len(parsed.key))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(key, parsed.key) == 1
}

// --- session --------------------------------------------------------------

type session struct {
	user     string
	created  time.Time
	expires  time.Time
	clientIP string
}

type sessionStore struct {
	mu sync.Mutex
	m  map[string]session
}

func newSessionStore() *sessionStore { return &sessionStore{m: map[string]session{}} }

func (s *sessionStore) create(user, clientIP string) string {
	id := randomHex(32)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(now)
	s.m[id] = session{user: user, created: now, expires: now.Add(sessionIdleTTL), clientIP: clientIP}
	return id
}

// get mengembalikan session yang masih berlaku dan sekaligus memperpanjang
// batas idle-nya.
func (s *sessionStore) get(id string) (session, bool) {
	if id == "" {
		return session{}, false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return session{}, false
	}
	if now.After(sess.expires) || now.After(sess.created.Add(sessionMaxTTL)) {
		delete(s.m, id)
		return session{}, false
	}
	sess.expires = now.Add(sessionIdleTTL)
	s.m[id] = sess
	return sess, true
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

func (s *sessionStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

func (s *sessionStore) gcLocked(now time.Time) {
	for id, sess := range s.m {
		if now.After(sess.expires) || now.After(sess.created.Add(sessionMaxTTL)) {
			delete(s.m, id)
		}
	}
}

// --- pembatas percobaan login ---------------------------------------------

type attemptRecord struct {
	failures    int
	lockedUntil time.Time
	lastFailure time.Time
}

// loginLimiter menahan brute force sekaligus melindungi CPU: satu verifikasi
// PBKDF2 600k iterasi itu mahal, jadi request yang membanjiri /login tidak
// boleh dibiarkan memaksa panel menghitungnya berkali-kali.
type loginLimiter struct {
	mu sync.Mutex
	m  map[string]*attemptRecord
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{m: map[string]*attemptRecord{}} }

// allowed melaporkan apakah key ini boleh mencoba login, dan kalau tidak,
// berapa lama lagi harus menunggu.
func (l *loginLimiter) allowed(key string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked(now)
	rec, ok := l.m[key]
	if !ok {
		return true, 0
	}
	if now.Before(rec.lockedUntil) {
		return false, rec.lockedUntil.Sub(now).Round(time.Second)
	}
	return true, 0
}

func (l *loginLimiter) fail(key string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.m[key]
	if !ok {
		rec = &attemptRecord{}
		l.m[key] = rec
	}
	rec.failures++
	rec.lastFailure = now
	if rec.failures >= loginMaxFailures {
		rec.lockedUntil = now.Add(loginLockout)
		rec.failures = 0
	}
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}

func (l *loginLimiter) gcLocked(now time.Time) {
	for key, rec := range l.m {
		if now.After(rec.lockedUntil) && now.Sub(rec.lastFailure) > loginLockout {
			delete(l.m, key)
		}
	}
}

// --- helper request -------------------------------------------------------

// loginKey memilih kunci pembatas untuk sebuah request.
//
// Kalau panel diakses lewat reverse proxy di mesin yang sama (Caddy), semua
// request datang dari 127.0.0.1, jadi RemoteAddr saja tidak membedakan siapa
// pun. X-Forwarded-For hanya dipercaya kalau peer langsungnya memang loopback,
// dan yang diambil adalah entri TERAKHIR — itu yang ditambahkan proxy tepercaya
// tadi, sedangkan entri sebelumnya bisa saja dipalsukan client.
func loginKey(r *http.Request) string {
	host := clientIP(r)
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	parts := strings.Split(xff, ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if net.ParseIP(last) == nil {
		return host
	}
	return last
}

// isSecureRequest menentukan apakah cookie boleh ditandai Secure. Lewat SSH
// tunnel koneksinya http biasa, sedangkan di belakang Caddy ada
// X-Forwarded-Proto. Salah menebak di sini hanya membuat cookie lebih longgar
// atau lebih ketat, tidak pernah membocorkan apa pun.
func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
