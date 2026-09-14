package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetectRcloneParsesVersionAndRejectsOldOnes(t *testing.T) {
	dir := t.TempDir()
	bin := fakeRcloneBin(t, dir)

	r, err := detectRclone(bin, filepath.Join(dir, "cache"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.version != "1.66.0" {
		t.Errorf("version = %q", r.version)
	}

	if err := os.WriteFile(filepath.Join(dir, "version"), []byte("rclone v1.53.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := detectRclone(bin, "", nil); err == nil || !strings.Contains(err.Error(), "terlalu lama") {
		t.Errorf("old version: err = %v", err)
	}
	if _, err := detectRclone(filepath.Join(dir, "does-not-exist"), "", nil); err == nil || !strings.Contains(err.Error(), "apt install rclone") {
		t.Errorf("missing binary: err = %v", err)
	}
}

func TestRcloneArgsNeverCarryCredentials(t *testing.T) {
	r := &rcloneRunner{bin: "rclone", cacheDir: "/var/lib/garagepanel/cache", extraArgs: []string{"--bwlimit", "10M"}}
	args := r.args(rcloneRun{Verb: "copy", ChunkPath: "/state/jobs/x/chunk.txt", Src: "src:backup/2024/", Dst: "garage:arsip/2024/", Transfers: 8})
	joined := strings.Join(args, " ")
	for _, want := range []string{"copy --files-from-raw /state/jobs/x/chunk.txt --no-traverse", "--no-check-dest -M", "--transfers 8", "--s3-upload-cutoff 8M", "--cache-dir /var/lib/garagepanel/cache", "--bwlimit 10M src:backup/2024/ garage:arsip/2024/", `--config `} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
	del := strings.Join(r.args(rcloneRun{Verb: "delete", ChunkPath: "c", Src: "garage:media/old/"}), " ")
	if strings.Contains(del, "--no-check-dest") || strings.Contains(del, "-M") || !strings.HasSuffix(del, " garage:media/old/") {
		t.Errorf("delete args wrong: %s", del)
	}
}

func TestRcloneRunStreamsStatsAndPerObjectErrors(t *testing.T) {
	dir := t.TempDir()
	bin := fakeRcloneBin(t, dir)
	r, err := detectRclone(bin, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	src := newFakeS3(t, "AK", "SK", "us-east-1")
	dst := newFakeS3(t, "GK", "GS", "garage")
	src.put("backup", "a.txt", []byte("hello"), "text/plain")
	src.put("backup", "sub/b.txt", []byte("world!"), "text/plain")
	chunk := filepath.Join(dir, "chunk.txt")
	if err := os.WriteFile(chunk, []byte("a.txt\nsub/b.txt\nfail-c.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srcRemote := Remote{Name: "src", Provider: "Wasabi", Endpoint: src.URL(), Region: "us-east-1", AccessKey: "AK", SecretKey: "SK"}
	dstRemote := Remote{Name: "dst", Provider: "Other", Endpoint: dst.URL(), Region: "garage", AccessKey: "GK", SecretKey: "GS"}

	t.Setenv("GARAGE_ADMIN_TOKEN", "must-not-leak-to-child")
	var events []rcloneEvent
	code, last, err := r.run(context.Background(), rcloneRun{
		Verb: "copy", ChunkPath: chunk, Src: "src:backup/", Dst: "dst:arsip/in/",
		Env: append(srcRemote.rcloneEnv("src"), dstRemote.rcloneEnv("dst")...), Transfers: 2,
	}, func(ev rcloneEvent) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if code != 6 {
		t.Errorf("exit code = %d, want 6 (partial failure)", code)
	}
	if got, _ := dst.get("arsip", "in/a.txt"); string(got) != "hello" {
		t.Errorf("a.txt not copied: %q", got)
	}
	if got, _ := dst.get("arsip", "in/sub/b.txt"); string(got) != "world!" {
		t.Errorf("sub/b.txt not copied: %q", got)
	}
	if ct := dst.contentType("arsip", "in/a.txt"); ct != "text/plain" {
		t.Errorf("content type not carried: %q", ct)
	}
	var lastStats *rcloneStats
	var failed []string
	for _, ev := range events {
		if ev.Stats != nil {
			lastStats = ev.Stats
		}
		if ev.Level == "error" && ev.Object != "" {
			failed = append(failed, ev.Object)
		}
	}
	if lastStats == nil || lastStats.Transfers != 2 || lastStats.Errors != 1 || lastStats.Bytes != 11 {
		t.Errorf("final stats = %+v", lastStats)
	}
	if strings.Join(failed, ",") != "fail-c.txt" {
		t.Errorf("failed keys = %v", failed)
	}
	if len(last) == 0 || !strings.Contains(strings.Join(last, "\n"), "fail-c.txt") {
		t.Errorf("last lines should carry the error: %v", last)
	}

	recs := readFakeRcloneRecords(t, dir)
	if len(recs) != 1 {
		t.Fatalf("recorded runs = %d", len(recs))
	}
	env := strings.Join(recs[0].Env, "\n")
	if !strings.Contains(env, "RCLONE_CONFIG_SRC_SECRET_ACCESS_KEY=SK") {
		t.Error("child must receive the remote's secret through its environment")
	}
	if strings.Contains(env, "GARAGE_ADMIN_TOKEN") || strings.Contains(env, "must-not-leak") {
		t.Error("child inherited the panel's environment")
	}
	if args := strings.Join(recs[0].Args, " "); strings.Contains(args, "SK") || strings.Contains(args, "GS") {
		t.Errorf("secret in argv: %s", args)
	}
}

func TestRcloneRunCancelInterruptsPromptly(t *testing.T) {
	dir := t.TempDir()
	bin := fakeRcloneBin(t, dir)
	r, err := detectRclone(bin, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte("30"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := newFakeS3(t, "AK", "SK", "garage")
	src.put("b", "a.txt", []byte("x"), "text/plain")
	chunk := filepath.Join(dir, "chunk.txt")
	if err := os.WriteFile(chunk, []byte("a.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rem := Remote{Name: "src", Provider: "Other", Endpoint: src.URL(), Region: "garage", AccessKey: "AK", SecretKey: "SK"}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, _, err = r.run(ctx, rcloneRun{Verb: "delete", ChunkPath: chunk, Src: "src:b/", Env: rem.rcloneEnv("src")}, nil)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("cancelled run must report the context error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancel took %v; SIGINT should stop the child promptly", elapsed)
	}
}

func TestRcloneRunReportsFatalExitWithLastLines(t *testing.T) {
	dir := t.TempDir()
	bin := fakeRcloneBin(t, dir)
	r, err := detectRclone(bin, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "exit"), []byte("7"), 0o600); err != nil {
		t.Fatal(err)
	}
	chunk := filepath.Join(dir, "chunk.txt")
	if err := os.WriteFile(chunk, []byte("a.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, last, err := r.run(context.Background(), rcloneRun{Verb: "copy", ChunkPath: chunk, Src: "src:b/", Dst: "dst:c/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 || chunkDone(code) {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(strings.Join(last, "\n"), "forced exit 7") {
		t.Errorf("last lines = %v", last)
	}
	for _, c := range []int{0, 5, 6} {
		if !chunkDone(c) {
			t.Errorf("exit %d should count as chunk done", c)
		}
	}
}

func TestParseRcloneLine(t *testing.T) {
	ev, ok := parseRcloneLine(`{"time":"2026-09-14T09:20:06Z","level":"notice","msg":"\nTransferred: ...","stats":{"bytes":3000012,"errors":0,"speed":12.5,"transfers":3,"totalTransfers":3},"source":"accounting/stats.go:549"}`)
	if !ok || ev.Stats == nil || ev.Stats.Bytes != 3000012 || ev.Stats.Transfers != 3 || ev.Stats.Speed != 12.5 || ev.Msg != "" {
		t.Errorf("stats line parsed wrong: ok=%v ev=%+v", ok, ev)
	}
	ev, ok = parseRcloneLine(`{"time":"x","level":"error","msg":"--files-from failed to read file: connection refused","object":"sub/b c.txt","objectType":"string"}`)
	if !ok || ev.Level != "error" || ev.Object != "sub/b c.txt" || !strings.Contains(ev.Msg, "connection refused") {
		t.Errorf("error line parsed wrong: %+v", ev)
	}
	if _, ok := parseRcloneLine("not json at all"); ok {
		t.Error("plain text must not parse")
	}
}
