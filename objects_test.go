package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func seedFolderTree(p *testPanel) {
	p.garage.addBucket("media", 0, 0, true)
	p.s3.put("media", "a/", []byte{}, "application/x-directory")
	p.s3.put("media", "a/b/", []byte{}, "application/x-directory")
	p.s3.put("media", "a/b/big.pdf", []byte(strings.Repeat("x", 5000)), "application/pdf")
	p.s3.put("media", "a/b/pic.png", []byte("png"), "image/png")
	p.s3.put("media", "a/b/small.txt", []byte("s"), "text/plain")
	p.s3.put("media", "a/b/c/deep.txt", []byte("d"), "text/plain")
	p.s3.put("media", "a/other.txt", []byte("o"), "text/plain")
}

func TestObjectsPageBreadcrumbsSortAndView(t *testing.T) {
	p := newTestPanel(t)
	seedFolderTree(p)

	resp, body := p.get(t, "/objects?bucket=media&prefix=a%2Fb%2F")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Breadcrumb: media › a › b, each a link to its prefix.
	for _, want := range []string{`href="/objects?bucket=media">media</a>`, `href="/objects?bucket=media&amp;prefix=a%2F">a</a>`, `>b</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("breadcrumb missing %s", want)
		}
	}
	if !strings.Contains(body, "📁 c") || strings.Contains(body, "📁 a/b/c") {
		t.Error("sub-folder should be shown by its own name")
	}
	if !strings.Contains(body, "Naik satu level") || !strings.Contains(body, `class="tile"`) {
		t.Error("expected the parent link and the default grid view")
	}
	// The marker object of the folder itself must not be listed as a file.
	if strings.Contains(body, `name="key" value="a/b/"`) {
		t.Error("folder marker listed as an object")
	}

	// Sorted by size descending: big.pdf first, small.txt last.
	_, body = p.get(t, "/objects?bucket=media&prefix=a%2Fb%2F&sort=size&dir=desc&view=list")
	big := strings.Index(body, "big.pdf")
	small := strings.Index(body, "small.txt")
	if big < 0 || small < 0 || big > small {
		t.Errorf("size desc order wrong: big at %d, small at %d", big, small)
	}
	if !strings.Contains(body, "<table>") || strings.Contains(body, `class="tile"`) {
		t.Error("view=list must render a table, not tiles")
	}
	if !strings.Contains(body, "hanya berlaku di dalam halaman ini") {
		t.Error("the sort caveat must be stated")
	}
	// The view choice is remembered.
	u, _ := url.Parse(p.srv.URL + "/objects")
	remembered := false
	for _, c := range p.client.Jar.Cookies(u) {
		if c.Name == viewCookieName && c.Value == "list" {
			remembered = true
		}
	}
	if !remembered {
		t.Error("view cookie not set")
	}
	_, body = p.get(t, "/objects?bucket=media&prefix=a%2Fb%2F")
	if !strings.Contains(body, "<table>") {
		t.Error("remembered list view not applied")
	}

	// Sort by name descending flips the order.
	_, body = p.get(t, "/objects?bucket=media&prefix=a%2Fb%2F&sort=name&dir=desc")
	if strings.Index(body, "small.txt") > strings.Index(body, "big.pdf") {
		t.Error("name desc order wrong")
	}
	// A prefix typed without its slash is normalised, not listed as a
	// partial match.
	resp, _ = p.get(t, "/objects?bucket=media&prefix=a%2Fb")
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "prefix=a%2Fb%2F") {
		t.Errorf("prefix without slash: status %d location %q", resp.StatusCode, loc)
	}
	// Unknown sort values fall back quietly.
	if resp, _ := p.get(t, "/objects?bucket=media&sort=%3Cscript%3E&dir=up&view=x"); resp.StatusCode != http.StatusOK {
		t.Errorf("bad sort params: status %d", resp.StatusCode)
	}
}

func TestBulkDeleteRemovesSelectedKeysAndReportsFailures(t *testing.T) {
	p := newTestPanel(t)
	seedFolderTree(p)
	p.s3.undeletable["a/b/pic.png"] = true

	resp := p.post(t, "/objects/delete-many", url.Values{
		"bucket": {"media"}, "prefix": {"a/b/"},
		"key": {"a/b/big.pdf", "a/b/small.txt", "a/b/pic.png"},
	})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, "2 objek dihapus, 1 gagal") || !strings.Contains(body, "a/b/pic.png") {
		t.Errorf("flash = %s", firstLines(body))
	}
	if _, ok := p.s3.get("media", "a/b/big.pdf"); ok {
		t.Error("big.pdf should be gone")
	}
	if _, ok := p.s3.get("media", "a/b/pic.png"); !ok {
		t.Error("the undeletable object must remain")
	}
	if p.s3.callCount("DeleteObjects") != 1 {
		t.Errorf("DeleteObjects called %d times, want 1", p.s3.callCount("DeleteObjects"))
	}

	// No selection, traversal, and too many keys are refused before S3.
	before := p.s3.callCount("DeleteObjects")
	resp = p.post(t, "/objects/delete-many", url.Values{"bucket": {"media"}, "prefix": {""}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "tidak ada objek yang dipilih") {
		t.Errorf("empty selection: %s", firstLines(body))
	}
	resp = p.post(t, "/objects/delete-many", url.Values{"bucket": {"media"}, "key": {"../etc/passwd"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "segmen") {
		t.Errorf("traversal key: %s", firstLines(body))
	}
	if p.s3.callCount("DeleteObjects") != before {
		t.Error("invalid requests must not reach S3")
	}
}

func TestCreateFolderWritesMarkerAndRefusesBadNames(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)

	resp := p.post(t, "/objects/mkdir", url.Values{"bucket": {"media"}, "prefix": {"docs/"}, "name": {"Laporan 2026"}})
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "prefix=docs%2FLaporan%202026%2F") {
		t.Errorf("should redirect into the new folder, got %q", loc)
	}
	body := p.followFlash(t, resp)
	if !strings.Contains(body, "dibuat") {
		t.Errorf("flash = %s", firstLines(body))
	}
	if _, ok := p.s3.get("media", "docs/Laporan 2026/"); !ok {
		t.Fatal("marker object not written")
	}
	if ct := p.s3.contentType("media", "docs/Laporan 2026/"); ct != "application/x-directory" {
		t.Errorf("marker content type = %q", ct)
	}

	resp = p.post(t, "/objects/mkdir", url.Values{"bucket": {"media"}, "prefix": {"docs/"}, "name": {"Laporan 2026"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "sudah ada") {
		t.Errorf("duplicate folder: %s", firstLines(body))
	}
	for _, bad := range []string{"..", "a/b", `a\b`, ""} {
		resp = p.post(t, "/objects/mkdir", url.Values{"bucket": {"media"}, "prefix": {""}, "name": {bad}})
		if body = p.followFlash(t, resp); !strings.Contains(body, "nama folder") {
			t.Errorf("bad name %q accepted: %s", bad, firstLines(body))
		}
	}
	if p.s3.count("media") != 1 {
		t.Errorf("objects = %d, want only the one marker", p.s3.count("media"))
	}
}

func TestDownloadFlagForcesAttachmentForImages(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)
	p.s3.put("media", "a.png", []byte("pngbytes"), "image/png")

	resp, body := p.get(t, "/preview?bucket=media&key=a.png&dl=1")
	if resp.StatusCode != http.StatusOK || body != "pngbytes" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("content type = %q", got)
	}
	if got := resp.Header.Get("Content-Disposition"); got != `attachment; filename="a.png"` {
		t.Errorf("disposition = %q", got)
	}
}

func TestUploadWithOriginalNameIntoFolder(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)

	res := p.uploadWith(t, "media", "Laporan Q3.PDF", []byte("pdf-1"), map[string]string{"mode": "name", "prefix": "docs/"})
	if !res.OK || res.Key != "docs/Laporan Q3.PDF" {
		t.Fatalf("result = %+v", res)
	}
	if got, _ := p.s3.get("media", "docs/Laporan Q3.PDF"); string(got) != "pdf-1" {
		t.Errorf("stored = %q", got)
	}
	if ct := p.s3.contentType("media", "docs/Laporan Q3.PDF"); ct != "application/pdf" {
		t.Errorf("content type = %q", ct)
	}

	// Same name again: refused unless overwrite is ticked.
	res = p.uploadWith(t, "media", "Laporan Q3.PDF", []byte("pdf-2"), map[string]string{"mode": "name", "prefix": "docs/"})
	if res.OK || !strings.Contains(res.Error, "sudah ada") {
		t.Errorf("duplicate name: %+v", res)
	}
	if got, _ := p.s3.get("media", "docs/Laporan Q3.PDF"); string(got) != "pdf-1" {
		t.Error("object must not have been replaced")
	}
	res = p.uploadWith(t, "media", "Laporan Q3.PDF", []byte("pdf-2"), map[string]string{"mode": "name", "prefix": "docs/", "overwrite": "1"})
	if !res.OK || !res.Overwritten {
		t.Errorf("overwrite: %+v", res)
	}
	if got, _ := p.s3.get("media", "docs/Laporan Q3.PDF"); string(got) != "pdf-2" {
		t.Error("object not replaced")
	}

	// Directories in the client's file name are dropped; dot-files and bad
	// extensions are refused.
	res = p.uploadWith(t, "media", `C:\Users\me\..\evil.jpg`, []byte("x"), map[string]string{"mode": "name"})
	if !res.OK || res.Key != "evil.jpg" {
		t.Errorf("path in file name: %+v", res)
	}
	for _, bad := range []string{".htaccess", "shell.sh", "noext"} {
		if res := p.uploadWith(t, "media", bad, []byte("x"), map[string]string{"mode": "name"}); res.OK {
			t.Errorf("%q accepted", bad)
		}
	}
	// Bad prefixes and modes are refused too.
	if res := p.uploadWith(t, "media", "a.jpg", []byte("x"), map[string]string{"mode": "name", "prefix": "no-slash"}); res.OK {
		t.Error("prefix without trailing slash accepted")
	}
	if res := p.uploadWith(t, "media", "a.jpg", []byte("x"), map[string]string{"mode": "random"}); res.OK {
		t.Error("unknown mode accepted")
	}

	// Hash mode inside a folder keeps content addressing under the prefix.
	res = p.uploadWith(t, "media", "photo.jpg", []byte("jpeg-bytes"), map[string]string{"mode": "hash", "prefix": "docs/"})
	if !res.OK || !strings.HasPrefix(res.Key, "docs/") || !strings.HasSuffix(res.Key, ".jpg") || len(res.Key) != len("docs/")+16+4 {
		t.Errorf("hash mode key = %q", res.Key)
	}
}

func TestRenameMoveAndCopySingleObjects(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)
	p.garage.addBucket("backup", 0, 0, false)
	p.s3.put("media", "docs/a.pdf", []byte("A"), "application/pdf")
	p.s3.put("media", "docs/b.pdf", []byte("B"), "application/pdf")
	p.s3.put("media", "docs/data.bin", []byte("bin"), "application/octet-stream")

	// Rename keeps the folder and the content type.
	resp := p.post(t, "/objects/rename", url.Values{"bucket": {"media"}, "prefix": {"docs/"}, "key": {"docs/a.pdf"}, "name": {"Laporan A.pdf"}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "diganti nama") {
		t.Errorf("rename: %s", firstLines(body))
	}
	if got, ok := p.s3.get("media", "docs/Laporan A.pdf"); !ok || string(got) != "A" {
		t.Error("renamed object missing")
	}
	if _, ok := p.s3.get("media", "docs/a.pdf"); ok {
		t.Error("old key still exists after rename")
	}
	if ct := p.s3.contentType("media", "docs/Laporan A.pdf"); ct != "application/pdf" {
		t.Errorf("content type after rename = %q", ct)
	}

	// Renaming onto an existing key is refused unless overwrite is ticked.
	resp = p.post(t, "/objects/rename", url.Values{"bucket": {"media"}, "prefix": {"docs/"}, "key": {"docs/b.pdf"}, "name": {"Laporan A.pdf"}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "sudah ada") {
		t.Errorf("rename onto existing: %s", firstLines(body))
	}
	if got, _ := p.s3.get("media", "docs/Laporan A.pdf"); string(got) != "A" {
		t.Error("target must be untouched")
	}
	resp = p.post(t, "/objects/rename", url.Values{"bucket": {"media"}, "prefix": {"docs/"}, "key": {"docs/b.pdf"}, "name": {"Laporan A.pdf"}, "overwrite": {"1"}})
	p.followFlash(t, resp)
	if got, _ := p.s3.get("media", "docs/Laporan A.pdf"); string(got) != "B" {
		t.Error("overwrite rename did not replace the target")
	}

	// An unlisted extension may stay, but not be introduced.
	resp = p.post(t, "/objects/rename", url.Values{"bucket": {"media"}, "prefix": {"docs/"}, "key": {"docs/data.bin"}, "name": {"data2.bin"}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "diganti nama") {
		t.Errorf("keeping .bin must be allowed: %s", firstLines(body))
	}
	resp = p.post(t, "/objects/rename", url.Values{"bucket": {"media"}, "prefix": {"docs/"}, "key": {"docs/data2.bin"}, "name": {"data.sh"}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "tidak diizinkan") {
		t.Errorf(".sh must be refused: %s", firstLines(body))
	}
	for _, bad := range []string{"../x.pdf", "a/b.pdf", ".hidden.pdf", ""} {
		resp = p.post(t, "/objects/rename", url.Values{"bucket": {"media"}, "prefix": {"docs/"}, "key": {"docs/data2.bin"}, "name": {bad}})
		if body := p.followFlash(t, resp); strings.Contains(body, "diganti nama") {
			t.Errorf("bad name %q accepted", bad)
		}
	}

	// Move across buckets keeps the base name.
	resp = p.post(t, "/objects/move", url.Values{"bucket": {"media"}, "prefix": {"docs/"}, "key": {"docs/Laporan A.pdf"}, "dst_bucket": {"backup"}, "dst_prefix": {"arsip/2026"}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "dipindah ke backup/arsip/2026/Laporan A.pdf") {
		t.Errorf("move: %s", firstLines(body))
	}
	if _, ok := p.s3.get("backup", "arsip/2026/Laporan A.pdf"); !ok {
		t.Error("moved object missing at destination")
	}
	if _, ok := p.s3.get("media", "docs/Laporan A.pdf"); ok {
		t.Error("moved object still at source")
	}

	// Copy keeps the source.
	resp = p.post(t, "/objects/copy", url.Values{"bucket": {"backup"}, "prefix": {"arsip/2026/"}, "key": {"arsip/2026/Laporan A.pdf"}, "dst_bucket": {"media"}, "dst_prefix": {""}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "disalin ke media/Laporan A.pdf") {
		t.Errorf("copy: %s", firstLines(body))
	}
	if _, ok := p.s3.get("backup", "arsip/2026/Laporan A.pdf"); !ok {
		t.Error("copy must keep the source")
	}
	if _, ok := p.s3.get("media", "Laporan A.pdf"); !ok {
		t.Error("copy missing at destination")
	}
	// Same place is refused.
	resp = p.post(t, "/objects/copy", url.Values{"bucket": {"media"}, "prefix": {""}, "key": {"Laporan A.pdf"}, "dst_bucket": {"media"}, "dst_prefix": {""}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "sumber dan tujuan sama") {
		t.Errorf("same place: %s", firstLines(body))
	}

	// A failed delete after a successful copy is reported as such.
	p.s3.put("media", "locked.pdf", []byte("L"), "application/pdf")
	p.s3.undeletable["locked.pdf"] = true
	resp = p.post(t, "/objects/move", url.Values{"bucket": {"media"}, "prefix": {""}, "key": {"locked.pdf"}, "dst_bucket": {"backup"}, "dst_prefix": {""}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "sudah disalin") || !strings.Contains(body, "gagal dihapus") {
		t.Errorf("copy-ok-delete-failed: %s", firstLines(body))
	}
	if _, ok := p.s3.get("backup", "locked.pdf"); !ok {
		t.Error("copy half should have landed")
	}
}

func TestFolderJobsStartFromTheObjectsPage(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)
	p.garage.addBucket("backup", 0, 0, false)
	p.s3.put("media", "photos/", []byte{}, "application/x-directory")
	p.s3.put("media", "photos/a.jpg", []byte("a"), "image/jpeg")
	p.s3.put("media", "photos/sub/b.jpg", []byte("b"), "image/jpeg")
	p.s3.put("media", "keep.txt", []byte("k"), "text/plain")

	_, body := p.get(t, "/objects?bucket=media&prefix=photos%2F")
	if !strings.Contains(body, "Folder ini: salin / pindah / hapus") {
		t.Fatalf("folder card missing: %s", firstLines(body))
	}

	// Delete needs the folder name typed.
	resp := p.post(t, "/objects/folder-job", url.Values{"bucket": {"media"}, "prefix": {"photos/"}, "action": {"delete"}, "confirm": {"photo"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "konfirmasi tidak cocok") {
		t.Errorf("wrong confirmation: %s", firstLines(body))
	}
	if len(p.jobsJSON(t)) != 0 {
		t.Fatal("no job should have been created")
	}

	// Copy the folder into another bucket: it keeps its own name.
	resp = p.post(t, "/objects/folder-job", url.Values{"bucket": {"media"}, "prefix": {"photos/"}, "action": {"copy"}, "dst_bucket": {"backup"}, "dst_prefix": {"arsip"}})
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/sync?") {
		t.Errorf("folder job should redirect to /sync, got %q", loc)
	}
	if body = p.followFlash(t, resp); !strings.Contains(body, "Salin folder garage:media/photos/ → garage:backup/arsip/photos/") {
		t.Errorf("copy job flash: %s", firstLines(body))
	}
	p.waitJobs(t, func(js []jobView) bool { return len(js) == 1 && !js[0].Active })
	for _, k := range []string{"arsip/photos/a.jpg", "arsip/photos/sub/b.jpg"} {
		if _, ok := p.s3.get("backup", k); !ok {
			t.Errorf("copied folder missing %s", k)
		}
	}

	// Move within the bucket, then delete with the right confirmation.
	resp = p.post(t, "/objects/folder-job", url.Values{"bucket": {"media"}, "prefix": {"photos/"}, "action": {"move"}, "dst_prefix": {"old/"}})
	p.followFlash(t, resp)
	p.waitJobs(t, func(js []jobView) bool { return len(js) == 2 && !js[0].Active && !js[1].Active })
	if _, ok := p.s3.get("media", "old/photos/a.jpg"); !ok {
		t.Error("moved folder missing")
	}
	for _, k := range p.s3.keys("media") {
		if strings.HasPrefix(k, "photos/") {
			t.Errorf("move left %s behind", k)
		}
	}
	resp = p.post(t, "/objects/folder-job", url.Values{"bucket": {"media"}, "prefix": {"old/photos/"}, "action": {"delete"}, "confirm": {"photos"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "(Hapus folder garage:media/old/photos/)") {
		t.Errorf("delete job flash: %s", firstLines(body))
	}
	p.waitJobs(t, func(js []jobView) bool {
		for _, j := range js {
			if j.Active {
				return false
			}
		}
		return len(js) == 3
	})
	if keys := p.s3.keys("media"); strings.Join(keys, ",") != "keep.txt" {
		t.Errorf("after delete job: %v", keys)
	}

	// The root of a bucket is never a folder job.
	resp = p.post(t, "/objects/folder-job", url.Values{"bucket": {"media"}, "prefix": {""}, "action": {"delete"}, "confirm": {"media"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "bukan akar bucket") {
		t.Errorf("root delete: %s", firstLines(body))
	}
}
