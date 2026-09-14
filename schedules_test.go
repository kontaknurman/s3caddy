package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestScheduler(t *testing.T, e *jobTestEnv) (*Scheduler, *time.Time) {
	t.Helper()
	sched, err := NewScheduler(filepath.Join(e.stateDir, schedulesFileName), e.mgr)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	sched.now = func() time.Time { return clock }
	e.mgr.now = func() time.Time { return clock }
	return sched, &clock
}

func TestParseEveryAndHuman(t *testing.T) {
	cases := map[string]time.Duration{"6h": 6 * time.Hour, "2d": 48 * time.Hour, "30m": 30 * time.Minute, "12": 12 * time.Hour, " 90m ": 90 * time.Minute}
	for in, want := range cases {
		got, err := ParseEvery(in)
		if err != nil || got != want {
			t.Errorf("ParseEvery(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0h", "5m", "31d", "abc", "1h30m", "-1h"} {
		if _, err := ParseEvery(bad); err == nil {
			t.Errorf("ParseEvery(%q) accepted", bad)
		}
	}
	for d, want := range map[Duration]string{Duration(6 * time.Hour): "setiap 6 jam", Duration(48 * time.Hour): "setiap 2 hari", Duration(30 * time.Minute): "setiap 30 menit", Duration(90 * time.Minute): "setiap 90 menit"} {
		if got := d.Human(); got != want {
			t.Errorf("%v.Human() = %q, want %q", time.Duration(d), got, want)
		}
	}
}

func TestScheduleFiresWhenDueAndSkipsBusyBuckets(t *testing.T) {
	e := newJobTestEnv(t)
	sched, clock := newTestScheduler(t, e)
	e.remote.put("backup", "in/a.txt", []byte("x"), "text/plain")

	item, err := sched.Add(e.importSpec("skip-existing"), 6*time.Hour, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !item.Enabled || !item.NextRun.Equal(clock.Add(6*time.Hour)) {
		t.Errorf("new schedule = %+v", item)
	}
	if info, _ := os.Stat(filepath.Join(e.stateDir, schedulesFileName)); info == nil || info.Mode().Perm() != 0o600 {
		t.Error("schedules.json missing or not private")
	}

	// Not due yet: nothing happens.
	sched.Tick(context.Background())
	if len(e.mgr.List()) != 0 {
		t.Fatal("fired before it was due")
	}

	// Due: a job is created and NextRun moves one interval on.
	*clock = clock.Add(6 * time.Hour)
	sched.Tick(context.Background())
	jobs := e.mgr.List()
	if len(jobs) != 1 || jobs[0].Src.Remote != "wasabi" || jobs[0].Mode != "skip-existing" {
		t.Fatalf("jobs after tick = %+v", jobs)
	}
	got, _ := sched.Get(item.ID)
	if !got.NextRun.Equal(clock.Add(6*time.Hour)) || len(got.Runs) != 1 || !got.Runs[0].OK || got.Runs[0].JobID != jobs[0].ID {
		t.Errorf("after firing: next=%v runs=%+v", got.NextRun, got.Runs)
	}
	waitJob(t, e.mgr, jobs[0].ID, terminal)

	// Down for a day: one run, and NextRun lands in the future, not four
	// missed slots ago.
	*clock = clock.Add(30 * time.Hour)
	sched.Tick(context.Background())
	got, _ = sched.Get(item.ID)
	if len(got.Runs) != 2 {
		t.Errorf("runs after a long gap = %d, want 2", len(got.Runs))
	}
	if !got.NextRun.After(*clock) || got.NextRun.Sub(*clock) > 6*time.Hour {
		t.Errorf("NextRun after gap = %v (now %v)", got.NextRun, *clock)
	}
	waitJob(t, e.mgr, got.Runs[0].JobID, terminal)

	// Bucket busy: the run is recorded as skipped and NextRun still moves.
	e.knob(t, "sleep", "0.5")
	e.remote.put("backup", "in/b.txt", []byte("x"), "text/plain")
	*clock = got.NextRun
	sched.Tick(context.Background()) // starts a job that holds the bucket
	*clock = clock.Add(6 * time.Hour)
	sched.Tick(context.Background()) // same buckets still busy
	got, _ = sched.Get(item.ID)
	if len(got.Runs) != 4 || got.Runs[0].OK || !strings.Contains(got.Runs[0].Result, "sedang dipakai job") {
		t.Errorf("busy run = %+v", got.Runs)
	}
	e.knob(t, "sleep", "0")

	// Disabled schedules never fire; re-enabling reschedules from now.
	if err := sched.SetEnabled(item.ID, false); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(48 * time.Hour)
	before := len(e.mgr.List())
	sched.Tick(context.Background())
	if len(e.mgr.List()) != before {
		t.Error("a disabled schedule fired")
	}
	if err := sched.SetEnabled(item.ID, true); err != nil {
		t.Fatal(err)
	}
	got, _ = sched.Get(item.ID)
	if !got.NextRun.After(*clock) {
		t.Errorf("re-enabled schedule should be rescheduled from now: %v", got.NextRun)
	}

	// Persisted across a restart.
	reloaded, err := NewScheduler(sched.path, e.mgr)
	if err != nil {
		t.Fatal(err)
	}
	if again, ok := reloaded.Get(item.ID); !ok || len(again.Runs) != 4 || !again.Enabled {
		t.Errorf("reloaded = %+v", again)
	}
	if !reloaded.UsesRemote("wasabi") || reloaded.UsesRemote("ghost") {
		t.Error("UsesRemote wrong")
	}
	if err := reloaded.Delete(item.ID); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.List()) != 0 {
		t.Error("schedule not deleted")
	}
}

func TestScheduleRunNowAndValidation(t *testing.T) {
	e := newJobTestEnv(t)
	sched, _ := newTestScheduler(t, e)
	e.remote.put("backup", "in/a.txt", []byte("x"), "text/plain")

	if _, err := sched.Add(e.importSpec("skip-existing"), 5*time.Minute, time.Time{}); err == nil {
		t.Error("interval below the minimum accepted")
	}
	bad := e.importSpec("skip-existing")
	bad.Src.Remote = "ghost"
	if _, err := sched.Add(bad, time.Hour, time.Time{}); err == nil {
		t.Error("unknown remote accepted")
	}
	prefix := JobSpec{Kind: JobDeletePrefix, Transfers: 1, Src: Endpoint{Bucket: "arsip", Prefix: "x/"}}
	if _, err := sched.Add(prefix, time.Hour, time.Time{}); err == nil {
		t.Error("only sync jobs can be scheduled")
	}

	first := time.Date(2026, 9, 15, 3, 0, 0, 0, time.UTC)
	item, err := sched.Add(e.importSpec("skip-existing"), 24*time.Hour, first)
	if err != nil {
		t.Fatal(err)
	}
	if !item.NextRun.Equal(first) {
		t.Errorf("first run = %v, want %v", item.NextRun, first)
	}
	run, err := sched.RunNow(context.Background(), item.ID)
	if err != nil || !run.OK || run.JobID == "" {
		t.Fatalf("RunNow: run=%+v err=%v", run, err)
	}
	got, _ := sched.Get(item.ID)
	if !got.NextRun.Equal(first) {
		t.Error("RunNow must not move the regular slot")
	}
	waitJob(t, e.mgr, run.JobID, terminal)
	if _, err := sched.RunNow(context.Background(), "0123456789abcdef"); err == nil {
		t.Error("unknown schedule accepted")
	}
	if err := sched.Delete("../x"); err == nil {
		t.Error("bad id accepted")
	}
}
