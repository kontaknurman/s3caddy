package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRemote(name string) Remote {
	return Remote{
		Name:      name,
		Provider:  "Wasabi",
		Endpoint:  "https://s3.ap-southeast-1.wasabisys.com",
		Region:    "ap-southeast-1",
		AccessKey: "WASABIACCESSKEY1",
		SecretKey: "wasabi-secret-value",
	}
}

func TestRemoteStoreRoundTripIsPrivateAndTrimsCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remotes.json")
	store, err := NewRemoteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	r := testRemote("wasabi-sg")
	r.AccessKey = " " + r.AccessKey + "\r\n" // pasted from Windows
	r.SecretKey = r.SecretKey + "\r"
	r.Endpoint = "https://S3.ap-southeast-1.wasabisys.com/"
	if err := store.Add(r); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("remotes.json mode = %o, want 0600", info.Mode().Perm())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".tmp-*")); len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}

	reloaded, err := NewRemoteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get("wasabi-sg")
	if !ok {
		t.Fatal("remote not found after reload")
	}
	if got.AccessKey != "WASABIACCESSKEY1" || got.SecretKey != "wasabi-secret-value" {
		t.Errorf("credentials not trimmed: %q / %q", got.AccessKey, got.SecretKey)
	}
	if got.Endpoint != "https://s3.ap-southeast-1.wasabisys.com" {
		t.Errorf("endpoint not normalised: %q", got.Endpoint)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}

	// Duplicates and the reserved name are refused.
	if err := store.Add(testRemote("wasabi-sg")); err == nil || !strings.Contains(err.Error(), "sudah ada") {
		t.Errorf("duplicate: err = %v", err)
	}
	if err := store.Add(testRemote("garage")); err == nil {
		t.Error("the name 'garage' is reserved for the panel's own Garage")
	}

	if err := store.Delete("wasabi-sg"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("wasabi-sg"); err == nil {
		t.Error("deleting twice must fail")
	}
	if len(store.List()) != 0 {
		t.Error("store should be empty")
	}
}

func TestRemoteStoreRefusesInvalidInput(t *testing.T) {
	store, err := NewRemoteStore(filepath.Join(t.TempDir(), "remotes.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Remote){
		"bad name":     func(r *Remote) { r.Name = "Wasabi SG" },
		"bad provider": func(r *Remote) { r.Provider = "wasabi" },
		"file scheme":  func(r *Remote) { r.Endpoint = "file:///etc/passwd" },
		"userinfo":     func(r *Remote) { r.Endpoint = "https://ak:sk@s3.wasabisys.com" },
		"path":         func(r *Remote) { r.Endpoint = "https://s3.wasabisys.com/bucket" },
		"metadata ip":  func(r *Remote) { r.Endpoint = "http://169.254.169.254" },
		"bad region":   func(r *Remote) { r.Region = "AP SE 1" },
		"empty key":    func(r *Remote) { r.AccessKey = "" },
		"empty secret": func(r *Remote) { r.SecretKey = "   " },
		"space inside": func(r *Remote) { r.SecretKey = "a b" },
	}
	for name, mutate := range cases {
		r := testRemote("ok")
		mutate(&r)
		if err := store.Add(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(store.List()) != 0 {
		t.Error("nothing should have been saved")
	}
}

func TestRemoteViewCarriesNoSecret(t *testing.T) {
	r := testRemote("wasabi-sg")
	v := r.View()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), r.SecretKey) || strings.Contains(string(raw), r.AccessKey) {
		t.Errorf("view leaks credentials: %s", raw)
	}
	if v.AccessKeyMasked != "WASA…KEY1" {
		t.Errorf("masked key = %q", v.AccessKeyMasked)
	}
	if v.Insecure {
		t.Error("https must not be flagged insecure")
	}
	r.Endpoint = "http://10.0.0.5:3900"
	if !r.View().Insecure {
		t.Error("http must be flagged insecure")
	}
	if maskAccessKey("short") != "•••••" {
		t.Errorf("short keys are fully masked, got %q", maskAccessKey("short"))
	}
}

func TestRemoteStoreRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remotes.json")
	if err := os.WriteFile(path, []byte(`{"remotes":[{"name":"../x","provider":"Wasabi"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRemoteStore(path); err == nil {
		t.Error("an invalid saved remote must be reported, not ignored")
	}
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRemoteStore(path); err == nil || !strings.Contains(err.Error(), "rusak") {
		t.Errorf("corrupt file: err = %v", err)
	}
}

func TestRcloneEnvMapsRemoteToAlias(t *testing.T) {
	env := testRemote("wasabi-sg").rcloneEnv("src")
	want := []string{
		"RCLONE_CONFIG_SRC_TYPE=s3",
		"RCLONE_CONFIG_SRC_PROVIDER=Wasabi",
		"RCLONE_CONFIG_SRC_ENDPOINT=https://s3.ap-southeast-1.wasabisys.com",
		"RCLONE_CONFIG_SRC_REGION=ap-southeast-1",
		"RCLONE_CONFIG_SRC_ACCESS_KEY_ID=WASABIACCESSKEY1",
		"RCLONE_CONFIG_SRC_SECRET_ACCESS_KEY=wasabi-secret-value",
		"RCLONE_CONFIG_SRC_FORCE_PATH_STYLE=true",
		"RCLONE_CONFIG_SRC_NO_CHECK_BUCKET=true",
	}
	joined := strings.Join(env, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("env missing %s:\n%s", w, joined)
		}
	}
	cfg := &Config{S3URL: "http://127.0.0.1:3900", S3Region: "garage", S3AccessKey: "GK1", S3SecretKey: "sk"}
	g := garageRemote(cfg).rcloneEnv(garageRemoteName)
	if !strings.Contains(strings.Join(g, "\n"), "RCLONE_CONFIG_GARAGE_PROVIDER=Other") {
		t.Errorf("garage side must use provider Other: %v", g)
	}
}

func TestRemoteProbeSurfacesProviderErrorWithRemoteHint(t *testing.T) {
	store, err := NewRemoteStore(filepath.Join(t.TempDir(), "remotes.json"))
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeS3(t, "AK", "SK", "us-east-1")
	fake.put("backup", "a.txt", []byte("x"), "text/plain")

	good := Remote{Name: "wasabi", Provider: "Wasabi", Endpoint: fake.URL(), Region: "us-east-1", AccessKey: "AK", SecretKey: "SK"}
	if err := store.Add(good); err != nil {
		t.Fatal(err)
	}
	if err := store.Probe(t.Context(), "wasabi", "backup"); err != nil {
		t.Errorf("probe with correct credentials failed: %v", err)
	}

	bad := good
	bad.Name = "wasabi-bad"
	bad.SecretKey = "WRONG"
	if err := store.Add(bad); err != nil {
		t.Fatal(err)
	}
	err = store.Probe(t.Context(), "wasabi-bad", "backup")
	if err == nil {
		t.Fatal("probe with a wrong secret must fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "SignatureDoesNotMatch") || !strings.Contains(msg, "wasabi-bad") {
		t.Errorf("provider message and remote name must be in the error: %s", msg)
	}
	if strings.Contains(msg, "garage.toml") {
		t.Error("a remote's 403 must not point at garage.toml")
	}
	if strings.Contains(msg, "WRONG") {
		t.Error("the secret must never appear in an error")
	}
	if err := store.Probe(t.Context(), "nope", "backup"); err == nil {
		t.Error("unknown remote must fail")
	}
	if err := store.Probe(t.Context(), "wasabi", "Bad Bucket"); err == nil {
		t.Error("invalid bucket name must fail before any request")
	}
}
