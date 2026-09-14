package main

import (
	"bufio"
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
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime/debug"
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

	// Login bawaan panel. Login aktif begitu PasswordHash diisi.
	Username     string
	PasswordHash string
	PanelDomain  string

	// Sync dengan S3 lain: state (remote + checkpoint job) dan rclone.
	StateDir        string
	RcloneBin       string
	RcloneExtraArgs []string

	webURL *url.URL
	s3URL  *url.URL
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// secretEnv membaca kredensial dan memangkas spasi di ujungnya.
//
// systemd tidak membuang "\r" dari EnvironmentFile yang berakhiran CRLF, dan
// satu spasi yang tak sengaja terbawa saat menempel juga ikut masuk. Keduanya
// menghasilkan kegagalan yang sangat membingungkan: tanda tangan SigV4 tidak
// cocok ("Invalid signature") padahal Access Key ID terlihat benar. Panel
// memberi peringatan supaya penyebabnya kelihatan, tapi tidak pernah mencetak
// nilainya.
func secretEnv(key string) string {
	raw := os.Getenv(key)
	trimmed := strings.Trim(raw, " \t\r\n")
	if raw != trimmed && trimmed != "" {
		log.Printf("PERINGATAN: %s punya spasi/baris baru di ujungnya dan sudah dipangkas. "+
			"Periksa /etc/garagepanel/garagepanel.env — file berakhiran CRLF adalah penyebab yang paling sering.", key)
	}
	return trimmed
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		AdminToken:  secretEnv("GARAGE_ADMIN_TOKEN"),
		AdminURL:    env("GARAGE_ADMIN_URL", "http://127.0.0.1:3903"),
		S3URL:       env("GARAGE_S3_URL", "http://127.0.0.1:3900"),
		WebURL:      env("GARAGE_WEB_URL", "http://127.0.0.1:3902"),
		S3AccessKey: secretEnv("GARAGE_S3_ACCESS_KEY"),
		S3SecretKey: secretEnv("GARAGE_S3_SECRET_KEY"),
		S3Region:    env("GARAGE_S3_REGION", "garage"),
		SitesDir:    env("CADDY_SITES_DIR", "/etc/caddy/sites"),
		S3APIDomain: strings.TrimSpace(os.Getenv("S3_API_DOMAIN")),
		Listen:      env("LISTEN", "127.0.0.1:8090"),

		Username:     env("PANEL_USERNAME", "admin"),
		PasswordHash: strings.TrimSpace(os.Getenv("PANEL_PASSWORD_HASH")),
		PanelDomain:  strings.TrimSpace(os.Getenv("PANEL_DOMAIN")),

		// systemd mengekspor STATE_DIRECTORY saat unit memakai StateDirectory=,
		// jadi tanpa setelan apa pun panel sudah menulis ke tempat yang benar.
		StateDir:        env("STATE_DIR", env("STATE_DIRECTORY", "/var/lib/garagepanel")),
		RcloneBin:       env("RCLONE_BIN", "rclone"),
		RcloneExtraArgs: strings.Fields(os.Getenv("RCLONE_EXTRA_ARGS")),
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

	if cfg.PasswordHash != "" {
		if _, err := ParsePasswordHash(cfg.PasswordHash); err != nil {
			return nil, fmt.Errorf("PANEL_PASSWORD_HASH tidak bisa dibaca: %w", err)
		}
		if err := ValidateLabel(cfg.Username); err != nil {
			return nil, fmt.Errorf("PANEL_USERNAME tidak valid: %w", err)
		}
	}
	if cfg.PanelDomain != "" {
		if err := ValidateDomain(cfg.PanelDomain); err != nil {
			return nil, fmt.Errorf("PANEL_DOMAIN tidak valid: %w", err)
		}
		// Menerima request dari sebuah domain berarti panel bisa dijangkau dari
		// luar lewat reverse proxy. Tanpa login, itu sama saja menaruh hak
		// menulis config Caddy dan reload systemd di internet terbuka.
		if cfg.PasswordHash == "" {
			return nil, errors.New("PANEL_DOMAIN diisi tapi PANEL_PASSWORD_HASH kosong.\n" +
				"Panel menolak melayani sebuah domain tanpa login — itu akan membuka\n" +
				"hak menulis config Caddy dan reload systemd ke siapa pun yang tahu alamatnya.\n" +
				"Buat hash-nya dulu:\n" +
				"  garagepanel -hash-password\n" +
				"lalu isi PANEL_PASSWORD_HASH, atau kosongkan PANEL_DOMAIN dan pakai SSH tunnel.")
		}
	}

	// The panel may only run this one command with elevated rights.
	cfg.ReloadCmd = strings.Fields(env("CADDY_RELOAD_CMD", "sudo -n /bin/systemctl reload caddy"))
	if len(cfg.ReloadCmd) == 0 {
		return nil, errors.New("CADDY_RELOAD_CMD kosong")
	}

	return cfg, nil
}

// authEnabled melaporkan apakah halaman login dipasang. Tanpa
// PANEL_PASSWORD_HASH panel berjalan seperti semula: loopback saja, SSH tunnel
// yang jadi autentikasinya.
func (c *Config) authEnabled() bool { return c.PasswordHash != "" }

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

	sessions *sessionStore
	logins   *loginLimiter
	// loginMu membuat verifikasi password berjalan satu per satu. PBKDF2 600k
	// iterasi itu sengaja mahal, jadi request paralel tidak boleh dibiarkan
	// menghabiskan CPU panel.
	loginMu sync.Mutex

	// Sync dengan S3 lain. Keduanya nil kalau syncDisabled terisi.
	remotes *RemoteStore
	jobs    *JobManager
	// syncDisabled berisi alasan halaman Sync tidak aktif (STATE_DIR tidak
	// bisa ditulis, rclone tidak ada, kredensial S3 kosong). Kosong = aktif.
	syncDisabled string
}

func main() {
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("garagepanel: ")

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-hash-password", "--hash-password", "hash-password":
			os.Exit(runHashPassword())
		case "-version", "--version", "version":
			fmt.Println(buildVersion())
			return
		case "-h", "-help", "--help", "help":
			printUsage(os.Stdout)
			return
		default:
			fmt.Fprintf(os.Stderr, "argumen tidak dikenal: %s\n\n", os.Args[1])
			printUsage(os.Stderr)
			os.Exit(2)
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nKonfigurasi tidak lengkap:\n\n%v\n\n", err)
		os.Exit(1)
	}

	log.Printf("versi %s", buildVersion())

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
	} else {
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if d := app.diagnoseS3Credentials(checkCtx); d != "" && !strings.Contains(d, "SUDAH COCOK") {
			for _, line := range strings.Split(d, "\n") {
				log.Printf("PERINGATAN: %s", line)
			}
		}
		checkCancel()
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

	if app.jobs != nil {
		// Job yang sedang berjalan saat panel terakhir berhenti dilanjutkan
		// dari checkpoint-nya.
		app.jobs.ResumeInterrupted()
	}

	idle := make(chan struct{})
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		log.Printf("berhenti…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// Job dihentikan (rclone dapat SIGINT, checkpoint terakhir disimpan)
		// sementara server menyelesaikan request yang masih berjalan.
		jobsDone := make(chan struct{})
		go func() {
			if app.jobs != nil {
				app.jobs.Shutdown(shutdownCtx)
			}
			close(jobsDone)
		}()
		_ = srv.Shutdown(shutdownCtx)
		<-jobsDone
		close(idle)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server berhenti: %v", err)
	}
	<-idle
}

// --- perintah baris perintah ----------------------------------------------

// buildVersion melaporkan build mana yang sedang berjalan.
//
// Go menyetempel revisi git ke dalam binary secara otomatis, jadi tidak perlu
// flag build khusus — "go build" biasa sudah cukup. Ini yang membuat hasil
// update bisa diperiksa, bukan sekadar diasumsikan.
func buildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "tidak diketahui"
	}
	var rev, when string
	var modified bool
	for _, setting := range bi.Settings {
		switch setting.Key {
		case "vcs.revision":
			rev = setting.Value
		case "vcs.time":
			when = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if rev == "" {
		return "tidak diketahui (dibuild di luar repo git)"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified {
		rev += "-dirty"
	}
	if t, err := time.Parse(time.RFC3339, when); err == nil {
		rev += " (" + t.Local().Format("2006-01-02 15:04") + ")"
	}
	return rev
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `garagepanel — panel admin Garage + Caddy

Penggunaan:
  garagepanel                 jalankan panel (konfigurasi lewat environment variable)
  garagepanel -hash-password  buat nilai PANEL_PASSWORD_HASH untuk halaman login
  garagepanel -version        tampilkan build yang sedang dipakai
  garagepanel -help           tampilkan bantuan ini

Environment variable wajib:
  GARAGE_ADMIN_TOKEN          token Admin API Garage

Selengkapnya ada di README.
`)
}

// runHashPassword membaca password dari stdin lalu mencetak baris
// PANEL_PASSWORD_HASH yang tinggal ditempel. Password tidak pernah dilewatkan
// sebagai argumen supaya tidak muncul di "ps" maupun di riwayat shell.
func runHashPassword() int {
	in := bufio.NewReader(os.Stdin)
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		interactive = true
	}

	if interactive {
		fmt.Fprintf(os.Stderr, "Password untuk login panel (minimal %d karakter).\n", minPasswordLen)
	}

	pass, err := readSecret(in, "Password: ", interactive)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tidak bisa membaca password: %v\n", err)
		return 1
	}

	if interactive {
		confirm, err := readSecret(in, "Ulangi   : ", interactive)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tidak bisa membaca password: %v\n", err)
			return 1
		}
		if confirm != pass {
			fmt.Fprintln(os.Stderr, "password tidak sama, tidak ada yang dibuat")
			return 1
		}
	}

	hash, err := HashPassword(pass)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	if interactive {
		fmt.Fprintln(os.Stderr, "\nSalin baris di bawah ke /etc/garagepanel/garagepanel.env:")
	}
	fmt.Printf("PANEL_PASSWORD_HASH=%s\n", hash)
	return 0
}

// readSecret membaca satu baris, dengan echo terminal dimatikan kalau bisa.
// stty dipakai supaya tidak perlu paket termios dari luar maupun "unsafe".
func readSecret(in *bufio.Reader, prompt string, interactive bool) (string, error) {
	if !interactive {
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	fmt.Fprint(os.Stderr, prompt)
	echoOff := setTerminalEcho(false) == nil
	if !echoOff {
		fmt.Fprint(os.Stderr, "(password akan terlihat saat diketik) ")
	}

	line, err := in.ReadString('\n')
	if echoOff {
		_ = setTerminalEcho(true)
		fmt.Fprintln(os.Stderr)
	}
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func setTerminalEcho(on bool) error {
	arg := "-echo"
	if on {
		arg = "echo"
	}
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func newApp(cfg *Config) (*App, error) {
	app := &App{
		cfg:      cfg,
		garage:   NewGarage(cfg.AdminURL, cfg.AdminToken),
		caddy:    NewCaddyManager(cfg.SitesDir, cfg.webURL.Host, cfg.s3URL.Host, cfg.ReloadCmd),
		flashes:  newFlashStore(),
		sessions: newSessionStore(),
		logins:   newLoginLimiter(),
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
	app.setupSync()
	return app, nil
}

// setupSync menyiapkan direktori state untuk fitur Sync. Kegagalan di sini
// tidak menghentikan panel: install lama memakai unit tanpa StateDirectory=,
// dan halaman lain tidak butuh direktori ini. Alasannya ditampilkan di
// halaman Sync beserta cara memperbaikinya.
func (a *App) setupSync() {
	if a.s3 == nil {
		a.syncDisabled = "GARAGE_S3_ACCESS_KEY dan GARAGE_S3_SECRET_KEY belum diatur, jadi panel tidak bisa membaca atau menulis bucket. Isi keduanya di file environment lalu restart garagepanel."
		return
	}
	if err := prepareStateDir(a.cfg.StateDir); err != nil {
		a.syncDisabled = fmt.Sprintf("Direktori state %s tidak bisa dipakai: %v.\n"+
			"Pasang unit systemd terbaru (ada baris StateDirectory=garagepanel) lalu restart, atau buat manual:\n"+
			"  sudo install -d -o garagepanel -g garagepanel -m 0700 %s", a.cfg.StateDir, err, a.cfg.StateDir)
		log.Printf("PERINGATAN: %s", strings.ReplaceAll(a.syncDisabled, "\n", " "))
		return
	}
	remotes, err := NewRemoteStore(filepath.Join(a.cfg.StateDir, remotesFileName))
	if err != nil {
		a.syncDisabled = fmt.Sprintf("Daftar remote tidak bisa dibaca: %v.\nPerbaiki atau hapus file itu, lalu restart garagepanel.", err)
		log.Printf("PERINGATAN: %s", strings.ReplaceAll(a.syncDisabled, "\n", " "))
		return
	}
	rclone, err := detectRclone(a.cfg.RcloneBin, filepath.Join(a.cfg.StateDir, "cache"), a.cfg.RcloneExtraArgs)
	if err != nil {
		a.syncDisabled = fmt.Sprintf("%v\nSetelah rclone terpasang, restart garagepanel. Halaman lain tidak terpengaruh.", err)
		log.Printf("PERINGATAN: %s", strings.ReplaceAll(a.syncDisabled, "\n", " "))
		return
	}
	jobs, err := NewJobManager(filepath.Join(a.cfg.StateDir, "jobs"), garageRemote(a.cfg), remotes, rclone)
	if err != nil {
		a.syncDisabled = fmt.Sprintf("Job tidak bisa dimuat dari %s: %v", filepath.Join(a.cfg.StateDir, "jobs"), err)
		log.Printf("PERINGATAN: %s", a.syncDisabled)
		return
	}
	a.remotes = remotes
	a.jobs = jobs
	log.Printf("sync aktif: rclone v%s, state di %s, %d remote, %d job tersimpan", rclone.version, a.cfg.StateDir, len(remotes.List()), len(jobs.List()))
}

// prepareStateDir membuat STATE_DIR beserta subdirektorinya dan memastikan
// panel bisa menulis di sana. File di dalamnya berisi secret remote, jadi mode
// yang longgar dilaporkan.
func prepareStateDir(dir string) error {
	if dir == "" {
		return errors.New("STATE_DIR kosong")
	}
	for _, sub := range []string{"", "jobs", "cache"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return err
		}
	}
	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return fmt.Errorf("tidak bisa menulis: %w", err)
	}
	probe.Close()
	os.Remove(probe.Name())

	if info, err := os.Stat(dir); err == nil && info.Mode().Perm()&0o077 != 0 {
		log.Printf("PERINGATAN: %s bisa dibaca user lain (mode %o). File di dalamnya memuat secret remote — rapikan dengan: chmod 0700 %s",
			dir, info.Mode().Perm(), dir)
	}
	return nil
}

// parseTemplates builds one template set per page, each combined with the
// shared layout. html/template is used throughout so every value is
// contextually auto-escaped.
func (a *App) parseTemplates() error {
	pages := []string{"buckets.html", "bucket_created.html", "domains.html", "objects.html", "whitelist.html", "error.html", "login.html", "sync.html", "sync_failed.html"}
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
		// dict lets a sub-template receive several values.
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, errors.New("dict: jumlah argumen ganjil")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, errors.New("dict: kunci harus string")
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
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
	mux.HandleFunc("POST /objects/delete-many", a.handleObjectsDeleteMany)
	mux.HandleFunc("POST /objects/mkdir", a.handleFolderCreate)
	mux.HandleFunc("POST /objects/rename", a.handleObjectRename)
	mux.HandleFunc("POST /objects/move", a.handleObjectTransfer(true))
	mux.HandleFunc("POST /objects/copy", a.handleObjectTransfer(false))
	mux.HandleFunc("POST /objects/folder-job", a.handleFolderJob)
	mux.HandleFunc("POST /upload", a.handleUpload)
	mux.HandleFunc("GET /preview", a.handlePreview)

	mux.HandleFunc("GET /login", a.handleLoginPage)
	mux.HandleFunc("POST /login", a.handleLogin)
	mux.HandleFunc("POST /logout", a.handleLogout)

	mux.HandleFunc("GET /sync", a.handleSync)
	mux.HandleFunc("GET /sync/jobs.json", a.handleSyncJobsJSON)
	mux.HandleFunc("GET /sync/jobs/failed", a.handleJobFailed)
	mux.HandleFunc("POST /sync/remotes/add", a.handleRemoteAdd)
	mux.HandleFunc("POST /sync/remotes/delete", a.handleRemoteDelete)
	mux.HandleFunc("POST /sync/remotes/test", a.handleRemoteTest)
	mux.HandleFunc("POST /sync/jobs/create", a.handleJobCreate)
	mux.HandleFunc("POST /sync/jobs/pause", a.jobAction("dijeda setelah rclone berhenti", func(id string) error { return a.jobs.Pause(id) }))
	mux.HandleFunc("POST /sync/jobs/resume", a.jobAction("dilanjutkan dari checkpoint", func(id string) error { return a.jobs.Resume(id) }))
	mux.HandleFunc("POST /sync/jobs/rerun", a.jobAction("dijalankan ulang dari awal", func(id string) error { return a.jobs.Rerun(id) }))
	mux.HandleFunc("POST /sync/jobs/cancel", a.jobAction("dibatalkan", func(id string) error { return a.jobs.Cancel(id) }))
	mux.HandleFunc("POST /sync/jobs/delete", a.jobAction("catatan dihapus", func(id string) error { return a.jobs.Delete(id) }))

	mux.HandleFunc("GET /whitelist", a.handleWhitelist)
	mux.HandleFunc("POST /whitelist/add", a.handleWhitelistAdd)
	mux.HandleFunc("POST /whitelist/delete", a.handleWhitelistDelete)
	mux.HandleFunc("POST /whitelist/remove-file", a.handleWhitelistRemoveFile)

	mux.HandleFunc("/", a.handleNotFound)

	return a.recoverMW(a.logMW(a.hostMW(a.csrfMW(a.authMW(mux)))))
}

func (a *App) handleNotFound(w http.ResponseWriter, r *http.Request) {
	a.renderError(w, r, http.StatusNotFound, "Halaman tidak ditemukan", fmt.Errorf("tidak ada rute untuk %s", r.URL.Path))
}

// --- middleware -----------------------------------------------------------

type ctxKey string

const (
	csrfCtxKey ctxKey = "csrf"
	userCtxKey ctxKey = "user"
)

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

// hostMW menolak request yang header Host-nya bukan nama loopback atau
// PANEL_DOMAIN. Listener-nya sendiri sudah terikat ke 127.0.0.1, tapi cek ini
// yang menahan serangan DNS rebinding dari halaman lain yang kebetulan terbuka
// di browser user.
func (a *App) hostMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")

		if a.cfg.PanelDomain != "" && strings.EqualFold(host, a.cfg.PanelDomain) {
			next.ServeHTTP(w, r)
			return
		}
		if host != "localhost" {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				msg := "panel hanya melayani host loopback (127.0.0.1 / localhost)"
				if a.cfg.PanelDomain != "" {
					msg += " atau " + a.cfg.PanelDomain
				}
				http.Error(w, msg, http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authMW menuntut session yang sah untuk semua rute selain halaman login.
func (a *App) authMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.cfg.authEnabled() || r.URL.Path == "/login" {
			next.ServeHTTP(w, r)
			return
		}

		if c, err := r.Cookie(sessionCookieName); err == nil {
			if sess, ok := a.sessions.get(c.Value); ok {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, sess.user)))
				return
			}
		}

		a.clearSessionCookie(w, r)

		// Endpoint upload dipanggil dari JavaScript: halaman login HTML tidak
		// ada gunanya di sana, jadi jawab dengan JSON yang bisa ditampilkan.
		if r.URL.Path == "/upload" || strings.Contains(r.Header.Get("Accept"), "application/json") {
			writeJSON(w, http.StatusUnauthorized, uploadResult{Error: "session habis — muat ulang halaman lalu login lagi"})
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "session habis — muat ulang halaman lalu login lagi", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
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
			// Origin "null" berarti browser sengaja tidak memberi tahu asalnya
			// (misalnya dari iframe ber-sandbox, atau karena referrer policy
			// yang ketat). Itu bukan bukti request lintas situs, jadi jangan
			// ditolak mentah-mentah — token CSRF di bawah yang jadi
			// penjaganya, dan token itu tidak bisa dibaca situs lain.
			if origin := r.Header.Get("Origin"); origin != "" && origin != "null" && !sameOrigin(origin, r.Host) {
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
	data["Version"] = buildVersion()
	data["AuthEnabled"] = a.cfg.authEnabled()
	data["User"] = currentUser(r)

	// Render into a buffer so a mid-template error cannot emit half a page.
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		log.Printf("render %s: %v", page, err)
		http.Error(w, "gagal merender halaman: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// same-origin, bukan no-referrer: dengan no-referrer Chrome mengirim
	// "Origin: null" pada submit form, sehingga cek origin di atas tidak lagi
	// bisa membedakan apa pun. same-origin tetap tidak membocorkan URL panel ke
	// situs luar.
	w.Header().Set("Referrer-Policy", "same-origin")
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

// --- login ----------------------------------------------------------------

// currentUser mengembalikan user yang sedang login, atau "" kalau login mati.
func currentUser(r *http.Request) string {
	if v, ok := r.Context().Value(userCtxKey).(string); ok {
		return v
	}
	return ""
}

func (a *App) setSessionCookie(w http.ResponseWriter, r *http.Request, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		// Lax, bukan Strict: navigasi biasa ke panel tetap membawa session,
		// sementara POST lintas situs tetap tertahan token CSRF yang cookie-nya
		// Strict.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionIdleTTL / time.Second),
	})
}

func (a *App) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// safeNext membersihkan parameter ?next= supaya tidak bisa dipakai sebagai
// open redirect ke situs lain setelah login.
func safeNext(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/buckets"
	}
	if strings.ContainsAny(raw, "\r\n\\") {
		return "/buckets"
	}
	if raw == "/login" || strings.HasPrefix(raw, "/login?") {
		return "/buckets"
	}
	return raw
}

func (a *App) renderLogin(w http.ResponseWriter, r *http.Request, errMsg, next string, code int) {
	a.render(w, r, "login.html", map[string]any{
		"Title":       "Masuk",
		"Page":        "",
		"HideNav":     true,
		"HideTitle":   true,
		"LoginError":  errMsg,
		"Next":        next,
		"Username":    a.cfg.Username,
		"MaxFailures": loginMaxFailures,
		"Lockout":     fmt.Sprintf("%d menit", int(loginLockout.Minutes())),
		"StatusCode":  code,
	})
}

func (a *App) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.authEnabled() {
		http.Redirect(w, r, "/buckets", http.StatusSeeOther)
		return
	}
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if _, ok := a.sessions.get(c.Value); ok {
			http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
			return
		}
	}
	a.renderLogin(w, r, "", safeNext(r.URL.Query().Get("next")), http.StatusOK)
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.authEnabled() {
		http.Redirect(w, r, "/buckets", http.StatusSeeOther)
		return
	}

	next := safeNext(r.PostFormValue("next"))
	key := loginKey(r)

	if ok, wait := a.logins.allowed(key); !ok {
		a.renderLogin(w, r, fmt.Sprintf("Terlalu banyak percobaan gagal. Coba lagi dalam %s.", wait), next, http.StatusTooManyRequests)
		return
	}

	user := strings.TrimSpace(r.PostFormValue("username"))
	pass := r.PostFormValue("password")

	// Satu verifikasi pada satu waktu, dan username tetap dibandingkan dalam
	// waktu konstan supaya tidak bocor lewat selisih waktu.
	a.loginMu.Lock()
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(a.cfg.Username)) == 1
	passOK := VerifyPassword(a.cfg.PasswordHash, pass)
	a.loginMu.Unlock()

	if !userOK || !passOK {
		a.logins.fail(key)
		// Username yang dicoba sengaja tidak ikut dicatat: kalau seseorang
		// salah mengetik password ke kolom username, isinya akan mendarat di
		// journal.
		log.Printf("login gagal dari %s", key)
		a.renderLogin(w, r, "Username atau password salah.", next, http.StatusUnauthorized)
		return
	}

	a.logins.reset(key)
	a.setSessionCookie(w, r, a.sessions.create(a.cfg.Username, key))
	// Token CSRF diputar ulang saat privilege berubah.
	http.SetCookie(w, &http.Cookie{
		Name:     "garagepanel_csrf",
		Value:    randomHex(32),
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteStrictMode,
	})
	log.Printf("login berhasil dari %s", key)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		a.sessions.delete(c.Value)
	}
	a.clearSessionCookie(w, r)
	if !a.cfg.authEnabled() {
		http.Redirect(w, r, "/buckets", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// diagnoseS3Credentials membandingkan kredensial S3 panel dengan yang benar-benar
// dimiliki Garage untuk access key itu.
//
// Panel sudah memegang token Admin API, jadi ia bisa menanyakan langsung —
// dan itu mengubah "Invalid signature" dari tebak-tebakan jadi jawaban pasti:
// kalau secret-nya memang cocok, penyebabnya pasti bukan secret.
// Mengembalikan "" kalau tidak ada yang perlu dilaporkan.
func (a *App) diagnoseS3Credentials(ctx context.Context) string {
	if a.cfg.S3AccessKey == "" || a.cfg.S3SecretKey == "" {
		return ""
	}
	info, err := a.garage.GetKeyInfo(ctx, a.cfg.S3AccessKey, true)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			return fmt.Sprintf("Diagnosis panel: GARAGE_S3_ACCESS_KEY (%s) tidak dikenal Garage.\n"+
				"Lihat daftar key yang ada dengan: garage key list", a.cfg.S3AccessKey)
		}
		return "" // Admin API sedang tidak bisa ditanya; jangan menambah kebisingan.
	}

	got := info.Secret()
	if got == "" {
		return "Diagnosis panel: Garage tidak mau menunjukkan secret key untuk dibandingkan.\n" +
			"Kalau kamu memakai admin token ber-scope, tambahkan scope GetKeyInfo."
	}
	if got == a.cfg.S3SecretKey {
		return "Diagnosis panel: GARAGE_S3_SECRET_KEY SUDAH COCOK dengan yang dimiliki Garage,\n" +
			"jadi penyebabnya bukan secret key. Tersisa dua kemungkinan: region tidak cocok\n" +
			"(poin 2 di atas — ini yang paling mungkin), atau jam server meleset (poin 3)."
	}
	return fmt.Sprintf("Diagnosis panel: GARAGE_S3_SECRET_KEY TIDAK COCOK dengan yang dimiliki Garage\n"+
		"untuk key %s. Yang di file env panjangnya %d karakter, milik Garage %d karakter.\n"+
		"Ambil nilai yang benar dengan:\n"+
		"  garage key info --show-secret %s\n"+
		"lalu perbarui GARAGE_S3_SECRET_KEY dan restart panel.",
		a.cfg.S3AccessKey, len(a.cfg.S3SecretKey), len(got), info.Name)
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

	publicURL, exposed := a.s3PublicEndpoint()

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
		// Endpoint yang dipakai aplikasi lain, bukan alamat loopback yang
		// dipakai panel sendiri untuk bicara ke Garage.
		"S3PublicURL":   publicURL,
		"S3PublicReady": exposed,
		"S3InternalURL": a.cfg.S3URL,
		"Region":        a.cfg.S3Region,
	})
}

// s3PublicEndpoint mengembalikan alamat S3 API yang bisa dipakai dari luar
// server, beserta apakah Caddy memang sudah mengeksposnya.
//
// GARAGE_S3_URL hanya alamat loopback yang dipakai panel sendiri; menampilkan
// itu sebagai "endpoint" akan menyesatkan siapa pun yang memakai key ini dari
// mesin lain.
func (a *App) s3PublicEndpoint() (endpoint string, exposed bool) {
	if a.cfg.S3APIDomain == "" {
		return "", false
	}
	endpoint = "https://" + a.cfg.S3APIDomain
	wl, err := a.caddy.ReadWhitelist()
	return endpoint, err == nil && wl.Exists && len(wl.Entries) > 0
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
	Key         string
	Name        string
	Ext         string
	Size        int64
	Modified    time.Time
	IsImage     bool
	PublicURL   string
	PreviewURL  string
	DownloadURL string
}

type folderRow struct {
	Prefix string
	Name   string
	URL    string
}

type crumb struct {
	Name   string
	Prefix string
	URL    string
}

// queryEscape is url.QueryEscape with spaces as %20 rather than "+". Both
// decode the same on the server; %20 survives html/template's attribute
// escaping (which turns a literal "+" into &#43;) and reads better in links.
func queryEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// listURL builds an /objects link; every parameter is escaped here so
// templates never assemble URLs by hand. The view is only carried when the
// link is meant to switch it; otherwise the cookie remembers it.
func listURL(bucket, prefix, sortBy, dir, view, token string) string {
	parts := []string{"bucket=" + queryEscape(bucket)}
	if prefix != "" {
		parts = append(parts, "prefix="+queryEscape(prefix))
	}
	if sortBy != "" && sortBy != "name" {
		parts = append(parts, "sort="+queryEscape(sortBy))
	}
	if dir == "desc" {
		parts = append(parts, "dir=desc")
	}
	if view != "" {
		parts = append(parts, "view="+queryEscape(view))
	}
	if token != "" {
		parts = append(parts, "token="+queryEscape(token))
	}
	return "/objects?" + strings.Join(parts, "&")
}

// breadcrumbs splits "a/b/c/" into clickable segments.
func breadcrumbs(bucket, prefix, sortBy, dir string) []crumb {
	out := []crumb{{Name: bucket, Prefix: "", URL: listURL(bucket, "", sortBy, dir, "", "")}}
	acc := ""
	for _, seg := range strings.Split(strings.TrimSuffix(prefix, "/"), "/") {
		if seg == "" {
			continue
		}
		acc += seg + "/"
		out = append(out, crumb{Name: seg, Prefix: acc, URL: listURL(bucket, acc, sortBy, dir, "", "")})
	}
	return out
}

const viewCookieName = "garagepanel_view"

func (a *App) handleObjects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	bucket := strings.TrimSpace(q.Get("bucket"))
	prefix := q.Get("prefix")
	token := q.Get("token")
	sortBy, dir := SortParams(q.Get("sort"), q.Get("dir"))

	// The grid/list choice is remembered per browser.
	view := q.Get("view")
	if view != "grid" && view != "list" {
		view = ""
		if c, err := r.Cookie(viewCookieName); err == nil && (c.Value == "grid" || c.Value == "list") {
			view = c.Value
		}
	} else {
		http.SetCookie(w, &http.Cookie{Name: viewCookieName, Value: view, Path: "/objects", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 365 * 24 * 3600})
	}
	if view == "" {
		view = "grid"
	}

	buckets, bucketsErr := a.garage.ListBuckets(ctx)
	data := map[string]any{
		"Title":      "Objek",
		"Page":       "objects",
		"Buckets":    bucketNamesFrom(buckets),
		"Bucket":     bucket,
		"Prefix":     prefix,
		"MaxUploadH": humanBytes(MaxUploadSize),
		"AllowedExt": strings.Join(AllowedExtensionList(), " "),
		"Sort":       sortBy,
		"Dir":        dir,
		"View":       view,
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
	// A folder is always "name/"; a hand-typed prefix without the slash would
	// list partial matches with empty folder names.
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		http.Redirect(w, r, listURL(bucket, prefix+"/", sortBy, dir, "", token), http.StatusSeeOther)
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

	result, err := a.s3.ListObjectsV2(ctx, bucket, ListOptions{Prefix: prefix, Delimiter: "/", ContinuationToken: token, MaxKeys: objectsPerPage})
	if err != nil {
		msg := err.Error()
		// Untuk penolakan tanda tangan, tanyakan langsung ke Garage supaya
		// user tidak perlu menebak penyebabnya satu per satu.
		var s3err *S3Error
		if errors.As(err, &s3err) && s3err.StatusCode == http.StatusForbidden &&
			strings.Contains(strings.ToLower(s3err.Message+s3err.Body), "signature") {
			if d := a.diagnoseS3Credentials(ctx); d != "" {
				msg += "\n\n" + d
			}
		}
		data["Error"] = msg
		a.render(w, r, "objects.html", data)
		return
	}

	var rows []objectRow
	images := 0
	for _, obj := range result.Objects {
		// A "directory marker" object is the prefix itself; skip it.
		if obj.Key == prefix {
			continue
		}
		row := objectRow{
			Key:         obj.Key,
			Name:        strings.TrimPrefix(obj.Key, prefix),
			Ext:         strings.TrimPrefix(strings.ToLower(path.Ext(obj.Key)), "."),
			Size:        obj.Size,
			Modified:    obj.LastModified,
			IsImage:     IsImageKey(obj.Key),
			PreviewURL:  "/preview?bucket=" + queryEscape(bucket) + "&key=" + queryEscape(obj.Key),
			DownloadURL: "/preview?bucket=" + queryEscape(bucket) + "&key=" + queryEscape(obj.Key) + "&dl=1",
		}
		if domain != "" {
			row.PublicURL = publicURL(domain, obj.Key)
		}
		if row.IsImage {
			images++
		}
		rows = append(rows, row)
	}
	sortRows(rows, sortBy, dir)

	folders := make([]folderRow, 0, len(result.CommonPrefixes))
	for _, cp := range result.CommonPrefixes {
		folders = append(folders, folderRow{
			Prefix: cp,
			Name:   strings.TrimSuffix(strings.TrimPrefix(cp, prefix), "/"),
			URL:    listURL(bucket, cp, sortBy, dir, "", ""),
		})
	}
	if dir == "desc" && sortBy == "name" {
		for i, j := 0, len(folders)-1; i < j; i, j = i+1, j-1 {
			folders[i], folders[j] = folders[j], folders[i]
		}
	}

	sortURL := map[string]string{}
	for _, col := range []string{"name", "size", "date"} {
		nextDir := "asc"
		if col == sortBy && dir == "asc" {
			nextDir = "desc"
		}
		sortURL[col] = listURL(bucket, prefix, col, nextDir, "", token)
	}

	data["Rows"] = rows
	data["Folders"] = folders
	data["Breadcrumbs"] = breadcrumbs(bucket, prefix, sortBy, dir)
	data["ParentPrefix"] = parentPrefix(prefix)
	data["ParentURL"] = listURL(bucket, parentPrefix(prefix), sortBy, dir, "", "")
	data["HasParent"] = prefix != ""
	data["NextURL"] = ""
	if result.IsTruncated {
		data["NextURL"] = listURL(bucket, prefix, sortBy, dir, "", result.NextToken)
	}
	data["FirstURL"] = listURL(bucket, prefix, sortBy, dir, "", "")
	data["GridURL"] = listURL(bucket, prefix, sortBy, dir, "grid", token)
	data["ListURL"] = listURL(bucket, prefix, sortBy, dir, "list", token)
	data["SortURL"] = sortURL
	data["IsTruncated"] = result.IsTruncated
	data["Count"] = len(rows)
	data["ImageCount"] = images
	data["Empty"] = len(rows) == 0 && len(folders) == 0
	data["SyncEnabled"] = a.syncDisabled == ""
	data["PerPage"] = objectsPerPage

	a.render(w, r, "objects.html", data)
}

// sortRows orders one page of objects. The listing itself is always in key
// order; sorting by size or date is only ever within the page.
func sortRows(rows []objectRow, sortBy, dir string) {
	less := func(i, j int) bool {
		switch sortBy {
		case "size":
			if rows[i].Size != rows[j].Size {
				return rows[i].Size < rows[j].Size
			}
		case "date":
			if !rows[i].Modified.Equal(rows[j].Modified) {
				return rows[i].Modified.Before(rows[j].Modified)
			}
		}
		return rows[i].Key < rows[j].Key
	}
	if dir == "desc" {
		sort.SliceStable(rows, func(i, j int) bool { return less(j, i) })
		return
	}
	sort.SliceStable(rows, less)
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

// objectsBack builds the redirect target after a mutation.
func objectsBack(bucket, prefix string) string {
	return listURL(bucket, prefix, "", "", "", "")
}

// handleObjectsDeleteMany removes the keys ticked in the listing with one
// DeleteObjects call.
func (a *App) handleObjectsDeleteMany(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		a.redirectErr(w, r, "/objects", err)
		return
	}
	bucket := strings.TrimSpace(r.PostFormValue("bucket"))
	prefix := r.PostFormValue("prefix")
	keys := r.PostForm["key"]

	if err := ValidateBucketName(bucket); err != nil {
		a.redirectErr(w, r, "/objects", err)
		return
	}
	back := objectsBack(bucket, prefix)
	if err := ValidatePrefix(prefix); err != nil {
		a.redirectErr(w, r, back, err)
		return
	}
	if len(keys) == 0 {
		a.redirectErr(w, r, back, errors.New("tidak ada objek yang dipilih"))
		return
	}
	if len(keys) > maxDeleteObjects {
		a.redirectErr(w, r, back, fmt.Errorf("maksimal %d objek sekali hapus, dapat %d", maxDeleteObjects, len(keys)))
		return
	}
	for _, key := range keys {
		if err := ValidateObjectKey(key); err != nil {
			a.redirectErr(w, r, back, err)
			return
		}
	}
	if a.s3 == nil {
		a.redirectErr(w, r, back, errors.New("kredensial S3 panel belum diatur"))
		return
	}
	failed, err := a.s3.DeleteObjects(ctx, bucket, keys)
	if err != nil {
		a.redirectErr(w, r, back, fmt.Errorf("gagal menghapus: %w", err))
		return
	}
	log.Printf("bucket %q: %d objek dihapus, %d gagal", bucket, len(keys)-len(failed), len(failed))
	if len(failed) > 0 {
		var lines []string
		for i, f := range failed {
			if i == 5 {
				lines = append(lines, fmt.Sprintf("… dan %d lainnya", len(failed)-5))
				break
			}
			lines = append(lines, f.String())
		}
		a.redirectErr(w, r, back, fmt.Errorf("%d objek dihapus, %d gagal:\n%s", len(keys)-len(failed), len(failed), strings.Join(lines, "\n")))
		return
	}
	a.redirectOK(w, r, back, fmt.Sprintf("%d objek dihapus.", len(keys)))
}

// handleFolderCreate writes the zero-byte "prefix/name/" marker that S3
// clients treat as a folder.
func (a *App) handleFolderCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket := strings.TrimSpace(r.PostFormValue("bucket"))
	prefix := r.PostFormValue("prefix")
	name := strings.TrimSpace(r.PostFormValue("name"))

	if err := ValidateBucketName(bucket); err != nil {
		a.redirectErr(w, r, "/objects", err)
		return
	}
	back := objectsBack(bucket, prefix)
	if err := ValidatePrefix(prefix); err != nil {
		a.redirectErr(w, r, back, err)
		return
	}
	if err := ValidateFolderName(name); err != nil {
		a.redirectErr(w, r, back, fmt.Errorf("nama folder: %w", err))
		return
	}
	if a.s3 == nil {
		a.redirectErr(w, r, back, errors.New("kredensial S3 panel belum diatur"))
		return
	}
	key := prefix + name + "/"
	if err := ValidateObjectKey(key); err != nil {
		a.redirectErr(w, r, back, err)
		return
	}
	if exists, _, err := a.s3.StatObject(ctx, bucket, key); err != nil {
		a.redirectErr(w, r, back, err)
		return
	} else if exists {
		a.redirectErr(w, r, back, fmt.Errorf("folder %q sudah ada", name))
		return
	}
	if err := a.s3.PutObject(ctx, bucket, key, []byte{}, "application/x-directory"); err != nil {
		a.redirectErr(w, r, back, fmt.Errorf("gagal membuat folder: %w", err))
		return
	}
	a.redirectOK(w, r, objectsBack(bucket, key), fmt.Sprintf("Folder %q dibuat.", name))
}

func (a *App) handleObjectDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket := strings.TrimSpace(r.PostFormValue("bucket"))
	key := r.PostFormValue("key")
	prefix := r.PostFormValue("prefix")

	back := objectsBack(bucket, prefix)

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
	OK          bool   `json:"ok"`
	Key         string `json:"key,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Deduped     bool   `json:"deduped,omitempty"`
	Overwritten bool   `json:"overwritten,omitempty"`
	PublicURL   string `json:"publicUrl,omitempty"`
	Preview     string `json:"preview,omitempty"`
	Error       string `json:"error,omitempty"`
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
	// maxMemory sengaja kecil. Isi file tetap dibaca seluruhnya nanti untuk
	// menghitung sha256, jadi kalau multipart juga menahannya di memori,
	// puncak pemakaian jadi dua kali ukuran file. Dengan batas kecil ini file
	// besar tumpah ke temp file (PrivateTmp di unit systemd) dan hanya
	// tersalin sekali.
	const multipartMemory = 4 << 20
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
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
	// Files land in the folder being browsed. An empty prefix is the root.
	prefix := r.FormValue("prefix")
	if err := ValidateJobPrefix(prefix); err != nil {
		writeJSON(w, http.StatusBadRequest, uploadResult{Error: err.Error()})
		return
	}
	mode := r.FormValue("mode")
	if mode == "" {
		mode = "hash"
	}
	if mode != "hash" && mode != "name" {
		writeJSON(w, http.StatusBadRequest, uploadResult{Error: "mode penamaan harus hash atau name"})
		return
	}
	overwrite := r.FormValue("overwrite") == "1"

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, uploadResult{Error: "tidak ada file pada request: " + err.Error()})
		return
	}
	defer file.Close()

	// Only the base name is ever looked at; directories in the client's
	// path are dropped before any validation.
	baseName := path.Base(strings.ReplaceAll(header.Filename, `\`, `/`))
	var ext, contentType string
	if mode == "name" {
		ext, contentType, err = ValidateObjectBaseName(baseName)
	} else {
		ext, contentType, err = ValidateUploadName(baseName)
	}
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

	var key string
	if mode == "name" {
		key = prefix + baseName
	} else {
		// Content addressing: the name is derived from the bytes, so
		// uploading the same file twice cannot create a duplicate.
		sum := sha256.Sum256(content)
		key = prefix + hex.EncodeToString(sum[:])[:16] + ext
	}
	if err := ValidateObjectKey(key); err != nil {
		writeJSON(w, http.StatusBadRequest, uploadResult{Error: err.Error()})
		return
	}

	res := uploadResult{Key: key, Size: int64(len(content))}

	exists, _, err := a.s3.StatObject(ctx, bucket, key)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, uploadResult{Key: key, Error: err.Error()})
		return
	}
	switch {
	case exists && mode == "hash":
		// Same bytes, same name: nothing to store.
		res.Deduped = true
	case exists && !overwrite:
		writeJSON(w, http.StatusConflict, uploadResult{Key: key, Error: fmt.Sprintf("%s sudah ada di folder ini — centang \"timpa yang sudah ada\" kalau memang mau menggantinya", baseName)})
		return
	default:
		if err := a.s3.PutObject(ctx, bucket, key, content, contentType); err != nil {
			writeJSON(w, http.StatusBadGateway, uploadResult{Key: key, Error: err.Error()})
			return
		}
		res.Overwritten = exists
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
	if IsImageKey(key) && r.URL.Query().Get("dl") != "1" {
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
