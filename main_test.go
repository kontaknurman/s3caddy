package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --- fake Garage Admin API ------------------------------------------------

type fakeGarage struct {
	mu      sync.Mutex
	buckets map[string]*BucketInfo // by id
	aliases map[string]string      // global alias -> id
	calls   []string
	allowed []string // access key ids passed to AllowBucketKey
	failOps map[string]string
	nextID  int
}

func newFakeGarage() *fakeGarage {
	return &fakeGarage{buckets: map[string]*BucketInfo{}, aliases: map[string]string{}, failOps: map[string]string{}}
}

func (f *fakeGarage) addBucket(name string, objects, size int64, public bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("bucket-id-%d", f.nextID)
	f.buckets[id] = &BucketInfo{
		ID:            id,
		Created:       "2026-01-01T00:00:00.000Z",
		GlobalAliases: []string{name},
		Objects:       objects,
		Bytes:         size,
		WebsiteAccess: public,
	}
	f.aliases[name] = id
	return id
}

func (f *fakeGarage) record(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op)
}

func (f *fakeGarage) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

func (f *fakeGarage) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":"Unauthorized","message":"bad token"}`)
			return
		}
		op := strings.TrimPrefix(r.URL.Path, "/v2/")
		f.record(op)
		f.mu.Lock()
		defer f.mu.Unlock()

		writeJSON := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
		fail := func(code int, msg string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "Error", "message": msg})
		}

		if msg, bad := f.failOps[op]; bad {
			fail(http.StatusInternalServerError, msg)
			return
		}

		switch op {
		case "GetClusterHealth":
			writeJSON(map[string]string{"status": "healthy"})

		case "ListBuckets":
			out := []BucketListItem{}
			for id, b := range f.buckets {
				out = append(out, BucketListItem{ID: id, Created: b.Created, GlobalAliases: b.GlobalAliases})
			}
			writeJSON(out)

		case "GetBucketInfo":
			id := r.URL.Query().Get("id")
			if alias := r.URL.Query().Get("globalAlias"); alias != "" {
				id = f.aliases[alias]
			}
			b, ok := f.buckets[id]
			if !ok {
				fail(http.StatusNotFound, "bucket tidak ada")
				return
			}
			writeJSON(b)

		case "CreateBucket":
			var body struct {
				GlobalAlias string `json:"globalAlias"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, exists := f.aliases[body.GlobalAlias]; exists {
				fail(http.StatusBadRequest, "bucket sudah ada")
				return
			}
			f.nextID++
			id := fmt.Sprintf("bucket-id-%d", f.nextID)
			b := &BucketInfo{ID: id, Created: "2026-01-01T00:00:00.000Z", GlobalAliases: []string{body.GlobalAlias}}
			f.buckets[id] = b
			f.aliases[body.GlobalAlias] = id
			writeJSON(b)

		case "DeleteBucket":
			id := r.URL.Query().Get("id")
			b, ok := f.buckets[id]
			if !ok {
				fail(http.StatusNotFound, "bucket tidak ada")
				return
			}
			if b.Objects > 0 {
				fail(http.StatusBadRequest, "bucket tidak kosong")
				return
			}
			for _, alias := range b.GlobalAliases {
				delete(f.aliases, alias)
			}
			delete(f.buckets, id)
			w.WriteHeader(http.StatusOK)

		case "UpdateBucket":
			id := r.URL.Query().Get("id")
			b, ok := f.buckets[id]
			if !ok {
				fail(http.StatusNotFound, "bucket tidak ada")
				return
			}
			var body struct {
				WebsiteAccess *struct {
					Enabled       bool   `json:"enabled"`
					IndexDocument string `json:"indexDocument"`
				} `json:"websiteAccess"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.WebsiteAccess != nil {
				b.WebsiteAccess = body.WebsiteAccess.Enabled
				if b.WebsiteAccess && body.WebsiteAccess.IndexDocument != indexDocument {
					t.Errorf("index document = %q, want %q", body.WebsiteAccess.IndexDocument, indexDocument)
				}
			}
			writeJSON(b)

		case "CreateKey":
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			secret := "SECRET-" + body.Name
			writeJSON(map[string]any{
				"accessKeyId":     "GK" + body.Name,
				"name":            body.Name,
				"secretAccessKey": secret,
				"expired":         false,
				"permissions":     map[string]bool{"createBucket": false},
				"buckets":         []any{},
			})

		case "AllowBucketKey":
			var body struct {
				BucketID    string        `json:"bucketId"`
				AccessKeyID string        `json:"accessKeyId"`
				Permissions BucketKeyPerm `json:"permissions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !body.Permissions.Read || !body.Permissions.Write {
				t.Errorf("AllowBucketKey should grant read+write, got %+v", body.Permissions)
			}
			if body.Permissions.Owner {
				t.Error("AllowBucketKey should not grant owner")
			}
			f.allowed = append(f.allowed, body.AccessKeyID)
			writeJSON(map[string]bool{"ok": true})

		default:
			fail(http.StatusNotFound, "operasi tidak dikenal: "+op)
		}
	}))
}

// --- test harness ---------------------------------------------------------

type testPanel struct {
	app      *App
	srv      *httptest.Server
	client   *http.Client
	sitesDir string
	garage   *fakeGarage
	s3Store  map[string][]byte
}

func newTestPanel(t *testing.T, opts ...func(*Config)) *testPanel {
	t.Helper()

	fg := newFakeGarage()
	garageSrv := fg.server(t)
	t.Cleanup(garageSrv.Close)

	store := map[string][]byte{}
	var storeMu sync.Mutex
	s3Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storeMu.Lock()
		defer storeMu.Unlock()
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
		key := ""
		if len(parts) == 2 {
			key, _ = url.PathUnescape(parts[1])
		}
		switch r.Method {
		case http.MethodGet:
			if key == "" { // ListObjectsV2
				w.Header().Set("Content-Type", "application/xml")
				io.WriteString(w, listXML)
				return
			}
			body, ok := store[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>tidak ada</Message></Error>`)
				return
			}
			w.Write(body)
		case http.MethodHead:
			if _, ok := store[key]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			store[key] = body
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			delete(store, key)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(s3Srv.Close)

	// Web endpoint (port 3902 in production): serves by Host header.
	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storeMu.Lock()
		defer storeMu.Unlock()
		if r.Host != "media" { // only "media" has website access in these tests
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, ok := store[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(body)
	}))
	t.Cleanup(webSrv.Close)

	dir := t.TempDir()
	cfg := &Config{
		AdminToken:  "test-token",
		AdminURL:    garageSrv.URL,
		S3URL:       s3Srv.URL,
		WebURL:      webSrv.URL,
		S3AccessKey: "GK-test",
		S3SecretKey: "secret-test",
		S3Region:    "garage",
		SitesDir:    dir,
		S3APIDomain: "s3.example.com",
		Listen:      "127.0.0.1:0",
		ReloadCmd:   reloadOK,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	var err error
	if cfg.webURL, err = parseEndpoint("GARAGE_WEB_URL", cfg.WebURL); err != nil {
		t.Fatal(err)
	}
	if cfg.s3URL, err = parseEndpoint("GARAGE_S3_URL", cfg.S3URL); err != nil {
		t.Fatal(err)
	}

	app, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.routes())
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // assert on the redirect itself
		},
	}

	return &testPanel{app: app, srv: srv, client: client, sitesDir: dir, garage: fg, s3Store: store}
}

func (p *testPanel) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	resp, err := p.client.Get(p.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// csrf primes the cookie and returns the token the forms must carry.
func (p *testPanel) csrf(t *testing.T) string {
	t.Helper()
	p.get(t, "/buckets")
	u, _ := url.Parse(p.srv.URL)
	for _, c := range p.client.Jar.Cookies(u) {
		if c.Name == "garagepanel_csrf" {
			return c.Value
		}
	}
	t.Fatal("no CSRF cookie was set")
	return ""
}

func (p *testPanel) post(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()
	form.Set("csrf", p.csrf(t))
	resp, err := p.client.PostForm(p.srv.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// followFlash follows a redirect and returns the rendered page, so the flash
// message can be asserted on.
func (p *testPanel) followFlash(t *testing.T, resp *http.Response) string {
	t.Helper()
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatalf("expected a redirect, got %d", resp.StatusCode)
	}
	_, body := p.get(t, loc)
	return body
}

// --- tests ----------------------------------------------------------------

func TestBucketsPageShowsSizeObjectsWebsiteAndDomainCount(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 42, 5*(1<<20), true)
	p.garage.addBucket("private-one", 0, 0, false)

	if err := p.app.caddy.AddDomain(t.Context(), "cdn.example.com", "media"); err != nil {
		t.Fatal(err)
	}
	if err := p.app.caddy.AddDomain(t.Context(), "img.example.com", "media"); err != nil {
		t.Fatal(err)
	}

	resp, body := p.get(t, "/buckets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{"media", "private-one", "5.0 MiB", ">42<", "public", "private", "cdn.example.com, img.example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("buckets page missing %q", want)
		}
	}
}

func TestCreateBucketRunsFourStepsAndShowsSecretOnce(t *testing.T) {
	p := newTestPanel(t)

	resp := p.post(t, "/buckets/create", url.Values{"name": {"foto"}, "public": {"on"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the result page is rendered directly so the secret is not lost)", resp.StatusCode)
	}

	// The four operations must run in this order.
	var seen []string
	for _, call := range p.garage.callLog() {
		switch call {
		case "CreateBucket", "CreateKey", "AllowBucketKey", "UpdateBucket":
			seen = append(seen, call)
		}
	}
	want := []string{"CreateBucket", "CreateKey", "AllowBucketKey", "AllowBucketKey", "UpdateBucket"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("call order = %v, want %v", seen, want)
	}

	// The bucket's own key and the panel's key must both be granted, otherwise
	// the object browser cannot read the bucket it just created.
	p.garage.mu.Lock()
	granted := strings.Join(p.garage.allowed, ",")
	p.garage.mu.Unlock()
	for _, key := range []string{"GKfoto-key", "GK-test"} {
		if !strings.Contains(granted, key) {
			t.Errorf("AllowBucketKey was not called for %q (got %q)", key, granted)
		}
	}

	// Fetch the rendered page again by repeating the POST is not possible, so
	// assert on the body of this response.
	resp2, err := p.client.PostForm(p.srv.URL+"/buckets/create", url.Values{
		"name": {"foto2"}, "public": {"on"}, "csrf": {p.csrf(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(resp2.Body)
	page := string(body)

	for _, want := range []string{"GKfoto2-key", "SECRET-foto2-key", "sekali saja", "Copy"} {
		if !strings.Contains(page, want) {
			t.Errorf("result page missing %q", want)
		}
	}
	if strings.Count(page, "SECRET-foto2-key") > 2 {
		t.Error("secret appears more often than the value plus its copy button")
	}

	// And the bucket really is public now.
	_, buckets := p.get(t, "/buckets")
	if !strings.Contains(buckets, "foto2") {
		t.Error("new bucket missing from the list")
	}
}

// The secret key is shown exactly once, so a failure after CreateBucket must
// still render the result page rather than throwing the credentials away — and
// a failed CreateKey leaves a nil *KeyInfo flowing into the template.
func TestCreateBucketReportsPartialFailureWithoutLosingThePage(t *testing.T) {
	p := newTestPanel(t)
	p.garage.mu.Lock()
	p.garage.failOps["CreateKey"] = "kuota key habis"
	p.garage.mu.Unlock()

	resp, err := p.client.PostForm(p.srv.URL+"/buckets/create", url.Values{
		"name": {"foto"}, "public": {"on"}, "csrf": {p.csrf(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(page, "kuota key habis") {
		t.Errorf("Garage's reason for the failed step is missing: %s", firstLines(page))
	}
	if !strings.Contains(page, "CreateBucket") {
		t.Error("the successful step should still be listed")
	}
	// Without a key there is nothing to show, and the page must say how to
	// finish the job by hand instead of rendering an empty credentials box.
	if !strings.Contains(page, "garage key create foto-key") {
		t.Errorf("expected manual recovery instructions: %s", firstLines(page))
	}
	// The bucket itself was created, so it stays in the list.
	if _, ok := p.garage.aliases["foto"]; !ok {
		t.Error("bucket should exist")
	}
}

func TestCreateBucketWithoutPublicSkipsWebsiteAccess(t *testing.T) {
	p := newTestPanel(t)
	p.post(t, "/buckets/create", url.Values{"name": {"arsip"}})
	for _, call := range p.garage.callLog() {
		if call == "UpdateBucket" {
			t.Fatal("website access must not be touched when the public box is unchecked")
		}
	}
}

func TestCreateBucketRejectsInvalidName(t *testing.T) {
	p := newTestPanel(t)
	resp := p.post(t, "/buckets/create", url.Values{"name": {"../etc/passwd"}})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, "tidak valid") {
		t.Errorf("expected a validation message, got: %s", firstLines(body))
	}
	for _, call := range p.garage.callLog() {
		if call == "CreateBucket" {
			t.Fatal("an invalid name must never reach Garage")
		}
	}
}

func TestCreateBucketSurfacesGarageError(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, false)
	resp := p.post(t, "/buckets/create", url.Values{"name": {"media"}})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, "bucket sudah ada") {
		t.Errorf("Garage's own message should be shown, got: %s", firstLines(body))
	}
}

func TestToggleWebsiteAccess(t *testing.T) {
	p := newTestPanel(t)
	id := p.garage.addBucket("media", 0, 0, false)

	p.post(t, "/buckets/website", url.Values{"bucket": {"media"}, "enable": {"1"}})
	if !p.garage.buckets[id].WebsiteAccess {
		t.Error("website access should be on")
	}
	p.post(t, "/buckets/website", url.Values{"bucket": {"media"}, "enable": {"0"}})
	if p.garage.buckets[id].WebsiteAccess {
		t.Error("website access should be off")
	}
}

func TestDeleteBucketRefusedWhileDomainsPoint(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)
	if err := p.app.caddy.AddDomain(t.Context(), "cdn.example.com", "media"); err != nil {
		t.Fatal(err)
	}

	resp := p.post(t, "/buckets/delete", url.Values{"bucket": {"media"}, "confirm": {"media"}})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, "cdn.example.com") {
		t.Errorf("the blocking domain must be named, got: %s", firstLines(body))
	}
	if _, ok := p.garage.aliases["media"]; !ok {
		t.Fatal("bucket must not have been deleted")
	}
}

func TestDeleteBucketNeedsExactConfirmation(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, false)

	resp := p.post(t, "/buckets/delete", url.Values{"bucket": {"media"}, "confirm": {"medi"}})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, "konfirmasi tidak cocok") {
		t.Errorf("expected a confirmation mismatch, got: %s", firstLines(body))
	}
	if _, ok := p.garage.aliases["media"]; !ok {
		t.Fatal("bucket must still exist")
	}

	resp = p.post(t, "/buckets/delete", url.Values{"bucket": {"media"}, "confirm": {"media"}})
	p.followFlash(t, resp)
	if _, ok := p.garage.aliases["media"]; ok {
		t.Fatal("bucket should have been deleted")
	}
}

func TestDeleteNonEmptyBucketShowsGarageReason(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 7, 100, false)
	resp := p.post(t, "/buckets/delete", url.Values{"bucket": {"media"}, "confirm": {"media"}})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, "harus kosong") {
		t.Errorf("expected the not-empty reason, got: %s", firstLines(body))
	}
}

func TestAddDomainWritesFileAndAppearsOnPage(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)

	resp := p.post(t, "/domains/create", url.Values{"domain": {"cdn.example.com"}, "bucket": {"media"}})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, "cdn.example.com") {
		t.Errorf("domain not listed: %s", firstLines(body))
	}

	data, err := os.ReadFile(filepath.Join(p.sitesDir, "cdn.example.com.caddy"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "# bucket: media\n") {
		t.Errorf("bucket tag missing from file:\n%s", data)
	}
	if !strings.Contains(string(data), "header_up Host media") {
		t.Errorf("Host rewrite missing:\n%s", data)
	}
}

func TestAddDomainRejectsUnknownBucket(t *testing.T) {
	p := newTestPanel(t)
	resp := p.post(t, "/domains/create", url.Values{"domain": {"cdn.example.com"}, "bucket": {"ghost"}})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, "tidak ditemukan") {
		t.Errorf("expected a missing-bucket error, got: %s", firstLines(body))
	}
	if _, err := os.Stat(filepath.Join(p.sitesDir, "cdn.example.com.caddy")); !os.IsNotExist(err) {
		t.Error("no file should have been written")
	}
}

func TestDomainReloadFailureIsRolledBackAndReported(t *testing.T) {
	p := newTestPanel(t, func(c *Config) { c.ReloadCmd = reloadFail })
	p.garage.addBucket("media", 0, 0, true)

	resp := p.post(t, "/domains/create", url.Values{"domain": {"cdn.example.com"}, "bucket": {"media"}})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, "Error during parsing") {
		t.Errorf("Caddy stderr must reach the user, got: %s", firstLines(body))
	}
	if _, err := os.Stat(filepath.Join(p.sitesDir, "cdn.example.com.caddy")); !os.IsNotExist(err) {
		t.Error("file must have been rolled back")
	}
}

func TestWhitelistAddAndRemove(t *testing.T) {
	p := newTestPanel(t)

	resp := p.post(t, "/whitelist/add", url.Values{"value": {"203.0.113.10"}, "label": {"server-app"}})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, "203.0.113.10") || !strings.Contains(body, "server-app") {
		t.Errorf("entry not shown: %s", firstLines(body))
	}

	// The last entry cannot be removed: an empty list blocks every write.
	resp = p.post(t, "/whitelist/delete", url.Values{"value": {"203.0.113.10"}})
	body = p.followFlash(t, resp)
	if !strings.Contains(body, "entri terakhir") {
		t.Errorf("expected the last-entry guard, got: %s", firstLines(body))
	}

	// With a second entry present, removal is allowed.
	p.post(t, "/whitelist/add", url.Values{"value": {"198.51.100.0/24"}, "label": {"kantor"}})
	p.post(t, "/whitelist/delete", url.Values{"value": {"203.0.113.10"}})
	wl, err := p.app.caddy.ReadWhitelist()
	if err != nil {
		t.Fatal(err)
	}
	if len(wl.Entries) != 1 || wl.Entries[0].Value != "198.51.100.0/24" {
		t.Errorf("unexpected entries: %+v", wl.Entries)
	}

	// Removing the file entirely is the documented escape hatch.
	resp = p.post(t, "/whitelist/remove-file", url.Values{"confirm": {whitelistFileName}})
	p.followFlash(t, resp)
	if _, err := os.Stat(p.app.caddy.WhitelistPath()); !os.IsNotExist(err) {
		t.Error("whitelist file should be gone")
	}
}

func TestWhitelistPageShowsRequesterIP(t *testing.T) {
	p := newTestPanel(t)
	_, body := p.get(t, "/whitelist")
	if !strings.Contains(body, "Tambahkan IP saya") {
		t.Error("missing the add-my-IP control")
	}
	if !strings.Contains(body, "127.0.0.1") {
		t.Error("requester IP not displayed")
	}
	if !strings.Contains(body, "SSH tunnel") {
		t.Error("the loopback caveat should be explained so the user does not lock themselves out")
	}
}

func TestWhitelistDisabledWithoutDomain(t *testing.T) {
	p := newTestPanel(t, func(c *Config) { c.S3APIDomain = "" })
	_, body := p.get(t, "/whitelist")
	if !strings.Contains(body, "S3_API_DOMAIN belum diatur") {
		t.Errorf("expected the disabled notice, got: %s", firstLines(body))
	}
}

func TestUploadIsContentAddressedAndDeduplicates(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)
	if err := p.app.caddy.AddDomain(t.Context(), "cdn.example.com", "media"); err != nil {
		t.Fatal(err)
	}

	content := []byte("pretend this is a png")
	sum := sha256.Sum256(content)
	wantKey := hex.EncodeToString(sum[:])[:16] + ".png"

	first := p.upload(t, "media", "Photo.PNG", content)
	if !first.OK {
		t.Fatalf("upload failed: %s", first.Error)
	}
	if first.Key != wantKey {
		t.Errorf("key = %q, want %q", first.Key, wantKey)
	}
	if first.Deduped {
		t.Error("first upload should not be reported as deduplicated")
	}
	if first.PublicURL != "https://cdn.example.com/"+wantKey {
		t.Errorf("public URL = %q", first.PublicURL)
	}
	if string(p.s3Store[wantKey]) != string(content) {
		t.Error("object content not stored")
	}

	// Same bytes, different file name: same key, no second store.
	second := p.upload(t, "media", "copy-of-photo.png", content)
	if !second.OK || second.Key != wantKey {
		t.Errorf("second upload = %+v", second)
	}
	if !second.Deduped {
		t.Error("identical content must be reported as already present")
	}
}

func TestUploadRejectsDisallowedExtension(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)
	res := p.upload(t, "media", "payload.php", []byte("<?php ?>"))
	if res.OK {
		t.Fatal("a .php upload must be rejected")
	}
	if !strings.Contains(res.Error, "tidak diizinkan") {
		t.Errorf("unexpected error: %q", res.Error)
	}
	if len(p.s3Store) != 0 {
		t.Error("nothing should have been stored")
	}
}

func TestUploadRequiresCSRFHeader(t *testing.T) {
	p := newTestPanel(t)
	body, contentType := multipartBody(t, "media", "a.png", []byte("x"))
	req, err := http.NewRequest(http.MethodPost, p.srv.URL+"/upload", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := p.client.Do(req) // no X-CSRF-Token
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func (p *testPanel) upload(t *testing.T, bucket, filename string, content []byte) uploadResult {
	t.Helper()
	body, contentType := multipartBody(t, bucket, filename, content)
	req, err := http.NewRequest(http.MethodPost, p.srv.URL+"/upload", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-CSRF-Token", p.csrf(t))
	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out uploadResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	return out
}

func multipartBody(t *testing.T, bucket, filename string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("bucket", bucket); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func TestPreviewServesImagesInlineAndForcesDownloadOtherwise(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)
	p.s3Store["a.png"] = []byte("pngbytes")
	p.s3Store["page.html"] = []byte("<script>alert(1)</script>")

	resp, body := p.get(t, "/preview?bucket=media&key=a.png")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("content type = %q, want image/png", got)
	}
	if got := resp.Header.Get("Content-Disposition"); got != "inline" {
		t.Errorf("disposition = %q, want inline", got)
	}
	if body != "pngbytes" {
		t.Errorf("body = %q", body)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff header missing")
	}

	// HTML must never render in the panel's own origin.
	resp, _ = p.get(t, "/preview?bucket=media&key=page.html")
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("html content type = %q, want application/octet-stream", got)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("html disposition = %q, want attachment", resp.Header.Get("Content-Disposition"))
	}
}

// A private bucket is not served by the web endpoint, so /preview must fall
// back to a signed GetObject rather than showing a broken image.
func TestPreviewFallsBackToSignedS3ForPrivateBucket(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("rahasia", 0, 0, false)
	p.s3Store["secret.png"] = []byte("private-bytes")

	resp, body := p.get(t, "/preview?bucket=rahasia&key=secret.png")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != "private-bytes" {
		t.Errorf("body = %q, want the object served through the S3 fallback", body)
	}
}

func TestPreviewRejectsTraversal(t *testing.T) {
	p := newTestPanel(t)
	for _, q := range []string{
		"/preview?bucket=media&key=../../etc/passwd",
		"/preview?bucket=media&key=/etc/passwd",
		"/preview?bucket=../etc&key=a.png",
	} {
		resp, _ := p.get(t, q)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestObjectsPageListsImagesAndFiles(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 2, 2063, true)

	resp, body := p.get(t, "/objects?bucket=media")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// From listXML: one jpg (grid) and one pdf (list), plus a folder.
	if !strings.Contains(body, `loading="lazy"`) {
		t.Error("thumbnails must be lazy-loaded")
	}
	if !strings.Contains(body, "hello world.jpg") {
		t.Error("image not listed")
	}
	// html/template renders "+" in HTML text as the &#43; entity, which the
	// browser decodes back to a plus sign.
	if !strings.Contains(body, "plus&#43;file.pdf") {
		t.Error("non-image not listed")
	}
	if !strings.Contains(body, "key=plus%2bfile.pdf") {
		t.Error("the plus in the key must be percent-encoded in the preview link")
	}
	if !strings.Contains(body, "thumbs/") {
		t.Error("folder prefix not listed")
	}
	if !strings.Contains(body, "Halaman berikutnya") {
		t.Error("pagination link missing for a truncated listing")
	}
	if !strings.Contains(body, "/preview?bucket=media&amp;key=hello%20world.jpg") {
		t.Error("preview URL for the image is missing or wrongly escaped")
	}
}

func TestObjectsPageWithoutS3Credentials(t *testing.T) {
	p := newTestPanel(t, func(c *Config) { c.S3AccessKey = ""; c.S3SecretKey = "" })
	p.garage.addBucket("media", 0, 0, true)
	_, body := p.get(t, "/objects?bucket=media")
	if !strings.Contains(body, "GARAGE_S3_ACCESS_KEY") {
		t.Errorf("expected a clear message about missing credentials, got: %s", firstLines(body))
	}
}

// --- security ------------------------------------------------------------

func TestPostWithoutCSRFTokenIsRejected(t *testing.T) {
	p := newTestPanel(t)
	p.csrf(t) // prime the cookie so only the form field is missing

	resp, err := p.client.PostForm(p.srv.URL+"/buckets/create", url.Values{"name": {"media"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	for _, call := range p.garage.callLog() {
		if call == "CreateBucket" {
			t.Fatal("a request without a CSRF token must never reach Garage")
		}
	}
}

func TestCrossOriginPostIsRejected(t *testing.T) {
	p := newTestPanel(t)
	form := url.Values{"name": {"media"}, "csrf": {p.csrf(t)}}
	req, err := http.NewRequest(http.MethodPost, p.srv.URL+"/buckets/create", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// DNS rebinding: a hostname that resolves to 127.0.0.1 must still be refused.
func TestNonLoopbackHostHeaderIsRejected(t *testing.T) {
	p := newTestPanel(t)
	req, err := http.NewRequest(http.MethodGet, p.srv.URL+"/buckets", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "panel.evil.example.com"
	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestHTMLIsEscaped(t *testing.T) {
	p := newTestPanel(t)
	// A flash message carrying markup must be escaped by html/template.
	id := p.app.flashes.add("error", `<img src=x onerror="alert(1)">`)
	_, body := p.get(t, "/buckets?m="+id)
	if strings.Contains(body, `<img src=x onerror=`) {
		t.Error("flash message was not escaped")
	}
	if !strings.Contains(body, "&lt;img src=x") {
		t.Error("expected the escaped form in the output")
	}
}

func TestUnknownRouteReturns404(t *testing.T) {
	p := newTestPanel(t)
	resp, _ := p.get(t, "/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func firstLines(s string) string {
	if len(s) > 700 {
		return s[:700] + "…"
	}
	return s
}

// --- login ----------------------------------------------------------------

// testPasswordHash dihitung sekali saja: PBKDF2 600k iterasi memang mahal.
var (
	testHashOnce  sync.Once
	testHashValue string
)

const testPassword = "password-panel-uji"

func testPasswordHash(t *testing.T) string {
	t.Helper()
	testHashOnce.Do(func() {
		h, err := HashPassword(testPassword)
		if err != nil {
			t.Fatal(err)
		}
		testHashValue = h
	})
	return testHashValue
}

func withLogin(t *testing.T) func(*Config) {
	hash := testPasswordHash(t)
	return func(c *Config) {
		c.Username = "admin"
		c.PasswordHash = hash
	}
}

// login melakukan POST /login dan mengembalikan responsnya (tanpa mengikuti redirect).
func (p *testPanel) login(t *testing.T, user, pass string) *http.Response {
	t.Helper()
	form := url.Values{"username": {user}, "password": {pass}, "csrf": {p.csrf(t)}}
	resp, err := p.client.PostForm(p.srv.URL+"/login", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestWithoutLoginConfiguredPanelStaysOpen(t *testing.T) {
	p := newTestPanel(t) // tanpa PANEL_PASSWORD_HASH
	resp, body := p.get(t, "/buckets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if strings.Contains(body, "Masuk ke panel") {
		t.Error("halaman login muncul padahal login tidak diaktifkan")
	}
}

func TestLoginRequiredWhenHashConfigured(t *testing.T) {
	p := newTestPanel(t, withLogin(t))

	resp, _ := p.get(t, "/buckets")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 ke /login", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/login?next=") {
		t.Errorf("Location = %q, want /login?next=…", loc)
	}

	// Halaman login sendiri harus bisa dibuka tanpa session.
	resp, body := p.get(t, "/login")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("halaman login status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, "Masuk ke panel") || !strings.Contains(body, `name="password"`) {
		t.Error("form login tidak dirender")
	}
	// Nav disembunyikan supaya tidak memancing klik yang pasti ditolak.
	if strings.Contains(body, `href="/whitelist"`) {
		t.Error("navigasi tidak boleh tampil di halaman login")
	}
}

func TestLoginSuccessThenAccessThenLogout(t *testing.T) {
	p := newTestPanel(t, withLogin(t))
	p.garage.addBucket("media", 1, 1024, true)

	resp := p.login(t, "admin", testPassword)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/buckets" {
		t.Errorf("Location = %q, want /buckets", loc)
	}
	var sessionSet bool
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			sessionSet = true
			if !c.HttpOnly {
				t.Error("cookie session harus HttpOnly")
			}
		}
	}
	if !sessionSet {
		t.Fatal("cookie session tidak dipasang")
	}

	// Sekarang panel bisa dipakai.
	resp, body := p.get(t, "/buckets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setelah login status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, "media") {
		t.Error("daftar bucket tidak tampil setelah login")
	}
	if !strings.Contains(body, "Keluar") {
		t.Error("tombol keluar tidak tampil")
	}

	// Logout mencabut session.
	if p.app.sessions.count() != 1 {
		t.Fatalf("jumlah session = %d, want 1", p.app.sessions.count())
	}
	resp = p.post(t, "/logout", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("logout status = %d", resp.StatusCode)
	}
	if p.app.sessions.count() != 0 {
		t.Error("session tidak dihapus saat logout")
	}
	resp, _ = p.get(t, "/buckets")
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("setelah logout harus diarahkan ke login, dapat %d", resp.StatusCode)
	}
}

func TestLoginRejectsWrongCredentials(t *testing.T) {
	p := newTestPanel(t, withLogin(t))

	for _, tc := range []struct{ user, pass string }{
		{"admin", "salah"},
		{"bukan-admin", testPassword},
		{"admin", ""},
	} {
		resp := p.login(t, tc.user, tc.pass)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s/%s: status = %d, want 401", tc.user, tc.pass, resp.StatusCode)
		}
		if p.app.sessions.count() != 0 {
			t.Fatal("session dibuat padahal kredensial salah")
		}
	}
}

func TestLoginRateLimited(t *testing.T) {
	p := newTestPanel(t, withLogin(t))

	for i := 0; i < loginMaxFailures; i++ {
		if resp := p.login(t, "admin", "salah"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("percobaan %d: status = %d, want 401", i+1, resp.StatusCode)
		}
	}
	resp := p.login(t, "admin", "salah")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 setelah %d kegagalan", resp.StatusCode, loginMaxFailures)
	}
	// Password yang benar pun ditolak selama masih terkunci.
	resp = p.login(t, "admin", testPassword)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 — penguncian harus berlaku juga untuk password benar", resp.StatusCode)
	}
}

func TestUploadWithoutSessionReturnsJSON(t *testing.T) {
	p := newTestPanel(t, withLogin(t))
	body, contentType := multipartBody(t, "media", "a.png", []byte("x"))
	req, err := http.NewRequest(http.MethodPost, p.srv.URL+"/upload", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-CSRF-Token", p.csrf(t))
	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content type = %q — JS pengunggah butuh JSON, bukan HTML login", ct)
	}
	var out uploadResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("jawaban bukan JSON: %v", err)
	}
	if out.Error == "" {
		t.Error("pesan error kosong")
	}
}

func TestPreviewRequiresLogin(t *testing.T) {
	p := newTestPanel(t, withLogin(t))
	p.s3Store["a.png"] = []byte("rahasia")

	resp, body := p.get(t, "/preview?bucket=media&key=a.png")
	if resp.StatusCode == http.StatusOK && strings.Contains(body, "rahasia") {
		t.Fatal("isi objek tersaji tanpa login")
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 ke login", resp.StatusCode)
	}
}

func TestLoginNextParameterCannotRedirectOffsite(t *testing.T) {
	p := newTestPanel(t, withLogin(t))
	form := url.Values{
		"username": {"admin"},
		"password": {testPassword},
		"csrf":     {p.csrf(t)},
		"next":     {"https://evil.example.com/"},
	}
	resp, err := p.client.PostForm(p.srv.URL+"/login", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/buckets" {
		t.Errorf("Location = %q — redirect ke luar harus diblokir", loc)
	}
}

func TestPanelDomainAcceptedInHostHeader(t *testing.T) {
	p := newTestPanel(t, withLogin(t), func(c *Config) { c.PanelDomain = "panel.example.com" })

	req, err := http.NewRequest(http.MethodGet, p.srv.URL+"/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "panel.example.com"
	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — PANEL_DOMAIN harus diterima", resp.StatusCode)
	}

	// Domain lain tetap ditolak.
	req, _ = http.NewRequest(http.MethodGet, p.srv.URL+"/login", nil)
	req.Host = "lain.example.com"
	resp2, err := p.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 untuk domain lain", resp2.StatusCode)
	}
}

// Melayani sebuah domain tanpa login berarti membuka hak menulis config Caddy
// ke internet. Panel harus menolak start.
func TestPanelDomainWithoutPasswordRefusesToStart(t *testing.T) {
	t.Setenv("GARAGE_ADMIN_TOKEN", "token")
	t.Setenv("PANEL_DOMAIN", "panel.example.com")
	t.Setenv("PANEL_PASSWORD_HASH", "")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig harus gagal")
	}
	if !strings.Contains(err.Error(), "PANEL_PASSWORD_HASH") {
		t.Errorf("pesan error tidak menyebut yang kurang: %v", err)
	}
}

func TestMalformedPasswordHashRefusesToStart(t *testing.T) {
	t.Setenv("GARAGE_ADMIN_TOKEN", "token")
	t.Setenv("PANEL_PASSWORD_HASH", "bukan-hash-yang-benar")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig harus gagal pada hash yang rusak")
	}
	if !strings.Contains(err.Error(), "hash-password") {
		t.Errorf("pesan error tidak menunjukkan cara memperbaiki: %v", err)
	}
}

// Regresi: dengan Referrer-Policy: no-referrer, Chrome mengirim "Origin: null"
// pada setiap submit form, dan cek origin dulu menolaknya — membuat SEMUA form
// di panel jadi 403 di browser sungguhan. http.Client bawaan Go tidak pernah
// mengirim header Origin, jadi tes lama tidak menangkapnya.
func TestFormPostWithNullOriginIsAccepted(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)

	form := url.Values{"bucket": {"media"}, "enable": {"1"}, "csrf": {p.csrf(t)}}
	req, err := http.NewRequest(http.MethodPost, p.srv.URL+"/buckets/website", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "null")

	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("Origin: null ditolak — semua form di browser akan gagal")
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}
}

// Origin dari situs lain tetap harus ditolak.
func TestFormPostWithForeignOriginStillRejected(t *testing.T) {
	p := newTestPanel(t)
	form := url.Values{"bucket": {"media"}, "enable": {"1"}, "csrf": {p.csrf(t)}}
	req, err := http.NewRequest(http.MethodPost, p.srv.URL+"/buckets/website", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example.com")

	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// Referrer-Policy harus tetap same-origin: no-referrer akan memicu Origin:null
// lagi, dan kebijakan yang lebih longgar akan membocorkan URL panel keluar.
func TestReferrerPolicyKeepsOriginUsable(t *testing.T) {
	p := newTestPanel(t)
	resp, _ := p.get(t, "/buckets")
	if got := resp.Header.Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("Referrer-Policy = %q, want same-origin", got)
	}
}
