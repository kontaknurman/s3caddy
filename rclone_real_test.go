package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realRcloneBin returns a real rclone to test against, or "" to skip. Set
// GARAGEPANEL_TEST_RCLONE=/path/to/rclone, or have rclone on PATH.
func realRcloneBin() string {
	if p := os.Getenv("GARAGEPANEL_TEST_RCLONE"); p != "" {
		return p
	}
	if p, err := exec.LookPath("rclone"); err == nil {
		return p
	}
	return ""
}

// The fake rclone mirrors rclone's contract; this test runs the real thing
// against two in-memory S3 servers, including a multipart upload.
func TestRealRcloneSyncsBetweenTwoS3Servers(t *testing.T) {
	bin := realRcloneBin()
	if bin == "" {
		t.Skip("no rclone binary (set GARAGEPANEL_TEST_RCLONE)")
	}
	e := newJobTestEnv(t)
	rc, err := detectRclone(bin, filepath.Join(e.stateDir, "cache"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := NewJobManager(filepath.Join(e.stateDir, "jobs"), garageRemote(e.cfg), e.remotes, rc)
	if err != nil {
		t.Fatal(err)
	}
	mgr.chunkKeys = 100

	big := make([]byte, 20<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	e.remote.put("backup", "in/big.bin", big, "application/octet-stream")
	e.remote.put("backup", "in/foto 2026/a b+c.jpg", []byte("jpeg-bytes"), "image/jpeg")
	for i := 0; i < 300; i++ {
		e.remote.put("backup", fmt.Sprintf("in/small/%04d.txt", i), []byte(fmt.Sprintf("small %d", i)), "text/plain")
	}
	e.garage.put("arsip", "out/small/0001.txt", []byte("small 1"), "text/plain") // already there

	st, err := mgr.Create(t.Context(), JobSpec{
		Kind: JobSync, Mode: "skip-existing", Transfers: 4,
		Src: Endpoint{Remote: "wasabi", Bucket: "backup", Prefix: "in/"},
		Dst: Endpoint{Bucket: "arsip", Prefix: "out/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, mgr, st.ID, terminal)
	if s.Status != JobDone || s.Counters.Failed != 0 {
		t.Fatalf("status=%s counters=%+v err=%q log=%v", s.Status, s.Counters, s.Error, s.LastLog)
	}
	if s.Counters.Transferred != 301 || s.Counters.Skipped != 1 {
		t.Errorf("counters = %+v", s.Counters)
	}
	if got, _ := e.garage.get("arsip", "out/big.bin"); !bytes.Equal(got, big) {
		t.Errorf("big object differs (len %d)", len(got))
	}
	if got, _ := e.garage.get("arsip", "out/foto 2026/a b+c.jpg"); string(got) != "jpeg-bytes" {
		t.Errorf("awkward key not copied: %q", got)
	}
	if ct := e.garage.contentType("arsip", "out/foto 2026/a b+c.jpg"); ct != "image/jpeg" {
		t.Errorf("content type not carried: %q", ct)
	}
	if e.garage.count("arsip") != 302 {
		t.Errorf("destination has %d objects", e.garage.count("arsip"))
	}
	if e.garage.callCount("CreateMultipartUpload") == 0 || e.garage.callCount("CompleteMultipartUpload") == 0 {
		t.Errorf("the 20 MiB object should have gone multipart: %v", e.garage.calls)
	}
	if strings.Contains(strings.Join(s.LastLog, "\n"), testRemoteSecret) {
		t.Error("secret in rclone log")
	}
}
