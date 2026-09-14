package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	reloadOK   = []string{"sh", "-c", "exit 0"}
	reloadFail = []string{"sh", "-c", "echo 'run: adapting config using caddyfile: Caddyfile:3 - Error during parsing' >&2; exit 1"}
)

func newTestCaddy(t *testing.T, reload []string) *CaddyManager {
	t.Helper()
	return NewCaddyManager(t.TempDir(), "127.0.0.1:3902", "127.0.0.1:3900", reload)
}

func TestAddDomainWritesFileAndTag(t *testing.T) {
	c := newTestCaddy(t, reloadOK)
	if err := c.AddDomain(context.Background(), "cdn.example.com", "media"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(c.dir, "cdn.example.com.caddy"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)

	want := "# bucket: media\n" +
		"cdn.example.com {\n" +
		"\treverse_proxy 127.0.0.1:3902 {\n" +
		"\t\theader_up Host media\n" +
		"\t}\n" +
		"\theader Cache-Control \"public, max-age=31536000, immutable\"\n" +
		"\theader X-Content-Type-Options nosniff\n" +
		"\tencode gzip zstd\n" +
		"}\n"
	if got != want {
		t.Errorf("site file mismatch\n got:\n%s\nwant:\n%s", got, want)
	}

	// The tag on line one is the only source of the mapping, so it must parse
	// straight back out.
	sites, err := c.ListSites()
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 {
		t.Fatalf("got %d sites, want 1", len(sites))
	}
	if sites[0].Domain != "cdn.example.com" || sites[0].Bucket != "media" || !sites[0].Managed {
		t.Errorf("round trip lost data: %+v", sites[0])
	}
}

func TestAddDomainRefusesDuplicate(t *testing.T) {
	c := newTestCaddy(t, reloadOK)
	ctx := context.Background()
	if err := c.AddDomain(ctx, "cdn.example.com", "media"); err != nil {
		t.Fatal(err)
	}
	err := c.AddDomain(ctx, "cdn.example.com", "other")
	if err == nil {
		t.Fatal("expected duplicate domain to be refused")
	}
	if !strings.Contains(err.Error(), "sudah ada") {
		t.Errorf("unexpected error: %v", err)
	}
}

// A failed reload must leave nothing behind and must report what Caddy said.
func TestAddDomainRollsBackOnReloadFailure(t *testing.T) {
	c := newTestCaddy(t, reloadFail)
	err := c.AddDomain(context.Background(), "cdn.example.com", "media")
	if err == nil {
		t.Fatal("expected reload failure")
	}
	if _, statErr := os.Stat(filepath.Join(c.dir, "cdn.example.com.caddy")); !os.IsNotExist(statErr) {
		t.Error("file should have been rolled back (removed)")
	}
	if !strings.Contains(err.Error(), "Error during parsing") {
		t.Errorf("Caddy stderr not surfaced: %v", err)
	}
	// The stub fails every reload, so the reload after the rollback fails too.
	// The message must still make clear the change itself was undone.
	if !strings.Contains(err.Error(), "SUDAH dibatalkan") {
		t.Errorf("undo not reported to the user: %v", err)
	}
	entries, _ := os.ReadDir(c.dir)
	if len(entries) != 0 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

// Deleting a domain must restore the exact previous file when the reload fails.
func TestRemoveDomainRestoresOnReloadFailure(t *testing.T) {
	dir := t.TempDir()
	ok := NewCaddyManager(dir, "127.0.0.1:3902", "127.0.0.1:3900", reloadOK)
	if err := ok.AddDomain(context.Background(), "cdn.example.com", "media"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "cdn.example.com.caddy"))
	if err != nil {
		t.Fatal(err)
	}

	broken := NewCaddyManager(dir, "127.0.0.1:3902", "127.0.0.1:3900", reloadFail)
	if err := broken.RemoveDomain(context.Background(), "cdn.example.com"); err == nil {
		t.Fatal("expected reload failure")
	}

	after, err := os.ReadFile(filepath.Join(dir, "cdn.example.com.caddy"))
	if err != nil {
		t.Fatalf("file should have been restored: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("restored file differs\n got: %q\nwant: %q", after, before)
	}
}

func TestListSitesClassifiesFiles(t *testing.T) {
	c := newTestCaddy(t, reloadOK)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(c.dir, name), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	write("a.example.com.caddy", "# bucket: media\na.example.com {\n}\n")
	write("b.example.com.caddy", "b.example.com {\n\treverse_proxy 127.0.0.1:9000\n}\n") // no tag
	write("_s3api.caddy", "# allow: 203.0.113.10 | x\ns3.example.com {\n}\n")            // internal
	write("notes.txt", "ignored")

	sites, err := c.ListSites()
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 {
		t.Fatalf("got %d sites, want 2 (underscore and non-caddy files must be skipped): %+v", len(sites), sites)
	}
	if !sites[0].Managed || sites[0].Bucket != "media" {
		t.Errorf("a.example.com should be managed: %+v", sites[0])
	}
	if sites[1].Managed {
		t.Errorf("b.example.com has no bucket tag so it must not be managed: %+v", sites[1])
	}
	if sites[1].Problem == "" {
		t.Error("untagged file should carry a problem description")
	}

	if got := DomainCounts(sites)["media"]; got != 1 {
		t.Errorf("DomainCounts[media] = %d, want 1", got)
	}
	if got := DomainsForBucket(sites, "media"); len(got) != 1 || got[0] != "a.example.com" {
		t.Errorf("DomainsForBucket = %v", got)
	}
	if got := FirstDomainForBucket(sites, "nobody"); got != "" {
		t.Errorf("FirstDomainForBucket for unknown bucket = %q, want empty", got)
	}
}

func TestSitePathRejectsTraversal(t *testing.T) {
	c := newTestCaddy(t, reloadOK)
	for _, bad := range []string{"../etc/passwd", "a/b.com", "..", ".hidden.com", "UPPER.com", "no-dot"} {
		if _, err := c.sitePath(bad); err == nil {
			t.Errorf("sitePath(%q) should have been rejected", bad)
		}
	}
}

func TestWhitelistRoundTrip(t *testing.T) {
	c := newTestCaddy(t, reloadOK)
	ctx := context.Background()

	wl, err := c.ReadWhitelist()
	if err != nil {
		t.Fatal(err)
	}
	if wl.Exists {
		t.Fatal("whitelist should not exist yet")
	}

	entries := []WhitelistEntry{
		{Value: "203.0.113.10", Label: "server-app-jakarta"},
		{Value: "198.51.100.5/24", Label: "kantor"}, // host bits must be masked off
	}
	if err := c.WriteWhitelist(ctx, "s3.example.com", entries); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(c.WhitelistPath())
	if err != nil {
		t.Fatal(err)
	}
	want := "# allow: 203.0.113.10 | server-app-jakarta\n" +
		"# allow: 198.51.100.0/24 | kantor\n" +
		"s3.example.com {\n" +
		"\t@write_denied {\n" +
		"\t\tnot method GET HEAD\n" +
		"\t\tnot remote_ip 203.0.113.10 198.51.100.0/24\n" +
		"\t}\n" +
		"\trespond @write_denied \"Forbidden: IP not whitelisted\" 403\n\n" +
		"\treverse_proxy 127.0.0.1:3900\n" +
		"}\n"
	if string(data) != want {
		t.Errorf("whitelist file mismatch\n got:\n%s\nwant:\n%s", data, want)
	}

	wl, err = c.ReadWhitelist()
	if err != nil {
		t.Fatal(err)
	}
	if !wl.Exists || len(wl.Entries) != 2 {
		t.Fatalf("read back %d entries, want 2: %+v", len(wl.Entries), wl)
	}
	if wl.Entries[1].Value != "198.51.100.0/24" || wl.Entries[1].Label != "kantor" {
		t.Errorf("entry not round-tripped: %+v", wl.Entries[1])
	}
	if wl.Domain != "s3.example.com" {
		t.Errorf("domain = %q, want s3.example.com", wl.Domain)
	}
}

func TestWhitelistRejectsEmptyList(t *testing.T) {
	c := newTestCaddy(t, reloadOK)
	err := c.WriteWhitelist(context.Background(), "s3.example.com", nil)
	if err == nil {
		t.Fatal("an empty whitelist must be refused")
	}
	if !strings.Contains(err.Error(), "memblokir semua operasi tulis") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWhitelistSkipsCorruptLines(t *testing.T) {
	c := newTestCaddy(t, reloadOK)
	body := "# allow: not-an-ip | bad\n" +
		"# allow: 203.0.113.10 | ok\n" +
		"# allow: 198.51.100.0/24\n" +
		"s3.example.com {\n}\n"
	if err := os.WriteFile(c.WhitelistPath(), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	wl, err := c.ReadWhitelist()
	if err != nil {
		t.Fatal(err)
	}
	if len(wl.Entries) != 2 {
		t.Fatalf("got %d entries, want 2 (the invalid one must be dropped): %+v", len(wl.Entries), wl.Entries)
	}
	if wl.Entries[1].Label != "" {
		t.Errorf("entry without a label should have an empty label: %+v", wl.Entries[1])
	}
}

func TestDeleteWhitelist(t *testing.T) {
	c := newTestCaddy(t, reloadOK)
	ctx := context.Background()
	if err := c.WriteWhitelist(ctx, "s3.example.com", []WhitelistEntry{{Value: "203.0.113.10"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteWhitelist(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.WhitelistPath()); !os.IsNotExist(err) {
		t.Error("whitelist file should be gone")
	}
	if err := c.DeleteWhitelist(ctx); err == nil {
		t.Error("deleting a missing whitelist should report that it is not there")
	}
}

// Kalau ada file lain di direktori sites yang sudah rusak lebih dulu, reload
// akan gagal terus. Panel tetap harus membatalkan perubahannya sendiri, dan
// pesannya tidak boleh menuduh rollback-nya yang gagal — file panel memang
// sudah bersih. Ditemukan saat menguji dengan Caddy sungguhan.
func TestPreExistingBreakageIsNotReportedAsFailedRollback(t *testing.T) {
	c := newTestCaddy(t, reloadFail)

	err := c.AddDomain(context.Background(), "cdn.example.com", "media")
	if err == nil {
		t.Fatal("reload seharusnya gagal")
	}

	// File panel harus sudah hilang lagi.
	if _, statErr := os.Stat(filepath.Join(c.dir, "cdn.example.com.caddy")); !os.IsNotExist(statErr) {
		t.Error("file panel tidak dibatalkan")
	}

	var re *ReloadError
	if !errors.As(err, &re) {
		t.Fatalf("tipe error tidak terduga: %T", err)
	}
	if re.RollbackErr != nil {
		t.Errorf("RollbackErr terisi padahal file berhasil dikembalikan: %v", re.RollbackErr)
	}
	if !re.RolledBack {
		t.Error("RolledBack harus true")
	}
	if re.StillBroken == nil {
		t.Error("StillBroken harus terisi karena reload kedua juga gagal")
	}

	msg := err.Error()
	if strings.Contains(msg, "rollback gagal") {
		t.Errorf("pesan menuduh rollback gagal padahal tidak:\n%s", msg)
	}
	if !strings.Contains(msg, "SUDAH dibatalkan") {
		t.Errorf("pesan tidak menjelaskan bahwa perubahan sudah dibatalkan:\n%s", msg)
	}
	if !strings.Contains(msg, "di luar perubahan ini") {
		t.Errorf("pesan tidak mengarahkan ke penyebab sebenarnya:\n%s", msg)
	}
}

// Caddy menolak config yang mendefinisikan satu alamat site dua kali dengan
// "ambiguous site definition". Panel harus menangkapnya sebelum menulis,
// bukan menyerahkannya ke reload lalu rollback.
func TestSiteAddressCollisionIsRefusedBeforeWriting(t *testing.T) {
	t.Run("domain bentrok dengan file domain lain", func(t *testing.T) {
		c := newTestCaddy(t, reloadOK)
		// File dengan nama berbeda, tapi mendeklarasikan alamat yang sama.
		if err := os.WriteFile(filepath.Join(c.dir, "lain.caddy"),
			[]byte("cdn.example.com {\n\trespond \"hai\"\n}\n"), 0o640); err != nil {
			t.Fatal(err)
		}

		err := c.AddDomain(context.Background(), "cdn.example.com", "media")
		if err == nil {
			t.Fatal("tabrakan alamat harus ditolak")
		}
		if !strings.Contains(err.Error(), "lain.caddy") {
			t.Errorf("pesan tidak menyebut file yang bentrok: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(c.dir, "cdn.example.com.caddy")); !os.IsNotExist(statErr) {
			t.Error("tidak boleh ada file yang ditulis")
		}
	})

	// Persis kasus yang dilaporkan: sebuah domain sudah dilayani, lalu
	// S3_API_DOMAIN diisi domain yang sama dan whitelist ditulis.
	t.Run("S3_API_DOMAIN bentrok dengan domain yang sudah ada", func(t *testing.T) {
		c := newTestCaddy(t, reloadOK)
		if err := c.AddDomain(context.Background(), "s3.example.com", "media"); err != nil {
			t.Fatal(err)
		}

		err := c.WriteWhitelist(context.Background(), "s3.example.com",
			[]WhitelistEntry{{Value: "203.0.113.10", Label: "app"}})
		if err == nil {
			t.Fatal("tabrakan alamat harus ditolak")
		}
		if !strings.Contains(err.Error(), "s3.example.com.caddy") {
			t.Errorf("pesan tidak menyebut file yang bentrok: %v", err)
		}
		if !strings.Contains(err.Error(), "S3_API_DOMAIN") {
			t.Errorf("pesan tidak menjelaskan sumber masalahnya: %v", err)
		}
		if _, statErr := os.Stat(c.WhitelistPath()); !os.IsNotExist(statErr) {
			t.Error("whitelist tidak boleh ditulis")
		}
	})

	// Menulis ulang whitelist yang sudah ada tidak boleh dianggap bentrok
	// dengan dirinya sendiri.
	t.Run("menulis ulang whitelist sendiri bukan tabrakan", func(t *testing.T) {
		c := newTestCaddy(t, reloadOK)
		ctx := context.Background()
		if err := c.WriteWhitelist(ctx, "s3.example.com", []WhitelistEntry{{Value: "203.0.113.10"}}); err != nil {
			t.Fatal(err)
		}
		if err := c.WriteWhitelist(ctx, "s3.example.com", []WhitelistEntry{
			{Value: "203.0.113.10"}, {Value: "198.51.100.0/24"},
		}); err != nil {
			t.Fatalf("menambah entri ke whitelist sendiri ditolak: %v", err)
		}
	})
}

func TestSiteAddresses(t *testing.T) {
	dir := t.TempDir()
	body := "# bucket: media\n" +
		"cdn.example.com {\n" +
		"\treverse_proxy 127.0.0.1:3902 {\n" + // menjorok: bukan deklarasi site
		"\t\theader_up Host media\n" +
		"\t}\n" +
		"}\n" +
		"a.example.com, b.example.com {\n" + // beberapa alamat sekaligus
		"\trespond \"x\"\n" +
		"}\n"
	path := filepath.Join(dir, "x.caddy")
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	got, err := siteAddresses(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cdn.example.com", "a.example.com", "b.example.com"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("siteAddresses = %v, want %v", got, want)
	}
}

// Kalau bentroknya ada di Caddyfile utama (di luar jangkauan panel), Caddy yang
// menangkapnya — dan pesannya harus diterjemahkan jadi langkah konkret.
func TestAmbiguousSiteHintIsAdded(t *testing.T) {
	hint := reloadHint(`Error: adapting config using caddyfile: ambiguous site definition: s3.audiensi.com
caddy.service: Control process exited`)
	if !strings.Contains(hint, "s3.audiensi.com") {
		t.Errorf("hint tidak menyebut alamatnya: %q", hint)
	}
	if !strings.Contains(hint, "grep -rn") {
		t.Errorf("hint tidak memberi cara mencarinya: %q", hint)
	}
	if reloadHint("error lain yang tidak relevan") != "" {
		t.Error("hint tidak boleh muncul untuk error lain")
	}
}
