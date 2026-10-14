package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// addSFTPRemote stores an SFTP remote with a pinned host key and a password
// the fake rclone accepts. Its folders live in a local directory, returned
// here: <rcDir>/sftp/<host>/<absolute path>.
func (e *jobTestEnv) addSFTPRemote(t *testing.T, name string) string {
	t.Helper()
	r := Remote{Name: name, Type: RemoteTypeSFTP, Host: name + ".example.com", Port: 22, User: "root",
		Password: testSFTPPassword, HostKeys: []string{testHostKey(t)}}
	if err := e.remotes.Add(r); err != nil {
		t.Fatal(err)
	}
	e.knob(t, "sftp-password", testSFTPPassword)
	return filepath.Join(e.rcDir, "sftp", r.Host)
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSFTPImportCopiesAFolderTreeIntoABucket(t *testing.T) {
	e := newJobTestEnv(t)
	root := e.addSFTPRemote(t, "vps")
	old := sortRunEntries
	sortRunEntries = 2 // the listing is sorted in several runs on disk
	t.Cleanup(func() { sortRunEntries = old })
	writeTree(t, filepath.Join(root, "var/www/uploads"), map[string]string{
		"komik-a/001.jpg":         "jpeg-a1",
		"komik-a/002.jpg":         "jpeg-a2",
		"komik-b/cover.png":       "png-b",
		"komik-b/fail-broken.jpg": "x", // the fake rclone fails fail-* keys
		"index.html":              "<h1>",
		"a b+c.webp":              "webp",
	})
	if err := os.MkdirAll(filepath.Join(root, "var/www/uploads/kosong"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.garage.put("komik", "uploads/komik-a/001.jpg", []byte("jpeg-a1"), "image/jpeg") // already there

	logs := &safeLog{}
	log.SetOutput(logs)
	defer log.SetOutput(os.Stderr)

	spec := JobSpec{Kind: JobSync, Mode: "skip-existing", Transfers: 2,
		Src: Endpoint{Remote: "vps", Prefix: "var/www/uploads/"},
		Dst: Endpoint{Bucket: "komik", Prefix: "uploads/"}}
	st, err := e.mgr.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, e.mgr, st.ID, terminal)
	if s.Status != JobDone {
		t.Fatalf("status = %s, err = %q", s.Status, s.Error)
	}
	if s.Counters.Listed != 6 || s.Counters.Skipped != 1 || s.Counters.Transferred != 4 || s.Counters.Failed != 1 || s.Counters.Chunks != 2 {
		t.Errorf("counters = %+v", s.Counters)
	}
	for rel, want := range map[string]string{"komik-a/002.jpg": "jpeg-a2", "komik-b/cover.png": "png-b", "index.html": "<h1>", "a b+c.webp": "webp"} {
		if got, _ := e.garage.get("komik", "uploads/"+rel); string(got) != want {
			t.Errorf("uploads/%s = %q, want %q", rel, got, want)
		}
	}
	if ct := e.garage.contentType("komik", "uploads/komik-b/cover.png"); ct != "image/png" {
		t.Errorf("content type from the file name = %q", ct)
	}
	if _, ok := e.garage.get("komik", "uploads/kosong/"); ok {
		t.Error("an empty directory has no object to copy")
	}
	if s.Direction() != "Impor" || s.Src.String() != "vps:/var/www/uploads/" || s.Phase != "" {
		t.Errorf("direction %q src %q phase %q", s.Direction(), s.Src.String(), s.Phase)
	}
	// A failed file is listed as the absolute path it has on the VPS.
	if failed, total, err := e.mgr.FailedKeys(st.ID, 10); err != nil || total != 1 || failed[0].Key != "/var/www/uploads/komik-b/fail-broken.jpg" {
		t.Errorf("failed keys = %+v (%d) %v", failed, total, err)
	}

	// Two copy runs (chunks of 3 keys), source as an absolute sftp path,
	// no -M (a file system has no S3 metadata), password only obscured.
	recs := readFakeRcloneRecords(t, e.rcDir)
	if len(recs) != 2 || strings.Join(recs[0].Keys, ",") != "a b+c.webp,index.html,komik-a/002.jpg" {
		t.Fatalf("rclone runs = %+v", recs)
	}
	for _, rec := range recs {
		args := strings.Join(rec.Args, " ")
		if !strings.HasSuffix(args, "src:/var/www/uploads/ dst:komik/uploads/") || strings.Contains(" "+args+" ", " -M ") {
			t.Errorf("rclone args = %s", args)
		}
		env := strings.Join(rec.Env, "\n")
		if strings.Contains(env, testSFTPPassword) || strings.Contains(args, testSFTPPassword) {
			t.Error("the plain password reached rclone")
		}
		if got, _ := rcloneReveal(envValue(env, "RCLONE_CONFIG_SRC_PASS")); got != testSFTPPassword || envValue(env, "RCLONE_CONFIG_SRC_TYPE") != "sftp" {
			t.Errorf("source env:\n%s", env)
		}
	}
	// The listing: recursive, files only, credentials only in the env.
	var listed bool
	for _, c := range readFakeRcloneCommands(t, e.rcDir) {
		args := strings.Join(c.Args, " ")
		if strings.Contains(args, testSFTPPassword) {
			t.Errorf("password in argv: %s", args)
		}
		if strings.HasPrefix(args, "lsjson src:/var/www/uploads/ -R --files-only") {
			listed = true
		}
	}
	if !listed {
		t.Error("the source folder was not listed with lsjson -R --files-only")
	}
	if left, _ := filepath.Glob(filepath.Join(e.stateDir, "jobs", st.ID, "listing-*")); len(left) != 0 {
		t.Errorf("listing files left behind: %v", left)
	}

	// A second run finds everything in place except the key that fails.
	st2, err := e.mgr.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	s2 := waitJob(t, e.mgr, st2.ID, terminal)
	if s2.Status != JobDone || s2.Counters.Transferred != 0 || s2.Counters.Skipped != 5 || s2.Counters.Failed != 1 {
		t.Errorf("rerun: status=%s counters=%+v", s2.Status, s2.Counters)
	}

	e.mgr.Shutdown(t.Context())
	for _, id := range []string{st.ID, st2.ID} {
		raw, _ := os.ReadFile(filepath.Join(e.stateDir, "jobs", id+".json"))
		if bytes.Contains(raw, []byte(testSFTPPassword)) {
			t.Error("job file contains the password")
		}
	}
	if strings.Contains(logs.String(), testSFTPPassword) {
		t.Error("log contains the password")
	}
}

func TestSFTPExportCreatesTheFolderAndRefreshesChangedFiles(t *testing.T) {
	e := newJobTestEnv(t)
	root := e.addSFTPRemote(t, "vps")
	e.garage.put("komik", "seri/", nil, "application/x-directory")
	e.garage.put("komik", "seri/001.jpg", []byte("aaaa"), "image/jpeg")
	e.garage.put("komik", "seri/002.jpg", []byte("bbbb"), "image/jpeg")
	e.garage.put("komik", "seri/kosong/", nil, "application/x-directory")
	e.garage.put("komik", "cover.jpg", []byte("cc"), "image/jpeg")

	spec := JobSpec{Kind: JobSync, Mode: "update-if-different", Transfers: 2,
		Src: Endpoint{Bucket: "komik"}, Dst: Endpoint{Remote: "vps", Prefix: "srv/mirror/komik/"}}
	dst := filepath.Join(root, "srv/mirror/komik")
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("the destination folder should not exist yet")
	}
	st, err := e.mgr.Create(t.Context(), spec) // the write probe creates it
	if err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, e.mgr, st.ID, terminal)
	if s.Status != JobDone || s.Counters.Transferred != 3 || s.Counters.Failed != 0 {
		t.Fatalf("status=%s counters=%+v err=%q", s.Status, s.Counters, s.Error)
	}
	for rel, want := range map[string]string{"seri/001.jpg": "aaaa", "seri/002.jpg": "bbbb", "cover.jpg": "cc"} {
		if got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel))); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", rel, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "seri", "kosong")); !os.IsNotExist(err) {
		t.Error("folder markers are not replayed onto a file system")
	}
	entries, _ := os.ReadDir(dst)
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".garagepanel-probe-") {
			t.Errorf("probe file left behind: %s", en.Name())
		}
	}
	if s.Direction() != "Ekspor" {
		t.Errorf("direction = %q", s.Direction())
	}

	// A changed size is copied again, the rest is left alone.
	e.garage.put("komik", "cover.jpg", []byte("new cover"), "image/jpeg")
	if err := os.WriteFile(filepath.Join(dst, "seri", "local-only.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := e.mgr.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	s2 := waitJob(t, e.mgr, st2.ID, terminal)
	if s2.Status != JobDone || s2.Counters.Transferred != 1 || s2.Counters.Skipped != 2 {
		t.Errorf("refresh: status=%s counters=%+v err=%q", s2.Status, s2.Counters, s2.Error)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "cover.jpg")); string(got) != "new cover" {
		t.Errorf("cover.jpg = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "seri", "local-only.txt")); err != nil {
		t.Error("an export never deletes at the destination")
	}
	for _, rec := range readFakeRcloneRecords(t, e.rcDir) {
		args := strings.Join(rec.Args, " ")
		if !strings.HasSuffix(args, "src:komik/ dst:/srv/mirror/komik/") || strings.Contains(" "+args+" ", " -M ") {
			t.Errorf("rclone args = %s", args)
		}
	}
}

func TestSFTPJobsRefuseUnsafeOrUnreachableFolders(t *testing.T) {
	e := newJobTestEnv(t)
	root := e.addSFTPRemote(t, "vps")
	writeTree(t, filepath.Join(root, "srv/data"), map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c"})
	e.garage.put("media", "x.txt", []byte("x"), "text/plain")
	imp := func(prefix string) JobSpec {
		return JobSpec{Kind: JobSync, Mode: "skip-existing", Transfers: 1,
			Src: Endpoint{Remote: "vps", Prefix: prefix}, Dst: Endpoint{Bucket: "masuk", Prefix: "in/"}}
	}
	exp := func(prefix string) JobSpec {
		return JobSpec{Kind: JobSync, Mode: "skip-existing", Transfers: 1,
			Src: Endpoint{Bucket: "media"}, Dst: Endpoint{Remote: "vps", Prefix: prefix}}
	}
	expect := func(name string, spec JobSpec, want ...string) {
		t.Helper()
		_, err := e.mgr.Create(t.Context(), spec)
		if err == nil {
			t.Errorf("%s: job created", name)
			return
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: err = %v, want %q", name, err, w)
			}
		}
	}
	expect("export into /etc", exp("etc/"), "folder sistem")
	expect("export into /root", exp("root/"), "terlalu umum")
	expect("export into a home", exp("home/budi/"), "home directory")
	expect("export into .ssh", exp("root/.ssh/"), "kunci rahasia")
	expect("import /var/lib", imp("var/lib/mysql/"), "state layanan")
	expect("import the whole disk", imp(""), `tidak boleh "/"`)
	bucketOnVPS := imp("data/")
	bucketOnVPS.Src.Bucket = "srv"
	expect("bucket on a vps", bucketOnVPS, "isi folder, bukan bucket")
	expect("missing source", imp("srv/nothing/"), "folder tidak ada")
	if cmds := readFakeRcloneCommands(t, e.rcDir); len(cmds) != 1 {
		t.Errorf("refused folders must not reach the server; rclone ran %d times", len(cmds))
	}

	// Login refused: rclone's own words plus what to do about root.
	e.knob(t, "sftp-password", "changed-on-the-server")
	expect("wrong password", imp("srv/data/"), "unable to authenticate", "PermitRootLogin prohibit-password")
	e.knob(t, "sftp-password", testSFTPPassword)

	// The server presents another host key than the one pinned.
	e.knob(t, "sftp-hostkey", testHostKey(t))
	expect("host key changed", imp("srv/data/"), "key mismatch", "TIDAK SAMA")
	if err := os.Remove(filepath.Join(e.rcDir, "sftp-hostkey")); err != nil {
		t.Fatal(err)
	}

	// Nothing pinned yet: refused before rclone runs.
	r, _ := e.remotes.Get("vps")
	r.HostKeys = nil
	if err := e.remotes.Update("vps", r); err != nil {
		t.Fatal(err)
	}
	ran := len(readFakeRcloneCommands(t, e.rcDir))
	expect("no pinned host key", imp("srv/data/"), "belum disematkan")
	if len(readFakeRcloneCommands(t, e.rcDir)) != ran {
		t.Error("an unpinned server must not even be connected to")
	}
	r.HostKeys = []string{testHostKey(t)}
	if err := e.remotes.Update("vps", r); err != nil {
		t.Fatal(err)
	}

	// A folder in use: a nested folder is refused, a sibling is not. The
	// running job is slow on purpose (cancelling interrupts it at once), so
	// it is still busy however slow this machine is.
	e.knob(t, "sleep", "30")
	running, err := e.mgr.Create(t.Context(), imp("srv/data/"))
	if err != nil {
		t.Fatal(err)
	}
	nested := exp("srv/data/sub/")
	nested.Src.Bucket = "lain"
	expect("nested folder", nested, "folder vps:/srv/data/sub/ sedang dipakai job "+running.ID)
	if _, err := os.Stat(filepath.Join(root, "srv/data/sub")); !os.IsNotExist(err) {
		t.Error("a refused job's write probe created a folder inside one in use")
	}
	sibling := exp("srv/data-baru/")
	sibling.Src.Bucket = "lain"
	if other, err := e.mgr.Create(t.Context(), sibling); err != nil {
		t.Errorf("a sibling folder is free: %v", err)
	} else {
		waitJob(t, e.mgr, other.ID, terminal)
	}
	if err := e.mgr.Cancel(running.ID); err != nil {
		t.Fatal(err)
	}
	if s := waitJob(t, e.mgr, running.ID, terminal); s.Status != JobCancelled {
		t.Errorf("status = %s", s.Status)
	}
}

func TestSFTPListingFailureFailsTheJobWithAHint(t *testing.T) {
	e := newJobTestEnv(t)
	root := e.addSFTPRemote(t, "vps")
	writeTree(t, filepath.Join(root, "srv/data"), map[string]string{"a.txt": "a"})
	e.knob(t, "lsjson-exit", "1") // the probe passes, the full listing does not

	st, err := e.mgr.Create(t.Context(), JobSpec{Kind: JobSync, Mode: "skip-existing", Transfers: 1,
		Src: Endpoint{Remote: "vps", Prefix: "srv/data/"}, Dst: Endpoint{Bucket: "masuk"}})
	if err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, e.mgr, st.ID, terminal)
	if s.Status != JobFailed || !strings.Contains(s.Error, "listing sumber vps:/srv/data/ gagal (exit 1)") ||
		!strings.Contains(s.Error, "permission denied") || !strings.Contains(s.Error, "tidak punya izin") {
		t.Errorf("status=%s err=%q", s.Status, s.Error)
	}
	if s.Phase != "" {
		t.Errorf("phase left on a failed job: %q", s.Phase)
	}
	if len(readFakeRcloneRecords(t, e.rcDir)) != 0 {
		t.Error("nothing may be copied from a partial listing")
	}
	if left, _ := filepath.Glob(filepath.Join(e.stateDir, "jobs", st.ID, "listing-*")); len(left) != 0 {
		t.Errorf("listing files left behind: %v", left)
	}
}

func TestSFTPMissingDestinationFolderIsAnEmptyListing(t *testing.T) {
	e := newJobTestEnv(t)
	e.addSFTPRemote(t, "vps")
	r, _ := e.remotes.Get("vps")
	h := &jobHandle{state: JobState{JobSpec: JobSpec{ID: "0123456789abcdef"}}}
	if err := os.MkdirAll(e.mgr.jobDir(h.state.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	side := Endpoint{Remote: "vps", Prefix: "srv/belum-ada/"}

	it, closeIt, err := e.mgr.openListing(t.Context(), h, "tujuan", "dst", r, side, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if obj, err := it.peek(t.Context()); obj != nil || err != nil {
		t.Errorf("missing destination: %v %v", obj, err)
	}
	closeIt()

	// The same folder as a source is an error that says what is wrong.
	if _, _, err := e.mgr.openListing(t.Context(), h, "sumber", "src", r, side, nil, ""); err == nil || !strings.Contains(err.Error(), "Folder tidak ada di VPS") {
		t.Errorf("missing source: %v", err)
	}
	if h.state.Phase != "" {
		t.Errorf("phase = %q", h.state.Phase)
	}
}

func TestSFTPProbeFindsTheFolderWithoutListingIt(t *testing.T) {
	e := newJobTestEnv(t)
	root := e.addSFTPRemote(t, "vps")
	r, _ := e.remotes.Get("vps")
	rc := e.mgr.rclone
	// A big flat folder: listing it would cost a round trip per ~100 names.
	files := map[string]string{}
	for i := 0; i < 300; i++ {
		files[fmt.Sprintf("img-%04d.jpg", i)] = "x"
	}
	writeTree(t, filepath.Join(root, "srv/komik/flat"), files)

	before := len(readFakeRcloneCommands(t, e.rcDir))
	if _, err := probeSFTP(t.Context(), rc, r, "srv/komik/flat/", false); err != nil {
		t.Fatal(err)
	}
	cmds := readFakeRcloneCommands(t, e.rcDir)[before:]
	if len(cmds) != 1 {
		t.Fatalf("probe ran %d commands", len(cmds))
	}
	if args := strings.Join(cmds[0].Args, " "); !strings.HasPrefix(args, "lsjson probe:/srv/komik/ ") || !strings.Contains(args, "--dirs-only") || !strings.Contains(args, "--max-depth 1") {
		t.Errorf("probe listed %s; it must look the folder up in its parent", args)
	}

	// A folder that is itself a link (data on another disk) is found, and a
	// job lists what it points to.
	writeTree(t, filepath.Join(root, "mnt/disk2/uploads"), map[string]string{"a.jpg": "aaa", "sub/b.jpg": "bb"})
	if err := os.MkdirAll(filepath.Join(root, "var/www"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "mnt/disk2/uploads"), filepath.Join(root, "var/www/uploads")); err != nil {
		t.Fatal(err)
	}
	if _, err := probeSFTP(t.Context(), rc, r, "var/www/uploads/", false); err != nil {
		t.Errorf("symlinked folder: %v", err)
	}
	// A file with the folder's name is not a folder.
	writeTree(t, filepath.Join(root, "srv"), map[string]string{"bukan-folder": "x"})
	if _, err := probeSFTP(t.Context(), rc, r, "srv/bukan-folder/", false); err == nil || !strings.Contains(err.Error(), "folder tidak ada") {
		t.Errorf("file taken for a folder: %v", err)
	}
	// Parent missing too.
	if _, err := probeSFTP(t.Context(), rc, r, "srv/tidak/ada/", false); err == nil || !strings.Contains(err.Error(), "folder tidak ada") {
		t.Errorf("missing parent: %v", err)
	}

	// A parent that may be entered but not read (mode 711): the folder's
	// own listing is asked instead.
	e.knob(t, "sftp-unreadable", "/srv/komik")
	if _, err := probeSFTP(t.Context(), rc, r, "srv/komik/flat/", false); err != nil {
		t.Errorf("unreadable parent: %v", err)
	}
	if _, err := probeSFTP(t.Context(), rc, r, "srv/komik/hilang/", false); err == nil || !strings.Contains(err.Error(), "folder tidak ada") {
		t.Errorf("unreadable parent, missing folder: %v", err)
	}
	// The folder itself unreadable too: the error says so.
	e.knob(t, "sftp-unreadable", "/srv\n/srv/komik")
	if _, err := probeSFTP(t.Context(), rc, r, "srv/komik/", false); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("unreadable folder: %v", err)
	}
}

func TestSFTPListingSkipsSymlinks(t *testing.T) {
	e := newJobTestEnv(t)
	root := e.addSFTPRemote(t, "vps")
	src := filepath.Join(root, "srv/web/uploads")
	writeTree(t, src, map[string]string{"a.jpg": "aaa", "komik/001.jpg": "1"})
	writeTree(t, filepath.Join(root, "root/.ssh"), map[string]string{"id_ed25519": "PRIVATE"})
	// A loop back up the tree and a link out of the folder.
	if err := os.Symlink("..", filepath.Join(src, "komik/loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "root/.ssh/id_ed25519"), filepath.Join(src, "kunci")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "root/.ssh"), filepath.Join(src, "ssh")); err != nil {
		t.Fatal(err)
	}

	st, err := e.mgr.Create(t.Context(), JobSpec{Kind: JobSync, Mode: "skip-existing", Transfers: 2,
		Src: Endpoint{Remote: "vps", Prefix: "srv/web/uploads/"}, Dst: Endpoint{Bucket: "web"}})
	if err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, e.mgr, st.ID, terminal)
	if s.Status != JobDone || s.Counters.Listed != 2 || s.Counters.Transferred != 2 {
		t.Errorf("status=%s counters=%+v err=%q", s.Status, s.Counters, s.Error)
	}
	if keys := strings.Join(e.garage.keys("web"), ","); keys != "a.jpg,komik/001.jpg" {
		t.Errorf("bucket holds %s; links must not be followed", keys)
	}
	r, _ := e.remotes.Get("vps")
	if got := envValue(strings.Join(r.rcloneEnv("src"), "\n"), "RCLONE_CONFIG_SRC_SKIP_LINKS"); got != "true" {
		t.Errorf("SKIP_LINKS = %q", got)
	}
}

func TestSFTPRemoteWithAnIPv6Host(t *testing.T) {
	e := newJobTestEnv(t)
	if err := e.remotes.Add(Remote{Name: "vps6", Type: RemoteTypeSFTP, Host: "[2001:DB8::10]", Port: 2222, User: "root",
		Password: testSFTPPassword, HostKeys: []string{testHostKey(t)}}); err != nil {
		t.Fatal(err)
	}
	e.knob(t, "sftp-password", testSFTPPassword)
	r, _ := e.remotes.Get("vps6")
	if r.Host != "2001:db8::10" || r.View().Endpoint != "sftp://root@[2001:db8::10]:2222" {
		t.Errorf("stored host %q, shown as %q", r.Host, r.View().Endpoint)
	}
	if got := envValue(strings.Join(r.rcloneEnv("src"), "\n"), "RCLONE_CONFIG_SRC_HOST"); got != "[2001:db8::10]" {
		t.Errorf("HOST = %q; rclone appends :port as is, so an IPv6 address needs brackets", got)
	}
	root := filepath.Join(e.rcDir, "sftp", "2001:db8::10")
	writeTree(t, filepath.Join(root, "srv/data"), map[string]string{"a.txt": "a"})
	st, err := e.mgr.Create(t.Context(), JobSpec{Kind: JobSync, Mode: "skip-existing", Transfers: 1,
		Src: Endpoint{Remote: "vps6", Prefix: "srv/data/"}, Dst: Endpoint{Bucket: "masuk"}})
	if err != nil {
		t.Fatal(err)
	}
	if s := waitJob(t, e.mgr, st.ID, terminal); s.Status != JobDone || s.Counters.Transferred != 1 {
		t.Errorf("status=%s counters=%+v err=%q", s.Status, s.Counters, s.Error)
	}
}

func TestRcloneConfigEchoNeverReachesLogsOrPages(t *testing.T) {
	e := newJobTestEnv(t)
	root := e.addSFTPRemote(t, "vps")
	writeTree(t, filepath.Join(root, "srv/data"), map[string]string{"fail-a.txt": "a", "b.txt": "b"})
	e.knob(t, "echo-config", "1") // what rclone 1.60 prints with -vv
	r, _ := e.remotes.Get("vps")

	// Job runs: LastLog keeps rclone's last lines, never the config echo.
	st, err := e.mgr.Create(t.Context(), JobSpec{Kind: JobSync, Mode: "skip-existing", Transfers: 1,
		Src: Endpoint{Remote: "vps", Prefix: "srv/data/"}, Dst: Endpoint{Bucket: "masuk"}})
	if err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, e.mgr, st.ID, terminal)
	if len(s.LastLog) == 0 {
		t.Fatal("no log lines kept; the test needs some")
	}
	for _, l := range s.LastLog {
		if strings.Contains(l, "from environment variable") || strings.Contains(l, "Setting pass") {
			t.Errorf("config echo kept: %s", l)
		}
	}
	// Probe errors: the line explaining the failure, never the echo.
	e.knob(t, "sftp-password", "changed")
	_, err = probeSFTP(t.Context(), e.mgr.rclone, r, "srv/data/", false)
	if err == nil || strings.Contains(err.Error(), "from environment variable") || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Errorf("probe error = %v", err)
	}
	lines := rcloneLogTail("2026/01/01 00:00:00 DEBUG : Setting key_pem=\"-----BEGIN PRIVATE KEY-----\\\\nAAAA\" for \"probe\" from environment variable RCLONE_CONFIG_PROBE_KEY_PEM\n")
	if len(lines) != 0 {
		t.Errorf("rcloneLogTail kept %q", lines)
	}
}

func TestResumeAndRerunCheckTheSpecAgain(t *testing.T) {
	e := newJobTestEnv(t)
	e.remote.put("backup", "in/a.txt", []byte("a"), "text/plain")
	st, err := e.mgr.Create(t.Context(), e.importSpec("skip-existing"))
	if err != nil {
		t.Fatal(err)
	}
	if s := waitJob(t, e.mgr, st.ID, terminal); s.Status != JobDone {
		t.Fatalf("status = %s", s.Status)
	}
	// The S3 remote is replaced by a VPS of the same name: the stored job
	// still names a bucket on it, which a VPS does not have.
	if err := e.remotes.Delete("wasabi"); err != nil {
		t.Fatal(err)
	}
	if err := e.remotes.Add(Remote{Name: "wasabi", Type: RemoteTypeSFTP, Host: "vps.example.com", Port: 22, User: "root",
		Password: testSFTPPassword, HostKeys: []string{testHostKey(t)}}); err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.Rerun(st.ID); err == nil || !strings.Contains(err.Error(), "isi folder, bukan bucket") {
		t.Errorf("rerun = %v", err)
	}
	if s, _ := e.mgr.Get(st.ID); s.Status != JobDone || s.Runs != 1 {
		t.Errorf("a refused rerun changed the job: %s runs=%d", s.Status, s.Runs)
	}
}

func TestSFTPImportResumesFromCheckpoint(t *testing.T) {
	e := newJobTestEnv(t)
	root := e.addSFTPRemote(t, "vps")
	files := map[string]string{}
	for i := 0; i < 7; i++ {
		files[fmt.Sprintf("%02d.jpg", i)] = "x"
	}
	writeTree(t, filepath.Join(root, "srv/img"), files)
	e.knob(t, "sleep", "0.15")

	spec := JobSpec{Kind: JobSync, Mode: "skip-existing", Transfers: 1,
		Src: Endpoint{Remote: "vps", Prefix: "srv/img/"}, Dst: Endpoint{Bucket: "arsip", Prefix: "img/"}}
	st, err := e.mgr.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, e.mgr, st.ID, func(s JobState) bool { return s.Counters.Chunks >= 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.mgr.Shutdown(ctx)

	onDisk := readJobFile(t, e, st.ID)
	if onDisk.Status != JobRunning || onDisk.Checkpoint != "02.jpg" {
		t.Fatalf("on disk: status=%s checkpoint=%q", onDisk.Status, onDisk.Checkpoint)
	}

	e.knob(t, "sleep", "0")
	mgr2 := e.open(t)
	mgr2.ResumeInterrupted()
	s := waitJob(t, mgr2, st.ID, terminal)
	if s.Status != JobDone || e.garage.count("arsip") != 7 {
		t.Fatalf("after resume: status=%s err=%q objects=%d", s.Status, s.Error, e.garage.count("arsip"))
	}
	recs := readFakeRcloneRecords(t, e.rcDir)
	for _, r := range recs[1:] {
		for _, k := range r.Keys {
			if k <= "02.jpg" {
				t.Errorf("resumed run re-copied %s, which was before the checkpoint", k)
			}
		}
	}
}
