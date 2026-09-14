package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a fake rclone: a wrapper script re-executes it
// with GARAGEPANEL_FAKE_RCLONE=1 (see fakeRcloneBin). The fake speaks
// rclone's --files-from-raw contract with the panel's own S3 client, so jobs
// really move objects between two fakeS3 servers, emits the same JSON log
// lines, honours SIGINT, and records argv + env for assertions.
func TestMain(m *testing.M) {
	if os.Getenv("GARAGEPANEL_FAKE_RCLONE") == "1" {
		os.Exit(fakeRcloneMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeRcloneBin writes a wrapper script in dir and returns its path. Knobs
// are files in dir: "sleep" (seconds per key), "exit" (forced exit code),
// "version" (version string to report).
func fakeRcloneBin(t *testing.T, dir string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "rclone")
	body := "#!/bin/sh\nexec env GARAGEPANEL_FAKE_RCLONE=1 FAKE_RCLONE_DIR=" + dir + " " + exe + " \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// fakeRcloneRecord is what one fake run writes to FAKE_RCLONE_DIR/run-*.json.
type fakeRcloneRecord struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
	Keys []string `json:"keys"`
}

func fakeRcloneMain(args []string) int {
	dir := os.Getenv("FAKE_RCLONE_DIR")
	knob := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return strings.TrimSpace(string(b))
	}
	if len(args) > 0 && args[0] == "version" {
		v := knob("version")
		if v == "" {
			v = "rclone v1.66.0"
		}
		fmt.Println(v)
		fmt.Println("- os/version: fake")
		return 0
	}
	logJSON := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintln(os.Stderr, string(b))
	}
	fail := func(msg string) int {
		logJSON(map[string]any{"level": "critical", "msg": msg})
		return 1
	}

	verb := ""
	chunkPath := ""
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case i == 0:
			verb = a
		case a == "--files-from-raw":
			i++
			chunkPath = args[i]
		case strings.HasPrefix(a, "-"):
			// Flags with a value: skip it unless it is boolean.
			switch a {
			case "--no-traverse", "--no-check-dest", "-M", "--use-json-log":
			default:
				i++
			}
		default:
			positional = append(positional, a)
		}
	}
	raw, err := os.ReadFile(chunkPath)
	if err != nil {
		return fail("--files-from-raw: " + err.Error())
	}
	var keys []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			keys = append(keys, line)
		}
	}
	rec := fakeRcloneRecord{Args: args, Env: os.Environ(), Keys: keys}
	recBytes, _ := json.MarshalIndent(rec, "", " ")
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("run-%d-%d.json", time.Now().UnixNano(), os.Getpid())), recBytes, 0o600)

	if code := knob("exit"); code != "" {
		n, _ := strconv.Atoi(code)
		logJSON(map[string]any{"level": "error", "msg": "forced exit " + code})
		return n
	}
	sleepPerKey := 0.0
	if s := knob("sleep"); s != "" {
		sleepPerKey, _ = strconv.ParseFloat(s, 64)
	}

	remote := func(alias string) (Remote, error) {
		prefix := "RCLONE_CONFIG_" + strings.ToUpper(alias) + "_"
		r := Remote{Name: alias}
		for _, kv := range os.Environ() {
			k, v, _ := strings.Cut(kv, "=")
			switch strings.TrimPrefix(k, prefix) {
			case "PROVIDER":
				r.Provider = v
			case "ENDPOINT":
				r.Endpoint = v
			case "REGION":
				r.Region = v
			case "ACCESS_KEY_ID":
				r.AccessKey = v
			case "SECRET_ACCESS_KEY":
				r.SecretKey = v
			}
		}
		if r.Endpoint == "" {
			return r, fmt.Errorf("didn't find section in config file (%q)", alias)
		}
		return r, nil
	}
	parseSpec := func(spec string) (Remote, string, string, error) {
		alias, rest, ok := strings.Cut(spec, ":")
		if !ok {
			return Remote{}, "", "", fmt.Errorf("bad remote spec %q", spec)
		}
		r, err := remote(alias)
		if err != nil {
			return Remote{}, "", "", err
		}
		bucket, prefix, _ := strings.Cut(rest, "/")
		return r, bucket, prefix, nil
	}

	if len(positional) == 0 {
		return fail("no source")
	}
	srcRemote, srcBucket, srcPrefix, err := parseSpec(positional[0])
	if err != nil {
		return fail("Failed to create file system for " + strconv.Quote(positional[0]) + ": " + err.Error())
	}
	src, err := srcRemote.Client()
	if err != nil {
		return fail(err.Error())
	}
	var dst *S3
	var dstBucket, dstPrefix string
	if verb != "delete" {
		if len(positional) < 2 {
			return fail("no destination")
		}
		dstRemote, b, p, err := parseSpec(positional[1])
		if err != nil {
			return fail("Failed to create file system for " + strconv.Quote(positional[1]) + ": " + err.Error())
		}
		dstBucket, dstPrefix = b, p
		if dst, err = dstRemote.Client(); err != nil {
			return fail(err.Error())
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stats := rcloneStats{TotalTransfers: int64(len(keys))}
	emitStats := func() {
		logJSON(map[string]any{"level": "notice", "msg": "stats", "stats": stats})
	}
	emitStats()
	for _, key := range keys {
		if ctx.Err() != nil {
			logJSON(map[string]any{"level": "error", "msg": "signal received, aborting"})
			return 143
		}
		if sleepPerKey > 0 {
			select {
			case <-time.After(time.Duration(sleepPerKey * float64(time.Second))):
			case <-ctx.Done():
				return 143
			}
		}
		srcKey := srcPrefix + key
		var opErr error
		switch {
		case strings.HasPrefix(key, "fail-") || strings.Contains(key, "/fail-"):
			opErr = fmt.Errorf("injected failure for %s", key)
		case verb == "delete":
			opErr = src.DeleteObject(ctx, srcBucket, srcKey)
		default:
			var body io.ReadCloser
			var hdr map[string][]string
			body, hdr, opErr = src.GetObject(ctx, srcBucket, srcKey)
			if opErr == nil {
				data, _ := io.ReadAll(body)
				body.Close()
				ct := ""
				if v := hdr["Content-Type"]; len(v) > 0 {
					ct = v[0]
				}
				opErr = dst.PutObject(ctx, dstBucket, dstPrefix+key, data, ct)
				if opErr == nil {
					stats.Bytes += int64(len(data))
					if verb == "move" {
						opErr = src.DeleteObject(ctx, srcBucket, srcKey)
					}
				}
			}
		}
		if opErr != nil {
			var s3err *S3Error
			if isS3Err := asS3Error(opErr, &s3err); isS3Err && s3err.NotFound() && verb != "delete" {
				// rclone silently skips a listed key that no longer exists.
				continue
			}
			stats.Errors++
			logJSON(map[string]any{"level": "error", "msg": opErr.Error(), "object": key, "objectType": "string"})
			continue
		}
		if verb == "delete" {
			stats.Deletes++
		} else {
			stats.Transfers++
		}
		emitStats()
	}
	emitStats()
	if stats.Errors > 0 {
		return 6
	}
	return 0
}

func asS3Error(err error, target **S3Error) bool {
	for e := err; e != nil; {
		if s, ok := e.(*S3Error); ok {
			*target = s
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// readFakeRcloneRecords returns every recorded run in dir, oldest first.
func readFakeRcloneRecords(t *testing.T, dir string) []fakeRcloneRecord {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "run-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out []fakeRcloneRecord
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		var rec fakeRcloneRecord
		if err := json.NewDecoder(bufio.NewReader(f)).Decode(&rec); err != nil {
			t.Fatal(err)
		}
		f.Close()
		out = append(out, rec)
	}
	return out
}
