package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// rclone does the transfers. The panel never lets it list a bucket: every run
// gets an explicit list of keys (--files-from-raw) and copies exactly those,
// so memory stays flat however many objects a bucket holds. Credentials
// reach it through the child's environment only — never argv, never a file.

const (
	// rcloneMinVersion is the oldest rclone the panel accepts: 1.59 added
	// --metadata (-M), which carries Content-Type and friends across.
	rcloneMinMajor = 1
	rcloneMinMinor = 59
	// rcloneStopGrace is how long a cancelled rclone gets after SIGINT to
	// abort its multipart uploads before it is killed.
	rcloneStopGrace = 15 * time.Second
	// rcloneLastLines is how many non-stats log lines are kept for the UI.
	rcloneLastLines = 20
	// rcloneMaxLineLen bounds one log line kept for the UI.
	rcloneMaxLineLen = 300
)

var rcloneVersionRe = regexp.MustCompile(`rclone v(\d+)\.(\d+)(?:\.(\d+))?`)

// rcloneRunner knows where rclone is and how to call it.
type rcloneRunner struct {
	bin       string
	version   string
	cacheDir  string
	extraArgs []string
}

// detectRclone finds rclone and checks its version. The lookup and the
// version call use a minimal environment, like every later run.
func detectRclone(bin, cacheDir string, extraArgs []string) (*rcloneRunner, error) {
	if bin == "" {
		bin = "rclone"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("rclone tidak ditemukan (%q): %w. Pasang dengan: sudo apt install rclone", bin, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.Env = minimalEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s version gagal: %v", path, err)
	}
	m := rcloneVersionRe.FindStringSubmatch(string(out))
	if m == nil {
		return nil, fmt.Errorf("%s version tidak bisa dibaca: %q", path, firstLine(string(out)))
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	version := m[1] + "." + m[2]
	if m[3] != "" {
		version += "." + m[3]
	}
	if major < rcloneMinMajor || (major == rcloneMinMajor && minor < rcloneMinMinor) {
		return nil, fmt.Errorf("rclone v%s terlalu lama; butuh minimal v%d.%d (Ubuntu 24.04: sudo apt install rclone; lainnya: https://rclone.org/install/)",
			version, rcloneMinMajor, rcloneMinMinor)
	}
	return &rcloneRunner{bin: path, version: version, cacheDir: cacheDir, extraArgs: extraArgs}, nil
}

// minimalEnv is the environment every rclone child starts from: PATH and
// nothing else. In particular GARAGE_ADMIN_TOKEN and PANEL_PASSWORD_HASH from
// the panel's own environment are never inherited.
func minimalEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "RCLONE_CONFIG="}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// rcloneStats is the subset of rclone's periodic "stats" object the panel
// shows. Field names follow rclone's JSON.
type rcloneStats struct {
	Bytes          int64   `json:"bytes"`
	Checks         int64   `json:"checks"`
	Deletes        int64   `json:"deletes"`
	Errors         int64   `json:"errors"`
	Transfers      int64   `json:"transfers"`
	TotalTransfers int64   `json:"totalTransfers"`
	TotalBytes     int64   `json:"totalBytes"`
	Speed          float64 `json:"speed"`
	FatalError     bool    `json:"fatalError"`
	RetryError     bool    `json:"retryError"`
}

// rcloneEvent is one parsed log line.
type rcloneEvent struct {
	Stats  *rcloneStats // set for periodic stats lines
	Level  string
	Msg    string
	Object string // the key, on per-object errors
}

// rcloneRun describes one invocation: one verb over one chunk of keys.
type rcloneRun struct {
	Verb      string // copy | move | delete
	ChunkPath string // file with one key per line, relative to Src
	Src       string // "src:bucket/prefix/"
	Dst       string // "dst:bucket/prefix/", empty for delete
	Env       []string
	Transfers int
}

// args renders the command line. Credentials are not in it.
func (r *rcloneRunner) args(spec rcloneRun) []string {
	transfers := spec.Transfers
	if transfers <= 0 {
		transfers = 4
	}
	args := []string{
		spec.Verb,
		"--files-from-raw", spec.ChunkPath,
		"--no-traverse",
		"--use-json-log",
		"--stats", "5s",
		"--stats-log-level", "NOTICE",
		"--log-level", "NOTICE",
		"--transfers", strconv.Itoa(transfers),
		"--checkers", strconv.Itoa(transfers),
		"--retries", "3",
		"--low-level-retries", "10",
		"--config", "",
	}
	if spec.Verb != "delete" {
		args = append(args,
			// The panel already decided which keys need copying, so rclone
			// must not spend a request per object on checking the target.
			"--no-check-dest",
			// Carry Content-Type, Cache-Control, Content-Disposition and
			// x-amz-meta-* across providers.
			"-M",
			// Anything above 8 MiB goes multipart in 8 MiB parts, two at a
			// time per transfer, so no object is ever buffered whole:
			// memory = transfers × 2 × 8 MiB.
			"--s3-upload-cutoff", "8M",
			"--s3-chunk-size", "8M",
			"--s3-upload-concurrency", "2",
		)
	}
	if r.cacheDir != "" {
		args = append(args, "--cache-dir", r.cacheDir)
	}
	args = append(args, r.extraArgs...)
	args = append(args, spec.Src)
	if spec.Dst != "" {
		args = append(args, spec.Dst)
	}
	return args
}

// run executes one rclone invocation and streams its log lines to onEvent.
// It returns rclone's exit code and the last non-stats log lines (for the
// UI). A cancelled ctx sends SIGINT, waits rcloneStopGrace, then kills; err
// is then ctx.Err().
func (r *rcloneRunner) run(ctx context.Context, spec rcloneRun, onEvent func(rcloneEvent)) (exitCode int, lastLines []string, err error) {
	cmd := exec.CommandContext(ctx, r.bin, r.args(spec)...)
	cmd.Env = append(minimalEnv(), spec.Env...)
	cmd.Stdout = io.Discard
	cmd.Stdin = nil
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = rcloneStopGrace
	if spec.ChunkPath != "" {
		cmd.Dir = filepath.Dir(spec.ChunkPath)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, nil, err
	}
	if err := cmd.Start(); err != nil {
		return -1, nil, fmt.Errorf("rclone gagal dijalankan: %w", err)
	}

	var ring []string
	keep := func(line string) {
		if len(line) > rcloneMaxLineLen {
			line = line[:rcloneMaxLineLen] + "…"
		}
		ring = append(ring, line)
		if len(ring) > rcloneLastLines {
			ring = ring[len(ring)-rcloneLastLines:]
		}
	}

	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		ev, ok := parseRcloneLine(line)
		if !ok {
			keep(line)
			continue
		}
		if ev.Stats == nil {
			keep(strings.TrimSpace(ev.Level + ": " + ev.Msg))
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}

	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return -1, ring, ctx.Err()
	}
	if waitErr == nil {
		return 0, ring, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode(), ring, nil
	}
	return -1, ring, fmt.Errorf("rclone: %w", waitErr)
}

// parseRcloneLine decodes one --use-json-log line.
func parseRcloneLine(line string) (rcloneEvent, bool) {
	if !strings.HasPrefix(line, "{") {
		return rcloneEvent{}, false
	}
	var raw struct {
		Level  string       `json:"level"`
		Msg    string       `json:"msg"`
		Object string       `json:"object"`
		Stats  *rcloneStats `json:"stats"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return rcloneEvent{}, false
	}
	ev := rcloneEvent{Level: raw.Level, Msg: raw.Msg, Object: raw.Object, Stats: raw.Stats}
	if ev.Stats != nil {
		// The msg of a stats line is a multi-line human table; drop it.
		ev.Msg = ""
	}
	return ev, true
}

// rcloneExitMeaning turns an exit code into words for the UI.
func rcloneExitMeaning(code int) string {
	switch code {
	case 0:
		return "selesai"
	case 1:
		return "argumen atau konfigurasi rclone salah"
	case 2:
		return "error yang tidak terkategori"
	case 3:
		return "direktori/prefix tidak ditemukan"
	case 4:
		return "file tidak ditemukan"
	case 5:
		return "error sementara (retry habis) — biasanya koneksi ke salah satu sisi"
	case 6:
		return "sebagian objek gagal ditransfer"
	case 7:
		return "error fatal"
	case 8:
		return "batas transfer tercapai"
	case 9:
		return "tidak ada yang ditransfer"
	default:
		return fmt.Sprintf("exit code %d", code)
	}
}

// chunkDone reports whether an exit code means the chunk was processed to the
// end (possibly with per-object failures, which are already recorded) rather
// than aborted.
func chunkDone(code int) bool {
	return code == 0 || code == 5 || code == 6 || code == 9
}
