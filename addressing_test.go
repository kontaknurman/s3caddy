package main

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostedRemote points a Remote at a fake S3 with a hostname that has no DNS
// entry for its bucket subdomains; s3DialOverride routes them anyway.
func hostedRemote(t *testing.T, fake *fakeS3, name, provider, addressing string) Remote {
	t.Helper()
	endpoint := "http://s3.test.invalid"
	fake.virtualBase = "s3.test.invalid"
	s3DialOverride[endpoint] = fake.hostOnly()
	t.Cleanup(func() { delete(s3DialOverride, endpoint) })
	return Remote{Name: name, Provider: provider, Endpoint: endpoint, Region: "fsn1", AccessKey: fake.accessKey, SecretKey: fake.secretKey, Addressing: addressing}
}

func TestVirtualHostAddressingPutsTheBucketInTheHost(t *testing.T) {
	fake := newFakeS3(t, "AK", "SK", "fsn1")
	fake.put("comicu", "a b.txt", []byte("x"), "text/plain")
	r := hostedRemote(t, fake, "hz", "Hetzner", AddressingVirtual)
	c, err := r.Client()
	if err != nil {
		t.Fatal(err)
	}
	if !c.VirtualHost() {
		t.Fatal("Hetzner remote should use virtual-host addressing")
	}
	req, err := c.newRequest(context.Background(), http.MethodGet, "comicu", "a b.txt", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Host != "comicu.s3.test.invalid" || req.URL.EscapedPath() != "/a%20b.txt" {
		t.Errorf("virtual-host request = %s %s", req.URL.Host, req.URL.EscapedPath())
	}
	if !strings.Contains(req.Header.Get("Authorization"), "SignedHeaders=host;") {
		t.Error("host must be signed")
	}
	// The fake accepts it (signature over the virtual host) and the
	// listing goes to the bucket named in the host.
	res, err := c.ListObjectsV2(context.Background(), "comicu", ListOptions{MaxKeys: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 1 || res.Objects[0].Key != "a b.txt" {
		t.Errorf("listing via virtual host = %+v", res.Objects)
	}
	if fake.lastHost != "comicu.s3.test.invalid" {
		t.Errorf("Host header = %q", fake.lastHost)
	}
	if fake.callCount("ListObjectsV2.virtual") != 1 {
		t.Error("request was not virtual-hosted on the wire")
	}
	// Bucket-less requests (none exist today) would still be path-style.
	if _, _, err := c.HeadObject(context.Background(), "comicu", "a b.txt"); err != nil {
		t.Errorf("HEAD via virtual host: %v", err)
	}
}

// Hetzner refuses path-style requests. A remote saved with the wrong style
// (or auto) must be probed with the other one and switched.
func TestProbeSwitchesToTheAddressingStyleThatWorks(t *testing.T) {
	fake := newFakeS3(t, "AK", "SK", "fsn1")
	fake.virtualOnly = true
	fake.put("comicu", "x.txt", []byte("x"), "text/plain")
	store, err := NewRemoteStore(filepath.Join(t.TempDir(), "remotes.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Saved as path-style on purpose (what the old panel did for every remote).
	if err := store.Add(hostedRemote(t, fake, "hz", "Other", AddressingPath)); err != nil {
		t.Fatal(err)
	}
	report, err := store.Probe(t.Context(), "hz", "comicu", true)
	if err != nil {
		t.Fatalf("probe should succeed via virtual host: %v", err)
	}
	if !report.Virtual || !report.Switched || !report.WriteOK {
		t.Errorf("report = %+v", report)
	}
	saved, _ := store.Get("hz")
	if saved.Addressing != AddressingVirtual {
		t.Errorf("addressing not persisted: %q", saved.Addressing)
	}
	env := strings.Join(saved.rcloneEnv("dst"), "\n")
	if !strings.Contains(env, "RCLONE_CONFIG_DST_FORCE_PATH_STYLE=false") {
		t.Errorf("rclone must be told to use virtual host too:\n%s", env)
	}
	// A second probe uses the saved style directly, no switching.
	report, err = store.Probe(t.Context(), "hz", "comicu", false)
	if err != nil || report.Switched {
		t.Errorf("second probe: report=%+v err=%v", report, err)
	}
	if fake.count("comicu") != 1 {
		t.Error("write probe left its test object behind")
	}
}

func TestProbeReportsReadOnlyBucketWithHint(t *testing.T) {
	fake := newFakeS3(t, "AK", "SK", "fsn1")
	fake.denyWrites = true
	fake.put("comicu", "x.txt", []byte("x"), "text/plain")
	store, err := NewRemoteStore(filepath.Join(t.TempDir(), "remotes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(hostedRemote(t, fake, "hz", "Hetzner", AddressingAuto)); err != nil {
		t.Fatal(err)
	}
	// Reading is fine.
	if _, err := store.Probe(t.Context(), "hz", "comicu", false); err != nil {
		t.Fatalf("read probe: %v", err)
	}
	// Writing is refused, and the message says which half failed and why it
	// might be.
	report, err := store.Probe(t.Context(), "hz", "comicu", true)
	if err == nil {
		t.Fatal("write probe must fail on a read-only bucket")
	}
	for _, want := range []string{"menulis objek uji ditolak", "AccessDenied", "hanya punya izin baca", "project yang sama"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
	if report.WriteOK {
		t.Error("WriteOK must be false")
	}
	if fake.count("comicu") != 1 {
		t.Error("nothing should have been written")
	}
}

func TestInvalidAccessKeyHintIsProviderSpecific(t *testing.T) {
	fake := newFakeS3(t, "AK", "SK", "us-west-004")
	fake.badKeyBody = `<Error><Code>InvalidAccessKeyId</Code><Message>Malformed Access Key Id</Message></Error>`
	store, err := NewRemoteStore(filepath.Join(t.TempDir(), "remotes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(hostedRemote(t, fake, "b2", "Backblaze", AddressingAuto)); err != nil {
		t.Fatal(err)
	}
	_, err = store.Probe(t.Context(), "b2", "comicu", false)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"Malformed Access Key Id", "keyID", "Master application key", "bukan secret"} {
		if !strings.Contains(msg, want) {
			t.Errorf("hint missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "belum punya izin") {
		t.Error("an unknown key must not be explained as a permission problem")
	}
}

func TestOtherProviderHints(t *testing.T) {
	c := &S3{label: "aws", provider: "AWS", region: "us-east-1"}
	redirect := c.hintFor(&S3Error{StatusCode: 301, Code: "PermanentRedirect", Message: "The bucket you are attempting to access must be addressed using the specified endpoint."})
	if !strings.Contains(redirect, "region") || !strings.Contains(redirect, `"us-east-1"`) {
		t.Errorf("redirect hint: %s", redirect)
	}
	malformed := c.hintFor(&S3Error{StatusCode: 400, Code: "AuthorizationHeaderMalformed", Message: "the region 'us-east-1' is wrong; expecting 'ap-southeast-1'"})
	if !strings.Contains(malformed, "region") {
		t.Errorf("malformed-auth hint: %s", malformed)
	}
	if hint := c.hintFor(&S3Error{StatusCode: 404, Code: "NoSuchBucket"}); !strings.Contains(hint, "Bucket tidak ada") {
		t.Errorf("no-such-bucket hint: %s", hint)
	}
	if hint := c.hintFor(&S3Error{StatusCode: 500, Code: "InternalError"}); hint != "" {
		t.Errorf("no hint expected for 500, got %q", hint)
	}
}

func TestRemoteUpdateKeepsSecretUnlessKeyChanges(t *testing.T) {
	store, err := NewRemoteStore(filepath.Join(t.TempDir(), "remotes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(testRemote("wasabi-sg")); err != nil {
		t.Fatal(err)
	}
	edited := testRemote("wasabi-sg")
	edited.Region = "eu-central-1"
	edited.Endpoint = "https://s3.eu-central-1.wasabisys.com"
	edited.SecretKey = "" // not retyped
	edited.Addressing = AddressingVirtual
	if err := store.Update("wasabi-sg", edited); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get("wasabi-sg")
	if got.Region != "eu-central-1" || got.SecretKey != "wasabi-secret-value" || got.Addressing != AddressingVirtual || got.UpdatedAt.IsZero() || got.CreatedAt.IsZero() {
		t.Errorf("after update: %+v", got.View())
	}
	// A new access key without its secret is refused.
	edited.AccessKey = "NEWKEY"
	if err := store.Update("wasabi-sg", edited); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Errorf("changed key without secret: err = %v", err)
	}
	edited.SecretKey = "new-secret"
	if err := store.Update("wasabi-sg", edited); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get("wasabi-sg")
	if got.AccessKey != "NEWKEY" || got.SecretKey != "new-secret" {
		t.Errorf("credentials not updated: %+v", got.View())
	}
	if err := store.Update("ghost", edited); err == nil {
		t.Error("unknown remote accepted")
	}
	reloaded, err := NewRemoteStore(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := reloaded.Get("wasabi-sg"); again.Addressing != AddressingVirtual || again.AccessKey != "NEWKEY" {
		t.Error("update not persisted")
	}
}

func TestJobCreateRefusesAReadOnlyDestination(t *testing.T) {
	e := newJobTestEnv(t)
	e.remote.put("backup", "in/a.txt", []byte("x"), "text/plain")
	e.garage.denyWrites = true
	_, err := e.mgr.Create(t.Context(), e.importSpec("overwrite"))
	if err == nil {
		t.Fatal("a destination the key cannot write to must be refused before the job exists")
	}
	if !strings.Contains(err.Error(), "tujuan tidak bisa diakses") || !strings.Contains(err.Error(), "menulis objek uji ditolak") {
		t.Errorf("error = %v", err)
	}
	if len(e.mgr.List()) != 0 {
		t.Error("no job should exist")
	}
	// The source only needs to be readable.
	e.garage.denyWrites = false
	e.remote.denyWrites = true
	if _, err := e.mgr.Create(t.Context(), e.importSpec("overwrite")); err != nil {
		t.Errorf("read-only source is fine for an import: %v", err)
	}
}

func TestRemoteEditAndTestThroughTheUI(t *testing.T) {
	p := newTestPanel(t)
	p.addTestRemote(t)
	p.remote.put("backup", "x.txt", []byte("x"), "text/plain")

	// Test with write: the message says both halves passed.
	resp := p.post(t, "/sync/remotes/test", url.Values{"name": {"wasabi"}, "bucket": {"backup"}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "menulis dan menghapus objek uji juga berhasil") {
		t.Errorf("test with write: %s", firstLines(body))
	}
	if p.remote.count("backup") != 1 {
		t.Error("probe object left behind")
	}
	// Read-only test skips the write.
	before := p.remote.callCount("PutObject")
	resp = p.post(t, "/sync/remotes/test", url.Values{"name": {"wasabi"}, "bucket": {"backup"}, "write": {"0"}})
	if body := p.followFlash(t, resp); strings.Contains(body, "objek uji juga berhasil") || !strings.Contains(body, "bisa membaca bucket") || p.remote.callCount("PutObject") != before {
		t.Errorf("read-only test wrote something: %s", firstLines(body))
	}

	// Edit: change the region without retyping the secret.
	resp = p.post(t, "/sync/remotes/update", url.Values{
		"name": {"wasabi"}, "provider": {"Wasabi"}, "endpoint": {p.remote.URL()},
		"region": {"us-east-1"}, "access_key": {"WASABIKEY"}, "secret_key": {""}, "addressing": {"path"},
	})
	if body := p.followFlash(t, resp); !strings.Contains(body, "diperbarui") {
		t.Errorf("edit: %s", firstLines(body))
	}
	got, _ := p.app.remotes.Get("wasabi")
	if got.SecretKey != testRemoteSecret || got.Addressing != AddressingPath {
		t.Errorf("after edit: %+v", got.View())
	}
	// Both key fields blank keeps the stored pair, as the form promises.
	resp = p.post(t, "/sync/remotes/update", url.Values{"name": {"wasabi"}, "provider": {"Wasabi"}, "endpoint": {p.remote.URL()}, "region": {"us-east-1"}, "addressing": {"auto"}})
	p.followFlash(t, resp)
	got, _ = p.app.remotes.Get("wasabi")
	if got.AccessKey != "WASABIKEY" || got.SecretKey != testRemoteSecret || got.Addressing != AddressingAuto {
		t.Errorf("after blank-key edit: %+v", got.View())
	}
	// A new access key without its secret is refused.
	resp = p.post(t, "/sync/remotes/update", url.Values{"name": {"wasabi"}, "provider": {"Wasabi"}, "endpoint": {p.remote.URL()}, "region": {"us-east-1"}, "access_key": {"NEWKEY"}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "secret key-nya harus diisi") {
		t.Errorf("new access key without secret: %s", firstLines(body))
	}
	_, body := p.get(t, "/sync")
	if !strings.Contains(body, `action="/sync/remotes/update"`) || !strings.Contains(body, "path-style") {
		t.Errorf("edit form or addressing label missing: %s", firstLines(body))
	}
	if strings.Contains(body, testRemoteSecret) {
		t.Error("secret rendered into the edit form")
	}

	// Editing a remote a running job uses is refused.
	p.garage.addBucket("arsip", 0, 0, false)
	if err := os.WriteFile(filepath.Join(p.rcDir, "sleep"), []byte("0.5"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.post(t, "/sync/jobs/create", url.Values{"direction": {"import"}, "remote": {"wasabi"}, "remote_bucket": {"backup"}, "garage_bucket": {"arsip"}, "mode": {"overwrite"}, "transfers": {"1"}})
	p.waitJobs(t, func(js []jobView) bool { return len(js) == 1 && js[0].Status == JobRunning })
	resp = p.post(t, "/sync/remotes/update", url.Values{"name": {"wasabi"}, "provider": {"Wasabi"}, "endpoint": {p.remote.URL()}, "region": {"us-east-1"}, "access_key": {"WASABIKEY"}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "sedang dipakai job") {
		t.Errorf("edit while in use: %s", firstLines(body))
	}
}
