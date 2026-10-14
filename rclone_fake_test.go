package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
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
	flags := map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case i == 0:
			verb = a
		case a == "--files-from-raw":
			i++
			chunkPath = args[i]
		case strings.HasPrefix(a, "-"):
			// Flags with a value: skip it unless it is boolean or written
			// as --flag=value.
			switch {
			case strings.Contains(a, "="):
				flags[a] = true
			case a == "--no-traverse", a == "--no-check-dest", a == "-M", a == "--use-json-log", a == "-R",
				a == "--files-only", a == "--dirs-only", a == "--no-mimetype", a == "--no-modtime":
				flags[a] = true
			default:
				i++
			}
		default:
			positional = append(positional, a)
		}
	}
	switch verb {
	case "lsjson", "mkdir", "rcat", "deletefile":
		return fakeRcloneFileCommand(verb, positional, flags, args)
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

	if len(positional) == 0 {
		return fail("no source")
	}
	src, err := fakeResolve(positional[0])
	if err != nil {
		return fail("Failed to create file system for " + strconv.Quote(positional[0]) + ": " + err.Error())
	}
	var dst fakeSide
	if verb != "delete" {
		if len(positional) < 2 {
			return fail("no destination")
		}
		if dst, err = fakeResolve(positional[1]); err != nil {
			return fail("Failed to create file system for " + strconv.Quote(positional[1]) + ": " + err.Error())
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
		var opErr error
		switch {
		case strings.HasPrefix(key, "fail-") || strings.Contains(key, "/fail-"):
			opErr = fmt.Errorf("injected failure for %s", key)
		case verb == "delete":
			opErr = src.remove(ctx, key)
		default:
			var data []byte
			var ct string
			data, ct, opErr = src.read(ctx, key)
			if opErr == nil {
				opErr = dst.write(ctx, key, data, ct)
				if opErr == nil {
					stats.Bytes += int64(len(data))
					if verb == "move" {
						opErr = src.remove(ctx, key)
						if opErr == nil {
							stats.Deletes++ // rclone counts a move as transfer + delete
						}
					}
				}
			}
		}
		if opErr != nil {
			if src.notFound(opErr) && verb != "delete" {
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

// fakeSide is one side of a fake rclone run: a bucket/prefix on an S3 server,
// or — for an sftp remote — a local directory standing in for the folder on
// the server (FAKE_RCLONE_DIR/sftp/<host>/<path>).
type fakeSide struct {
	s3     *S3
	bucket string
	prefix string
	dir    string
	root   string // the fake server's "/" (sftp)
	// skipLinks is the remote's skip_links option (sftp).
	skipLinks bool
}

// fakeResolve turns "alias:path" plus RCLONE_CONFIG_<ALIAS>_* into a side,
// checking SFTP credentials and host keys the way the real thing would.
func fakeResolve(spec string) (fakeSide, error) {
	alias, rest, ok := strings.Cut(spec, ":")
	if !ok {
		return fakeSide{}, fmt.Errorf("bad remote spec %q", spec)
	}
	prefix := "RCLONE_CONFIG_" + strings.ToUpper(alias) + "_"
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, prefix) {
			env[strings.TrimPrefix(k, prefix)] = v
		}
	}
	if env["TYPE"] == "sftp" {
		if err := fakeSFTPLogin(env); err != nil {
			return fakeSide{}, fmt.Errorf("NewFs: couldn't connect SSH: %w", err)
		}
		if !strings.HasPrefix(rest, "/") {
			return fakeSide{}, fmt.Errorf("fake rclone wants absolute sftp paths, got %q", rest)
		}
		if strings.Contains(env["HOST"], ":") && !strings.HasPrefix(env["HOST"], "[") {
			// What real rclone does with a bare IPv6 address.
			return fakeSide{}, fmt.Errorf("NewFs: couldn't connect SSH: dial tcp: address %s:%s: too many colons in address", env["HOST"], env["PORT"])
		}
		root := filepath.Join(os.Getenv("FAKE_RCLONE_DIR"), "sftp", strings.Trim(env["HOST"], "[]"))
		return fakeSide{dir: filepath.Join(root, filepath.FromSlash(rest)), root: root, skipLinks: env["SKIP_LINKS"] == "true"}, nil
	}
	// The fake always talks path-style: <bucket>.127.0.0.1 would not
	// resolve. FORCE_PATH_STYLE is still recorded for assertions.
	r := Remote{Name: alias, Addressing: AddressingPath, Provider: env["PROVIDER"], Endpoint: env["ENDPOINT"],
		Region: env["REGION"], AccessKey: env["ACCESS_KEY_ID"], SecretKey: env["SECRET_ACCESS_KEY"]}
	if r.Endpoint == "" {
		return fakeSide{}, fmt.Errorf("didn't find section in config file (%q)", alias)
	}
	c, err := r.Client()
	if err != nil {
		return fakeSide{}, err
	}
	bucket, p, _ := strings.Cut(rest, "/")
	return fakeSide{s3: c, bucket: bucket, prefix: p}, nil
}

// fakeSFTPLogin checks what the SSH handshake would: a known_hosts file with
// the server's key (knob "sftp-hostkey"), and the password (knob
// "sftp-password", which must arrive obscured) or a one-line key_pem.
func fakeSFTPLogin(env map[string]string) error {
	knob := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(os.Getenv("FAKE_RCLONE_DIR"), name))
		return strings.TrimSpace(string(b))
	}
	kh, err := os.ReadFile(env["KNOWN_HOSTS_FILE"])
	if err != nil {
		return fmt.Errorf("couldn't parse known_hosts_file: %w", err)
	}
	var lines []string
	for _, l := range strings.Split(string(kh), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return errors.New("ssh: handshake failed: knownhosts: key is unknown")
	}
	if want := knob("sftp-hostkey"); want != "" && !strings.Contains(string(kh), want) {
		return errors.New("ssh: handshake failed: knownhosts: key mismatch")
	}
	authFail := errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none password], no supported methods remain")
	switch {
	case env["KEY_PEM"] != "":
		if strings.Contains(env["KEY_PEM"], "\n") {
			return errors.New("pem key not formatted properly: invalid syntax")
		}
		if _, err := validatePrivateKey(strings.ReplaceAll(env["KEY_PEM"], `\n`, "\n")); err != nil {
			return fmt.Errorf("failed to parse private key file: %w", err)
		}
		if knob("sftp-password") != "" && knob("sftp-accept-keys") == "" {
			return authFail
		}
	case env["PASS"] != "":
		pw, err := rcloneReveal(env["PASS"])
		if err != nil {
			return fmt.Errorf("failed to read password: %w", err)
		}
		if want := knob("sftp-password"); want != "" && pw != want {
			return authFail
		}
	default:
		return authFail
	}
	return nil
}

func (s fakeSide) path(rel string) string { return filepath.Join(s.dir, filepath.FromSlash(rel)) }

func (s fakeSide) read(ctx context.Context, rel string) ([]byte, string, error) {
	if s.s3 == nil {
		data, err := os.ReadFile(s.path(rel))
		// rclone guesses Content-Type from the name for an S3 destination.
		return data, mime.TypeByExtension(filepath.Ext(rel)), err
	}
	body, hdr, err := s.s3.GetObject(ctx, s.bucket, s.prefix+rel)
	if err != nil {
		return nil, "", err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	ct := ""
	if v := hdr["Content-Type"]; len(v) > 0 {
		ct = v[0]
	}
	return data, ct, err
}

func (s fakeSide) write(ctx context.Context, rel string, data []byte, ct string) error {
	if s.s3 == nil {
		if err := os.MkdirAll(filepath.Dir(s.path(rel)), 0o755); err != nil {
			return err
		}
		return os.WriteFile(s.path(rel), data, 0o644)
	}
	return s.s3.PutObject(ctx, s.bucket, s.prefix+rel, data, ct)
}

func (s fakeSide) remove(ctx context.Context, rel string) error {
	if s.s3 == nil {
		return os.Remove(s.path(rel))
	}
	return s.s3.DeleteObject(ctx, s.bucket, s.prefix+rel)
}

func (s fakeSide) notFound(err error) bool {
	if s.s3 == nil {
		return errors.Is(err, os.ErrNotExist)
	}
	var s3err *S3Error
	return asS3Error(err, &s3err) && s3err.NotFound()
}

// fakeRcloneFileCommand handles the short commands the panel runs against an
// SFTP folder: lsjson (listing and probe), mkdir, rcat, deletefile. Errors
// are written in rclone's plain-text log format.
func fakeRcloneFileCommand(verb string, positional []string, flags map[string]bool, args []string) int {
	dir := os.Getenv("FAKE_RCLONE_DIR")
	rec := fakeRcloneRecord{Args: args, Env: os.Environ()}
	recBytes, _ := json.MarshalIndent(rec, "", " ")
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("cmd-%d-%d.json", time.Now().UnixNano(), os.Getpid())), recBytes, 0o600)
	logf := func(level, format string, a ...any) {
		fmt.Fprintf(os.Stderr, "2026/01/01 00:00:00 %-6s: %s\n", level, fmt.Sprintf(format, a...))
	}
	if len(positional) == 0 {
		logf("CRITICAL", "no path")
		return 1
	}
	side, err := fakeResolve(positional[0])
	if err != nil {
		logf("CRITICAL", "Failed to create file system for %q: %v", positional[0], err)
		return 1
	}
	if side.s3 != nil {
		logf("CRITICAL", "fake rclone only does %s on sftp remotes", verb)
		return 1
	}
	switch verb {
	case "mkdir":
		if err := os.MkdirAll(side.dir, 0o755); err != nil {
			logf("ERROR", "%v", err)
			return 1
		}
	case "rcat":
		data, _ := io.ReadAll(os.Stdin)
		if err := os.WriteFile(side.dir, data, 0o644); err != nil {
			logf("ERROR", "Failed to rcat: %v", err)
			return 1
		}
	case "deletefile":
		if err := os.Remove(side.dir); err != nil {
			logf("ERROR", "Failed to deletefile: %v", err)
			return 1
		}
	case "lsjson":
		// Knob "lsjson-exit" breaks the recursive listing of a job, not the
		// one-level listing of a probe: a subfolder the user may not read.
		if code, _ := strconv.Atoi(readKnob(dir, "lsjson-exit")); code != 0 && flags["-R"] {
			logf("ERROR", "private: error listing: permission denied")
			return code
		}
		// Knob "sftp-unreadable": absolute paths on the fake server, one per
		// line, whose listing is refused (directories with mode 711).
		for _, p := range strings.Split(readKnob(dir, "sftp-unreadable"), "\n") {
			if p = strings.TrimSpace(p); p != "" && filepath.Clean(side.dir) == filepath.Join(side.root, filepath.FromSlash(p)) {
				logf("ERROR", "error listing: permission denied")
				return 1
			}
		}
		info, err := os.Stat(side.dir) // the root itself may be a link
		if err != nil || !info.IsDir() {
			logf("ERROR", "error listing: directory not found")
			logf("NOTICE", "Failed to lsjson with 2 errors: last error was: error in ListJSON: directory not found")
			return 3
		}
		skipLinks := side.skipLinks && !flags["--sftp-skip-links=false"]
		var items []string
		add := func(rel, name string, size int64, isDir bool) {
			if (isDir && flags["--files-only"]) || (!isDir && flags["--dirs-only"]) {
				return
			}
			b, _ := json.Marshal(map[string]any{"Path": rel, "Name": name, "Size": size, "ModTime": "", "IsDir": isDir})
			items = append(items, string(b))
		}
		walkRoot := side.dir
		if resolved, err := filepath.EvalSymlinks(side.dir); err == nil {
			walkRoot = resolved
		}
		walk := func(path string, d os.DirEntry, err error) error {
			if err != nil || path == walkRoot {
				return err
			}
			rel, _ := filepath.Rel(walkRoot, path)
			rel = filepath.ToSlash(rel)
			if d.Type()&os.ModeSymlink != 0 {
				// Like the sftp backend: skipped with skip_links, otherwise
				// stat'ed and shown as what it points to (not descended into).
				if skipLinks {
					return nil
				}
				if fi, err := os.Stat(path); err == nil {
					add(rel, d.Name(), fi.Size(), fi.IsDir())
				}
				return nil
			}
			if d.IsDir() {
				add(rel, d.Name(), -1, true)
				if !flags["-R"] {
					return filepath.SkipDir
				}
				return nil
			}
			fi, _ := d.Info()
			add(rel, d.Name(), fi.Size(), false)
			return nil
		}
		if err := filepath.WalkDir(walkRoot, walk); err != nil {
			logf("ERROR", "%v", err)
			return 1
		}
		// rclone lists directory by directory, in no global order.
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
		fmt.Println("[")
		fmt.Println(strings.Join(items, ",\n"))
		fmt.Println("]")
	}
	return 0
}

func readKnob(dir, name string) string {
	b, _ := os.ReadFile(filepath.Join(dir, name))
	return strings.TrimSpace(string(b))
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
	return readFakeRcloneFiles(t, dir, "run-*.json")
}

// readFakeRcloneCommands returns the recorded short commands (lsjson, mkdir,
// rcat, deletefile), oldest first.
func readFakeRcloneCommands(t *testing.T, dir string) []fakeRcloneRecord {
	t.Helper()
	return readFakeRcloneFiles(t, dir, "cmd-*.json")
}

func readFakeRcloneFiles(t *testing.T, dir, pattern string) []fakeRcloneRecord {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, pattern))
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
