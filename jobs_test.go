package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testGarageSecret = "garage-secret-value"
	testRemoteSecret = "wasabi-secret-value"
)

type jobTestEnv struct {
	stateDir string
	rcDir    string
	garage   *fakeS3
	remote   *fakeS3
	remotes  *RemoteStore
	cfg      *Config
	mgr      *JobManager
}

func newJobTestEnv(t *testing.T) *jobTestEnv {
	t.Helper()
	e := &jobTestEnv{
		stateDir: filepath.Join(t.TempDir(), "state"),
		rcDir:    t.TempDir(),
		garage:   newFakeS3(t, "GKPANEL", testGarageSecret, "garage"),
		remote:   newFakeS3(t, "WASABIKEY", testRemoteSecret, "us-east-1"),
	}
	if err := prepareStateDir(e.stateDir); err != nil {
		t.Fatal(err)
	}
	store, err := NewRemoteStore(filepath.Join(e.stateDir, remotesFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(Remote{Name: "wasabi", Provider: "Wasabi", Endpoint: e.remote.URL(), Region: "us-east-1", AccessKey: "WASABIKEY", SecretKey: testRemoteSecret}); err != nil {
		t.Fatal(err)
	}
	e.remotes = store
	e.cfg = &Config{S3URL: e.garage.URL(), S3Region: "garage", S3AccessKey: "GKPANEL", S3SecretKey: testGarageSecret}
	e.mgr = e.open(t)
	return e
}

// open builds a manager over the state dir, as the panel does at start-up.
func (e *jobTestEnv) open(t *testing.T) *JobManager {
	t.Helper()
	rc, err := detectRclone(fakeRcloneBin(t, e.rcDir), filepath.Join(e.stateDir, "cache"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := NewJobManager(filepath.Join(e.stateDir, "jobs"), garageRemote(e.cfg), e.remotes, rc)
	if err != nil {
		t.Fatal(err)
	}
	mgr.chunkKeys = 3
	return mgr
}

func (e *jobTestEnv) knob(t *testing.T, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.rcDir, name), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *jobTestEnv) importSpec(mode string) JobSpec {
	return JobSpec{
		Kind: JobSync, Mode: mode, Transfers: 2,
		Src: Endpoint{Remote: "wasabi", Bucket: "backup", Prefix: "in/"},
		Dst: Endpoint{Bucket: "arsip", Prefix: "out/"},
	}
}

func waitJob(t *testing.T, mgr *JobManager, id string, cond func(JobState) bool) JobState {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		s, ok := mgr.Get(id)
		if !ok {
			t.Fatalf("job %s disappeared", id)
		}
		if cond(s) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	s, _ := mgr.Get(id)
	t.Fatalf("job %s did not reach the expected state; last: status=%s counters=%+v err=%q reason=%q", id, s.Status, s.Counters, s.Error, s.PauseReason)
	return s
}

func terminal(s JobState) bool { return s.Status.Terminal() }

// safeLog is a log sink that may be read while runner goroutines still write.
type safeLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *safeLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *safeLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func readJobFile(t *testing.T, e *jobTestEnv, id string) JobState {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.stateDir, "jobs", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var s JobState
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSyncImportSkipsExistingObjects(t *testing.T) {
	e := newJobTestEnv(t)
	for _, k := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"} {
		e.remote.put("backup", "in/"+k, []byte("data-"+k), "text/plain")
	}
	e.garage.put("arsip", "out/b.txt", []byte("data-b.txt"), "text/plain")
	e.garage.put("arsip", "out/d.txt", []byte("other"), "text/plain")
	e.garage.put("arsip", "out/only-here.txt", []byte("keep"), "text/plain")

	logs := &safeLog{}
	log.SetOutput(logs)
	defer log.SetOutput(os.Stderr)

	st, err := e.mgr.Create(t.Context(), e.importSpec("skip-existing"))
	if err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, e.mgr, st.ID, terminal)
	if s.Status != JobDone {
		t.Fatalf("status = %s, err = %q", s.Status, s.Error)
	}
	if s.Counters.Listed != 5 || s.Counters.Transferred != 3 || s.Counters.Skipped != 2 || s.Counters.Failed != 0 || s.Counters.Chunks != 1 {
		t.Errorf("counters = %+v", s.Counters)
	}
	if s.Checkpoint != "e.txt" {
		t.Errorf("checkpoint = %q", s.Checkpoint)
	}
	for _, k := range []string{"a.txt", "c.txt", "e.txt"} {
		if got, _ := e.garage.get("arsip", "out/"+k); string(got) != "data-"+k {
			t.Errorf("out/%s = %q", k, got)
		}
	}
	if got, _ := e.garage.get("arsip", "out/d.txt"); string(got) != "other" {
		t.Error("skip-existing must not overwrite an object that exists")
	}
	if _, ok := e.garage.get("arsip", "out/only-here.txt"); !ok {
		t.Error("sync must never delete at the destination")
	}
	if s.Direction() != "Impor" {
		t.Errorf("direction = %q", s.Direction())
	}

	recs := readFakeRcloneRecords(t, e.rcDir)
	if len(recs) != 1 || strings.Join(recs[0].Keys, ",") != "a.txt,c.txt,e.txt" {
		t.Fatalf("rclone runs = %+v", recs)
	}
	args := strings.Join(recs[0].Args, " ")
	if !strings.Contains(args, "copy --files-from-raw") || !strings.HasSuffix(args, "src:backup/in/ dst:arsip/out/") {
		t.Errorf("rclone args = %s", args)
	}
	env := strings.Join(recs[0].Env, "\n")
	if !strings.Contains(env, "RCLONE_CONFIG_SRC_ENDPOINT="+e.remote.URL()) || !strings.Contains(env, "RCLONE_CONFIG_DST_ENDPOINT="+e.garage.URL()) {
		t.Errorf("env sides wrong:\n%s", env)
	}

	// Wait for the runner goroutine itself, which logs after it persists.
	e.mgr.Shutdown(t.Context())
	raw, _ := os.ReadFile(filepath.Join(e.stateDir, "jobs", st.ID+".json"))
	for _, secret := range []string{testGarageSecret, testRemoteSecret} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Error("job file contains a secret")
		}
		if strings.Contains(logs.String(), secret) {
			t.Error("log contains a secret")
		}
	}
	if info, _ := os.Stat(filepath.Join(e.stateDir, "jobs", st.ID+".json")); info.Mode().Perm() != 0o600 {
		t.Errorf("job file mode = %o", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(e.stateDir, "jobs", st.ID, "chunk.txt")); !os.IsNotExist(err) {
		t.Error("chunk file must be removed after the run")
	}
	if !strings.Contains(logs.String(), "job "+st.ID) {
		t.Error("job start/finish should be logged")
	}
}

func TestSyncExportUpdateIfDifferentAndOverwrite(t *testing.T) {
	e := newJobTestEnv(t)
	e.garage.put("media", "a.jpg", []byte("aaaa"), "image/jpeg")
	e.garage.put("media", "b.jpg", []byte("bbbb"), "image/jpeg")
	e.remote.put("mirror", "a.jpg", []byte("aaaa"), "image/jpeg")     // same size
	e.remote.put("mirror", "b.jpg", []byte("old-bbbb"), "image/jpeg") // different size

	export := JobSpec{Kind: JobSync, Mode: "update-if-different", Transfers: 1,
		Src: Endpoint{Bucket: "media"}, Dst: Endpoint{Remote: "wasabi", Bucket: "mirror"}}
	st, err := e.mgr.Create(t.Context(), export)
	if err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, e.mgr, st.ID, terminal)
	if s.Status != JobDone || s.Counters.Transferred != 1 || s.Counters.Skipped != 1 {
		t.Fatalf("status=%s counters=%+v err=%q", s.Status, s.Counters, s.Error)
	}
	if got, _ := e.remote.get("mirror", "b.jpg"); string(got) != "bbbb" {
		t.Errorf("b.jpg not refreshed: %q", got)
	}
	if s.Direction() != "Ekspor" {
		t.Errorf("direction = %q", s.Direction())
	}

	// Overwrite never lists the destination.
	before := e.remote.callCount("ListObjectsV2")
	export.Mode = "overwrite"
	st2, err := e.mgr.Create(t.Context(), export)
	if err != nil {
		t.Fatal(err)
	}
	s2 := waitJob(t, e.mgr, st2.ID, terminal)
	if s2.Status != JobDone || s2.Counters.Transferred != 2 || s2.Counters.Skipped != 0 {
		t.Fatalf("overwrite: status=%s counters=%+v", s2.Status, s2.Counters)
	}
	// Exactly one extra listing: the probe at creation.
	if n := e.remote.callCount("ListObjectsV2") - before; n != 1 {
		t.Errorf("overwrite listed the destination %d times beyond the probe", n-1)
	}
}

func TestJobResumesAfterShutdownFromCheckpoint(t *testing.T) {
	e := newJobTestEnv(t)
	for i := 0; i < 7; i++ {
		e.remote.put("backup", fmt.Sprintf("in/%02d.txt", i), []byte("x"), "text/plain")
	}
	e.knob(t, "sleep", "0.15")

	st, err := e.mgr.Create(t.Context(), e.importSpec("skip-existing"))
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, e.mgr, st.ID, func(s JobState) bool { return s.Counters.Chunks >= 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.mgr.Shutdown(ctx)

	onDisk := readJobFile(t, e, st.ID)
	if onDisk.Status != JobRunning {
		t.Fatalf("after shutdown the job must still be %s on disk, got %s", JobRunning, onDisk.Status)
	}
	if onDisk.Checkpoint != "02.txt" {
		t.Errorf("checkpoint = %q, want the last key of the first finished chunk", onDisk.Checkpoint)
	}
	copiedBefore := e.garage.count("arsip")
	if copiedBefore < 3 {
		t.Errorf("first chunk should have landed: %d objects", copiedBefore)
	}

	// "Restart": a new manager over the same directory.
	e.knob(t, "sleep", "0")
	mgr2 := e.open(t)
	mgr2.ResumeInterrupted()
	s := waitJob(t, mgr2, st.ID, terminal)
	if s.Status != JobDone {
		t.Fatalf("after resume: status=%s err=%q", s.Status, s.Error)
	}
	if e.garage.count("arsip") != 7 {
		t.Errorf("destination has %d objects, want 7", e.garage.count("arsip"))
	}
	if e.remote.callCount("ListObjectsV2.start-after") == 0 || e.garage.callCount("ListObjectsV2.start-after") == 0 {
		t.Error("resume must list both sides with start-after")
	}
	// The resumed run only ever handled keys after the checkpoint.
	recs := readFakeRcloneRecords(t, e.rcDir)
	for _, r := range recs[1:] {
		for _, k := range r.Keys {
			if k <= "02.txt" {
				t.Errorf("resumed run re-copied %s, which was before the checkpoint", k)
			}
		}
	}
	if s.Runs != 1 {
		t.Errorf("runs = %d; a shutdown/restart is not a manual resume", s.Runs)
	}
}

func TestPauseResumeAndCancelPersistIntent(t *testing.T) {
	e := newJobTestEnv(t)
	for i := 0; i < 6; i++ {
		e.remote.put("backup", fmt.Sprintf("in/%02d.txt", i), []byte("x"), "text/plain")
	}
	e.knob(t, "sleep", "0.2")

	st, err := e.mgr.Create(t.Context(), e.importSpec("skip-existing"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := e.mgr.Pause(st.ID); err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, e.mgr, st.ID, func(s JobState) bool { return s.Status == JobPaused })
	if readJobFile(t, e, st.ID).Status != JobPaused {
		t.Error("paused status must be on disk")
	}
	if err := e.mgr.Pause(st.ID); err == nil {
		t.Error("pausing a paused job must fail")
	}
	_ = s

	e.knob(t, "sleep", "0")
	if err := e.mgr.Resume(st.ID); err != nil {
		t.Fatal(err)
	}
	s = waitJob(t, e.mgr, st.ID, terminal)
	if s.Status != JobDone || s.Runs != 2 {
		t.Fatalf("after resume: status=%s runs=%d err=%q", s.Status, s.Runs, s.Error)
	}
	if e.garage.count("arsip") != 6 {
		t.Errorf("destination has %d objects, want 6", e.garage.count("arsip"))
	}

	// Cancel a running job.
	e.knob(t, "sleep", "0.2")
	for i := 0; i < 6; i++ {
		e.remote.put("backup2", fmt.Sprintf("in/%02d.txt", i), []byte("x"), "text/plain")
	}
	spec := e.importSpec("overwrite")
	spec.Src.Bucket = "backup2"
	spec.Dst.Bucket = "arsip2"
	st2, err := e.mgr.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := e.mgr.Cancel(st2.ID); err != nil {
		t.Fatal(err)
	}
	s2 := waitJob(t, e.mgr, st2.ID, terminal)
	if s2.Status != JobCancelled || s2.FinishedAt == nil {
		t.Errorf("status = %s", s2.Status)
	}
	if err := e.mgr.Resume(st2.ID); err == nil {
		t.Error("a cancelled job cannot be resumed")
	}
	if err := e.mgr.Delete(st.ID); err != nil {
		t.Errorf("deleting a finished job: %v", err)
	}
	if _, ok := e.mgr.Get(st.ID); ok {
		t.Error("deleted job still listed")
	}
	if _, err := os.Stat(filepath.Join(e.stateDir, "jobs", st.ID)); !os.IsNotExist(err) {
		t.Error("job directory not removed")
	}
}

func TestFailedKeysAreRecordedAndAnOutagePausesTheJob(t *testing.T) {
	e := newJobTestEnv(t)
	e.mgr.chunkKeys = 100
	for _, k := range []string{"ok1.txt", "fail-a.txt", "ok2.txt", "fail-b.txt", "ok3.txt"} {
		e.remote.put("backup", "in/"+k, []byte("x"), "text/plain")
	}
	st, err := e.mgr.Create(t.Context(), e.importSpec("overwrite"))
	if err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, e.mgr, st.ID, terminal)
	if s.Status != JobDone || s.Counters.Failed != 2 || s.Counters.Transferred != 3 {
		t.Fatalf("status=%s counters=%+v err=%q", s.Status, s.Counters, s.Error)
	}
	keys, total, err := e.mgr.FailedKeys(st.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(keys) != 2 || keys[0].Key != "in/fail-a.txt" || !strings.Contains(keys[0].Error, "injected") {
		t.Errorf("failed keys = %+v (total %d)", keys, total)
	}
	if info, _ := os.Stat(filepath.Join(e.stateDir, "jobs", st.ID, "failed.jsonl")); info == nil || info.Mode().Perm() != 0o600 {
		t.Error("failed.jsonl missing or not private")
	}

	// An outage: most of a chunk fails → pause, no checkpoint, nothing
	// written off.
	for i := 0; i < 12; i++ {
		e.remote.put("backup3", fmt.Sprintf("in/fail-%02d.txt", i), []byte("x"), "text/plain")
	}
	e.remote.put("backup3", "in/ok.txt", []byte("x"), "text/plain")
	spec := e.importSpec("overwrite")
	spec.Src.Bucket = "backup3"
	spec.Dst.Bucket = "arsip3"
	st2, err := e.mgr.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	s2 := waitJob(t, e.mgr, st2.ID, func(s JobState) bool { return s.Status == JobPaused })
	if !strings.Contains(s2.PauseReason, "12 dari 13") {
		t.Errorf("pause reason = %q", s2.PauseReason)
	}
	if s2.Checkpoint != "" || s2.Counters.Failed != 0 {
		t.Errorf("an outage must not advance the checkpoint or count failures: %+v checkpoint=%q", s2.Counters, s2.Checkpoint)
	}
	if _, total, _ := e.mgr.FailedKeys(st2.ID, 10); total != 0 {
		t.Errorf("failed.jsonl has %d lines, want 0", total)
	}
}

func TestFatalRcloneExitFailsTheJobWithItsLastLines(t *testing.T) {
	e := newJobTestEnv(t)
	e.remote.put("backup", "in/a.txt", []byte("x"), "text/plain")
	e.knob(t, "exit", "7")
	st, err := e.mgr.Create(t.Context(), e.importSpec("overwrite"))
	if err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, e.mgr, st.ID, terminal)
	if s.Status != JobFailed || !strings.Contains(s.Error, "exit code 7") || !strings.Contains(s.Error, "fatal") {
		t.Errorf("status=%s err=%q", s.Status, s.Error)
	}
	if len(s.LastLog) == 0 || !strings.Contains(strings.Join(s.LastLog, "\n"), "forced exit 7") {
		t.Errorf("last log = %v", s.LastLog)
	}
	if s.Checkpoint != "" {
		t.Error("a failed chunk must not be checkpointed")
	}
	// Resume works once the cause is fixed.
	e.knob(t, "exit", "")
	if err := e.mgr.Resume(st.ID); err != nil {
		t.Fatal(err)
	}
	if s = waitJob(t, e.mgr, st.ID, terminal); s.Status != JobDone || s.Error != "" {
		t.Errorf("after resume: %s %q", s.Status, s.Error)
	}
}

func TestJobLimitsOneJobPerBucketAndTwoRunning(t *testing.T) {
	e := newJobTestEnv(t)
	e.knob(t, "sleep", "0.3")
	for _, b := range []string{"bk1", "bk2", "bk3"} {
		e.remote.put(b, "in/a.txt", []byte("x"), "text/plain")
	}
	spec := e.importSpec("overwrite")
	spec.Src.Bucket = "bk1"
	st1, err := e.mgr.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	// Same destination bucket → refused, whichever the source.
	spec.Src.Bucket = "bk2"
	if _, err := e.mgr.Create(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "sedang dipakai job") {
		t.Errorf("same bucket: err = %v", err)
	}
	// Same source bucket as another job's source → also refused.
	spec.Src.Bucket = "bk1"
	spec.Dst.Bucket = "arsip-lain"
	if _, err := e.mgr.Create(t.Context(), spec); err == nil {
		t.Error("a bucket in use by a running job must be refused")
	}
	spec.Src.Bucket = "bk2"
	spec.Dst.Bucket = "arsip2"
	st2, err := e.mgr.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.Src.Bucket = "bk3"
	spec.Dst.Bucket = "arsip3"
	if _, err := e.mgr.Create(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "batas") {
		t.Errorf("third running job: err = %v", err)
	}
	if e.mgr.Running() != 2 {
		t.Errorf("running = %d", e.mgr.Running())
	}
	waitJob(t, e.mgr, st1.ID, terminal)
	waitJob(t, e.mgr, st2.ID, terminal)
	if _, err := e.mgr.Create(t.Context(), spec); err != nil {
		t.Errorf("after the others finished: %v", err)
	}
}

func TestPrefixJobsRunServerSideAndHandleFolderMarkers(t *testing.T) {
	e := newJobTestEnv(t)
	e.garage.put("media", "photos/", []byte{}, "application/x-directory")
	e.garage.put("media", "photos/a.jpg", []byte("aa"), "image/jpeg")
	e.garage.put("media", "photos/sub/", []byte{}, "application/x-directory")
	e.garage.put("media", "photos/sub/b.jpg", []byte("bb"), "image/jpeg")
	e.garage.put("media", "photos/sub/empty/", []byte{}, "application/x-directory")
	e.garage.put("media", "other.txt", []byte("keep"), "text/plain")

	copySpec := JobSpec{Kind: JobCopyPrefix, Transfers: 2, Src: Endpoint{Bucket: "media", Prefix: "photos/"}, Dst: Endpoint{Bucket: "backup", Prefix: "foto/"}}
	st, err := e.mgr.Create(t.Context(), copySpec)
	if err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, e.mgr, st.ID, terminal)
	if s.Status != JobDone || s.Counters.Transferred != 2 || s.Counters.Failed != 0 {
		t.Fatalf("copy: status=%s counters=%+v err=%q", s.Status, s.Counters, s.Error)
	}
	for _, k := range []string{"foto/a.jpg", "foto/sub/b.jpg", "foto/sub/", "foto/sub/empty/"} {
		if _, ok := e.garage.get("backup", k); !ok {
			t.Errorf("copy: %s missing at destination", k)
		}
	}
	if e.garage.count("media") != 6 {
		t.Error("copy must leave the source untouched")
	}
	recs := readFakeRcloneRecords(t, e.rcDir)
	args := strings.Join(recs[len(recs)-1].Args, " ")
	if !strings.HasSuffix(args, "garage:media/photos/ garage:backup/foto/") {
		t.Errorf("copy args = %s", args)
	}
	env := strings.Join(recs[len(recs)-1].Env, "\n")
	if !strings.Contains(env, "RCLONE_CONFIG_GARAGE_ENDPOINT="+e.garage.URL()) || strings.Contains(env, "RCLONE_CONFIG_SRC_") {
		t.Errorf("prefix jobs must use the single garage alias:\n%s", env)
	}

	moveSpec := JobSpec{Kind: JobMovePrefix, Transfers: 2, Src: Endpoint{Bucket: "media", Prefix: "photos/"}, Dst: Endpoint{Bucket: "media", Prefix: "arsip/2026/"}}
	st, err = e.mgr.Create(t.Context(), moveSpec)
	if err != nil {
		t.Fatal(err)
	}
	if s = waitJob(t, e.mgr, st.ID, terminal); s.Status != JobDone || s.Counters.Transferred != 2 {
		t.Fatalf("move: status=%s counters=%+v err=%q", s.Status, s.Counters, s.Error)
	}
	for _, k := range e.garage.keys("media") {
		if strings.HasPrefix(k, "photos/") {
			t.Errorf("move left %s behind", k)
		}
	}
	for _, k := range []string{"arsip/2026/a.jpg", "arsip/2026/sub/b.jpg", "arsip/2026/sub/empty/", "other.txt"} {
		if _, ok := e.garage.get("media", k); !ok {
			t.Errorf("move: %s missing", k)
		}
	}

	delSpec := JobSpec{Kind: JobDeletePrefix, Transfers: 2, Src: Endpoint{Bucket: "media", Prefix: "arsip/"}}
	st, err = e.mgr.Create(t.Context(), delSpec)
	if err != nil {
		t.Fatal(err)
	}
	if s = waitJob(t, e.mgr, st.ID, terminal); s.Status != JobDone || s.Counters.Transferred != 2 {
		t.Fatalf("delete: status=%s counters=%+v err=%q", s.Status, s.Counters, s.Error)
	}
	if keys := e.garage.keys("media"); strings.Join(keys, ",") != "other.txt" {
		t.Errorf("after delete: %v", keys)
	}
	recs = readFakeRcloneRecords(t, e.rcDir)
	if args := strings.Join(recs[len(recs)-1].Args, " "); !strings.HasPrefix(args, "delete ") || !strings.HasSuffix(args, "garage:media/arsip/") {
		t.Errorf("delete args = %s", args)
	}
}

func TestJobSpecValidation(t *testing.T) {
	e := newJobTestEnv(t)
	e.garage.put("media", "photos/a.jpg", []byte("x"), "image/jpeg")
	cases := map[string]JobSpec{
		"move into itself":    {Kind: JobMovePrefix, Transfers: 1, Src: Endpoint{Bucket: "media", Prefix: "photos/"}, Dst: Endpoint{Bucket: "media", Prefix: "photos/2024/"}},
		"copy onto itself":    {Kind: JobCopyPrefix, Transfers: 1, Src: Endpoint{Bucket: "media", Prefix: "photos/"}, Dst: Endpoint{Bucket: "media", Prefix: "photos/"}},
		"sync both garage":    {Kind: JobSync, Mode: "overwrite", Transfers: 1, Src: Endpoint{Bucket: "media"}, Dst: Endpoint{Bucket: "backup"}},
		"sync both remote":    {Kind: JobSync, Mode: "overwrite", Transfers: 1, Src: Endpoint{Remote: "wasabi", Bucket: "a"}, Dst: Endpoint{Remote: "wasabi", Bucket: "b"}},
		"unknown remote":      {Kind: JobSync, Mode: "overwrite", Transfers: 1, Src: Endpoint{Remote: "ghost", Bucket: "a"}, Dst: Endpoint{Bucket: "media"}},
		"bad mode":            {Kind: JobSync, Mode: "mirror", Transfers: 1, Src: Endpoint{Remote: "wasabi", Bucket: "a"}, Dst: Endpoint{Bucket: "media"}},
		"bad transfers":       {Kind: JobSync, Mode: "overwrite", Transfers: 99, Src: Endpoint{Remote: "wasabi", Bucket: "a"}, Dst: Endpoint{Bucket: "media"}},
		"prefix without /":    {Kind: JobSync, Mode: "overwrite", Transfers: 1, Src: Endpoint{Remote: "wasabi", Bucket: "a", Prefix: "in"}, Dst: Endpoint{Bucket: "media"}},
		"delete whole bucket": {Kind: JobDeletePrefix, Transfers: 1, Src: Endpoint{Bucket: "media"}},
		"delete on remote":    {Kind: JobDeletePrefix, Transfers: 1, Src: Endpoint{Remote: "wasabi", Bucket: "a", Prefix: "x/"}},
		"bad garage bucket":   {Kind: JobSync, Mode: "overwrite", Transfers: 1, Src: Endpoint{Remote: "wasabi", Bucket: "a"}, Dst: Endpoint{Bucket: "Bad Name"}},
		"unknown kind":        {Kind: "mirror", Transfers: 1, Src: Endpoint{Bucket: "media"}},
	}
	for name, spec := range cases {
		if _, err := e.mgr.Create(t.Context(), spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(e.mgr.List()) != 0 {
		t.Error("nothing should have been created")
	}

	// A remote that refuses the probe stops the job before it exists, with
	// the provider's message.
	e.remote.deny = `<Error><Code>AccessDenied</Code><Message>bucket policy says no</Message></Error>`
	_, err := e.mgr.Create(t.Context(), e.importSpec("overwrite"))
	if err == nil || !strings.Contains(err.Error(), "bucket policy says no") || !strings.Contains(err.Error(), "sumber tidak bisa diakses") {
		t.Errorf("probe failure: err = %v", err)
	}
}

func TestRerunStartsOverAndUsesRemoteBlocksDeletion(t *testing.T) {
	e := newJobTestEnv(t)
	e.remote.put("backup", "in/a.txt", []byte("x"), "text/plain")
	e.remote.put("backup", "in/fail-b.txt", []byte("x"), "text/plain")
	st, err := e.mgr.Create(t.Context(), e.importSpec("overwrite"))
	if err != nil {
		t.Fatal(err)
	}
	if e.mgr.UsesRemote("wasabi") != true {
		t.Error("a running job uses its remote")
	}
	s := waitJob(t, e.mgr, st.ID, terminal)
	if s.Counters.Failed != 1 {
		t.Fatalf("counters = %+v", s.Counters)
	}
	if e.mgr.UsesRemote("wasabi") {
		t.Error("a finished job does not block remote deletion")
	}
	if err := e.mgr.Rerun(st.ID); err != nil {
		t.Fatal(err)
	}
	s = waitJob(t, e.mgr, st.ID, terminal)
	if s.Runs != 2 || s.Counters.Transferred != 1 || s.Counters.Failed != 1 {
		t.Errorf("after rerun: runs=%d counters=%+v", s.Runs, s.Counters)
	}
	if _, total, _ := e.mgr.FailedKeys(st.ID, 10); total != 1 {
		t.Errorf("failed.jsonl should have been reset before the rerun: %d lines", total)
	}
	if err := e.mgr.Rerun(st.ID + "x"); err == nil {
		t.Error("bad id accepted")
	}
	if err := e.mgr.Delete("../../etc/passwd"); err == nil {
		t.Error("path-like id accepted")
	}
}

func TestResumeInterruptedSettlesPausingAndCancelling(t *testing.T) {
	e := newJobTestEnv(t)
	dir := filepath.Join(e.stateDir, "jobs")
	write := func(id string, status JobStatus) {
		s := JobState{JobSpec: JobSpec{ID: id, Kind: JobSync, Mode: "overwrite", Transfers: 1,
			Src: Endpoint{Remote: "wasabi", Bucket: "b-" + id[:4]}, Dst: Endpoint{Bucket: "d-" + id[:4]}, CreatedAt: time.Now()}, Status: status}
		raw, _ := json.Marshal(s)
		if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("aaaa000000000001", JobPausing)
	write("bbbb000000000002", JobCancelling)
	if err := os.WriteFile(filepath.Join(dir, "garbage.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := e.open(t)
	mgr.ResumeInterrupted()
	a, _ := mgr.Get("aaaa000000000001")
	b, _ := mgr.Get("bbbb000000000002")
	if a.Status != JobPaused || b.Status != JobCancelled {
		t.Errorf("statuses = %s / %s", a.Status, b.Status)
	}
	if len(mgr.List()) != 2 {
		t.Errorf("corrupt file should be skipped, got %d jobs", len(mgr.List()))
	}
}
