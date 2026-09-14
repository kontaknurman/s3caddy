package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// A schedule starts the same sync job again every so often — "every 6 hours"
// or "every day at 03:00". With the skip-existing mode that is a standing
// mirror: each run only copies what appeared since the last one. Schedules
// live in STATE_DIR/schedules.json; they carry a remote's name, never its
// credentials.

const (
	schedulesFileName = "schedules.json"
	schedulesFileMode = 0o600
	// scheduleMinEvery keeps a schedule from re-listing a huge bucket every
	// few minutes; scheduleMaxEvery keeps the arithmetic sane.
	scheduleMinEvery = 15 * time.Minute
	scheduleMaxEvery = 30 * 24 * time.Hour
	// schedulerTick is how often due schedules are looked for.
	schedulerTick = 30 * time.Second
	// scheduleMaxRuns bounds the run history kept per schedule.
	scheduleMaxRuns = 20
)

// Schedule is one recurring job.
type Schedule struct {
	ID        string        `json:"id"`
	Spec      JobSpec       `json:"spec"` // ID and CreatedAt are ignored
	Every     Duration      `json:"every"`
	Enabled   bool          `json:"enabled"`
	NextRun   time.Time     `json:"nextRun"`
	CreatedAt time.Time     `json:"createdAt"`
	Runs      []ScheduleRun `json:"runs,omitempty"`
}

// ScheduleRun is one firing, newest first in Schedule.Runs.
type ScheduleRun struct {
	At     time.Time `json:"at"`
	JobID  string    `json:"jobId,omitempty"`
	Result string    `json:"result"` // "job dimulai" or why not
	OK     bool      `json:"ok"`
}

// Duration marshals as seconds so the JSON stays readable.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(time.Duration(d) / time.Second))
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var secs int64
	if err := json.Unmarshal(b, &secs); err != nil {
		return err
	}
	*d = Duration(time.Duration(secs) * time.Second)
	return nil
}

// Human renders "setiap 6 jam" / "setiap 2 hari" / "setiap 30 menit".
func (d Duration) Human() string {
	v := time.Duration(d)
	switch {
	case v >= 24*time.Hour && v%(24*time.Hour) == 0:
		return fmt.Sprintf("setiap %d hari", v/(24*time.Hour))
	case v >= time.Hour && v%time.Hour == 0:
		return fmt.Sprintf("setiap %d jam", v/time.Hour)
	default:
		return fmt.Sprintf("setiap %d menit", v/time.Minute)
	}
}

// ValidateEvery checks an interval.
func ValidateEvery(d time.Duration) error {
	if d < scheduleMinEvery {
		return fmt.Errorf("interval minimal %d menit", int(scheduleMinEvery.Minutes()))
	}
	if d > scheduleMaxEvery {
		return fmt.Errorf("interval maksimal %d hari", int(scheduleMaxEvery.Hours()/24))
	}
	return nil
}

// ParseEvery reads "6h", "2d", "30m", "90m", or a plain number of hours.
func ParseEvery(s string) (time.Duration, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, errors.New("interval tidak boleh kosong")
	}
	unit := time.Hour
	switch {
	case strings.HasSuffix(s, "m"):
		unit = time.Minute
		s = strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "h"):
		s = strings.TrimSuffix(s, "h")
	case strings.HasSuffix(s, "d"):
		unit = 24 * time.Hour
		s = strings.TrimSuffix(s, "d")
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("interval %q tidak dikenal; contoh: 30m, 6h, 1d", s)
		}
		n = n*10 + int64(c-'0')
		if n > 1_000_000 {
			return 0, errors.New("interval terlalu besar")
		}
	}
	if n == 0 {
		return 0, errors.New("interval harus lebih dari nol")
	}
	d := time.Duration(n) * unit
	if err := ValidateEvery(d); err != nil {
		return 0, err
	}
	return d, nil
}

// Scheduler owns the schedules and fires them.
type Scheduler struct {
	path string
	jobs *JobManager
	now  func() time.Time

	mu    sync.Mutex
	items []Schedule

	stop chan struct{}
	done chan struct{}
}

type schedulesFile struct {
	Schedules []Schedule `json:"schedules"`
}

// NewScheduler loads STATE_DIR/schedules.json. Nothing fires until Start.
func NewScheduler(path string, jobs *JobManager) (*Scheduler, error) {
	s := &Scheduler{path: path, jobs: jobs, now: time.Now}
	data, exists, err := readIfExists(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return s, nil
	}
	var doc schedulesFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s rusak: %w", path, err)
	}
	for _, it := range doc.Schedules {
		if err := ValidateJobID(it.ID); err != nil || ValidateEvery(time.Duration(it.Every)) != nil {
			return nil, fmt.Errorf("%s: jadwal %q tidak valid", path, it.ID)
		}
	}
	s.items = doc.Schedules
	return s, nil
}

// List returns the schedules, newest first.
func (s *Scheduler) List() []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Schedule, len(s.items))
	copy(out, s.items)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Get returns one schedule.
func (s *Scheduler) Get(id string) (Schedule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		if it.ID == id {
			return it, true
		}
	}
	return Schedule{}, false
}

// Add validates the job spec the way a one-off job is validated (without
// probing — the probe happens at every run) and saves the schedule. The
// first run is at firstRun, or one interval from now when zero.
func (s *Scheduler) Add(spec JobSpec, every time.Duration, firstRun time.Time) (Schedule, error) {
	if err := ValidateEvery(every); err != nil {
		return Schedule{}, err
	}
	if spec.Kind != JobSync {
		return Schedule{}, errors.New("hanya job sync (impor/ekspor) yang bisa dijadwalkan")
	}
	spec.ID = ""
	spec.CreatedAt = time.Time{}
	if err := s.jobs.validateSpec(spec); err != nil {
		return Schedule{}, err
	}
	now := s.now()
	if firstRun.IsZero() {
		firstRun = now.Add(every)
	}
	item := Schedule{
		ID:        randomHex(8),
		Spec:      spec,
		Every:     Duration(every),
		Enabled:   true,
		NextRun:   firstRun.UTC().Truncate(time.Minute),
		CreatedAt: now.UTC().Truncate(time.Second),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	updated := append(append([]Schedule{}, s.items...), item)
	if err := s.saveLocked(updated); err != nil {
		return Schedule{}, err
	}
	s.items = updated
	log.Printf("jadwal %s: %s %s → %s, %s, pertama %s", item.ID, spec.Kind, spec.Src, spec.Dst, item.Every.Human(), item.NextRun.Format(time.RFC3339))
	return item, nil
}

// SetEnabled pauses or resumes a schedule. Resuming reschedules from now.
func (s *Scheduler) SetEnabled(id string, enabled bool) error {
	return s.update(id, func(it *Schedule) error {
		it.Enabled = enabled
		if enabled && it.NextRun.Before(s.now()) {
			it.NextRun = s.now().Add(time.Duration(it.Every)).UTC().Truncate(time.Minute)
		}
		return nil
	})
}

// Delete removes a schedule. Jobs it started are untouched.
func (s *Scheduler) Delete(id string) error {
	if err := ValidateJobID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []Schedule
	found := false
	for _, it := range s.items {
		if it.ID == id {
			found = true
			continue
		}
		kept = append(kept, it)
	}
	if !found {
		return fmt.Errorf("jadwal %s tidak ada", id)
	}
	if err := s.saveLocked(kept); err != nil {
		return err
	}
	s.items = kept
	return nil
}

// UsesRemote reports whether a schedule refers to the remote.
func (s *Scheduler) UsesRemote(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		if it.Spec.Src.Remote == name || it.Spec.Dst.Remote == name {
			return true
		}
	}
	return false
}

// RunNow fires a schedule immediately; the next regular run keeps its slot.
func (s *Scheduler) RunNow(ctx context.Context, id string) (ScheduleRun, error) {
	it, ok := s.Get(id)
	if !ok {
		return ScheduleRun{}, fmt.Errorf("jadwal %s tidak ada", id)
	}
	run := s.fire(ctx, it)
	err := s.update(id, func(it *Schedule) error {
		it.Runs = prependRun(it.Runs, run)
		return nil
	})
	return run, err
}

// fire starts the job and reports what happened. A busy bucket or a probe
// failure is reported, not retried: the next tick will try again.
func (s *Scheduler) fire(ctx context.Context, it Schedule) ScheduleRun {
	run := ScheduleRun{At: s.now().UTC().Truncate(time.Second)}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	state, err := s.jobs.Create(ctx, it.Spec)
	if err != nil {
		run.Result = "dilewati: " + firstLine(err.Error())
		return run
	}
	run.OK = true
	run.JobID = state.ID
	run.Result = "job " + state.ID + " dimulai"
	return run
}

// update edits one schedule under the lock and persists.
func (s *Scheduler) update(id string, fn func(*Schedule) error) error {
	if err := ValidateJobID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	updated := append([]Schedule{}, s.items...)
	for i := range updated {
		if updated[i].ID != id {
			continue
		}
		updated[i].Runs = append([]ScheduleRun(nil), updated[i].Runs...)
		if err := fn(&updated[i]); err != nil {
			return err
		}
		if err := s.saveLocked(updated); err != nil {
			return err
		}
		s.items = updated
		return nil
	}
	return fmt.Errorf("jadwal %s tidak ada", id)
}

func prependRun(runs []ScheduleRun, run ScheduleRun) []ScheduleRun {
	out := append([]ScheduleRun{run}, runs...)
	if len(out) > scheduleMaxRuns {
		out = out[:scheduleMaxRuns]
	}
	return out
}

func (s *Scheduler) saveLocked(items []Schedule) error {
	if items == nil {
		items = []Schedule{}
	}
	data, err := json.MarshalIndent(schedulesFile{Schedules: items}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.path, append(data, '\n'), schedulesFileMode); err != nil {
		return err
	}
	return os.Chmod(s.path, schedulesFileMode)
}

// Tick fires every enabled schedule whose time has come, then moves its
// NextRun forward by whole intervals past now (so a panel that was down for
// a day does not fire four missed 6-hourly runs back to back).
func (s *Scheduler) Tick(ctx context.Context) {
	now := s.now()
	var due []Schedule
	s.mu.Lock()
	for _, it := range s.items {
		if it.Enabled && !it.NextRun.After(now) {
			due = append(due, it)
		}
	}
	s.mu.Unlock()

	for _, it := range due {
		run := s.fire(ctx, it)
		if !run.OK {
			log.Printf("jadwal %s: %s", it.ID, run.Result)
		}
		_ = s.update(it.ID, func(it *Schedule) error {
			it.Runs = prependRun(it.Runs, run)
			every := time.Duration(it.Every)
			next := it.NextRun
			for !next.After(now) {
				next = next.Add(every)
			}
			it.NextRun = next
			return nil
		})
	}
}

// Start runs the ticker until Stop.
func (s *Scheduler) Start() {
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(schedulerTick)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				s.Tick(context.Background())
			}
		}
	}()
}

// Stop ends the ticker; a firing in progress finishes first.
func (s *Scheduler) Stop() {
	if s.stop == nil {
		return
	}
	close(s.stop)
	<-s.done
	s.stop = nil
}
