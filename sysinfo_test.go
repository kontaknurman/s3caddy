package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A fake /proc + /sys + /etc tree. Numbers are chosen so the rates between
// the two samples come out round.
const (
	fixStatA = "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 50 0 50 400 0 0 0 0 0 0\ncpu1 50 0 50 400 0 0 0 0 0 0\nintr 0 0\nctxt 1\n"
	fixStatB = "cpu  200 0 200 1000 50 0 0 50 0 0\ncpu0 150 0 150 500 0 0 0 0 0 0\ncpu1 50 0 50 600 0 0 0 0 0 0\nintr 0 0\nctxt 2\n"

	fixMeminfo = "MemTotal:       16000000 kB\nMemFree:         1000000 kB\nMemAvailable:    8000000 kB\nBuffers:          500000 kB\nCached:          3000000 kB\nSwapCached:            0 kB\nShmem:            100000 kB\nSReclaimable:     200000 kB\nSwapTotal:       2000000 kB\nSwapFree:        1500000 kB\n"

	fixNetDevA = "Inter-|   Receive                                                |  Transmit\n face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n  eth0:    1000      10    1    2    0     0          0         0     5000      50    0    3    0     0       0          0\n    lo:   66425      10    0    0    0     0          0         0    66425      10    0    0    0     0       0          0\n"
	fixNetDevB = "Inter-|   Receive                                                |  Transmit\n face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n  eth0:    6000      20    1    2    0     0          0         0    15000      80    0    3    0     0       0          0\n    lo:   66425      10    0    0    0     0          0         0    66425      10    0    0    0     0       0          0\n"

	fixDiskA = "   8       0 sda 100 0 2000 50 200 0 4000 80 0 1000 130 0 0 0 0 0 0\n   8       1 sda1 100 0 2000 50 200 0 4000 80 0 1000 130 0 0 0 0 0 0\n   7       0 loop0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
	fixDiskB = "   8       0 sda 200 0 4000 90 300 0 6000 120 0 2500 210 0 0 0 0 0 0\n   8       1 sda1 200 0 4000 90 300 0 6000 120 0 2500 210 0 0 0 0 0 0\n   7       0 loop0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n"

	fixMounts = "/dev/sda1 / ext4 rw,relatime 0 0\n/dev/sda1 /mnt/bind ext4 rw 0 0\ntank/garage /var/lib/garage\\040data zfs rw,xattr 0 0\ntmpfs /run tmpfs rw 0 0\n/dev/loop3 /snap/core/1 squashfs ro 0 0\nproc /proc proc rw 0 0\n"

	// utime+stime 150 → 300 ticks: 1.5 s of CPU over the 5 s interval = 30%.
	fixGarageStatA = "1234 (garage) S 1 1234 1234 0 -1 4194560 100 0 0 0 100 50 0 0 20 0 12 0 300000 800000000 51200 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 1 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
	fixGarageStatB = "1234 (garage) S 1 1234 1234 0 -1 4194560 100 0 0 0 200 100 0 0 20 0 12 0 300000 800000000 51200 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 1 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
	fixCaddyStat   = "2345 (caddy) S 1 2345 2345 0 -1 4194560 100 0 0 0 10 5 0 0 20 0 8 0 350000 500000000 12800 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 1 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
	fixInitStat    = "1 (systemd) S 0 1 1 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 1 0 1 100000000 2560 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 1 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
)

// writeFixture (re)writes the given files under root.
func writeFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// newFixtureMonitor builds a monitor over a fake tree with the "A" sample
// written. The clock starts at t0 and only moves when the test says so.
func newFixtureMonitor(t *testing.T) (*sysMonitor, string, *time.Time, *int) {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, root, map[string]string{
		"proc/stat":                            fixStatA,
		"proc/meminfo":                         fixMeminfo,
		"proc/loadavg":                         "1.50 0.80 0.40 2/345 9999\n",
		"proc/uptime":                          "3600.00 7000.00\n",
		"proc/cpuinfo":                         "processor\t: 0\nmodel name\t: Fake CPU @ 3.0GHz\nprocessor\t: 1\nmodel name\t: Fake CPU @ 3.0GHz\n",
		"proc/sys/kernel/osrelease":            "6.8.0-fake\n",
		"proc/mounts":                          fixMounts,
		"proc/diskstats":                       fixDiskA,
		"proc/net/dev":                         fixNetDevA,
		"proc/pressure/cpu":                    "some avg10=1.50 avg60=0.50 avg300=0.10 total=1\n",
		"proc/pressure/memory":                 "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
		"proc/1/comm":                          "systemd\n",
		"proc/1/stat":                          fixInitStat,
		"proc/1234/comm":                       "garage\n",
		"proc/1234/stat":                       fixGarageStatA,
		"proc/1234/status":                     "Name:\tgarage\nState:\tS (sleeping)\nVmRSS:\t  204800 kB\nThreads:\t12\n",
		"proc/2345/comm":                       "caddy\n",
		"proc/2345/stat":                       fixCaddyStat,
		"proc/2345/status":                     "VmRSS:\t   51200 kB\n",
		"proc/999/comm":                        "bash\n",
		"proc/999/stat":                        "999 (bash) S 1 999 999 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n",
		"etc/os-release":                       "NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04 LTS\"\n",
		"sys/block/sda/size":                   "1000000\n",
		"sys/class/hwmon/hwmon0/name":          "coretemp\n",
		"sys/class/hwmon/hwmon0/temp1_input":   "45000\n",
		"sys/class/hwmon/hwmon0/temp1_label":   "Package id 0\n",
		"sys/class/hwmon/hwmon0/temp2_input":   "42000\n",
		"sys/class/hwmon/hwmon0/temp3_input":   "-1\n", // disconnected sensor
		"sys/class/hwmon/hwmon0/pwm1":          "0\n",
		"sys/class/thermal/thermal_zone0/type": "x86_pkg_temp\n",
		"sys/class/thermal/thermal_zone0/temp": "44000\n",
	})
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	sleeps := 0
	m := &sysMonitor{
		procRoot: filepath.Join(root, "proc"),
		sysRoot:  filepath.Join(root, "sys"),
		etcRoot:  filepath.Join(root, "etc"),
		now:      func() time.Time { return now },
		sleep:    func(time.Duration) { sleeps++ },
		selfPID:  2345, // pretend caddy is us, to exercise the Self flag
		statfs: func(path string) (fsUsage, error) {
			switch path {
			case "/":
				return fsUsage{Total: 100 << 30, Free: 40 << 30, Avail: 35 << 30, Inodes: 1000, InodesFree: 900}, nil
			case "/var/lib/garage data":
				return fsUsage{Total: 4 << 40, Free: 1 << 40, Avail: 1 << 40}, nil // zfs: no inode count
			}
			return fsUsage{}, os.ErrNotExist
		},
	}
	return m, root, &now, &sleeps
}

func TestSysMonitorSnapshotRatesBetweenSamples(t *testing.T) {
	m, root, now, sleeps := newFixtureMonitor(t)

	// First call: no baseline, so a short second sample is taken.
	st := m.Snapshot()
	if *sleeps != 1 {
		t.Errorf("first snapshot should take a fallback sample, slept %d times", *sleeps)
	}
	if st.Host.OS != "Ubuntu 24.04 LTS" || st.Host.Kernel != "6.8.0-fake" || st.Host.CPUModel != "Fake CPU @ 3.0GHz" || st.Host.Cores != 2 {
		t.Errorf("host = %+v", st.Host)
	}
	if st.Host.Uptime != time.Hour || !st.Host.BootedAt.Equal(now.Add(-time.Hour)) {
		t.Errorf("uptime = %v booted %v", st.Host.Uptime, st.Host.BootedAt)
	}
	m2 := st.Memory
	if m2.Total != 16000000*1024 || m2.Used != 8000000*1024 || m2.Pct != 50 || m2.Cached != 3200000*1024 || m2.SwapUsed != 500000*1024 || m2.SwapPct != 25 {
		t.Errorf("memory = %+v", m2)
	}
	if st.CPU.Load1 != 1.5 || st.CPU.Running != 2 || st.CPU.Tasks != 345 {
		t.Errorf("load = %+v", st.CPU)
	}
	if len(st.Disks) != 2 || st.Disks[0].Mount != "/" || st.Disks[1].Mount != "/var/lib/garage data" {
		t.Fatalf("disks = %+v", st.Disks)
	}
	if d := st.Disks[0]; d.Used != 60<<30 || d.Avail != 35<<30 || int(d.Pct) != 63 || int(d.InodesPct) != 10 || d.FSType != "ext4" {
		t.Errorf("root disk = %+v", d)
	}
	if d := st.Disks[1]; d.InodesPct != -1 || int(d.Pct) != 75 || d.Device != "tank/garage" {
		t.Errorf("zfs disk = %+v", d)
	}
	if len(st.Temps) != 2 || st.Temps[0].Label != "coretemp 2" || st.Temps[0].Celsius != 42 || st.Temps[1].Label != "coretemp Package id 0" || st.Temps[1].Celsius != 45 {
		t.Errorf("temps = %+v", st.Temps)
	}
	if len(st.Pressure) != 2 || st.Pressure[0].Resource != "cpu" || st.Pressure[0].SomeAvg10 != 1.5 || st.Pressure[0].SomeAvg60 != 0.5 {
		t.Errorf("pressure = %+v", st.Pressure)
	}
	if len(st.Procs) != 2 || st.Procs[0].Name != "garage" || st.Procs[0].PID != 1234 || st.Procs[1].Name != "caddy" || !st.Procs[1].Self {
		t.Fatalf("procs = %+v", st.Procs)
	}
	if p := st.Procs[0]; p.RSS != 204800*1024 || p.Threads != 12 || p.State != "tidur" || !p.StartedAt.Equal(st.Host.BootedAt.Add(3000*time.Second)) {
		t.Errorf("garage proc = %+v", p)
	}
	if len(st.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", st.Warnings)
	}

	// Second call 5 s later against the "B" counters: rates appear.
	writeFixture(t, root, map[string]string{"proc/stat": fixStatB, "proc/diskstats": fixDiskB, "proc/net/dev": fixNetDevB, "proc/1234/stat": fixGarageStatB, "proc/uptime": "3605.00 7010.00\n"})
	*now = now.Add(5 * time.Second)
	st = m.Snapshot()
	if *sleeps != 1 {
		t.Errorf("a recent baseline must be reused, slept %d times", *sleeps)
	}
	if st.Interval != 5*time.Second {
		t.Errorf("interval = %v", st.Interval)
	}
	c := st.CPU
	if c.Pct != 50 || c.User != 20 || c.System != 20 || c.IOWait != 10 || c.Steal != 10 || c.Idle != 40 {
		t.Errorf("cpu = %+v", c)
	}
	if len(c.PerCore) != 2 || int(c.PerCore[0]*10) != 666 || c.PerCore[1] != 0 {
		t.Errorf("per core = %v", c.PerCore)
	}
	if len(st.DiskIO) != 1 || st.DiskIO[0].Name != "sda" {
		t.Fatalf("disk io = %+v", st.DiskIO)
	}
	if io := st.DiskIO[0]; io.ReadBps != 204800 || io.WriteBps != 204800 || io.ReadIOPS != 20 || io.WriteIOPS != 20 || io.UtilPct != 30 {
		t.Errorf("sda io = %+v", io)
	}
	if len(st.Net) != 1 || st.Net[0].Name != "eth0" {
		t.Fatalf("net = %+v", st.Net)
	}
	if n := st.Net[0]; n.RxBps != 1000 || n.TxBps != 2000 || n.RxTotal != 6000 || n.RxErrors != 1 || n.TxDropped != 3 {
		t.Errorf("eth0 = %+v", n)
	}
	if p := st.Procs[0]; p.Name != "garage" || p.CPUPct != 30 {
		t.Errorf("garage cpu = %+v", p)
	}
	if p := st.Procs[1]; p.CPUPct != 0 {
		t.Errorf("caddy (unchanged) cpu = %+v", p)
	}

	// A stale baseline (page closed for a while) is not used for rates.
	*now = now.Add(10 * time.Minute)
	st = m.Snapshot()
	if *sleeps != 2 || st.Interval != 0 {
		t.Errorf("stale baseline: slept %d, interval %v", *sleeps, st.Interval)
	}
}

func TestSysMonitorReportsWhatItCannotRead(t *testing.T) {
	m, root, now, _ := newFixtureMonitor(t)
	if err := os.RemoveAll(filepath.Join(root, "proc", "1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "proc", "stat")); err != nil {
		t.Fatal(err)
	}
	st := m.Snapshot()
	joined := strings.Join(st.Warnings, "\n")
	if !strings.Contains(joined, "hidepid") || !strings.Contains(joined, "/proc/stat tidak bisa dibaca") {
		t.Errorf("warnings = %v", st.Warnings)
	}
	if st.CPU.Pct != 0 || len(st.CPU.PerCore) != 0 {
		t.Errorf("cpu without /proc/stat = %+v", st.CPU)
	}
	// Everything else is still there.
	if st.Memory.Pct != 50 || len(st.Disks) != 2 || len(st.Procs) != 2 {
		t.Errorf("partial snapshot lost data: mem %+v disks %d procs %d", st.Memory, len(st.Disks), len(st.Procs))
	}
	// A mount whose statfs hangs is reported, not waited for, and is then
	// left alone for a while instead of parking one goroutine per refresh.
	// The abandoned statfs goroutine outlives the call, so the counter it
	// bumps must be atomic for the race detector's sake.
	slow := m.statfs
	var rootCalls atomic.Int32
	m.statfs = func(path string) (fsUsage, error) {
		if path == "/" {
			rootCalls.Add(1)
			time.Sleep(statfsTimeout + 500*time.Millisecond)
		}
		return slow(path)
	}
	*now = now.Add(5 * time.Second)
	start := time.Now()
	st = m.Snapshot()
	if time.Since(start) > statfsTimeout+time.Second {
		t.Errorf("snapshot waited %v for a stuck mount", time.Since(start))
	}
	if len(st.Disks) != 1 || !strings.Contains(strings.Join(st.Warnings, "\n"), "statfs /: tidak menjawab") {
		t.Errorf("stuck mount: disks %+v warnings %v", st.Disks, st.Warnings)
	}
	*now = now.Add(5 * time.Second)
	st = m.Snapshot()
	if rootCalls.Load() != 1 || !strings.Contains(strings.Join(st.Warnings, "\n"), "dicoba lagi") {
		t.Errorf("stuck mount retried too soon: %d calls, warnings %v", rootCalls.Load(), st.Warnings)
	}
	*now = now.Add(stuckMountRetry)
	m.statfs = slow
	st = m.Snapshot()
	if len(st.Disks) != 2 {
		t.Errorf("mount not retried after %v: disks %+v warnings %v", stuckMountRetry, st.Disks, st.Warnings)
	}
}

func TestSysinfoParsers(t *testing.T) {
	mounts := parseMounts("/dev/sda1 / ext4 rw 0 0\n/dev/sda1 /home ext4 rw 0 0\n/dev/nvme0n1p1 /mnt/with\\040space\\134x xfs rw 0 0\nnone /proc proc rw 0 0\nsysfs /sys sysfs rw 0 0\n/dev/sdb1 /var/lib/docker/overlay2 ext4 rw 0 0\n")
	if len(mounts) != 2 || mounts[0].mount != "/" || mounts[1].mount != `/mnt/with space\x` || mounts[1].device != "/dev/nvme0n1p1" {
		t.Errorf("mounts = %+v", mounts)
	}

	p, ok := parseProcStatLine("42 (my (odd) proc) R 1 42 42 0 -1 0 0 0 0 0 300 200 0 0 20 0 7 0 12345 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n")
	if !ok || p.comm != "my (odd) proc" || p.state != "R" || p.ticks != 500 || p.threads != 7 || p.startTick != 12345 {
		t.Errorf("stat line = %+v ok=%v", p, ok)
	}
	if _, ok := parseProcStatLine("garbage"); ok {
		t.Error("garbage accepted")
	}

	total, cores := parseProcStat("cpu  1 2 3 4 5 6 7 8 9 10\ncpu0 1 2 3 4 5 6 7 8 9 10\n")
	if total.user != 1 || total.steal != 8 || total.total() != 36 || len(cores) != 1 {
		t.Errorf("proc stat = %+v %+v", total, cores)
	}
	if u := cpuUsage(cpuTicks{}, cpuTicks{}); u.Pct != 0 {
		t.Errorf("no ticks should mean 0%%, got %+v", u)
	}

	model, n := parseCPUInfo("processor\t: 0\nmodel name\t: A  B\nprocessor\t: 1\n")
	if model != "A B" || n != 2 {
		t.Errorf("cpuinfo = %q %d", model, n)
	}
	if got := parseOSRelease("ID=debian\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n"); got != "Debian GNU/Linux 12 (bookworm)" {
		t.Errorf("os-release = %q", got)
	}
	if pr, ok := parsePressure("io", "some avg10=2.50 avg60=1.00 avg300=0.50 total=10\nfull avg10=0.75 avg60=0.25 avg300=0.00 total=5\n"); !ok || pr.SomeAvg10 != 2.5 || pr.FullAvg10 != 0.75 || pr.FullAvg60 != 0.25 {
		t.Errorf("pressure = %+v ok=%v", pr, ok)
	}
	for in, want := range map[time.Duration]string{
		45 * time.Second:            "45 detik",
		12 * time.Minute:            "12 menit",
		3*time.Hour + 5*time.Minute: "3 jam 5 menit",
		50 * time.Hour:              "2 hari 2 jam",
	} {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", in, got, want)
		}
	}
	if got := humanRate(1536 * 1024); got != "1.5 MiB/s" {
		t.Errorf("humanRate = %q", got)
	}
}
