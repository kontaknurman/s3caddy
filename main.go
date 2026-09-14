package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

const (
	objectsPerPage = 100
	indexDocument  = "index.html"
	// maxPreviewBytes caps what /preview will stream back.
	maxPreviewBytes = 64 << 20
)

// --- configuration --------------------------------------------------------

// Config is the full runtime configuration, all of it from the environment.
type Config struct {
	AdminToken  string
	AdminURL    string
	S3URL       string
	WebURL      string
	S3AccessKey string
	S3SecretKey string
	S3Region    string
	SitesDir    string
	S3APIDomain string
	Listen      string
	ReloadCmd   []string

	webURL *url.URL
	s3URL  *url.URL
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		AdminToken:  os.Getenv("GARAGE_ADMIN_TOKEN"),
		AdminURL:    env("GARAGE_ADMIN_URL", "http://127.0.0.1:3903"),
		S3URL:       env("GARAGE_S3_URL", "http://127.0.0.1:3900"),
		WebURL:      env("GARAGE_WEB_URL", "http://127.0.0.1:3902"),
		S3AccessKey: os.Getenv("GARAGE_S3_ACCESS_KEY"),
		S3SecretKey: os.Getenv("GARAGE_S3_SECRET_KEY"),
		S3Region:    env("GARAGE_S3_REGION", "garage"),
		SitesDir:    env("CADDY_SITES_DIR", "/etc/caddy/sites"),
		S3APIDomain: strings.TrimSpace(os.Getenv("S3_API_DOMAIN")),
		Listen:      env("LISTEN", "127.0.0.1:8090"),
	}

	if strings.TrimSpace(cfg.AdminToken) == "" {
		return nil, errors.New("GARAGE_ADMIN_TOKEN belum diisi.\n" +
			"Panel tidak bisa bicara dengan Garage tanpa token admin.\n" +
			"Ambil token dari /etc/garage.toml (admin.admin_token) lalu jalankan ulang dengan\n" +
			"  GARAGE_ADMIN_TOKEN=... garagepanel\n" +
			"atau isi Environment= di unit systemd.")
	}

	var err error
	if cfg.webURL, err = parseEndpoint("GARAGE_WEB_URL", cfg.WebURL); err != nil {
		return nil, err
	}
	if cfg.s3URL, err = parseEndpoint("GARAGE_S3_URL", cfg.S3URL); err != nil {
		return nil, err
	}
	if _, err := parseEndpoint("GARAGE_ADMIN_URL", cfg.AdminURL); err != nil {
		return nil, err
	}

	if cfg.S3APIDomain != "" {
		if err := ValidateDomain(cfg.S3APIDomain); err != nil {
			return nil, fmt.Errorf("S3_API_DOMAIN tidak valid: %w", err)
		}
	}

	if err := checkLoopbackListen(cfg.Listen); err != nil {
		return nil, err
	}

	// The panel may only run this one command with elevated rights.
	cfg.ReloadCmd = strings.Fields(env("CADDY_RELOAD_CMD", "sudo -n /bin/systemctl reload caddy"))
	if len(cfg.ReloadCmd) == 0 {
		return nil, errors.New("CADDY_RELOAD_CMD kosong")
	}

	return cfg, nil
}

func parseEndpoint(name, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s tidak valid: %w", name, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%s harus berupa URL lengkap seperti http://127.0.0.1:3900, bukan %q", name, raw)
	}
	return u, nil
}

// checkLoopbackListen enforces that the panel can only ever bind to loopback.
// The panel writes Caddy config and reloads a systemd unit, so it is reached
// through an SSH tunnel and never exposed directly.
func checkLoopbackListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("LISTEN harus berbentuk host:port (contoh 127.0.0.1:8090): %w", err)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("LISTEN: port %q tidak valid", port)
	}
	if host == "" {
		return errors.New("LISTEN tidak boleh mengikat ke semua interface. " +
			"Panel ini punya hak menulis config Caddy dan reload systemd, jadi harus hanya di loopback — " +
			"gunakan 127.0.0.1:<port> dan akses lewat SSH tunnel")
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("LISTEN harus alamat loopback (127.0.0.1 atau ::1), bukan %q. "+
			"Panel ini tidak boleh diekspos ke jaringan — akses lewat SSH tunnel", host)
	}
	return nil
}

// --- application ----------------------------------------------------------

// App holds everything the handlers need.
type App struct {
	cfg     *Config
	garage  *Garage
	s3      *S3
	caddy   *CaddyManager
	pages   map[string]*template.Template
	flashes *flashStore
}

func main() {
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("garagepanel: ")

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nKonfigurasi tidak lengkap:\n\n%v\n\n", err)
		os.Exit(1)
	}

	app, err := newApp(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nGagal start: %v\n\n", err)
		os.Exit(1)
	}

	// Tell the operator early if the token or the endpoint is wrong, but keep
	// running: the UI reports the same error in a way they can act on.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := app.garage.Health(ctx); err != nil {
		log.Printf("PERINGATAN: Garage Admin API belum bisa dihubungi: %v", err)
	} else {
		log.Printf("terhubung ke Garage Admin API di %s", cfg.AdminURL)
	}
	cancel()

	if app.s3 == nil {
		log.Printf("PERINGATAN: GARAGE_S3_ACCESS_KEY/GARAGE_S3_SECRET_KEY belum diisi — halaman objek dinonaktifkan")
	}
	if cfg.S3APIDomain == "" {
		log.Printf("PERINGATAN: S3_API_DOMAIN belum diisi — halaman IP whitelist dinonaktifkan")
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           app.routes(),
		ReadHeaderTimeout: 15 * time.Second,
		WriteTimeout:      10 * time.Minute, // uploads and previews
		IdleTimeout:       2 * time.Minute,
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nTidak bisa listen di %s: %v\n\n", cfg.Listen, err)
		os.Exit(1)
	}
	log.Printf("siap di http://%s (loopback saja — akses lewat SSH tunnel)", cfg.Listen)

	idle := make(chan struct{})
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		log.Printf("berhenti…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		close(idle)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server berhenti: %v", err)
	}
	<-idle
}

func newApp(cfg *Config) (*App, error) {
	app := &App{
		cfg:     cfg,
		garage:  NewGarage(cfg.AdminURL, cfg.AdminToken),
		caddy:   NewCaddyManager(cfg.SitesDir, cfg.webURL.Host, cfg.s3URL.Host, cfg.ReloadCmd),
		flashes: newFlashStore(),
	}

	if cfg.S3AccessKey != "" && cfg.S3SecretKey != "" {
		s3, err := NewS3(cfg.S3URL, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Region)
		if err != nil {
			return nil, err
		}
		app.s3 = s3
	}

	if err := app.parseTemplates(); err != nil {
		return nil, err
	}
	return app, nil
}

// parseTemplates builds one template set per page, each combined with the
// shared layout. html/template is used throughout so every value is
// contextually auto-escaped.
func (a *App) parseTemplates() error {
	pages := []string{"buckets.html", "bucket_created.html", "domains.html", "objects.html", "whitelist.html", "error.html"}
	a.pages = make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		t, err := template.New("layout.html").Funcs(templateFuncs()).ParseFS(templateFS, "templates/layout.html", "templates/"+page)
		if err != nil {
			return fmt.Errorf("template %s: %w", page, err)
		}
		a.pages[page] = t
	}
	return nil
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"humanBytes": humanBytes,
		"humanTime":  humanTime,
		"commaSep":   func(s []string) string { return strings.Join(s, ", ") },
	}
}

// --- routing --------------------------------------------------------------

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/buckets", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /buckets", a.handleBuckets)
	mux.HandleFunc("POST /buckets/create", a.handleBucketCreate)
	mux.HandleFunc("POST /buckets/website", a.handleBucketWebsite)
	mux.HandleFunc("POST /buckets/delete", a.handleBucketDelete)

	mux.HandleFunc("GET /domains", a.handleDomains)
	mux.HandleFunc("POST /domains/create", a.handleDomainCreate)
	mux.HandleFunc("POST /domains/delete", a.handleDomainDelete)

	mux.HandleFunc("GET /objects", a.handleObjects)
	mux.HandleFunc("POST /objects/delete", a.handleObjectDelete)
	mux.HandleFunc("POST /upload", a.handleUpload)
	mux.HandleFunc("GET /preview", a.handlePreview)

	mux.HandleFunc("GET /whitelist", a.handleWhitelist)
	mux.HandleFunc("POST /whitelist/add", a.handleWhitelistAdd)
	mux.HandleFunc("POST /whitelist/delete", a.handleWhitelistDelete)
	mux.HandleFunc("POST /whitelist/remove-file", a.handleWhitelistRemoveFile)

	mux.HandleFunc("/", a.handleNotFound)

	return a.recoverMW(a.logMW(a.loopbackMW(a.csrfMW(mux))))
}

func (a *App) handleNotFound(w http.ResponseWriter, r *http.Request) {
	a.renderError(w, r, http.StatusNotFound, "Halaman tidak ditemukan", fmt.Errorf("tidak ada rute untuk %s", r.URL.Path))
}

// --- middleware -----------------------------------------------------------

type ctxKey string

const csrfCtxKey ctxKey = "csrf"

func (a *App) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC %s %s: %v", r.Method, r.URL.Path, rec)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (a *App) logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		// Path only, never the query string or any header: nothing sensitive
		// should ever reach the journal.
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

// loopbackMW rejects requests whose Host header is not a loopback name. Even
// though the listener is bound to 127.0.0.1, this blocks DNS-rebinding attempts
// from a browser the user happens to have open.
func (a *App) loopbackMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host != "localhost" {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				http.Error(w, "panel hanya melayani host loopback (127.0.0.1 / localhost)", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// csrfMW implements the double-submit cookie pattern. The panel has no login,
// so without this any page in the user's browser could POST to the tunnelled
// port and change Caddy config.
func (a *App) csrfMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if c, err := r.Cookie("garagepanel_csrf"); err == nil && len(c.Value) == 64 {
			token = c.Value
		}
		if token == "" {
			token = randomHex(32)
			http.SetCookie(w, &http.Cookie{
				Name:     "garagepanel_csrf",
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
				http.Error(w, "origin ditolak", http.StatusForbidden)
				return
			}
			sent := r.Header.Get("X-CSRF-Token")
			if sent == "" && !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				if err := r.ParseForm(); err == nil {
					sent = r.PostForm.Get("csrf")
				}
			}
			if subtle.ConstantTimeCompare([]byte(sent), []byte(token)) != 1 {
				http.Error(w, "token CSRF tidak cocok — muat ulang halaman lalu coba lagi", http.StatusForbidden)
				return
			}
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfCtxKey, token)))
	})
}

func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == host
}

func csrfToken(r *http.Request) string {
	if v, ok := r.Context().Value(csrfCtxKey).(string); ok {
		return v
	}
	return ""
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is unrecoverable for a panel that relies on CSRF
		// tokens, so fail loudly rather than continue with a weak token.
		panic("crypto/rand tidak tersedia: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// --- flash messages -------------------------------------------------------

type flash struct {
	Kind    string // "ok" or "error"
	Msg     string
	expires time.Time
}

type flashStore struct {
	mu sync.Mutex
	m  map[string]flash
}

func newFlashStore() *flashStore { return &flashStore{m: map[string]flash{}} }

func (s *flashStore) add(kind, msg string) string {
	id := randomHex(8)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.m {
		if v.expires.Before(now) {
			delete(s.m, k)
		}
	}
	s.m[id] = flash{Kind: kind, Msg: msg, expires: now.Add(10 * time.Minute)}
	return id
}

func (s *flashStore) take(id string) *flash {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.m[id]
	if !ok || f.expires.Before(time.Now()) {
		delete(s.m, id)
		return nil
	}
	delete(s.m, id)
	return &f
}

// redirectFlash stores a message server-side and redirects with only its id in
// the URL, so nothing user-controlled is reflected back into the page.
func (a *App) redirectFlash(w http.ResponseWriter, r *http.Request, target, kind, msg string) {
	id := a.flashes.add(kind, msg)
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	http.Redirect(w, r, target+sep+"m="+id, http.StatusSeeOther)
}

func (a *App) redirectOK(w http.ResponseWriter, r *http.Request, target, msg string) {
	a.redirectFlash(w, r, target, "ok", msg)
}

func (a *App) redirectErr(w http.ResponseWriter, r *http.Request, target string, err error) {
	a.redirectFlash(w, r, target, "error", err.Error())
}

// --- rendering ------------------------------------------------------------

func (a *App) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	t, ok := a.pages[page]
	if !ok {
		http.Error(w, "template tidak ditemukan: "+page, http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["CSRF"] = csrfToken(r)
	data["Flash"] = a.flashes.take(r.URL.Query().Get("m"))
	data["HasS3"] = a.s3 != nil
	data["S3APIDomain"] = a.cfg.S3APIDomain

	// Render into a buffer so a mid-template error cannot emit half a page.
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		log.Printf("render %s: %v", page, err)
		http.Error(w, "gagal merender halaman: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	if code, ok := data["StatusCode"].(int); ok {
		w.WriteHeader(code)
	}
	_, _ = io.WriteString(w, buf.String())
}

func (a *App) renderError(w http.ResponseWriter, r *http.Request, code int, title string, err error) {
	a.render(w, r, "error.html", map[string]any{
		"Title":      title,
		"Page":       "",
		"Detail":     err.Error(),
		"StatusCode": code,
	})
}

// --- helpers --------------------------------------------------------------

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func humanTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// forwardedIP returns the first X-Forwarded-For entry, if it parses. It is only
// ever used to pre-fill a form field that is validated before use.
func forwardedIP(r *http.Request) string {
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return ""
	}
	first := strings.TrimSpace(strings.Split(xff, ",")[0])
	if net.ParseIP(first) == nil {
		return ""
	}
	return first
}

// bucketNamesFrom returns the sorted global aliases of every bucket, for the
// dropdowns that must be populated live from Garage.
func bucketNamesFrom(items []BucketListItem) []string {
	var names []string
	for _, b := range items {
		for _, alias := range b.GlobalAliases {
			names = append(names, alias)
		}
	}
	sort.Strings(names)
	return names
}

// --- page 1: buckets ------------------------------------------------------

type bucketRow struct {
	Name        string
	ID          string
	Objects     int64
	Bytes       int64
	Public      bool
	DomainCount int
	Domains     []string
	Err         string
}

func (a *App) handleBuckets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	details, err := a.garage.ListBucketsWithInfo(ctx)
	if err != nil {
		a.renderError(w, r, http.StatusBadGateway, "Tidak bisa membaca daftar bucket", err)
		return
	}

	sites, sitesErr := a.caddy.ListSites()
	counts := DomainCounts(sites)

	rows := make([]bucketRow, 0, len(details))
	var totalBytes, totalObjects int64
	for _, d := range details {
		row := bucketRow{
			Name: d.Item.Name(),
			ID:   d.Item.ID,
		}
		if d.Err != nil {
			row.Err = d.Err.Error()
		}
		if d.Info != nil {
			row.Objects = d.Info.Objects
			row.Bytes = d.Info.Bytes
			row.Public = d.Info.WebsiteAccess
			totalBytes += d.Info.Bytes
			totalObjects += d.Info.Objects
		}
		if row.Name != "" {
			row.Domains = DomainsForBucket(sites, row.Name)
			row.DomainCount = counts[row.Name]
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	data := map[string]any{
		"Title":        "Buckets",
		"Page":         "buckets",
		"Buckets":      rows,
		"TotalBytes":   totalBytes,
		"TotalObjects": totalObjects,
	}
	if sitesErr != nil {
		data["SitesWarning"] = sitesErr.Error()
	}
	a.render(w, r, "buckets.html", data)
}

// step records one stage of the create-bucket sequence for the result page.
type step struct {
	Name string
	OK   bool
	Err  string
}

func (a *App) handleBucketCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PostFormValue("name"))
	public := r.PostFormValue("public") == "on"

	if err := ValidateBucketName(name); err != nil {
		a.redirectErr(w, r, "/buckets", err)
		return
	}

	var steps []step
	fail := func(label string, err error) {
		steps = append(steps, step{Name: label, Err: err.Error()})
	}
	ok := func(label string) {
		steps = append(steps, step{Name: label, OK: true})
	}

	// 1. CreateBucket
	bucket, err := a.garage.CreateBucket(ctx, name)
	if err != nil {
		a.redirectErr(w, r, "/buckets", fmt.Errorf("bucket %q gagal dibuat: %w", name, err))
		return
	}
	ok(fmt.Sprintf("CreateBucket %q", name))

	// 2. CreateKey. From here on failures are reported on the result page
	// rather than by redirect, because the secret key is shown exactly once and
	// must not be thrown away just because a later step failed.
	keyName := name + "-key"
	key, keyErr := a.garage.CreateKey(ctx, keyName)
	if keyErr != nil {
		fail(fmt.Sprintf("CreateKey %q", keyName), keyErr)
	} else {
		ok(fmt.Sprintf("CreateKey %q", keyName))

		// 3. AllowBucketKey: read + write
		perm := BucketKeyPerm{Read: true, Write: true}
		if err := a.garage.AllowBucketKey(ctx, bucket.ID, key.AccessKeyID, perm); err != nil {
			fail("AllowBucketKey (read+write)", err)
		} else {
			ok("AllowBucketKey (read+write)")
		}
	}

	// The panel browses and uploads with its own key, so that key needs access
	// to the new bucket too — otherwise the object browser would show nothing
	// for a bucket that was just created here.
	if a.cfg.S3AccessKey != "" {
		perm := BucketKeyPerm{Read: true, Write: true}
		if err := a.garage.AllowBucketKey(ctx, bucket.ID, a.cfg.S3AccessKey, perm); err != nil {
			fail("AllowBucketKey untuk key panel sendiri", err)
		} else {
			ok("AllowBucketKey untuk key panel sendiri (read+write)")
		}
	}

	// 4. Website access, only when asked for.
	if public {
		if err := a.garage.SetWebsiteAccess(ctx, bucket.ID, true, indexDocument); err != nil {
			fail("Aktifkan website access", err)
		} else {
			ok("Aktifkan website access (index: " + indexDocument + ")")
		}
	}

	log.Printf("bucket %q dibuat (id %s, public=%v)", name, bucket.ID, public)

	allOK := true
	for _, s := range steps {
		if !s.OK {
			allOK = false
		}
	}

	a.render(w, r, "bucket_created.html", map[string]any{
		"Title":     "Bucket dibuat",
		"Page":      "buckets",
		"Bucket":    name,
		"BucketID":  bucket.ID,
		"Public":    public,
		"Steps":     steps,
		"AllOK":     allOK,
		"AccessKey": keyAccessID(key),
		"SecretKey": key.Secret(),
		"S3URL":     a.cfg.S3URL,
		"Region":    a.cfg.S3Region,
	})
}

func keyAccessID(k *KeyInfo) string {
	if k == nil {
		return ""
	}
	return k.AccessKeyID
}

func (a *App) handleBucketWebsite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PostFormValue("bucket"))
	enable := r.PostFormValue("enable") == "1"

	if err := ValidateBucketName(name); err != nil {
		a.redirectErr(w, r, "/buckets", err)
		return
	}
	bucket, err := a.garage.GetBucketByName(ctx, name)
	if err != nil {
		a.redirectErr(w, r, "/buckets", err)
		return
	}
	if err := a.garage.SetWebsiteAccess(ctx, bucket.ID, enable, indexDocument); err != nil {
		a.redirectErr(w, r, "/buckets", fmt.Errorf("gagal mengubah website access bucket %q: %w", name, err))
		return
	}
	state := "private"
	if enable {
		state = "public"
	}
	a.redirectOK(w, r, "/buckets", fmt.Sprintf("Bucket %q sekarang %s.", name, state))
}

func (a *App) handleBucketDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PostFormValue("bucket"))
	confirm := strings.TrimSpace(r.PostFormValue("confirm"))

	if err := ValidateBucketName(name); err != nil {
		a.redirectErr(w, r, "/buckets", err)
		return
	}
	if confirm != name {
		a.redirectErr(w, r, "/buckets", fmt.Errorf("konfirmasi tidak cocok: ketik ulang persis %q untuk menghapus", name))
		return
	}

	// A bucket with domains pointing at it must not disappear under Caddy.
	sites, err := a.caddy.ListSites()
	if err != nil {
		a.redirectErr(w, r, "/buckets", fmt.Errorf("tidak bisa memeriksa domain sebelum menghapus: %w", err))
		return
	}
	if domains := DomainsForBucket(sites, name); len(domains) > 0 {
		a.redirectErr(w, r, "/buckets", fmt.Errorf(
			"bucket %q masih dipakai oleh %d domain: %s. Hapus domain itu dulu di halaman Domains.",
			name, len(domains), strings.Join(domains, ", ")))
		return
	}

	bucket, err := a.garage.GetBucketByName(ctx, name)
	if err != nil {
		a.redirectErr(w, r, "/buckets", err)
		return
	}
	if err := a.garage.DeleteBucket(ctx, bucket.ID); err != nil {
		a.redirectErr(w, r, "/buckets", fmt.Errorf("gagal menghapus bucket %q: %w", name, err))
		return
	}
	log.Printf("bucket %q dihapus (id %s)", name, bucket.ID)
	a.redirectOK(w, r, "/buckets", fmt.Sprintf("Bucket %q dihapus. Access key-nya tidak ikut terhapus — hapus manual lewat CLI kalau tidak dipakai lagi.", name))
}

// --- page 2: domains ------------------------------------------------------

type domainRow struct {
	Domain    string
	Bucket    string
	FileName  string
	Managed   bool
	Problem   string
	BucketOK  bool
	BucketMsg string
}

func (a *App) handleDomains(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	sites, sitesErr := a.caddy.ListSites()
	if sitesErr != nil {
		a.renderError(w, r, http.StatusInternalServerError, "Tidak bisa membaca direktori Caddy", sitesErr)
		return
	}

	// The dropdown is filled live from Garage, not from anything cached.
	buckets, bucketsErr := a.garage.ListBuckets(ctx)
	known := map[string]bool{}
	for _, b := range buckets {
		for _, alias := range b.GlobalAliases {
			known[alias] = true
		}
	}

	rows := make([]domainRow, 0, len(sites))
	for _, s := range sites {
		row := domainRow{
			Domain:   s.Domain,
			Bucket:   s.Bucket,
			FileName: path.Base(s.File),
			Managed:  s.Managed,
			Problem:  s.Problem,
		}
		if s.Managed {
			if bucketsErr != nil {
				row.BucketOK = true // unknown; do not cry wolf
			} else if known[s.Bucket] {
				row.BucketOK = true
			} else {
				row.BucketMsg = "bucket tidak ada di Garage"
			}
		}
		rows = append(rows, row)
	}

	data := map[string]any{
		"Title":     "Domains",
		"Page":      "domains",
		"Domains":   rows,
		"Buckets":   bucketNamesFrom(buckets),
		"SitesDir":  a.cfg.SitesDir,
		"WebTarget": a.cfg.webURL.Host,
	}
	if bucketsErr != nil {
		data["BucketsWarning"] = bucketsErr.Error()
	}
	a.render(w, r, "domains.html", data)
}

func (a *App) handleDomainCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	domain := strings.ToLower(strings.TrimSpace(r.PostFormValue("domain")))
	bucket := strings.TrimSpace(r.PostFormValue("bucket"))

	if err := ValidateDomain(domain); err != nil {
		a.redirectErr(w, r, "/domains", err)
		return
	}
	if err := ValidateBucketName(bucket); err != nil {
		a.redirectErr(w, r, "/domains", err)
		return
	}

	// The bucket must really exist, otherwise the domain would 404 forever.
	if _, err := a.garage.GetBucketByName(ctx, bucket); err != nil {
		a.redirectErr(w, r, "/domains", err)
		return
	}

	if err := a.caddy.AddDomain(ctx, domain, bucket); err != nil {
		a.redirectErr(w, r, "/domains", fmt.Errorf("gagal menambah domain %q: %w", domain, err))
		return
	}
	log.Printf("domain %q -> bucket %q ditulis dan Caddy di-reload", domain, bucket)
	a.redirectOK(w, r, "/domains", fmt.Sprintf(
		"Domain %s sekarang menunjuk ke bucket %q dan Caddy sudah di-reload. Pastikan DNS %s mengarah ke server ini agar sertifikat bisa terbit.",
		domain, bucket, domain))
}

func (a *App) handleDomainDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	domain := strings.ToLower(strings.TrimSpace(r.PostFormValue("domain")))
	confirm := strings.TrimSpace(r.PostFormValue("confirm"))

	if err := ValidateDomain(domain); err != nil {
		a.redirectErr(w, r, "/domains", err)
		return
	}
	if confirm != domain {
		a.redirectErr(w, r, "/domains", fmt.Errorf("konfirmasi tidak cocok: ketik ulang persis %q untuk menghapus", domain))
		return
	}
	if err := a.caddy.RemoveDomain(ctx, domain); err != nil {
		a.redirectErr(w, r, "/domains", fmt.Errorf("gagal menghapus domain %q: %w", domain, err))
		return
	}
	log.Printf("domain %q dihapus dan Caddy di-reload", domain)
	a.redirectOK(w, r, "/domains", fmt.Sprintf("Domain %s dihapus dan Caddy sudah di-reload.", domain))
}

// --- page 3: object browser ----------------------------------------------

type objectRow struct {
	Key       string
	Name      string
	Size      int64
	Modified  time.Time
	IsImage   bool
	PublicURL string
}

func (a *App) handleObjects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	bucket := strings.TrimSpace(q.Get("bucket"))
	prefix := q.Get("prefix")
	token := q.Get("token")

	buckets, bucketsErr := a.garage.ListBuckets(ctx)
	data := map[string]any{
		"Title":      "Objek",
		"Page":       "objects",
		"Buckets":    bucketNamesFrom(buckets),
		"Bucket":     bucket,
		"Prefix":     prefix,
		"MaxUploadH": humanBytes(MaxUploadSize),
		"AllowedExt": strings.Join(AllowedExtensionList(), " "),
	}
	if bucketsErr != nil {
		data["BucketsWarning"] = bucketsErr.Error()
	}

	if bucket == "" {
		a.render(w, r, "objects.html", data)
		return
	}
	if err := ValidateBucketName(bucket); err != nil {
		a.renderError(w, r, http.StatusBadRequest, "Bucket tidak valid", err)
		return
	}
	if err := ValidatePrefix(prefix); err != nil {
		a.renderError(w, r, http.StatusBadRequest, "Prefix tidak valid", err)
		return
	}
	if a.s3 == nil {
		data["Error"] = "GARAGE_S3_ACCESS_KEY dan GARAGE_S3_SECRET_KEY belum diatur, jadi panel tidak bisa membaca isi bucket. Isi keduanya di unit systemd lalu restart garagepanel."
		a.render(w, r, "objects.html", data)
		return
	}

	sites, _ := a.caddy.ListSites()
	domain := FirstDomainForBucket(sites, bucket)
	data["Domain"] = domain

	result, err := a.s3.ListObjectsV2(ctx, bucket, prefix, token, "/", objectsPerPage)
	if err != nil {
		data["Error"] = err.Error()
		a.render(w, r, "objects.html", data)
		return
	}

	var images, others []objectRow
	for _, obj := range result.Objects {
		// A "directory marker" object is the prefix itself; skip it.
		if obj.Key == prefix {
			continue
		}
		row := objectRow{
			Key:      obj.Key,
			Name:     strings.TrimPrefix(obj.Key, prefix),
			Size:     obj.Size,
			Modified: obj.LastModified,
			IsImage:  IsImageKey(obj.Key),
		}
		if domain != "" {
			row.PublicURL = publicURL(domain, obj.Key)
		}
		if row.IsImage {
			images = append(images, row)
		} else {
			others = append(others, row)
		}
	}

	data["Images"] = images
	data["Others"] = others
	data["Folders"] = result.CommonPrefixes
	data["ParentPrefix"] = parentPrefix(prefix)
	data["HasParent"] = prefix != ""
	data["NextToken"] = result.NextToken
	data["IsTruncated"] = result.IsTruncated
	data["Count"] = len(images) + len(others)
	data["Empty"] = len(images) == 0 && len(others) == 0 && len(result.CommonPrefixes) == 0

	a.render(w, r, "objects.html", data)
}

// publicURL builds the URL a visitor would use, which is always
// https://<domain>/<key> because Caddy proxies the whole path through.
func publicURL(domain, key string) string {
	u := url.URL{Scheme: "https", Host: domain, Path: "/" + key}
	return u.String()
}

func parentPrefix(prefix string) string {
	trimmed := strings.TrimSuffix(prefix, "/")
	idx := strings.LastIndex(trimmed, "/")
	if idx < 0 {
		return ""
	}
	return trimmed[:idx+1]
}

func (a *App) handleObjectDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket := strings.TrimSpace(r.PostFormValue("bucket"))
	key := r.PostFormValue("key")
	prefix := r.PostFormValue("prefix")

	back := "/objects?bucket=" + url.QueryEscape(bucket)
	if prefix != "" {
		back += "&prefix=" + url.QueryEscape(prefix)
	}

	if err := ValidateBucketName(bucket); err != nil {
		a.redirectErr(w, r, "/objects", err)
		return
	}
	if err := ValidateObjectKey(key); err != nil {
		a.redirectErr(w, r, back, err)
		return
	}
	if a.s3 == nil {
		a.redirectErr(w, r, back, errors.New("kredensial S3 panel belum diatur"))
		return
	}
	if err := a.s3.DeleteObject(ctx, bucket, key); err != nil {
		a.redirectErr(w, r, back, fmt.Errorf("gagal menghapus %q: %w", key, err))
		return
	}
	a.redirectOK(w, r, back, fmt.Sprintf("Objek %q dihapus.", key))
}

// uploadResult is the JSON answer of /upload, one file per request.
type uploadResult struct {
	OK        bool   `json:"ok"`
	Key       string `json:"key,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Deduped   bool   `json:"deduped,omitempty"`
	PublicURL string `json:"publicUrl,omitempty"`
	Preview   string `json:"preview,omitempty"`
	Error     string `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if a.s3 == nil {
		writeJSON(w, http.StatusServiceUnavailable, uploadResult{Error: "kredensial S3 panel belum diatur"})
		return
	}

	// Hard cap on the whole request so a huge body can never be buffered.
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadSize+(1<<20))
	if err := r.ParseMultipartForm(MaxUploadSize + (1 << 20)); err != nil {
		writeJSON(w, http.StatusBadRequest, uploadResult{
			Error: fmt.Sprintf("upload ditolak (maksimal %s per file): %v", humanBytes(MaxUploadSize), err),
		})
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	bucket := strings.TrimSpace(r.FormValue("bucket"))
	if err := ValidateBucketName(bucket); err != nil {
		writeJSON(w, http.StatusBadRequest, uploadResult{Error: err.Error()})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, uploadResult{Error: "tidak ada file pada request: " + err.Error()})
		return
	}
	defer file.Close()

	ext, contentType, err := ValidateUploadName(header.Filename)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, uploadResult{Error: err.Error()})
		return
	}

	content, err := io.ReadAll(io.LimitReader(file, MaxUploadSize+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, uploadResult{Error: "gagal membaca file: " + err.Error()})
		return
	}
	if int64(len(content)) > MaxUploadSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, uploadResult{
			Error: fmt.Sprintf("file lebih besar dari batas %s", humanBytes(MaxUploadSize)),
		})
		return
	}
	if len(content) == 0 {
		writeJSON(w, http.StatusBadRequest, uploadResult{Error: "file kosong"})
		return
	}

	// Content addressing: the name is derived from the bytes, so uploading the
	// same file twice cannot create a duplicate.
	sum := sha256.Sum256(content)
	key := hex.EncodeToString(sum[:])[:16] + ext

	res := uploadResult{Key: key, Size: int64(len(content))}

	exists, _, err := a.s3.StatObject(ctx, bucket, key)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, uploadResult{Key: key, Error: err.Error()})
		return
	}
	if exists {
		res.Deduped = true
	} else if err := a.s3.PutObject(ctx, bucket, key, content, contentType); err != nil {
		writeJSON(w, http.StatusBadGateway, uploadResult{Key: key, Error: err.Error()})
		return
	}

	sites, _ := a.caddy.ListSites()
	if domain := FirstDomainForBucket(sites, bucket); domain != "" {
		res.PublicURL = publicURL(domain, key)
	}
	res.Preview = "/preview?bucket=" + url.QueryEscape(bucket) + "&key=" + url.QueryEscape(key)
	res.OK = true
	writeJSON(w, http.StatusOK, res)
}

// handlePreview streams an object through the panel itself.
//
// It goes to the Garage web endpoint with the bucket in the Host header, which
// is how that endpoint selects a bucket. For a bucket whose website access is
// off, the web endpoint has nothing to serve, so the panel falls back to a
// signed GetObject on the S3 API. Either way no public URL is involved and a
// private bucket still previews.
func (a *App) handlePreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket := strings.TrimSpace(r.URL.Query().Get("bucket"))
	key := r.URL.Query().Get("key")

	if err := ValidateBucketName(bucket); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := ValidateObjectKey(key); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	body, upstreamLen, err := a.fetchObject(ctx, bucket, key)
	if err != nil {
		status := http.StatusBadGateway
		var s3err *S3Error
		if errors.As(err, &s3err) && s3err.NotFound() {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	defer body.Close()

	// The panel serves this from its own origin, so only known raster image
	// types are allowed to render inline; everything else is forced to
	// download. That keeps an uploaded .html or .svg from running as script in
	// the panel's origin.
	base := path.Base(key)
	if IsImageKey(key) {
		w.Header().Set("Content-Type", ContentTypeForKey(key))
		w.Header().Set("Content-Disposition", "inline")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sanitizeFilename(base)))
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=60")
	if upstreamLen > 0 && upstreamLen <= maxPreviewBytes {
		w.Header().Set("Content-Length", strconv.FormatInt(upstreamLen, 10))
	}

	if _, err := io.Copy(w, io.LimitReader(body, maxPreviewBytes)); err != nil {
		log.Printf("preview %s/%s terputus: %v", bucket, key, err)
	}
}

// fetchObject returns the object body, trying the web endpoint first and the
// signed S3 API second.
func (a *App) fetchObject(ctx context.Context, bucket, key string) (io.ReadCloser, int64, error) {
	webErr := error(nil)

	u := *a.cfg.webURL
	u.Path = "/" + key
	u.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err == nil {
		// Port 3902 maps a bucket by Host header, never by path.
		req.Host = bucket
		client := &http.Client{Timeout: 2 * time.Minute}
		resp, err := client.Do(req)
		switch {
		case err != nil:
			webErr = fmt.Errorf("web endpoint %s: %v", a.cfg.WebURL, unwrapURLError(err))
		case resp.StatusCode >= 200 && resp.StatusCode <= 299:
			return resp.Body, resp.ContentLength, nil
		default:
			resp.Body.Close()
			webErr = fmt.Errorf("web endpoint menjawab HTTP %d (bucket private atau objek tidak ada)", resp.StatusCode)
		}
	} else {
		webErr = err
	}

	if a.s3 == nil {
		return nil, 0, fmt.Errorf("%w; fallback S3 tidak tersedia karena kredensial S3 panel belum diatur", webErr)
	}
	body, hdr, err := a.s3.GetObject(ctx, bucket, key)
	if err != nil {
		return nil, 0, err
	}
	size := int64(-1)
	if cl := hdr.Get("Content-Length"); cl != "" {
		if n, convErr := strconv.ParseInt(cl, 10, 64); convErr == nil {
			size = n
		}
	}
	return body, size, nil
}

// sanitizeFilename keeps a Content-Disposition filename free of quotes and
// control characters.
func sanitizeFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if out == "" {
		return "download"
	}
	return out
}

// --- page 4: S3 API IP whitelist -----------------------------------------

func (a *App) handleWhitelist(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Title":       "IP Whitelist S3 API",
		"Page":        "whitelist",
		"ClientIP":    clientIP(r),
		"ForwardedIP": forwardedIP(r),
		"FileName":    whitelistFileName,
		"SitesDir":    a.cfg.SitesDir,
		"S3Target":    a.cfg.s3URL.Host,
	}

	if a.cfg.S3APIDomain == "" {
		data["Disabled"] = "S3_API_DOMAIN belum diatur, jadi panel tidak tahu domain mana yang harus dipasang di " + whitelistFileName + ". Isi S3_API_DOMAIN (contoh: s3.domainmu.com) di unit systemd lalu restart garagepanel."
		a.render(w, r, "whitelist.html", data)
		return
	}

	wl, err := a.caddy.ReadWhitelist()
	if err != nil {
		a.renderError(w, r, http.StatusInternalServerError, "Tidak bisa membaca "+whitelistFileName, err)
		return
	}

	ip := net.ParseIP(clientIP(r))
	data["ClientIsLoopback"] = ip != nil && ip.IsLoopback()
	data["Whitelist"] = wl
	data["Entries"] = wl.Entries
	data["Exists"] = wl.Exists
	data["FileDomain"] = wl.Domain
	data["DomainMismatch"] = wl.Exists && wl.Domain != "" && wl.Domain != a.cfg.S3APIDomain
	a.render(w, r, "whitelist.html", data)
}

func (a *App) handleWhitelistAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.cfg.S3APIDomain == "" {
		a.redirectErr(w, r, "/whitelist", errors.New("S3_API_DOMAIN belum diatur"))
		return
	}

	value := strings.TrimSpace(r.PostFormValue("value"))
	label := strings.TrimSpace(r.PostFormValue("label"))

	canonical, err := ValidateIPOrCIDR(value)
	if err != nil {
		a.redirectErr(w, r, "/whitelist", err)
		return
	}
	if label != "" {
		if err := ValidateLabel(label); err != nil {
			a.redirectErr(w, r, "/whitelist", err)
			return
		}
	}

	wl, err := a.caddy.ReadWhitelist()
	if err != nil {
		a.redirectErr(w, r, "/whitelist", err)
		return
	}
	for _, e := range wl.Entries {
		if e.Value == canonical {
			a.redirectErr(w, r, "/whitelist", fmt.Errorf("%s sudah ada di daftar", canonical))
			return
		}
	}
	entries := append(append([]WhitelistEntry{}, wl.Entries...), WhitelistEntry{Value: canonical, Label: label})

	if err := a.caddy.WriteWhitelist(ctx, a.cfg.S3APIDomain, entries); err != nil {
		a.redirectErr(w, r, "/whitelist", fmt.Errorf("gagal menyimpan whitelist: %w", err))
		return
	}
	log.Printf("whitelist S3 API: tambah %s", canonical)
	a.redirectOK(w, r, "/whitelist", fmt.Sprintf("%s ditambahkan ke whitelist dan Caddy sudah di-reload.", canonical))
}

func (a *App) handleWhitelistDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.cfg.S3APIDomain == "" {
		a.redirectErr(w, r, "/whitelist", errors.New("S3_API_DOMAIN belum diatur"))
		return
	}

	value := strings.TrimSpace(r.PostFormValue("value"))
	canonical, err := ValidateIPOrCIDR(value)
	if err != nil {
		a.redirectErr(w, r, "/whitelist", err)
		return
	}

	wl, err := a.caddy.ReadWhitelist()
	if err != nil {
		a.redirectErr(w, r, "/whitelist", err)
		return
	}

	var kept []WhitelistEntry
	found := false
	for _, e := range wl.Entries {
		if e.Value == canonical {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		a.redirectErr(w, r, "/whitelist", fmt.Errorf("%s tidak ada di daftar", canonical))
		return
	}
	if len(kept) == 0 {
		a.redirectErr(w, r, "/whitelist", fmt.Errorf(
			"%s adalah entri terakhir. Daftar kosong akan memblokir SEMUA operasi tulis ke S3 API. "+
				"Tambah IP lain dulu, atau pakai tombol \"Hapus file %s\" kalau memang S3 API tidak mau diekspos lewat Caddy.",
			canonical, whitelistFileName))
		return
	}

	if err := a.caddy.WriteWhitelist(ctx, a.cfg.S3APIDomain, kept); err != nil {
		a.redirectErr(w, r, "/whitelist", fmt.Errorf("gagal menyimpan whitelist: %w", err))
		return
	}
	log.Printf("whitelist S3 API: hapus %s", canonical)
	a.redirectOK(w, r, "/whitelist", fmt.Sprintf("%s dihapus dari whitelist dan Caddy sudah di-reload.", canonical))
}

func (a *App) handleWhitelistRemoveFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	confirm := strings.TrimSpace(r.PostFormValue("confirm"))
	if confirm != whitelistFileName {
		a.redirectErr(w, r, "/whitelist", fmt.Errorf("konfirmasi tidak cocok: ketik ulang persis %q", whitelistFileName))
		return
	}
	if err := a.caddy.DeleteWhitelist(ctx); err != nil {
		a.redirectErr(w, r, "/whitelist", fmt.Errorf("gagal menghapus %s: %w", whitelistFileName, err))
		return
	}
	log.Printf("whitelist S3 API: file %s dihapus", whitelistFileName)
	a.redirectOK(w, r, "/whitelist", fmt.Sprintf(
		"%s dihapus dan Caddy sudah di-reload. S3 API sekarang tidak diekspos lewat Caddy sama sekali.", whitelistFileName))
}
