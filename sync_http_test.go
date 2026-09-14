package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// addTestRemote registers the panel's second fake S3 as remote "wasabi".
func (p *testPanel) addTestRemote(t *testing.T) {
	t.Helper()
	resp := p.post(t, "/sync/remotes/add", url.Values{
		"name": {"wasabi"}, "provider": {"Wasabi"}, "endpoint": {p.remote.URL()},
		"region": {"us-east-1"}, "access_key": {"WASABIKEY"}, "secret_key": {testRemoteSecret + "\r\n"},
	})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, `Remote &#34;wasabi&#34; disimpan`) {
		t.Fatalf("remote not saved: %s", firstLines(body))
	}
}

func (p *testPanel) jobsJSON(t *testing.T) []jobView {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, p.srv.URL+"/sync/jobs.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("jobs.json status = %d", resp.StatusCode)
	}
	var out []jobView
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (p *testPanel) waitJobs(t *testing.T, cond func([]jobView) bool) []jobView {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		jobs := p.jobsJSON(t)
		if cond(jobs) {
			return jobs
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("jobs did not reach the expected state: %+v", p.jobsJSON(t))
	return nil
}

func TestSyncPageManagesRemotesWithoutShowingSecrets(t *testing.T) {
	p := newTestPanel(t)
	_, body := p.get(t, "/sync")
	if !strings.Contains(body, "Belum ada remote") || !strings.Contains(body, "rclone v1.66.0") {
		t.Errorf("empty sync page: %s", firstLines(body))
	}

	p.addTestRemote(t)
	_, body = p.get(t, "/sync")
	if !strings.Contains(body, "WASA…IKEY") || !strings.Contains(body, p.remote.URL()) {
		t.Errorf("remote not listed: %s", firstLines(body))
	}
	if strings.Contains(body, testRemoteSecret) || strings.Contains(body, "WASABIKEY<") {
		t.Error("credentials leaked into the page")
	}
	if !strings.Contains(body, "tanpa TLS") {
		t.Error("an http endpoint must be flagged")
	}
	if !strings.Contains(body, `name="direction"`) {
		t.Error("the job form should appear once a remote exists")
	}

	raw, err := os.ReadFile(filepath.Join(p.app.cfg.StateDir, "remotes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"secretKey": "`+testRemoteSecret+`"`) {
		t.Error("the trimmed secret must be on disk")
	}

	// Tes koneksi: a bucket that exists, then one the remote refuses.
	p.remote.put("backup", "x.txt", []byte("x"), "text/plain")
	resp := p.post(t, "/sync/remotes/test", url.Values{"name": {"wasabi"}, "bucket": {"backup"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "kredensialnya benar") {
		t.Errorf("probe ok: %s", firstLines(body))
	}
	p.remote.deny = `<Error><Code>AccessDenied</Code><Message>policy says no</Message></Error>`
	resp = p.post(t, "/sync/remotes/test", url.Values{"name": {"wasabi"}, "bucket": {"backup"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "policy says no") {
		t.Errorf("probe failure must show the provider message: %s", firstLines(body))
	}
	p.remote.deny = ""

	// Invalid input never reaches the store.
	resp = p.post(t, "/sync/remotes/add", url.Values{"name": {"evil"}, "provider": {"Other"}, "endpoint": {"file:///etc/passwd"}, "region": {"x"}, "access_key": {"a"}, "secret_key": {"b"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "remote tidak disimpan") {
		t.Errorf("bad endpoint accepted: %s", firstLines(body))
	}

	resp = p.post(t, "/sync/remotes/delete", url.Values{"name": {"wasabi"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "dihapus") {
		t.Errorf("delete: %s", firstLines(body))
	}
	if len(p.app.remotes.List()) != 0 {
		t.Error("remote still stored")
	}
}

func TestSyncJobFromFormRunsAndIsPolled(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("arsip", 0, 0, false)
	p.addTestRemote(t)
	for _, k := range []string{"a.txt", "b.txt", "fail-c.txt"} {
		p.remote.put("backup", "in/"+k, []byte("data-"+k), "text/plain")
	}
	p.s3.put("arsip", "out/b.txt", []byte("data-b.txt"), "text/plain")

	resp := p.post(t, "/sync/jobs/create", url.Values{
		"direction": {"import"}, "remote": {"wasabi"}, "remote_bucket": {"backup"}, "remote_prefix": {"in/"},
		"garage_bucket": {"arsip"}, "garage_prefix": {"out/"}, "mode": {"skip-existing"}, "transfers": {"2"},
	})
	body := p.followFlash(t, resp)
	if !strings.Contains(body, "dimulai") || !strings.Contains(body, "wasabi:backup/in/") {
		t.Fatalf("job not started: %s", firstLines(body))
	}

	jobs := p.waitJobs(t, func(js []jobView) bool { return len(js) == 1 && !js[0].Active })
	j := jobs[0]
	if j.Status != JobDone || j.Counters.Transferred != 1 || j.Counters.Skipped != 1 || j.Counters.Failed != 1 {
		t.Errorf("job = status %s counters %+v err %q", j.Status, j.Counters, j.Error)
	}
	if j.Direction != "Impor" || j.SrcLabel != "wasabi:backup/in/" || j.DstLabel != "garage:arsip/out/" || !j.CanDelete || j.CanPause {
		t.Errorf("view = %+v", j)
	}
	if got, _ := p.s3.get("arsip", "out/a.txt"); string(got) != "data-a.txt" {
		t.Errorf("a.txt not imported: %q", got)
	}

	_, body = p.get(t, "/sync")
	for _, want := range []string{`data-job="` + j.ID + `"`, "selesai", "lihat key yang gagal", ">1<"} {
		if !strings.Contains(body, want) {
			t.Errorf("sync page missing %q: %s", want, firstLines(body))
		}
	}
	if strings.Contains(body, testRemoteSecret) || strings.Contains(body, testGarageSecret) {
		t.Error("secret on the sync page")
	}

	resp, body = p.get(t, "/sync/jobs/failed?id="+j.ID)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "in/fail-c.txt") || !strings.Contains(body, "injected") {
		t.Errorf("failed page: %d %s", resp.StatusCode, firstLines(body))
	}
	resp, body = p.get(t, "/sync/jobs/failed?id="+j.ID+"&download=1")
	if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") || !strings.Contains(body, `"key":"in/fail-c.txt"`) {
		t.Errorf("download: %q %s", resp.Header.Get("Content-Disposition"), firstLines(body))
	}
	if resp, _ := p.get(t, "/sync/jobs/failed?id=../../etc/passwd"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("path-like id: status %d", resp.StatusCode)
	}

	// Rerun, then delete the record.
	resp = p.post(t, "/sync/jobs/rerun", url.Values{"id": {j.ID}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "dijalankan ulang") {
		t.Errorf("rerun: %s", firstLines(body))
	}
	p.waitJobs(t, func(js []jobView) bool { return len(js) == 1 && !js[0].Active && js[0].Runs == 2 })
	resp = p.post(t, "/sync/jobs/delete", url.Values{"id": {j.ID}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "catatan dihapus") {
		t.Errorf("delete: %s", firstLines(body))
	}
	if len(p.jobsJSON(t)) != 0 {
		t.Error("job still listed")
	}
}

func TestSyncJobPauseAndRemoteInUse(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("arsip", 0, 0, false)
	p.addTestRemote(t)
	for i := 0; i < 4; i++ {
		p.remote.put("backup", "in/"+string(rune('a'+i))+".txt", []byte("x"), "text/plain")
	}
	if err := os.WriteFile(filepath.Join(p.rcDir, "sleep"), []byte("0.3"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.post(t, "/sync/jobs/create", url.Values{
		"direction": {"import"}, "remote": {"wasabi"}, "remote_bucket": {"backup"}, "remote_prefix": {"in/"},
		"garage_bucket": {"arsip"}, "mode": {"overwrite"}, "transfers": {"1"},
	})
	jobs := p.waitJobs(t, func(js []jobView) bool { return len(js) == 1 && js[0].Status == JobRunning })
	id := jobs[0].ID

	resp := p.post(t, "/sync/remotes/delete", url.Values{"name": {"wasabi"}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "masih dipakai job") {
		t.Errorf("remote in use must not be deletable: %s", firstLines(body))
	}
	resp = p.post(t, "/sync/jobs/pause", url.Values{"id": {id}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "dijeda") {
		t.Errorf("pause: %s", firstLines(body))
	}
	p.waitJobs(t, func(js []jobView) bool { return js[0].Status == JobPaused })
	_, body := p.get(t, "/sync")
	if !strings.Contains(body, "Lanjutkan") || !strings.Contains(body, "dijeda") {
		t.Errorf("paused job should offer resume: %s", firstLines(body))
	}
	resp = p.post(t, "/sync/jobs/cancel", url.Values{"id": {id}})
	p.followFlash(t, resp)
	p.waitJobs(t, func(js []jobView) bool { return js[0].Status == JobCancelled })
	resp = p.post(t, "/sync/remotes/delete", url.Values{"name": {"wasabi"}})
	if body := p.followFlash(t, resp); !strings.Contains(body, "dihapus") {
		t.Errorf("after cancel the remote can go: %s", firstLines(body))
	}
}

func TestSyncDisabledWhenRcloneIsMissing(t *testing.T) {
	p := newTestPanel(t, func(c *Config) { c.RcloneBin = "/nonexistent/rclone" })
	_, body := p.get(t, "/sync")
	if !strings.Contains(body, "Fitur Sync nonaktif") || !strings.Contains(body, "apt install rclone") {
		t.Errorf("disabled notice missing: %s", firstLines(body))
	}
	resp := p.post(t, "/sync/jobs/create", url.Values{"direction": {"import"}})
	if body = p.followFlash(t, resp); !strings.Contains(body, "rclone tidak ditemukan") {
		t.Errorf("mutations must be refused: %s", firstLines(body))
	}
	if resp, _ := p.get(t, "/buckets"); resp.StatusCode != http.StatusOK {
		t.Error("other pages must keep working")
	}
	req, _ := http.NewRequest(http.MethodGet, p.srv.URL+"/sync/jobs.json", nil)
	req.Header.Set("Accept", "application/json")
	if resp, err := p.client.Do(req); err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("jobs.json while disabled: %v %v", resp, err)
	}
}

func TestSyncDisabledWhenStateDirUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	p := newTestPanel(t, func(c *Config) { c.StateDir = "/proc/garagepanel-cannot-exist" })
	_, body := p.get(t, "/sync")
	if !strings.Contains(body, "Fitur Sync nonaktif") || !strings.Contains(body, "install -d -o garagepanel") {
		t.Errorf("disabled notice missing: %s", firstLines(body))
	}
}

func TestSyncPagesRequireLoginAndCSRF(t *testing.T) {
	p := newTestPanel(t, withLogin(t))
	req, _ := http.NewRequest(http.MethodGet, p.srv.URL+"/sync/jobs.json", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("jobs.json without login: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp, _ := p.get(t, "/sync"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("/sync without login: %d", resp.StatusCode)
	}
	p.login(t, "admin", testPassword)
	for _, path := range []string{"/sync/remotes/add", "/sync/remotes/delete", "/sync/remotes/test", "/sync/jobs/create", "/sync/jobs/pause", "/sync/jobs/resume", "/sync/jobs/rerun", "/sync/jobs/cancel", "/sync/jobs/delete"} {
		resp, err := p.client.PostForm(p.srv.URL+path, url.Values{"name": {"x"}, "id": {"0123456789abcdef"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s without CSRF token: %d", path, resp.StatusCode)
		}
	}
}
