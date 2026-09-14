package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// A job is one long-running transfer: a bucket sync with a remote, or a
// folder copy/move/delete inside Garage. Jobs run as goroutines inside the
// panel and checkpoint to STATE_DIR/jobs/<id>.json after every chunk, so a
// restart of the panel — or the whole server — picks them up where they were.
// Job files never contain credentials, only the remote's name.

// JobKind is what a job does.
type JobKind string

const (
	JobSync         JobKind = "sync"
	JobDeletePrefix JobKind = "delete-prefix"
	JobCopyPrefix   JobKind = "copy-prefix"
	JobMovePrefix   JobKind = "move-prefix"
)

func (k JobKind) valid() bool {
	switch k {
	case JobSync, JobDeletePrefix, JobCopyPrefix, JobMovePrefix:
		return true
	}
	return false
}

// Label is the kind in words, for the UI.
func (k JobKind) Label() string {
	switch k {
	case JobSync:
		return "Sync"
	case JobDeletePrefix:
		return "Hapus folder"
	case JobCopyPrefix:
		return "Salin folder"
	case JobMovePrefix:
		return "Pindah folder"
	}
	return string(k)
}

// rcloneVerb is the rclone subcommand for the kind.
func (k JobKind) rcloneVerb() string {
	switch k {
	case JobDeletePrefix:
		return "delete"
	case JobMovePrefix:
		return "move"
	}
	return "copy"
}

// JobStatus is where a job is in its life. pausing/cancelling are written to
// disk *before* rclone is stopped, so a crash in between does not bring a
// job back to life that the operator had just paused.
type JobStatus string

const (
	JobRunning    JobStatus = "running"
	JobPausing    JobStatus = "pausing"
	JobPaused     JobStatus = "paused"
	JobCancelling JobStatus = "cancelling"
	JobCancelled  JobStatus = "cancelled"
	JobDone       JobStatus = "done"
	JobFailed     JobStatus = "failed"
)

// Terminal reports whether the job is over.
func (s JobStatus) Terminal() bool {
	return s == JobDone || s == JobFailed || s == JobCancelled
}

// Label is the status in words, for the UI.
func (s JobStatus) Label() string {
	switch s {
	case JobRunning:
		return "berjalan"
	case JobPausing:
		return "menjeda…"
	case JobPaused:
		return "dijeda"
	case JobCancelling:
		return "membatalkan…"
	case JobCancelled:
		return "dibatalkan"
	case JobDone:
		return "selesai"
	case JobFailed:
		return "gagal"
	}
	return string(s)
}

// Endpoint is one side of a job. Remote "" means the panel's own Garage.
type Endpoint struct {
	Remote string `json:"remote"`
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix"`
}

// IsGarage reports whether the side is the panel's own Garage.
func (e Endpoint) IsGarage() bool { return e.Remote == "" }

// String renders the side the way rclone would name it.
func (e Endpoint) String() string {
	name := e.Remote
	if name == "" {
		name = garageRemoteName
	}
	return name + ":" + e.Bucket + "/" + e.Prefix
}

// bucketKey identifies the bucket for conflict checks.
func (e Endpoint) bucketKey() string {
	name := e.Remote
	if name == "" {
		name = garageRemoteName
	}
	return name + "/" + e.Bucket
}

// JobSpec is what the operator asked for.
type JobSpec struct {
	ID        string    `json:"id"`
	Kind      JobKind   `json:"kind"`
	Src       Endpoint  `json:"src"`
	Dst       Endpoint  `json:"dst"`
	Mode      string    `json:"mode"`
	Transfers int       `json:"transfers"`
	CreatedAt time.Time `json:"createdAt"`
}

// Direction says which way a sync goes.
func (s JobSpec) Direction() string {
	switch {
	case s.Kind != JobSync:
		return s.Kind.Label()
	case s.Src.IsGarage():
		return "Ekspor"
	default:
		return "Impor"
	}
}

// JobCounters are the running totals. Transferred counts deleted objects for
// a delete job.
type JobCounters struct {
	Listed      int64 `json:"listed"`
	Transferred int64 `json:"transferred"`
	Skipped     int64 `json:"skipped"`
	Failed      int64 `json:"failed"`
	Bytes       int64 `json:"bytes"`
	Chunks      int64 `json:"chunks"`
}

// JobState is the on-disk record of a job.
type JobState struct {
	JobSpec
	Status JobStatus `json:"status"`
	// Checkpoint is the relative key of the last object whose chunk finished.
	// Empty means "from the beginning".
	Checkpoint    string      `json:"checkpoint"`
	Counters      JobCounters `json:"counters"`
	Speed         float64     `json:"speed"`
	PauseReason   string      `json:"pauseReason"`
	Error         string      `json:"error"`
	LastLog       []string    `json:"lastLog"`
	Runs          int         `json:"runs"`
	StartedAt     time.Time   `json:"startedAt"`
	UpdatedAt     time.Time   `json:"updatedAt"`
	FinishedAt    *time.Time  `json:"finishedAt"`
	RcloneVersion string      `json:"rcloneVersion"`
}

// FailedKey is one line of <job>/failed.jsonl.
type FailedKey struct {
	Key   string    `json:"key"`
	Error string    `json:"error"`
	At    time.Time `json:"at"`
}

const (
	jobFileMode = 0o600
	// maxRunningJobs bounds concurrent rclone processes, and with them memory.
	maxRunningJobs = 2
	// failedLogCap stops a broken remote from filling the disk with one line
	// per object.
	failedLogCap = 1_000_000
	// outageMinFailures: a chunk in which at least this many keys failed *and*
	// at least half of them did is treated as an outage. The job pauses, the
	// checkpoint is not advanced and the keys are not written off as failed,
	// so resuming after the outage simply retries them.
	outageMinFailures = 10
	jobLogEveryChunks = 100
)

type jobHandle struct {
	mu     sync.Mutex
	state  JobState
	cancel context.CancelFunc // non-nil while a runner goroutine is alive
	live   rcloneStats        // the rclone run in progress
	// chunkFailed collects the per-object errors of the run in progress,
	// keyed by relative key so a retried object is counted once.
	chunkFailed map[string]FailedKey
	// pendingFailed are counted failures not yet appended to failed.jsonl.
	pendingFailed []FailedKey
}

// isRunning reports whether a runner goroutine is alive for the job.
func (h *jobHandle) isRunning() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cancel != nil
}

func (h *jobHandle) snapshot() JobState {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.state
	s.LastLog = append([]string(nil), h.state.LastLog...)
	// Fold the run in progress into the counters the UI sees. Failures are
	// only counted for good once their chunk is committed, so an interrupted
	// chunk (whose keys are examined again on resume) never counts twice.
	if s.Kind == JobDeletePrefix {
		s.Counters.Transferred += h.live.Deletes
	} else {
		s.Counters.Transferred += h.live.Transfers
	}
	s.Counters.Bytes += h.live.Bytes
	s.Counters.Failed += int64(len(h.chunkFailed) + len(h.pendingFailed))
	return s
}

// JobManager owns every job and its files.
type JobManager struct {
	dir     string
	garage  Remote
	remotes *RemoteStore
	rclone  *rcloneRunner

	chunkKeys  int
	maxRunning int
	now        func() time.Time

	mu   sync.Mutex
	jobs map[string]*jobHandle
	wg   sync.WaitGroup
}

// NewJobManager loads the jobs saved under dir. Nothing starts running until
// ResumeInterrupted is called.
func NewJobManager(dir string, garage Remote, remotes *RemoteStore, rclone *rcloneRunner) (*JobManager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("direktori job: %w", err)
	}
	m := &JobManager{
		dir:        dir,
		garage:     garage,
		remotes:    remotes,
		rclone:     rclone,
		chunkKeys:  syncChunkKeys,
		maxRunning: maxRunningJobs,
		now:        time.Now,
		jobs:       map[string]*jobHandle{},
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if ValidateJobID(id) != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var state JobState
		if err := json.Unmarshal(raw, &state); err != nil || state.ID != id || !state.Kind.valid() {
			log.Printf("PERINGATAN: file job %s rusak dan diabaikan", name)
			continue
		}
		m.jobs[id] = &jobHandle{state: state}
	}
	return m, nil
}

func (m *JobManager) jobPath(id string) string { return filepath.Join(m.dir, id+".json") }
func (m *JobManager) jobDir(id string) string  { return filepath.Join(m.dir, id) }

// persist writes the job file atomically.
func (m *JobManager) persist(h *jobHandle) error {
	h.mu.Lock()
	h.state.UpdatedAt = m.now()
	raw, err := json.MarshalIndent(h.state, "", "  ")
	id := h.state.ID
	h.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(m.jobPath(id), append(raw, '\n'), jobFileMode)
}

// --- creating ---------------------------------------------------------------

// sides resolves both endpoints to Remote values (with credentials).
func (m *JobManager) sides(spec JobSpec) (src, dst Remote, err error) {
	resolve := func(e Endpoint) (Remote, error) {
		if e.IsGarage() {
			return m.garage, nil
		}
		r, ok := m.remotes.Get(e.Remote)
		if !ok {
			return Remote{}, fmt.Errorf("remote %q tidak ada (sudah dihapus?)", e.Remote)
		}
		return r, nil
	}
	if src, err = resolve(spec.Src); err != nil {
		return
	}
	if spec.Kind != JobDeletePrefix {
		dst, err = resolve(spec.Dst)
	}
	return
}

func validateEndpoint(e Endpoint, remotes *RemoteStore) error {
	if e.IsGarage() {
		if err := ValidateBucketName(e.Bucket); err != nil {
			return err
		}
	} else {
		if err := ValidateRemoteName(e.Remote); err != nil {
			return err
		}
		if _, ok := remotes.Get(e.Remote); !ok {
			return fmt.Errorf("remote %q tidak ada", e.Remote)
		}
		if err := ValidateRemoteBucketName(e.Bucket); err != nil {
			return err
		}
	}
	return ValidateJobPrefix(e.Prefix)
}

// validateSpec checks a new job before anything is touched.
func (m *JobManager) validateSpec(spec JobSpec) error {
	if !spec.Kind.valid() {
		return fmt.Errorf("jenis job %q tidak dikenal", spec.Kind)
	}
	if err := ValidateTransfers(spec.Transfers); err != nil {
		return err
	}
	if err := validateEndpoint(spec.Src, m.remotes); err != nil {
		return fmt.Errorf("sumber: %w", err)
	}
	switch spec.Kind {
	case JobSync:
		if err := ValidateSyncMode(spec.Mode); err != nil {
			return err
		}
		if err := validateEndpoint(spec.Dst, m.remotes); err != nil {
			return fmt.Errorf("tujuan: %w", err)
		}
		if spec.Src.IsGarage() == spec.Dst.IsGarage() {
			return errors.New("satu sisi harus Garage dan sisi lainnya remote (impor atau ekspor)")
		}
	case JobDeletePrefix:
		if !spec.Src.IsGarage() || spec.Src.Prefix == "" {
			return errors.New("hapus folder hanya untuk folder di Garage")
		}
	case JobCopyPrefix, JobMovePrefix:
		if !spec.Src.IsGarage() || !spec.Dst.IsGarage() || spec.Src.Prefix == "" {
			return errors.New("salin/pindah folder hanya untuk folder di Garage")
		}
		if err := validateEndpoint(spec.Dst, m.remotes); err != nil {
			return fmt.Errorf("tujuan: %w", err)
		}
		if spec.Src.Bucket == spec.Dst.Bucket && strings.HasPrefix(spec.Dst.Prefix, spec.Src.Prefix) {
			return fmt.Errorf("tujuan %q berada di dalam (atau sama dengan) sumber %q", spec.Dst.Prefix, spec.Src.Prefix)
		}
	}
	return nil
}

// Create validates, probes both sides, saves and starts a job.
func (m *JobManager) Create(ctx context.Context, spec JobSpec) (JobState, error) {
	spec.ID = randomHex(8)
	spec.CreatedAt = m.now()
	if spec.Kind != JobSync {
		spec.Mode = "overwrite"
	}
	if err := m.validateSpec(spec); err != nil {
		return JobState{}, err
	}
	src, dst, err := m.sides(spec)
	if err != nil {
		return JobState{}, err
	}
	if err := probeRemote(ctx, src, spec.Src.Bucket); err != nil {
		return JobState{}, fmt.Errorf("sumber tidak bisa diakses: %w", err)
	}
	if spec.Kind != JobDeletePrefix {
		if err := probeRemote(ctx, dst, spec.Dst.Bucket); err != nil {
			return JobState{}, fmt.Errorf("tujuan tidak bisa diakses: %w", err)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkLimitsLocked(spec, ""); err != nil {
		return JobState{}, err
	}
	now := m.now()
	h := &jobHandle{state: JobState{
		JobSpec:       spec,
		Status:        JobRunning,
		Runs:          1,
		StartedAt:     now,
		UpdatedAt:     now,
		RcloneVersion: m.rclone.version,
	}}
	if err := os.MkdirAll(m.jobDir(spec.ID), 0o700); err != nil {
		return JobState{}, err
	}
	if err := m.persist(h); err != nil {
		return JobState{}, err
	}
	m.jobs[spec.ID] = h
	m.startLocked(h)
	if spec.Kind == JobDeletePrefix {
		log.Printf("job %s: %s %s dimulai", spec.ID, spec.Kind, spec.Src)
	} else {
		log.Printf("job %s: %s %s → %s dimulai", spec.ID, spec.Kind, spec.Src, spec.Dst)
	}
	return h.snapshot(), nil
}

// checkLimitsLocked refuses a job that would touch a bucket another unfinished
// job uses, or that would exceed the running limit.
func (m *JobManager) checkLimitsLocked(spec JobSpec, exceptID string) error {
	running := 0
	for id, other := range m.jobs {
		if id == exceptID {
			continue
		}
		s := other.snapshot()
		if s.Status.Terminal() {
			continue
		}
		if other.isRunning() {
			running++
		}
		for _, mine := range []Endpoint{spec.Src, spec.Dst} {
			if mine.Bucket == "" {
				continue
			}
			for _, theirs := range []Endpoint{s.Src, s.Dst} {
				if theirs.Bucket != "" && theirs.bucketKey() == mine.bucketKey() {
					return fmt.Errorf("bucket %s sedang dipakai job %s (%s); tunggu sampai selesai atau batalkan dulu", mine.bucketKey(), s.ID, s.Status.Label())
				}
			}
		}
	}
	if running >= m.maxRunning {
		return fmt.Errorf("sudah %d job berjalan (batas %d); tunggu atau jeda salah satunya", running, m.maxRunning)
	}
	return nil
}

func (m *JobManager) startLocked(h *jobHandle) {
	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.cancel = cancel
	h.live = rcloneStats{}
	h.chunkFailed = nil
	h.pendingFailed = nil
	h.mu.Unlock()
	m.wg.Add(1)
	go m.run(ctx, h)
}

// --- running ----------------------------------------------------------------

func (m *JobManager) run(ctx context.Context, h *jobHandle) {
	defer m.wg.Done()
	err := m.execute(ctx, h)
	m.finish(ctx, h, err)
}

// finish records the outcome. A cancelled context means one of three things,
// told apart by the status written before the cancel: pausing → paused,
// cancelling → cancelled, still running → the panel is shutting down and the
// job must come back after restart.
func (m *JobManager) finish(ctx context.Context, h *jobHandle, err error) {
	h.mu.Lock()
	id := h.state.ID
	switch {
	case ctx.Err() != nil:
		switch h.state.Status {
		case JobPausing:
			h.state.Status = JobPaused
		case JobCancelling:
			h.state.Status = JobCancelled
		}
	case err != nil:
		h.state.Status = JobFailed
		h.state.Error = err.Error()
	case h.state.Status == JobRunning:
		h.state.Status = JobDone
	}
	if h.state.Status.Terminal() {
		t := m.now()
		h.state.FinishedAt = &t
	}
	status := h.state.Status
	reason := h.state.PauseReason
	h.cancel = nil
	h.live = rcloneStats{}
	h.chunkFailed = nil
	h.pendingFailed = nil
	h.mu.Unlock()

	if perr := m.persist(h); perr != nil {
		log.Printf("job %s: gagal menyimpan state: %v", id, perr)
	}
	switch {
	case err != nil && ctx.Err() == nil:
		log.Printf("job %s: gagal: %s", id, firstLine(err.Error()))
	case status == JobPaused && reason != "":
		log.Printf("job %s: dijeda otomatis: %s", id, firstLine(reason))
	default:
		log.Printf("job %s: %s", id, status.Label())
	}
}

// execute is the chunk loop. It returns ctx.Err() when interrupted, an error
// when the job must stop, and nil when the source was walked to the end or
// the job paused itself.
func (m *JobManager) execute(ctx context.Context, h *jobHandle) error {
	spec := h.snapshot().JobSpec
	srcRemote, dstRemote, err := m.sides(spec)
	if err != nil {
		return err
	}
	srcC, err := srcRemote.Client()
	if err != nil {
		return err
	}
	var dstC *S3
	if spec.Kind != JobDeletePrefix {
		if dstC, err = dstRemote.Client(); err != nil {
			return err
		}
	}

	var env []string
	var srcSpec, dstSpec string
	if spec.Kind == JobSync {
		env = append(srcRemote.rcloneEnv("src"), dstRemote.rcloneEnv("dst")...)
		srcSpec = "src:" + spec.Src.Bucket + "/" + spec.Src.Prefix
		dstSpec = "dst:" + spec.Dst.Bucket + "/" + spec.Dst.Prefix
	} else {
		env = m.garage.rcloneEnv(garageRemoteName)
		srcSpec = garageRemoteName + ":" + spec.Src.Bucket + "/" + spec.Src.Prefix
		if spec.Kind != JobDeletePrefix {
			dstSpec = garageRemoteName + ":" + spec.Dst.Bucket + "/" + spec.Dst.Prefix
		}
	}

	jobDir := m.jobDir(spec.ID)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		return err
	}
	chunkPath := filepath.Join(jobDir, "chunk.txt")
	defer os.Remove(chunkPath)

	checkpoint := h.snapshot().Checkpoint
	startAfter := func(prefix string) string {
		if checkpoint == "" {
			return ""
		}
		return prefix + checkpoint
	}
	builder := &chunkBuilder{
		src:  newKeyIterator(srcC, "sumber", spec.Src.Bucket, spec.Src.Prefix, startAfter(spec.Src.Prefix)),
		mode: spec.Mode,
		max:  m.chunkKeys,
		onInvalid: func(key, reason string) {
			h.mu.Lock()
			h.addFailedLocked(FailedKey{Key: key, Error: reason, At: m.now()})
			h.mu.Unlock()
		},
	}
	if spec.Kind == JobSync && spec.Mode != "overwrite" {
		builder.dst = newKeyIterator(dstC, "tujuan", spec.Dst.Bucket, spec.Dst.Prefix, startAfter(spec.Dst.Prefix))
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, done, err := builder.next(ctx, chunkPath)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}

		if res.Keys > 0 {
			code, last, err := m.rclone.run(ctx, rcloneRun{
				Verb:      spec.Kind.rcloneVerb(),
				ChunkPath: chunkPath,
				Src:       srcSpec,
				Dst:       dstSpec,
				Env:       env,
				Transfers: spec.Transfers,
			}, h.onRcloneEvent(spec.Src.Prefix, m.now))
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}

			h.mu.Lock()
			h.state.LastLog = redactAll(last, srcRemote.SecretKey, dstRemote.SecretKey)
			live := h.live
			failedNow := h.chunkFailed
			h.live = rcloneStats{}
			h.chunkFailed = nil
			h.mu.Unlock()

			if !chunkDone(code) {
				return fmt.Errorf("rclone berhenti dengan exit code %d (%s). Baris log terakhir ada di bawah. Chunk ini akan diperiksa ulang saat job dilanjutkan.", code, rcloneExitMeaning(code))
			}
			failedCount := len(failedNow)
			if failedCount >= outageMinFailures && failedCount*2 >= res.Keys {
				h.mu.Lock()
				h.state.Status = JobPaused
				h.state.PauseReason = fmt.Sprintf("%d dari %d objek di chunk terakhir gagal — kemungkinan salah satu sisi sedang tidak bisa dihubungi. Chunk ini belum dicatat sebagai selesai; periksa koneksi lalu Lanjutkan, objek yang gagal akan dicoba lagi.", failedCount, res.Keys)
				h.mu.Unlock()
				return nil
			}
			// Record in key order so the log reads like the listing.
			failedKeys := make([]string, 0, len(failedNow))
			for k := range failedNow {
				failedKeys = append(failedKeys, k)
			}
			sort.Strings(failedKeys)
			// rclone counts a moved object as one transfer *and* one delete;
			// a deleted one only as a delete. Count objects, not operations.
			done := live.Transfers
			if spec.Kind == JobDeletePrefix {
				done = live.Deletes
			}
			h.mu.Lock()
			for _, k := range failedKeys {
				h.addFailedLocked(failedNow[k])
			}
			h.state.Counters.Transferred += done
			h.state.Counters.Bytes += live.Bytes
			h.mu.Unlock()
		}

		if len(res.Markers) > 0 {
			m.handleMarkers(ctx, spec, res.Markers, srcC, dstC, h)
		}

		h.mu.Lock()
		h.state.Counters.Listed += res.Listed
		h.state.Counters.Skipped += res.Skipped
		if res.Last != "" {
			h.state.Checkpoint = res.Last
		}
		if res.Keys > 0 {
			h.state.Counters.Chunks++
		}
		chunks := h.state.Counters.Chunks
		pending := h.pendingFailed
		h.pendingFailed = nil
		h.state.Counters.Failed += int64(len(pending))
		failedTotal := h.state.Counters.Failed
		h.mu.Unlock()

		if err := m.appendFailed(spec.ID, pending); err != nil {
			return err
		}
		if err := m.persist(h); err != nil {
			return err
		}
		if failedTotal >= failedLogCap {
			h.mu.Lock()
			h.state.Status = JobPaused
			h.state.PauseReason = fmt.Sprintf("sudah %d objek gagal dicatat; job dijeda supaya daftar kegagalan tidak membengkak. Periksa daftar key gagal.", failedTotal)
			h.mu.Unlock()
			return nil
		}
		if chunks > 0 && chunks%jobLogEveryChunks == 0 {
			s := h.snapshot()
			log.Printf("job %s: %d chunk, %d objek diperiksa, %d ditransfer, %d dilewati, %d gagal",
				spec.ID, chunks, s.Counters.Listed, s.Counters.Transferred, s.Counters.Skipped, s.Counters.Failed)
		}
		if done {
			break
		}
	}

	// The prefix's own marker object is not a file for rclone; finish the
	// job by removing it where the folder itself goes away.
	if spec.Kind == JobDeletePrefix || spec.Kind == JobMovePrefix {
		if err := srcC.DeleteObject(ctx, spec.Src.Bucket, spec.Src.Prefix); err != nil && ctx.Err() == nil {
			var s3err *S3Error
			if !errors.As(err, &s3err) || !s3err.NotFound() {
				log.Printf("job %s: marker folder %q tidak bisa dihapus: %v", spec.ID, spec.Src.Prefix, firstLine(err.Error()))
			}
		}
	}
	return ctx.Err()
}

// handleMarkers deals with the zero-byte "folder/" objects rclone is not
// given: create them at the destination for copies and moves, remove them at
// the source for moves and deletes. Failures are recorded like any other key.
func (m *JobManager) handleMarkers(ctx context.Context, spec JobSpec, markers []string, srcC, dstC *S3, h *jobHandle) {
	record := func(rel string, err error) {
		h.mu.Lock()
		h.addFailedLocked(FailedKey{Key: spec.Src.Prefix + rel, Error: "marker folder: " + firstLine(err.Error()), At: m.now()})
		h.mu.Unlock()
	}
	for _, rel := range markers {
		if ctx.Err() != nil {
			return
		}
		switch spec.Kind {
		case JobSync, JobCopyPrefix, JobMovePrefix:
			if err := dstC.PutObject(ctx, spec.Dst.Bucket, spec.Dst.Prefix+rel, []byte{}, "application/x-directory"); err != nil {
				record(rel, err)
				continue
			}
		}
		switch spec.Kind {
		case JobMovePrefix, JobDeletePrefix:
			if err := srcC.DeleteObject(ctx, spec.Src.Bucket, spec.Src.Prefix+rel); err != nil {
				record(rel, err)
			}
		}
	}
}

// redactAll scrubs secrets from log lines before they are kept anywhere.
func redactAll(lines []string, secrets ...string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		for _, secret := range secrets {
			if secret != "" {
				line = strings.ReplaceAll(line, secret, "[secret]")
			}
		}
		out = append(out, line)
	}
	return out
}

// onRcloneEvent folds rclone's log lines into the handle.
func (h *jobHandle) onRcloneEvent(srcPrefix string, now func() time.Time) func(rcloneEvent) {
	return func(ev rcloneEvent) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if ev.Stats != nil {
			h.live = *ev.Stats
			h.state.Speed = ev.Stats.Speed
			h.state.UpdatedAt = now()
			return
		}
		if ev.Level == "error" && ev.Object != "" {
			if h.chunkFailed == nil {
				h.chunkFailed = map[string]FailedKey{}
			}
			h.chunkFailed[ev.Object] = FailedKey{Key: srcPrefix + ev.Object, Error: ev.Msg, At: now()}
		}
	}
}

// addFailedLocked queues a failed key; it is counted and written to the log
// when its chunk is committed.
func (h *jobHandle) addFailedLocked(f FailedKey) {
	h.pendingFailed = append(h.pendingFailed, f)
}

// appendFailed writes failed keys to <job>/failed.jsonl.
func (m *JobManager) appendFailed(id string, keys []FailedKey) error {
	if len(keys) == 0 {
		return nil
	}
	path := filepath.Join(m.jobDir(id), "failed.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, jobFileMode)
	if err != nil {
		return fmt.Errorf("tidak bisa menulis daftar key gagal: %w", err)
	}
	w := bufio.NewWriter(f)
	for _, k := range keys {
		raw, err := json.Marshal(k)
		if err != nil {
			f.Close()
			return err
		}
		w.Write(raw)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// --- controlling ------------------------------------------------------------

func (m *JobManager) handle(id string) (*jobHandle, error) {
	if err := ValidateJobID(id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.jobs[id]
	if !ok {
		return nil, fmt.Errorf("job %s tidak ada", id)
	}
	return h, nil
}

// Pause asks a running job to stop after the current chunk's rclone run is
// interrupted. The intent is written to disk first.
func (m *JobManager) Pause(id string) error {
	h, err := m.handle(id)
	if err != nil {
		return err
	}
	h.mu.Lock()
	if h.state.Status != JobRunning || h.cancel == nil {
		status := h.state.Status
		h.mu.Unlock()
		return fmt.Errorf("job %s tidak sedang berjalan (%s)", id, status.Label())
	}
	h.state.Status = JobPausing
	cancel := h.cancel
	h.mu.Unlock()
	if err := m.persist(h); err != nil {
		return err
	}
	cancel()
	return nil
}

// Cancel stops a job for good. A paused job is cancelled on the spot.
func (m *JobManager) Cancel(id string) error {
	h, err := m.handle(id)
	if err != nil {
		return err
	}
	h.mu.Lock()
	if h.state.Status.Terminal() {
		status := h.state.Status
		h.mu.Unlock()
		return fmt.Errorf("job %s sudah %s", id, status.Label())
	}
	cancel := h.cancel
	if cancel == nil {
		h.state.Status = JobCancelled
		t := m.now()
		h.state.FinishedAt = &t
	} else {
		h.state.Status = JobCancelling
	}
	h.mu.Unlock()
	if err := m.persist(h); err != nil {
		return err
	}
	if cancel != nil {
		cancel()
	} else {
		log.Printf("job %s: dibatalkan", id)
	}
	return nil
}

// Resume restarts a paused or failed job from its checkpoint.
func (m *JobManager) Resume(id string) error {
	h, err := m.handle(id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h.mu.Lock()
	if h.state.Status != JobPaused && h.state.Status != JobFailed {
		status := h.state.Status
		h.mu.Unlock()
		return fmt.Errorf("job %s tidak bisa dilanjutkan dari status %s", id, status.Label())
	}
	spec := h.state.JobSpec
	h.mu.Unlock()
	if err := m.checkLimitsLocked(spec, id); err != nil {
		return err
	}
	h.mu.Lock()
	h.state.Status = JobRunning
	h.state.PauseReason = ""
	h.state.Error = ""
	h.state.Runs++
	h.mu.Unlock()
	if err := m.persist(h); err != nil {
		return err
	}
	m.startLocked(h)
	log.Printf("job %s: dilanjutkan dari checkpoint", id)
	return nil
}

// Rerun starts a finished job again from the beginning.
func (m *JobManager) Rerun(id string) error {
	h, err := m.handle(id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h.mu.Lock()
	if !h.state.Status.Terminal() {
		status := h.state.Status
		h.mu.Unlock()
		return fmt.Errorf("job %s masih %s", id, status.Label())
	}
	spec := h.state.JobSpec
	h.mu.Unlock()
	if err := m.checkLimitsLocked(spec, id); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(m.jobDir(id), "failed.jsonl")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	h.mu.Lock()
	h.state.Status = JobRunning
	h.state.Checkpoint = ""
	h.state.Counters = JobCounters{}
	h.state.Speed = 0
	h.state.PauseReason = ""
	h.state.Error = ""
	h.state.LastLog = nil
	h.state.Runs++
	h.state.StartedAt = m.now()
	h.state.FinishedAt = nil
	h.mu.Unlock()
	if err := m.persist(h); err != nil {
		return err
	}
	m.startLocked(h)
	log.Printf("job %s: dijalankan ulang dari awal", id)
	return nil
}

// Delete removes a finished job and its files.
func (m *JobManager) Delete(id string) error {
	h, err := m.handle(id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h.mu.Lock()
	terminal := h.state.Status.Terminal()
	status := h.state.Status
	h.mu.Unlock()
	if !terminal {
		return fmt.Errorf("job %s masih %s; batalkan dulu", id, status.Label())
	}
	if err := os.RemoveAll(m.jobDir(id)); err != nil {
		return err
	}
	if err := os.Remove(m.jobPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(m.jobs, id)
	return nil
}

// List returns every job, newest first, with in-progress counters folded in.
func (m *JobManager) List() []JobState {
	m.mu.Lock()
	handles := make([]*jobHandle, 0, len(m.jobs))
	for _, h := range m.jobs {
		handles = append(handles, h)
	}
	m.mu.Unlock()
	out := make([]JobState, 0, len(handles))
	for _, h := range handles {
		out = append(out, h.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Get returns one job.
func (m *JobManager) Get(id string) (JobState, bool) {
	h, err := m.handle(id)
	if err != nil {
		return JobState{}, false
	}
	return h.snapshot(), true
}

// UsesRemote reports whether an unfinished job refers to the remote.
func (m *JobManager) UsesRemote(name string) bool {
	for _, s := range m.List() {
		if !s.Status.Terminal() && (s.Src.Remote == name || s.Dst.Remote == name) {
			return true
		}
	}
	return false
}

// FailedKeys returns the last `tail` failed keys and the total count.
func (m *JobManager) FailedKeys(id string, tail int) ([]FailedKey, int, error) {
	if _, err := m.handle(id); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(filepath.Join(m.jobDir(id), "failed.jsonl"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()
	var ring []FailedKey
	total := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var k FailedKey
		if err := json.Unmarshal(scanner.Bytes(), &k); err != nil {
			continue
		}
		total++
		ring = append(ring, k)
		if tail > 0 && len(ring) > tail {
			ring = ring[1:]
		}
	}
	return ring, total, scanner.Err()
}

// FailedKeysPath is the file a download handler streams.
func (m *JobManager) FailedKeysPath(id string) (string, error) {
	if _, err := m.handle(id); err != nil {
		return "", err
	}
	return filepath.Join(m.jobDir(id), "failed.jsonl"), nil
}

// Running counts jobs with a live runner.
func (m *JobManager) Running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, h := range m.jobs {
		h.mu.Lock()
		if h.cancel != nil {
			n++
		}
		h.mu.Unlock()
	}
	return n
}

// --- start-up and shutdown --------------------------------------------------

// ResumeInterrupted restarts the jobs that were running when the panel last
// stopped, and settles the ones caught mid-pause or mid-cancel.
func (m *JobManager) ResumeInterrupted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.jobs))
	for id := range m.jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	running := 0
	for _, id := range ids {
		h := m.jobs[id]
		h.mu.Lock()
		status := h.state.Status
		spec := h.state.JobSpec
		h.mu.Unlock()
		switch status {
		case JobRunning:
			if running >= m.maxRunning {
				h.mu.Lock()
				h.state.Status = JobPaused
				h.state.PauseReason = fmt.Sprintf("dijeda saat panel start: sudah %d job berjalan (batas %d). Lanjutkan manual.", running, m.maxRunning)
				h.mu.Unlock()
				_ = m.persist(h)
				continue
			}
			if err := m.checkLimitsLocked(spec, id); err != nil {
				h.mu.Lock()
				h.state.Status = JobPaused
				h.state.PauseReason = "dijeda saat panel start: " + err.Error()
				h.mu.Unlock()
				_ = m.persist(h)
				continue
			}
			running++
			m.startLocked(h)
			log.Printf("job %s: dilanjutkan setelah panel start (checkpoint %q)", id, h.snapshot().Checkpoint)
		case JobPausing:
			h.mu.Lock()
			h.state.Status = JobPaused
			h.mu.Unlock()
			_ = m.persist(h)
		case JobCancelling:
			h.mu.Lock()
			h.state.Status = JobCancelled
			t := m.now()
			h.state.FinishedAt = &t
			h.mu.Unlock()
			_ = m.persist(h)
		}
	}
}

// Shutdown interrupts every running job without changing its status, so it
// resumes after restart, and waits for the runners to save their state.
func (m *JobManager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	for _, h := range m.jobs {
		h.mu.Lock()
		if h.cancel != nil {
			h.cancel()
		}
		h.mu.Unlock()
	}
	m.mu.Unlock()

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		log.Printf("PERINGATAN: job belum selesai menyimpan state saat batas waktu shutdown habis")
	}
}
