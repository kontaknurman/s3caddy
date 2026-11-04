package main

import (
	"html"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// newKeyTestPanel has two buckets, the panel's own key on both, and the fake
// Garage accepting any permission.
func newKeyTestPanel(t *testing.T) *testPanel {
	t.Helper()
	p := newTestPanel(t)
	p.garage.anyPerms = true
	p.garage.addBucket("media", 3, 1024, true)
	p.garage.addBucket("backup", 0, 0, false)
	rw := BucketKeyPerm{Read: true, Write: true}
	p.garage.addKey("GK-test", "garagepanel", map[string]BucketKeyPerm{"media": rw, "backup": rw})
	return p
}

// resetKey posts the reset form and returns the response, its body (or the
// page it redirects to) and the Admin API calls the POST made.
func resetKey(t *testing.T, p *testPanel, bucket, key string) (*http.Response, string, []string) {
	t.Helper()
	form := url.Values{"bucket": {bucket}, "key": {key}, "csrf": {p.csrf(t)}}
	before := len(p.garage.callLog())
	resp, err := p.client.PostForm(p.srv.URL+"/buckets/keys/reset", form)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	calls := p.garage.callLog()[before:]
	body := string(raw)
	if resp.StatusCode == http.StatusSeeOther {
		body = p.followFlash(t, resp)
	}
	// html/template writes "+" as "&#43;"; compare against what the page says.
	return resp, html.UnescapeString(body), calls
}

// keyPage returns the key page of a bucket as the browser shows its text.
func keyPage(t *testing.T, p *testPanel, bucket string) string {
	t.Helper()
	resp, body := p.get(t, "/buckets/keys?bucket="+bucket)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("key page of %s: status %d", bucket, resp.StatusCode)
	}
	return html.UnescapeString(body)
}

func mutating(calls []string) []string {
	var out []string
	for _, c := range calls {
		if c == "CreateKey" || c == "AllowBucketKey" || c == "DeleteKey" {
			out = append(out, c)
		}
	}
	return out
}

func TestBucketsPageLinksToTheKeyPage(t *testing.T) {
	p := newKeyTestPanel(t)
	_, body := p.get(t, "/buckets")
	if !strings.Contains(body, `href="/buckets/keys?bucket=media"`) {
		t.Error("bucket row has no link to its key page")
	}
}

func TestBucketKeysPageListsKeysAndWhatTheyReach(t *testing.T) {
	p := newKeyTestPanel(t)
	p.garage.addKey("GKmedia", "media-key", map[string]BucketKeyPerm{"media": {Read: true, Write: true}})
	p.garage.addKey("GKshared", "shared-key", map[string]BucketKeyPerm{"media": {Read: true}, "backup": {Write: true}})

	body := keyPage(t, p, "media")
	for _, want := range []string{"media-key", "GKmedia", "read + write", "shared-key", "backup (write)", "key panel",
		`name="key" value="GKmedia"`, `name="key" value="GKshared"`} {
		if !strings.Contains(body, want) {
			t.Errorf("key page lacks %q", want)
		}
	}
	if strings.Contains(body, `name="key" value="GK-test"`) {
		t.Error("the panel's own key must not get a reset button")
	}

	// The backup page lists only the keys that reach backup.
	body = keyPage(t, p, "backup")
	if strings.Contains(body, "media-key") || !strings.Contains(body, "shared-key") {
		t.Errorf("backup key page lists the wrong keys")
	}
}

func TestKeyResetRecreatesTheKeyEverywhereAndDeletesTheOldOne(t *testing.T) {
	p := newKeyTestPanel(t)
	p.garage.addBucket("arsip", 0, 0, false)
	k := p.garage.addKey("GKshared", "shared-key", map[string]BucketKeyPerm{
		"media":  {Read: true, Owner: true},
		"backup": {Write: true},
		"arsip":  {Owner: true},
	})
	exp := "2030-01-01T00:00:00Z"
	k.CreateBucket, k.Expiration = true, &exp

	resp, body, calls := resetKey(t, p, "media", "GKshared")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", resp.StatusCode, body)
	}
	if got := strings.Join(calls, ","); got != "GetCurrentAdminTokenInfo,GetBucketInfo,GetKeyInfo,CreateKey,AllowBucketKey,AllowBucketKey,AllowBucketKey,DeleteKey" {
		t.Errorf("calls = %s", got)
	}
	for _, want := range []string{"berhasil di-reset", "GKshared-key", "SECRET-shared-key", "sekali saja",
		"media</strong>: read + owner", "backup</strong>: write", "arsip</strong>: owner", "membuat bucket baru", "2030-01-01T00:00:00Z"} {
		if !strings.Contains(body, want) {
			t.Errorf("result page lacks %q", want)
		}
	}

	if p.garage.key("GKshared") != nil {
		t.Error("the old key still exists")
	}
	nk := p.garage.key("GKshared-key")
	if nk == nil {
		t.Fatal("no new key")
	}
	media, backup, arsip := p.garage.aliases["media"], p.garage.aliases["backup"], p.garage.aliases["arsip"]
	if nk.Name != "shared-key" || !nk.CreateBucket || nk.Expiration == nil || *nk.Expiration != exp ||
		nk.Perms[media] != (BucketKeyPerm{Read: true, Owner: true}) || nk.Perms[backup] != (BucketKeyPerm{Write: true}) ||
		nk.Perms[arsip] != (BucketKeyPerm{Owner: true}) {
		t.Errorf("new key = %+v", nk)
	}
	if strings.Contains(body, "masih aktif") {
		t.Error("a complete reset must not warn that the old key is still active")
	}
}

func TestKeyResetWithoutExpirationKeepsTheNewKeyFromExpiring(t *testing.T) {
	p := newKeyTestPanel(t)
	p.garage.addKey("GKmedia", "media-key", map[string]BucketKeyPerm{"media": {Read: true, Write: true}})
	if resp, body, _ := resetKey(t, p, "media", "GKmedia"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, flashOf(body))
	}
	// The fake fails the test when CreateKey gets neither an expiration nor
	// neverExpires; here it must be neverExpires.
	if nk := p.garage.key("GKmedia-key"); nk == nil || nk.Expiration != nil || nk.CreateBucket {
		t.Errorf("new key = %+v", nk)
	}
}

func TestKeyResetRefusesWithoutChangingAnything(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(p *testPanel)
		bucket string
		key    string
		want   string
	}{
		{"panel key", func(*testPanel) {}, "media", "GK-test", "dipakai panel sendiri"},
		{"key of another bucket", func(p *testPanel) {
			p.garage.addKey("GKother", "other-key", map[string]BucketKeyPerm{"backup": {Read: true}})
		}, "media", "GKother", "tidak punya akses ke bucket"},
		{"unknown key", func(*testPanel) {}, "media", "GKnope", "tidak punya akses ke bucket"},
		{"local alias", func(p *testPanel) {
			k := p.garage.addKey("GKalias", "alias-key", map[string]BucketKeyPerm{"media": {Read: true}})
			k.LocalAliases[p.garage.aliases["backup"]] = []string{"cadangan"}
		}, "media", "GKalias", "alias lokal cadangan"},
		{"expired", func(p *testPanel) {
			k := p.garage.addKey("GKold", "old-key", map[string]BucketKeyPerm{"media": {Read: true}})
			k.Expired = true
		}, "media", "GKold", "sudah kedaluwarsa"},
		{"missing scope", func(p *testPanel) {
			p.garage.addKey("GKmedia", "media-key", map[string]BucketKeyPerm{"media": {Read: true}})
			p.garage.noScope["DeleteKey"] = true
		}, "media", "GKmedia", "tidak punya scope DeleteKey"},
		{"bad key id", func(*testPanel) {}, "media", "GK media", "access key id tidak valid"},
		{"bad bucket", func(*testPanel) {}, "Media!", "GKmedia", "nama bucket"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newKeyTestPanel(t)
			c.setup(p)
			keysBefore := len(p.garage.keyModel)
			resp, body, calls := resetKey(t, p, c.bucket, c.key)
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("status = %d, want a redirect with an error", resp.StatusCode)
			}
			if msg := html.UnescapeString(flashOf(body)); !strings.Contains(msg, c.want) || !strings.Contains(msg, "flash error") {
				t.Errorf("flash = %q, want an error with %q", msg, c.want)
			}
			if m := mutating(calls); len(m) > 0 {
				t.Errorf("refused reset still called %v", m)
			}
			if len(p.garage.keyModel) != keysBefore {
				t.Error("refused reset changed the keys")
			}
		})
	}
}

func TestKeyPageExplainsWhatBlocksAReset(t *testing.T) {
	p := newKeyTestPanel(t)
	k := p.garage.addKey("GKalias", "alias-key", map[string]BucketKeyPerm{"media": {Read: true}})
	k.LocalAliases[p.garage.aliases["media"]] = []string{"foto"}
	body := keyPage(t, p, "media")
	if !strings.Contains(body, "alias lokal foto") || strings.Contains(body, `name="key" value="GKalias"`) {
		t.Error("a key with a local alias must show why it cannot be reset, without a reset button")
	}

	p.garage.noScope["DeleteKey"] = true
	body = keyPage(t, p, "media")
	if !strings.Contains(body, "tidak punya scope DeleteKey") || strings.Contains(body, `action="/buckets/keys/reset"`) {
		t.Error("without the DeleteKey scope the page must say so and offer no reset")
	}

	// A Garage that cannot tell the token's scope: the list still loads, and a
	// key Garage will not show says which scope it takes.
	p.garage.noTokenInfo = true
	p.garage.noScope = map[string]bool{"GetKeyInfo": true}
	body = keyPage(t, p, "media")
	if !strings.Contains(body, "tambahkan scope GetKeyInfo") {
		t.Error("a 403 on GetKeyInfo must name the missing scope")
	}
}

func TestKeyResetProceedsWhenGarageCannotTellTheScope(t *testing.T) {
	p := newKeyTestPanel(t)
	p.garage.noTokenInfo = true
	p.garage.addKey("GKmedia", "media-key", map[string]BucketKeyPerm{"media": {Read: true, Write: true}})
	resp, body, _ := resetKey(t, p, "media", "GKmedia")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "berhasil di-reset") {
		t.Fatalf("status = %d: %s", resp.StatusCode, flashOf(body))
	}
}

func TestKeyResetSentAgainDoesNotResetTwice(t *testing.T) {
	p := newKeyTestPanel(t)
	p.garage.addKey("GKmedia", "media-key", map[string]BucketKeyPerm{"media": {Read: true, Write: true}})
	if resp, _, _ := resetKey(t, p, "media", "GKmedia"); resp.StatusCode != http.StatusOK {
		t.Fatalf("first reset: status %d", resp.StatusCode)
	}
	// A refresh of the result page sends the same form again.
	resp, body, calls := resetKey(t, p, "media", "GKmedia")
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(flashOf(body), "sudah di-reset") {
		t.Fatalf("second reset: status %d, flash %q", resp.StatusCode, flashOf(body))
	}
	if m := mutating(calls); len(m) > 0 {
		t.Errorf("second reset called %v", m)
	}
	if p.garage.key("GKmedia-key") == nil {
		t.Error("the key made by the first reset is gone")
	}
}

func TestKeyResetUndoesAHalfMadeKey(t *testing.T) {
	cases := []struct {
		name  string
		setup func(p *testPanel)
		want  string
	}{
		{"CreateKey fails", func(p *testPanel) { p.garage.failOps["CreateKey"] = "disk penuh" },
			"key baru gagal dibuat"},
		{"a grant fails", func(p *testPanel) { p.garage.failAllow[p.garage.aliases["backup"]] = "layout sedang berubah" },
			"izin bucket backup gagal diberikan"},
		{"old key cannot be deleted", func(p *testPanel) { p.garage.failDelete["GKshared"] = "timeout" },
			"key lama gagal dihapus"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newKeyTestPanel(t)
			p.garage.addKey("GKshared", "shared-key", map[string]BucketKeyPerm{"media": {Read: true}, "backup": {Write: true}})
			c.setup(p)
			resp, body, _ := resetKey(t, p, "media", "GKshared")
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("status = %d, want a redirect with an error", resp.StatusCode)
			}
			msg := html.UnescapeString(flashOf(body))
			if !strings.Contains(msg, c.want) || !strings.Contains(msg, "tidak ada yang berubah") {
				t.Errorf("flash = %q", msg)
			}
			if p.garage.key("GKshared") == nil {
				t.Error("the old key was lost")
			}
			if p.garage.key("GKshared-key") != nil {
				t.Error("the half-made new key was left behind")
			}
		})
	}
}

func TestKeyResetHandsOutTheNewKeyWhenNeitherKeyCanBeDeleted(t *testing.T) {
	p := newKeyTestPanel(t)
	p.garage.addKey("GKshared", "shared-key", map[string]BucketKeyPerm{"media": {Read: true}})
	p.garage.failDelete["GKshared"] = "timeout"
	p.garage.failDelete["GKshared-key"] = "timeout"

	resp, body, _ := resetKey(t, p, "media", "GKshared")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{"SECRET-shared-key", "masih aktif", "garage key delete --yes GKshared", "sama-sama aktif"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "berhasil di-reset") {
		t.Error("an unfinished reset must not claim success")
	}
}

func TestKeyResetCountsAnOldKeyThatIsGoneAsDeleted(t *testing.T) {
	p := newKeyTestPanel(t)
	p.garage.addKey("GKshared", "shared-key", map[string]BucketKeyPerm{"media": {Read: true}})
	// Garage deletes the key, but the answer is lost.
	p.garage.failDelete["GKshared"] = "connection reset"
	p.garage.deleteThenFail["GKshared"] = true

	resp, body, _ := resetKey(t, p, "media", "GKshared")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "berhasil di-reset") {
		t.Fatalf("status = %d: %s", resp.StatusCode, flashOf(body))
	}
	if p.garage.key("GKshared-key") == nil {
		t.Error("the new key was rolled back although the old one is gone")
	}
}

func TestKeyResetNeedsTheCSRFToken(t *testing.T) {
	p := newKeyTestPanel(t)
	p.garage.addKey("GKmedia", "media-key", map[string]BucketKeyPerm{"media": {Read: true}})
	p.csrf(t)
	before := len(p.garage.callLog())
	resp, err := p.client.PostForm(p.srv.URL+"/buckets/keys/reset", url.Values{"bucket": {"media"}, "key": {"GKmedia"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if calls := p.garage.callLog()[before:]; len(calls) > 0 {
		t.Errorf("a POST without the token reached Garage: %v", calls)
	}
}

func TestAdminTokenInfoMissing(t *testing.T) {
	full := &AdminTokenInfo{Scope: []string{"*"}}
	if m := full.Missing(keyResetOps...); m != nil {
		t.Errorf("full token misses %v", m)
	}
	scoped := &AdminTokenInfo{Scope: []string{"GetBucketInfo", "CreateKey", "AllowBucketKey"}}
	if m := scoped.Missing(keyResetOps...); !slices.Equal(m, []string{"GetKeyInfo", "DeleteKey"}) {
		t.Errorf("missing = %v", m)
	}
}

func TestPermLabel(t *testing.T) {
	for p, want := range map[BucketKeyPerm]string{
		{Read: true, Write: true}:              "read + write",
		{Write: true}:                          "write",
		{Read: true, Write: true, Owner: true}: "read + write + owner",
		{}:                                     "tanpa izin",
	} {
		if got := permLabel(p); got != want {
			t.Errorf("permLabel(%+v) = %q, want %q", p, got, want)
		}
	}
}

func TestValidateAccessKeyID(t *testing.T) {
	for _, ok := range []string{"GK841c4ccdf4943712677351d5", "GKnothex123", "GK-test"} {
		if err := ValidateAccessKeyID(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "GK 1", "GK/../x", "GK?id=x", strings.Repeat("a", 129), "GK\n1"} {
		if ValidateAccessKeyID(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
