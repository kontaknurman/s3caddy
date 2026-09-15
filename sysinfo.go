package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The Status page reads the kernel's own accounting from /proc and /sys: no
// external commands, no root, nothing the sandboxed unit forbids. Everything
// here is plain text parsing. Rates (CPU %, disk and network throughput,
// per-process CPU) come from the difference between two samples; the
// monitor keeps the previous sample so a page that polls every few seconds
// gets averages over exactly that interval.

const (
	// clockTicks is USER_HZ, the unit of the CPU counters in /proc/stat and
	// /proc/<pid>/stat. It is 100 on every Linux architecture Go supports.
	clockTicks = 100
	// A previous sample older than sysMaxInterval (nobody had the page open)
	// or younger than sysMinInterval (two tabs polling at once) is not a
	// useful baseline; a short fresh sample is taken instead.
	sysMinInterval    = 500 * time.Millisecond
	sysMaxInterval    = 2 * time.Minute
	sysFallbackSample = 400 * time.Millisecond
	// statfs on a hung network mount can block for minutes; the page must not.
	statfsTimeout = 2 * time.Second
	sysMaxTemps   = 16
	sysMaxDisks   = 16
	sysMaxIfaces  = 8
)

// watchedProcs are the daemons the panel reports on, by comm name.
var watchedProcs = map[string]bool{"garage": true, "caddy": true, "garagepanel": true, "rclone": true}

// SysStatus is one snapshot of the server.
type SysStatus struct {
	At       time.Time
	Interval time.Duration // what the rates are averaged over
	Host     HostInfo
	CPU      CPUInfo
	Memory   MemInfo
	Pressure []Pressure
	Disks    []DiskInfo
	DiskIO   []DiskIO
	Net      []NetInfo
	Temps    []TempInfo
	Procs    []ProcInfo
	Warnings []string // files that could not be read, in words
}

// HostInfo is what rarely changes.
type HostInfo struct {
	Hostname string
	OS       string
	Kernel   string
	Arch     string
	CPUModel string
	Cores    int
	Uptime   time.Duration
	BootedAt time.Time
}

// CPUInfo is the utilisation over the interval plus the load averages.
type CPUInfo struct {
	Pct     float64 // busy = everything but idle and iowait
	User    float64
	System  float64 // system + irq + softirq
	IOWait  float64
	Steal   float64
	Idle    float64
	PerCore []float64
	Load1   float64
	Load5   float64
	Load15  float64
	Running int // runnable tasks right now
	Tasks   int // all tasks
}

// MemInfo is /proc/meminfo in bytes. Used is what applications hold: total
// minus MemAvailable, so page cache does not count as "used".
type MemInfo struct {
	Total, Used, Available, Free, Buffers, Cached, Shared int64
	Pct                                                   float64
	SwapTotal, SwapUsed                                   int64
	SwapPct                                               float64
}

// Pressure is one /proc/pressure/<resource> file (PSI, kernel 4.20+).
type Pressure struct {
	Resource                                   string // cpu, memory, io
	SomeAvg10, SomeAvg60, FullAvg10, FullAvg60 float64
}

// DiskInfo is a mounted filesystem.
type DiskInfo struct {
	Mount, Device, FSType string
	Total, Used, Avail    int64
	Pct                   float64 // used / (used + avail), like df
	InodesPct             float64 // -1 when the filesystem has no inode count (btrfs, zfs)
}

// DiskIO is the throughput of one block device over the interval.
type DiskIO struct {
	Name                string
	ReadBps, WriteBps   float64
	ReadIOPS, WriteIOPS float64
	UtilPct             float64 // share of the interval the device was busy
}

// NetInfo is one network interface.
type NetInfo struct {
	Name                 string
	RxBps, TxBps         float64
	RxTotal, TxTotal     int64
	RxErrors, TxErrors   int64
	RxDropped, TxDropped int64
	RxPackets, TxPackets int64
}

// TempInfo is one temperature sensor.
type TempInfo struct {
	Label   string
	Celsius float64
}

// ProcInfo is one watched process.
type ProcInfo struct {
	Name      string // comm
	PID       int
	Self      bool // the panel itself
	State     string
	CPUPct    float64
	RSS       int64
	Threads   int
	StartedAt time.Time
}

// --- raw counters ----------------------------------------------------------

type cpuTicks struct {
	user, nice, system, idle, iowait, irq, softirq, steal int64
}

func (c cpuTicks) total() int64 {
	return c.user + c.nice + c.system + c.idle + c.iowait + c.irq + c.softirq + c.steal
}

type netCounters struct {
	rxBytes, rxPackets, rxErrs, rxDrop int64
	txBytes, txPackets, txErrs, txDrop int64
}

type diskCounters struct {
	reads, readSectors, writes, writeSectors, ioTicks int64
}

type procSample struct {
	comm      string
	state     string
	ticks     int64 // utime + stime
	threads   int
	startTick int64 // starttime, ticks since boot
	rss       int64
}

type loadInfo struct {
	load1, load5, load15 float64
	running, tasks       int
}

type mountEntry struct {
	device, mount, fstype string
}

// fsUsage is what statfs answers, in bytes.
type fsUsage struct {
	Total, Free, Avail int64
	Inodes, InodesFree int64
}

// sysSample is everything read in one pass.
type sysSample struct {
	at       time.Time
	cpu      cpuTicks
	cores    []cpuTicks
	procs    map[int]procSample
	net      map[string]netCounters
	disk     map[string]diskCounters
	mem      map[string]int64
	load     loadInfo
	uptime   float64
	press    []Pressure
	temps    []TempInfo
	warnings []string
}

// sysMonitor reads the server. The roots and clocks are fields so tests can
// point it at a fixture tree and step time by hand.
type sysMonitor struct {
	procRoot string
	sysRoot  string
	etcRoot  string
	statfs   func(path string) (fsUsage, error)
	now      func() time.Time
	sleep    func(time.Duration)
	selfPID  int

	mu   sync.Mutex
	prev *sysSample
	// hostCache holds the parts that never change while the panel runs.
	hostCache *HostInfo
	// stuck remembers mounts whose statfs timed out, and until when they
	// are left alone, so a hung NFS share costs one blocked goroutine per
	// stuckMountRetry rather than one per page refresh.
	stuck map[string]time.Time
}

// stuckMountRetry is how long a mount that did not answer is skipped.
const stuckMountRetry = 5 * time.Minute

func newSysMonitor() *sysMonitor {
	return &sysMonitor{
		procRoot: "/proc",
		sysRoot:  "/sys",
		etcRoot:  "/etc",
		statfs:   statfsUsage,
		now:      time.Now,
		sleep:    time.Sleep,
		selfPID:  os.Getpid(),
	}
}

// Snapshot reads the server now. Rates are computed against the previous
// snapshot when that is recent enough, otherwise against a short second
// sample taken after a brief wait.
func (m *sysMonitor) Snapshot() SysStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.sample()
	prev := m.prev
	if prev == nil || cur.at.Sub(prev.at) < sysMinInterval || cur.at.Sub(prev.at) > sysMaxInterval {
		m.sleep(sysFallbackSample)
		prev = cur
		cur = m.sample()
	}
	m.prev = cur
	return m.build(prev, cur)
}

// --- reading ---------------------------------------------------------------

func (m *sysMonitor) procPath(parts ...string) string {
	return filepath.Join(append([]string{m.procRoot}, parts...)...)
}

func readText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (m *sysMonitor) sample() *sysSample {
	s := &sysSample{at: m.now(), procs: map[int]procSample{}, net: map[string]netCounters{}, disk: map[string]diskCounters{}}
	warn := func(what string, err error) {
		s.warnings = append(s.warnings, fmt.Sprintf("%s tidak bisa dibaca: %v", what, err))
	}

	if text, err := readText(m.procPath("stat")); err != nil {
		warn("/proc/stat", err)
	} else {
		s.cpu, s.cores = parseProcStat(text)
	}
	if text, err := readText(m.procPath("meminfo")); err != nil {
		warn("/proc/meminfo", err)
	} else {
		s.mem = parseMeminfo(text)
	}
	if text, err := readText(m.procPath("loadavg")); err != nil {
		warn("/proc/loadavg", err)
	} else {
		s.load = parseLoadavg(text)
	}
	if text, err := readText(m.procPath("uptime")); err != nil {
		warn("/proc/uptime", err)
	} else {
		s.uptime = parseUptime(text)
	}
	if text, err := readText(m.procPath("net", "dev")); err != nil {
		warn("/proc/net/dev", err)
	} else {
		s.net = parseNetDev(text)
	}
	if text, err := readText(m.procPath("diskstats")); err != nil {
		warn("/proc/diskstats", err)
	} else {
		s.disk = parseDiskstats(text, m.isWholeDisk)
	}
	for _, res := range []string{"cpu", "memory", "io"} {
		if text, err := readText(m.procPath("pressure", res)); err == nil {
			if p, ok := parsePressure(res, text); ok {
				s.press = append(s.press, p)
			}
		}
	}
	s.temps = m.readTemps()
	m.readProcs(s)
	return s
}

// parseProcStat returns the aggregate line and one entry per core.
func parseProcStat(text string) (total cpuTicks, cores []cpuTicks) {
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		var t cpuTicks
		vals := []*int64{&t.user, &t.nice, &t.system, &t.idle, &t.iowait, &t.irq, &t.softirq, &t.steal}
		for i, p := range vals {
			if i+1 < len(f) {
				*p, _ = strconv.ParseInt(f[i+1], 10, 64)
			}
		}
		if f[0] == "cpu" {
			total = t
		} else {
			cores = append(cores, t)
		}
	}
	return total, cores
}

// parseMeminfo maps each key to bytes.
func parseMeminfo(text string) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(text, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && f[1] == "kB" {
			v *= 1024
		}
		out[strings.TrimSpace(key)] = v
	}
	return out
}

func parseLoadavg(text string) loadInfo {
	var l loadInfo
	f := strings.Fields(text)
	if len(f) < 4 {
		return l
	}
	l.load1, _ = strconv.ParseFloat(f[0], 64)
	l.load5, _ = strconv.ParseFloat(f[1], 64)
	l.load15, _ = strconv.ParseFloat(f[2], 64)
	if r, t, ok := strings.Cut(f[3], "/"); ok {
		l.running, _ = strconv.Atoi(r)
		l.tasks, _ = strconv.Atoi(t)
	}
	return l
}

func parseUptime(text string) float64 {
	f := strings.Fields(text)
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// parseNetDev reads the per-interface counters; the two header lines are
// skipped because they carry no colon-terminated name.
func parseNetDev(text string) map[string]netCounters {
	out := map[string]netCounters{}
	for _, line := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if name == "" || len(f) < 12 {
			continue
		}
		n := func(i int) int64 { v, _ := strconv.ParseInt(f[i], 10, 64); return v }
		out[name] = netCounters{
			rxBytes: n(0), rxPackets: n(1), rxErrs: n(2), rxDrop: n(3),
			txBytes: n(8), txPackets: n(9), txErrs: n(10), txDrop: n(11),
		}
	}
	return out
}

// parseDiskstats keeps whole devices (the caller says which) that have ever
// done any I/O, so idle loop devices do not clutter the page.
func parseDiskstats(text string, whole func(name string) bool) map[string]diskCounters {
	out := map[string]diskCounters{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 14 {
			continue
		}
		name := f[2]
		if !whole(name) {
			continue
		}
		n := func(i int) int64 { v, _ := strconv.ParseInt(f[i], 10, 64); return v }
		c := diskCounters{reads: n(3), readSectors: n(5), writes: n(7), writeSectors: n(9), ioTicks: n(12)}
		if c.reads == 0 && c.writes == 0 {
			continue
		}
		out[name] = c
	}
	return out
}

// isWholeDisk reports whether /sys/block knows the name as a disk rather
// than a partition (partitions live under their disk's directory).
func (m *sysMonitor) isWholeDisk(name string) bool {
	if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") {
		return false
	}
	_, err := os.Stat(filepath.Join(m.sysRoot, "block", name))
	return err == nil
}

// parsePressure reads "some avg10=0.00 avg60=0.00 …" / "full …".
func parsePressure(resource, text string) (Pressure, bool) {
	p := Pressure{Resource: resource}
	found := false
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		get := func(key string) float64 {
			for _, kv := range f[1:] {
				if k, v, ok := strings.Cut(kv, "="); ok && k == key {
					x, _ := strconv.ParseFloat(v, 64)
					return x
				}
			}
			return 0
		}
		switch f[0] {
		case "some":
			p.SomeAvg10, p.SomeAvg60, found = get("avg10"), get("avg60"), true
		case "full":
			p.FullAvg10, p.FullAvg60, found = get("avg10"), get("avg60"), true
		}
	}
	return p, found
}

// parseMounts lists the mounts worth showing: real filesystems, one entry
// per device (bind mounts repeat the device), skipping the system's own
// pseudo mounts. Mount points are octal-escaped in /proc/mounts.
func parseMounts(text string) []mountEntry {
	var out []mountEntry
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		device, mount, fstype := unescapeMount(f[0]), unescapeMount(f[1]), f[2]
		if !realFSTypes[fstype] || hiddenMount(mount) {
			continue
		}
		if seen[device] {
			continue
		}
		seen[device] = true
		out = append(out, mountEntry{device: device, mount: mount, fstype: fstype})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mount < out[j].mount })
	return out
}

var realFSTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true,
	"jfs": true, "reiserfs": true, "vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "fuseblk": true,
	"nfs": true, "nfs4": true, "cifs": true, "virtiofs": true,
}

func hiddenMount(mount string) bool {
	for _, p := range []string{"/snap/", "/var/lib/docker/", "/var/lib/containers/", "/run/", "/proc", "/sys/", "/dev/"} {
		if strings.HasPrefix(mount, p) {
			return true
		}
	}
	return false
}

// unescapeMount turns \040 style octal escapes back into characters.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseCPUInfo returns the model name and the number of logical CPUs.
func parseCPUInfo(text string) (model string, cores int) {
	for _, line := range strings.Split(text, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "processor":
			cores++
		case "model name", "Model", "Hardware":
			if model == "" {
				model = strings.Join(strings.Fields(val), " ")
			}
		}
	}
	return model, cores
}

// parseOSRelease returns PRETTY_NAME from /etc/os-release.
func parseOSRelease(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// parseProcStatLine reads /proc/<pid>/stat. The command name is in
// parentheses and may itself contain spaces or parentheses, so the fields
// are counted from the last ')'.
func parseProcStatLine(text string) (procSample, bool) {
	open := strings.IndexByte(text, '(')
	closeIdx := strings.LastIndexByte(text, ')')
	if open < 0 || closeIdx < open {
		return procSample{}, false
	}
	p := procSample{comm: text[open+1 : closeIdx]}
	f := strings.Fields(text[closeIdx+1:])
	// f[0] is field 3 (state); utime=14, stime=15, num_threads=20, starttime=22.
	if len(f) < 20 {
		return procSample{}, false
	}
	p.state = f[0]
	utime, _ := strconv.ParseInt(f[11], 10, 64)
	stime, _ := strconv.ParseInt(f[12], 10, 64)
	p.ticks = utime + stime
	p.threads, _ = strconv.Atoi(f[17])
	p.startTick, _ = strconv.ParseInt(f[19], 10, 64)
	return p, true
}

// readProcs finds the watched daemons (and the panel itself) in /proc.
func (m *sysMonitor) readProcs(s *sysSample) {
	entries, err := os.ReadDir(m.procRoot)
	if err != nil {
		s.warnings = append(s.warnings, fmt.Sprintf("/proc tidak bisa dibaca: %v", err))
		return
	}
	pid1 := false
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if pid == 1 {
			pid1 = true
		}
		comm, err := readText(m.procPath(e.Name(), "comm"))
		if err != nil {
			continue
		}
		comm = strings.TrimSpace(comm)
		if !watchedProcs[comm] && pid != m.selfPID {
			continue
		}
		stat, err := readText(m.procPath(e.Name(), "stat"))
		if err != nil {
			continue
		}
		p, ok := parseProcStatLine(stat)
		if !ok {
			continue
		}
		p.comm = comm
		if status, err := readText(m.procPath(e.Name(), "status")); err == nil {
			for _, line := range strings.Split(status, "\n") {
				if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
					f := strings.Fields(v)
					if len(f) > 0 {
						kb, _ := strconv.ParseInt(f[0], 10, 64)
						p.rss = kb * 1024
					}
				}
			}
		}
		s.procs[pid] = p
	}
	if !pid1 {
		s.warnings = append(s.warnings, "PID 1 tidak terlihat dari /proc — kemungkinan /proc dipasang dengan hidepid, jadi proses user lain (garage, caddy) tidak bisa dilaporkan")
	}
}

// readTemps reads hwmon sensors, falling back to thermal zones.
func (m *sysMonitor) readTemps() []TempInfo {
	var out []TempInfo
	base := filepath.Join(m.sysRoot, "class", "hwmon")
	chips, _ := os.ReadDir(base)
	for _, chip := range chips {
		dir := filepath.Join(base, chip.Name())
		name, _ := readText(filepath.Join(dir, "name"))
		name = strings.TrimSpace(name)
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			idx, ok := strings.CutSuffix(strings.TrimPrefix(f.Name(), "temp"), "_input")
			if !ok || !strings.HasPrefix(f.Name(), "temp") {
				continue
			}
			raw, err := readText(filepath.Join(dir, f.Name()))
			if err != nil {
				continue
			}
			milli, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
			if err != nil || milli <= 0 || milli > 200000 {
				continue
			}
			label, _ := readText(filepath.Join(dir, "temp"+idx+"_label"))
			label = strings.TrimSpace(label)
			full := name
			switch {
			case label != "":
				full = name + " " + label
			case idx != "1":
				full = name + " " + idx
			}
			out = append(out, TempInfo{Label: full, Celsius: float64(milli) / 1000})
		}
	}
	if len(out) == 0 {
		base := filepath.Join(m.sysRoot, "class", "thermal")
		zones, _ := os.ReadDir(base)
		for _, z := range zones {
			if !strings.HasPrefix(z.Name(), "thermal_zone") {
				continue
			}
			raw, err := readText(filepath.Join(base, z.Name(), "temp"))
			if err != nil {
				continue
			}
			milli, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
			if err != nil || milli <= 0 || milli > 200000 {
				continue
			}
			typ, _ := readText(filepath.Join(base, z.Name(), "type"))
			typ = strings.TrimSpace(typ)
			if typ == "" {
				typ = z.Name()
			}
			out = append(out, TempInfo{Label: typ, Celsius: float64(milli) / 1000})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	if len(out) > sysMaxTemps {
		out = out[:sysMaxTemps]
	}
	return out
}

// host reads the slow-changing facts once.
func (m *sysMonitor) host(uptime float64, now time.Time) HostInfo {
	if m.hostCache == nil {
		h := HostInfo{Arch: runtime.GOARCH}
		h.Hostname, _ = os.Hostname()
		if text, err := readText(filepath.Join(m.etcRoot, "os-release")); err == nil {
			h.OS = parseOSRelease(text)
		}
		if text, err := readText(m.procPath("sys", "kernel", "osrelease")); err == nil {
			h.Kernel = strings.TrimSpace(text)
		}
		if text, err := readText(m.procPath("cpuinfo")); err == nil {
			h.CPUModel, h.Cores = parseCPUInfo(text)
		}
		if h.Cores == 0 {
			h.Cores = runtime.NumCPU()
		}
		m.hostCache = &h
	}
	h := *m.hostCache
	h.Uptime = time.Duration(uptime * float64(time.Second))
	h.BootedAt = now.Add(-h.Uptime)
	return h
}

// --- building the snapshot -------------------------------------------------

func pct(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}

func cpuUsage(a, b cpuTicks) (c CPUInfo) {
	d := cpuTicks{
		user: b.user - a.user, nice: b.nice - a.nice, system: b.system - a.system, idle: b.idle - a.idle,
		iowait: b.iowait - a.iowait, irq: b.irq - a.irq, softirq: b.softirq - a.softirq, steal: b.steal - a.steal,
	}
	total := d.total()
	if total <= 0 {
		return c
	}
	c.User = pct(d.user+d.nice, total)
	c.System = pct(d.system+d.irq+d.softirq, total)
	c.IOWait = pct(d.iowait, total)
	c.Steal = pct(d.steal, total)
	c.Idle = pct(d.idle, total)
	c.Pct = pct(total-d.idle-d.iowait, total)
	return c
}

func (m *sysMonitor) build(prev, cur *sysSample) SysStatus {
	interval := cur.at.Sub(prev.at)
	secs := interval.Seconds()
	if secs <= 0 {
		secs = 1
	}
	rate := func(a, b int64) float64 {
		if b < a { // counter reset or wrapped
			return 0
		}
		return float64(b-a) / secs
	}

	st := SysStatus{At: cur.at, Interval: interval, Warnings: cur.warnings, Pressure: cur.press, Temps: cur.temps}
	st.Host = m.host(cur.uptime, cur.at)

	st.CPU = cpuUsage(prev.cpu, cur.cpu)
	if len(prev.cores) == len(cur.cores) {
		for i := range cur.cores {
			st.CPU.PerCore = append(st.CPU.PerCore, cpuUsage(prev.cores[i], cur.cores[i]).Pct)
		}
	}
	st.CPU.Load1, st.CPU.Load5, st.CPU.Load15 = cur.load.load1, cur.load.load5, cur.load.load15
	st.CPU.Running, st.CPU.Tasks = cur.load.running, cur.load.tasks

	mem := cur.mem
	st.Memory = MemInfo{
		Total: mem["MemTotal"], Available: mem["MemAvailable"], Free: mem["MemFree"],
		Buffers: mem["Buffers"], Cached: mem["Cached"] + mem["SReclaimable"], Shared: mem["Shmem"],
		SwapTotal: mem["SwapTotal"],
	}
	if st.Memory.Available == 0 && st.Memory.Total > 0 { // kernels before 3.14
		st.Memory.Available = st.Memory.Free + st.Memory.Buffers + st.Memory.Cached
	}
	st.Memory.Used = st.Memory.Total - st.Memory.Available
	st.Memory.Pct = pct(st.Memory.Used, st.Memory.Total)
	st.Memory.SwapUsed = st.Memory.SwapTotal - mem["SwapFree"]
	st.Memory.SwapPct = pct(st.Memory.SwapUsed, st.Memory.SwapTotal)

	st.Disks, st.Warnings = m.readDisks(st.Warnings)

	for name, b := range cur.disk {
		a, ok := prev.disk[name]
		if !ok {
			continue
		}
		io := DiskIO{
			Name:     name,
			ReadBps:  rate(a.readSectors, b.readSectors) * 512,
			WriteBps: rate(a.writeSectors, b.writeSectors) * 512,
			ReadIOPS: rate(a.reads, b.reads), WriteIOPS: rate(a.writes, b.writes),
			UtilPct: rate(a.ioTicks, b.ioTicks) / 10, // ms per second → percent
		}
		if io.UtilPct > 100 {
			io.UtilPct = 100
		}
		st.DiskIO = append(st.DiskIO, io)
	}
	sort.Slice(st.DiskIO, func(i, j int) bool { return st.DiskIO[i].Name < st.DiskIO[j].Name })

	for name, b := range cur.net {
		if name == "lo" {
			continue
		}
		a, ok := prev.net[name]
		if !ok {
			continue
		}
		st.Net = append(st.Net, NetInfo{
			Name: name, RxBps: rate(a.rxBytes, b.rxBytes), TxBps: rate(a.txBytes, b.txBytes),
			RxTotal: b.rxBytes, TxTotal: b.txBytes, RxErrors: b.rxErrs, TxErrors: b.txErrs,
			RxDropped: b.rxDrop, TxDropped: b.txDrop, RxPackets: b.rxPackets, TxPackets: b.txPackets,
		})
	}
	// Busiest interfaces first; ties by name so the order is stable.
	sort.Slice(st.Net, func(i, j int) bool {
		ti, tj := st.Net[i].RxTotal+st.Net[i].TxTotal, st.Net[j].RxTotal+st.Net[j].TxTotal
		if ti != tj {
			return ti > tj
		}
		return st.Net[i].Name < st.Net[j].Name
	})
	if len(st.Net) > sysMaxIfaces {
		st.Net = st.Net[:sysMaxIfaces]
	}

	for pid, p := range cur.procs {
		info := ProcInfo{Name: p.comm, PID: pid, Self: pid == m.selfPID, State: procStateLabel(p.state), RSS: p.rss, Threads: p.threads}
		info.StartedAt = st.Host.BootedAt.Add(time.Duration(p.startTick) * time.Second / clockTicks)
		if a, ok := prev.procs[pid]; ok && a.startTick == p.startTick && p.ticks >= a.ticks {
			info.CPUPct = float64(p.ticks-a.ticks) / clockTicks / secs * 100
		}
		st.Procs = append(st.Procs, info)
	}
	sort.Slice(st.Procs, func(i, j int) bool {
		ri, rj := procRank(st.Procs[i]), procRank(st.Procs[j])
		if ri != rj {
			return ri < rj
		}
		return st.Procs[i].PID < st.Procs[j].PID
	})
	return st
}

func procRank(p ProcInfo) int {
	switch {
	case p.Name == "garage":
		return 0
	case p.Name == "caddy":
		return 1
	case p.Self || p.Name == "garagepanel":
		return 2
	}
	return 3
}

func procStateLabel(s string) string {
	switch s {
	case "R":
		return "berjalan"
	case "S":
		return "tidur"
	case "D":
		return "menunggu I/O"
	case "Z":
		return "zombie"
	case "T", "t":
		return "dihentikan"
	}
	return s
}

// readDisks lists mounted filesystems with their usage.
func (m *sysMonitor) readDisks(warnings []string) ([]DiskInfo, []string) {
	text, err := readText(m.procPath("mounts"))
	if err != nil {
		return nil, append(warnings, fmt.Sprintf("/proc/mounts tidak bisa dibaca: %v", err))
	}
	var out []DiskInfo
	now := m.now()
	for _, mnt := range parseMounts(text) {
		if len(out) >= sysMaxDisks {
			break
		}
		if until, ok := m.stuck[mnt.mount]; ok && now.Before(until) {
			warnings = append(warnings, fmt.Sprintf("statfs %s: tidak menjawab (mount macet?); dicoba lagi %s", mnt.mount, humanDuration(until.Sub(now))))
			continue
		}
		u, err := m.statfsTimeout(mnt.mount)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("statfs %s: %v", mnt.mount, err))
			continue
		}
		delete(m.stuck, mnt.mount)
		d := DiskInfo{Mount: mnt.mount, Device: mnt.device, FSType: mnt.fstype, Total: u.Total, Used: u.Total - u.Free, Avail: u.Avail, InodesPct: -1}
		d.Pct = pct(d.Used, d.Used+d.Avail)
		if u.Inodes > 0 {
			d.InodesPct = pct(u.Inodes-u.InodesFree, u.Inodes)
		}
		out = append(out, d)
	}
	return out, warnings
}

// statfsTimeout gives statfs a deadline; a stuck NFS mount otherwise hangs
// the whole page. The goroutine is abandoned on timeout and finishes on its
// own whenever the kernel lets it.
func (m *sysMonitor) statfsTimeout(path string) (fsUsage, error) {
	type result struct {
		u   fsUsage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		u, err := m.statfs(path)
		ch <- result{u, err}
	}()
	select {
	case r := <-ch:
		return r.u, r.err
	case <-time.After(statfsTimeout):
		if m.stuck == nil {
			m.stuck = map[string]time.Time{}
		}
		m.stuck[path] = m.now().Add(stuckMountRetry)
		return fsUsage{}, errors.New("tidak menjawab (mount macet?)")
	}
}

// --- humanising ------------------------------------------------------------

// humanRate formats bytes per second.
func humanRate(bps float64) string {
	if bps < 0 {
		bps = 0
	}
	return humanBytes(int64(bps)) + "/s"
}

// humanDuration writes a duration the way people say it: "3 hari 4 jam",
// "12 menit", "45 detik".
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	secs := int64(d.Seconds())
	days, hours, mins := secs/86400, secs%86400/3600, secs%3600/60
	switch {
	case days > 0:
		return fmt.Sprintf("%d hari %d jam", days, hours)
	case hours > 0:
		return fmt.Sprintf("%d jam %d menit", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%d menit", mins)
	}
	return fmt.Sprintf("%d detik", secs)
}
