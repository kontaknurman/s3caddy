package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// --- page 6: server status ---------------------------------------------------
//
// The page renders a statusView once, then polls /status.json every few
// seconds and copies the same view's strings into the elements marked with
// data-field / data-meter. Every number therefore has a ready-made human
// form here, so the template and the script never format anything.

// panelStartedAt is when this process came up.
var panelStartedAt = time.Now()

// Thresholds turn a percentage into a colour; each meter has its own pair.
type levelSpec struct{ warn, bad float64 }

var (
	levelCPU    = levelSpec{70, 90}
	levelMem    = levelSpec{80, 95}
	levelSwap   = levelSpec{30, 70}
	levelDisk   = levelSpec{80, 90}
	levelInodes = levelSpec{80, 95}
	levelUtil   = levelSpec{70, 90}
	levelTemp   = levelSpec{70, 85}
	levelPSI    = levelSpec{10, 40}
)

func (l levelSpec) of(v float64) string {
	switch {
	case v >= l.bad:
		return "bad"
	case v >= l.warn:
		return "warn"
	}
	return "ok"
}

type statusView struct {
	At       string         `json:"at"`
	Interval string         `json:"interval"`
	Warnings []string       `json:"warnings"`
	Host     hostView       `json:"host"`
	CPU      cpuView        `json:"cpu"`
	Memory   memView        `json:"memory"`
	Pressure []pressureView `json:"pressure"`
	Disks    []diskView     `json:"disks"`
	DiskIO   []diskIOView   `json:"diskIO"`
	Net      []netView      `json:"net"`
	Temps    []tempView     `json:"temps"`
	Procs    []procView     `json:"procs"`
	Garage   garageView     `json:"garage"`
	Panel    panelView      `json:"panel"`
	// Keys fingerprints the lists; when it changes the page reloads so rows
	// that appeared or vanished (a new rclone, a new mount) get rendered.
	Keys string `json:"keys"`
}

type hostView struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Kernel   string `json:"kernel"`
	Arch     string `json:"arch"`
	CPUModel string `json:"cpuModel"`
	Cores    int    `json:"cores"`
	UptimeH  string `json:"uptimeH"`
	BootedH  string `json:"bootedH"`
}

type coreView struct {
	Key   string  `json:"key"`
	Index int     `json:"index"`
	Pct   float64 `json:"pct"`
	PctH  string  `json:"pctH"`
	Level string  `json:"level"`
}

type cpuView struct {
	Pct       float64    `json:"pct"`
	PctH      string     `json:"pctH"`
	Level     string     `json:"level"`
	UserH     string     `json:"userH"`
	SystemH   string     `json:"systemH"`
	IOWaitH   string     `json:"ioWaitH"`
	StealH    string     `json:"stealH"`
	IdleH     string     `json:"idleH"`
	PerCore   []coreView `json:"perCore"`
	Load1H    string     `json:"load1H"`
	Load5H    string     `json:"load5H"`
	Load15H   string     `json:"load15H"`
	LoadLevel string     `json:"loadLevel"`
	Running   int        `json:"running"`
	Tasks     int        `json:"tasks"`
}

type memView struct {
	Pct        float64 `json:"pct"`
	PctH       string  `json:"pctH"`
	Level      string  `json:"level"`
	UsedH      string  `json:"usedH"`
	TotalH     string  `json:"totalH"`
	AvailableH string  `json:"availableH"`
	FreeH      string  `json:"freeH"`
	CachedH    string  `json:"cachedH"`
	BuffersH   string  `json:"buffersH"`
	SharedH    string  `json:"sharedH"`
	HasSwap    bool    `json:"hasSwap"`
	SwapPct    float64 `json:"swapPct"`
	SwapPctH   string  `json:"swapPctH"`
	SwapLevel  string  `json:"swapLevel"`
	SwapUsedH  string  `json:"swapUsedH"`
	SwapTotalH string  `json:"swapTotalH"`
}

type pressureView struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Some10H string `json:"some10H"`
	Some60H string `json:"some60H"`
	Full10H string `json:"full10H"`
	Full60H string `json:"full60H"`
	Level   string `json:"level"`
}

type diskView struct {
	Key     string  `json:"key"`
	Mount   string  `json:"mount"`
	Device  string  `json:"device"`
	FSType  string  `json:"fsType"`
	Pct     float64 `json:"pct"`
	PctH    string  `json:"pctH"`
	Level   string  `json:"level"`
	UsedH   string  `json:"usedH"`
	TotalH  string  `json:"totalH"`
	AvailH  string  `json:"availH"`
	InodesH string  `json:"inodesH"`
}

type diskIOView struct {
	Key        string  `json:"key"`
	Name       string  `json:"name"`
	ReadH      string  `json:"readH"`
	WriteH     string  `json:"writeH"`
	ReadIOPSH  string  `json:"readIOPSH"`
	WriteIOPSH string  `json:"writeIOPSH"`
	UtilPct    float64 `json:"utilPct"`
	UtilH      string  `json:"utilH"`
	Level      string  `json:"level"`
}

type netView struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	RxH      string `json:"rxH"`
	TxH      string `json:"txH"`
	RxTotalH string `json:"rxTotalH"`
	TxTotalH string `json:"txTotalH"`
	ErrorsH  string `json:"errorsH"`
	Level    string `json:"level"`
}

type tempView struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Pct      float64 `json:"pct"` // for the meter: 0..100 = 0..100 °C
	CelsiusH string  `json:"celsiusH"`
	Level    string  `json:"level"`
}

type procView struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	PID      int    `json:"pid"`
	Self     bool   `json:"self"`
	State    string `json:"state"`
	CPUH     string `json:"cpuH"`
	RSSH     string `json:"rssH"`
	Threads  int    `json:"threads"`
	StartedH string `json:"startedH"`
	UptimeH  string `json:"uptimeH"`
}

type healthView struct {
	Status           string `json:"status"`
	StatusLabel      string `json:"statusLabel"`
	Level            string `json:"level"`
	KnownNodes       int    `json:"knownNodes"`
	ConnectedNodes   int    `json:"connectedNodes"`
	StorageNodes     int    `json:"storageNodes"`
	StorageNodesUp   int    `json:"storageNodesUp"`
	Partitions       int    `json:"partitions"`
	PartitionsQuorum int    `json:"partitionsQuorum"`
	PartitionsAllOK  int    `json:"partitionsAllOk"`
}

type nodeView struct {
	Key         string  `json:"key"`
	ShortID     string  `json:"shortId"`
	Hostname    string  `json:"hostname"`
	Addr        string  `json:"addr"`
	Zone        string  `json:"zone"`
	Tags        string  `json:"tags"`
	CapacityH   string  `json:"capacityH"`
	Version     string  `json:"version"`
	IsUp        bool    `json:"isUp"`
	StatusLabel string  `json:"statusLabel"`
	Level       string  `json:"level"`
	Draining    bool    `json:"draining"`
	DataPct     float64 `json:"dataPct"`
	DataH       string  `json:"dataH"`
	DataLevel   string  `json:"dataLevel"`
	MetaPct     float64 `json:"metaPct"`
	MetaH       string  `json:"metaH"`
	MetaLevel   string  `json:"metaLevel"`
}

type garageView struct {
	Health        *healthView `json:"health"`
	HealthErr     string      `json:"healthErr"`
	LayoutVersion int         `json:"layoutVersion"`
	Nodes         []nodeView  `json:"nodes"`
	NodesErr      string      `json:"nodesErr"`
	ScopeHint     string      `json:"scopeHint"`
	AdminURL      string      `json:"adminUrl"`
}

type panelView struct {
	Version       string `json:"version"`
	GoVersion     string `json:"goVersion"`
	UptimeH       string `json:"uptimeH"`
	Goroutines    int    `json:"goroutines"`
	HeapH         string `json:"heapH"`
	SyncEnabled   bool   `json:"syncEnabled"`
	SyncDisabled  string `json:"syncDisabled"`
	RcloneVersion string `json:"rcloneVersion"`
	JobsRunning   int    `json:"jobsRunning"`
	JobsTotal     int    `json:"jobsTotal"`
	Schedules     int    `json:"schedules"`
	Remotes       int    `json:"remotes"`
	Listen        string `json:"listen"`
}

func pctH(v float64) string  { return strconv.FormatFloat(v, 'f', 0, 64) + "%" }
func pct1H(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) + "%" }

// buildStatusView turns a snapshot into strings. Pure, so tests can feed it
// a hand-made SysStatus.
func buildStatusView(s SysStatus) statusView {
	v := statusView{
		At:       s.At.Local().Format("15:04:05"),
		Interval: humanDuration(s.Interval.Round(time.Second)),
		Warnings: s.Warnings,
	}
	if s.Interval < time.Second {
		v.Interval = fmt.Sprintf("%d ms", s.Interval.Milliseconds())
	}
	v.Host = hostView{
		Hostname: s.Host.Hostname, OS: s.Host.OS, Kernel: s.Host.Kernel, Arch: s.Host.Arch,
		CPUModel: s.Host.CPUModel, Cores: s.Host.Cores,
		UptimeH: humanDuration(s.Host.Uptime), BootedH: humanTime(s.Host.BootedAt),
	}

	c := s.CPU
	v.CPU = cpuView{
		Pct: c.Pct, PctH: pctH(c.Pct), Level: levelCPU.of(c.Pct),
		UserH: pctH(c.User), SystemH: pctH(c.System), IOWaitH: pctH(c.IOWait), StealH: pctH(c.Steal), IdleH: pctH(c.Idle),
		Load1H: strconv.FormatFloat(c.Load1, 'f', 2, 64), Load5H: strconv.FormatFloat(c.Load5, 'f', 2, 64), Load15H: strconv.FormatFloat(c.Load15, 'f', 2, 64),
		Running: c.Running, Tasks: c.Tasks,
	}
	if s.Host.Cores > 0 {
		v.CPU.LoadLevel = levelSpec{70, 100}.of(c.Load1 / float64(s.Host.Cores) * 100)
	} else {
		v.CPU.LoadLevel = "ok"
	}
	for i, p := range c.PerCore {
		v.CPU.PerCore = append(v.CPU.PerCore, coreView{Key: strconv.Itoa(i), Index: i, Pct: p, PctH: pctH(p), Level: levelCPU.of(p)})
	}

	m := s.Memory
	v.Memory = memView{
		Pct: m.Pct, PctH: pctH(m.Pct), Level: levelMem.of(m.Pct),
		UsedH: humanBytes(m.Used), TotalH: humanBytes(m.Total), AvailableH: humanBytes(m.Available), FreeH: humanBytes(m.Free),
		CachedH: humanBytes(m.Cached), BuffersH: humanBytes(m.Buffers), SharedH: humanBytes(m.Shared),
		HasSwap: m.SwapTotal > 0, SwapPct: m.SwapPct, SwapPctH: pctH(m.SwapPct), SwapLevel: levelSwap.of(m.SwapPct),
		SwapUsedH: humanBytes(m.SwapUsed), SwapTotalH: humanBytes(m.SwapTotal),
	}

	for _, p := range s.Pressure {
		label := map[string]string{"cpu": "CPU", "memory": "Memori", "io": "I/O"}[p.Resource]
		if label == "" {
			label = p.Resource
		}
		worst := p.SomeAvg10
		if p.FullAvg10 > worst {
			worst = p.FullAvg10
		}
		v.Pressure = append(v.Pressure, pressureView{
			Key: p.Resource, Label: label,
			Some10H: pct1H(p.SomeAvg10), Some60H: pct1H(p.SomeAvg60), Full10H: pct1H(p.FullAvg10), Full60H: pct1H(p.FullAvg60),
			Level: levelPSI.of(worst),
		})
	}

	for _, d := range s.Disks {
		dv := diskView{
			Key: d.Mount, Mount: d.Mount, Device: d.Device, FSType: d.FSType,
			Pct: d.Pct, PctH: pctH(d.Pct), Level: levelDisk.of(d.Pct),
			UsedH: humanBytes(d.Used), TotalH: humanBytes(d.Total), AvailH: humanBytes(d.Avail), InodesH: "—",
		}
		if d.InodesPct >= 0 {
			dv.InodesH = pctH(d.InodesPct)
			if lvl := levelInodes.of(d.InodesPct); lvl == "bad" {
				dv.Level = "bad"
			}
		}
		v.Disks = append(v.Disks, dv)
	}

	for _, d := range s.DiskIO {
		v.DiskIO = append(v.DiskIO, diskIOView{
			Key: d.Name, Name: d.Name, ReadH: humanRate(d.ReadBps), WriteH: humanRate(d.WriteBps),
			ReadIOPSH: strconv.FormatFloat(d.ReadIOPS, 'f', 0, 64), WriteIOPSH: strconv.FormatFloat(d.WriteIOPS, 'f', 0, 64),
			UtilPct: d.UtilPct, UtilH: pctH(d.UtilPct), Level: levelUtil.of(d.UtilPct),
		})
	}

	for _, n := range s.Net {
		errs := n.RxErrors + n.TxErrors + n.RxDropped + n.TxDropped
		nv := netView{
			Key: n.Name, Name: n.Name, RxH: humanRate(n.RxBps), TxH: humanRate(n.TxBps),
			RxTotalH: humanBytes(n.RxTotal), TxTotalH: humanBytes(n.TxTotal), Level: "ok",
		}
		if errs > 0 {
			nv.ErrorsH = fmt.Sprintf("%d error, %d drop", n.RxErrors+n.TxErrors, n.RxDropped+n.TxDropped)
			nv.Level = "warn"
		} else {
			nv.ErrorsH = "tanpa error"
		}
		v.Net = append(v.Net, nv)
	}

	for _, t := range s.Temps {
		v.Temps = append(v.Temps, tempView{
			Key: t.Label, Label: t.Label, Pct: t.Celsius, CelsiusH: strconv.FormatFloat(t.Celsius, 'f', 0, 64) + " °C", Level: levelTemp.of(t.Celsius),
		})
	}

	for _, p := range s.Procs {
		pv := procView{
			Key: strconv.Itoa(p.PID), Name: p.Name, PID: p.PID, Self: p.Self, State: p.State,
			CPUH: pct1H(p.CPUPct), RSSH: humanBytes(p.RSS), Threads: p.Threads,
			StartedH: humanTime(p.StartedAt), UptimeH: humanDuration(s.At.Sub(p.StartedAt)),
		}
		v.Procs = append(v.Procs, pv)
	}
	return v
}

// garageStatus asks the cluster; both calls get one short deadline so a
// dead Garage does not stall the page.
func (a *App) garageStatus(ctx context.Context) garageView {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v := garageView{AdminURL: a.cfg.AdminURL}
	health, err := a.garage.GetClusterHealth(ctx)
	if err != nil {
		v.HealthErr = err.Error()
	} else {
		hv := healthView{
			Status: health.Status, KnownNodes: health.KnownNodes, ConnectedNodes: health.ConnectedNodes,
			StorageNodes: health.StorageNodes, StorageNodesUp: health.StorageNodesUp,
			Partitions: health.Partitions, PartitionsQuorum: health.PartitionsQuorum, PartitionsAllOK: health.PartitionsAllOK,
		}
		switch health.Status {
		case "healthy":
			hv.StatusLabel, hv.Level = "sehat", "ok"
		case "degraded":
			hv.StatusLabel, hv.Level = "terdegradasi", "warn"
		case "unavailable":
			hv.StatusLabel, hv.Level = "tidak tersedia", "bad"
		default:
			hv.StatusLabel, hv.Level = health.Status, "warn"
		}
		v.Health = &hv
	}
	status, err := a.garage.GetClusterStatus(ctx)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden {
			v.ScopeHint = "Token admin tidak punya scope GetClusterStatus, jadi daftar node dan pemakaian disk Garage tidak bisa ditampilkan. " +
				"Buat ulang token dengan scope itu (README, Langkah 0c) atau pakai admin_token dari garage.toml."
		}
		v.NodesErr = err.Error()
		return v
	}
	v.LayoutVersion = status.LayoutVersion
	for _, n := range status.Nodes {
		nv := nodeView{Key: n.ID, ShortID: n.ID, Hostname: n.Hostname, Addr: n.Addr, Version: n.GarageVersion, IsUp: n.IsUp, Draining: n.Draining, CapacityH: "—", DataH: "—", MetaH: "—", DataLevel: "ok", MetaLevel: "ok"}
		if len(nv.ShortID) > 16 {
			nv.ShortID = nv.ShortID[:16]
		}
		if n.Role != nil {
			nv.Zone = n.Role.Zone
			nv.Tags = strings.Join(n.Role.Tags, ", ")
			if n.Role.Capacity != nil {
				nv.CapacityH = humanBytes(*n.Role.Capacity)
			} else {
				nv.CapacityH = "gateway"
			}
		}
		switch {
		case n.IsUp && n.Draining:
			nv.StatusLabel, nv.Level = "aktif, draining", "warn"
		case n.IsUp:
			nv.StatusLabel, nv.Level = "aktif", "ok"
		case n.LastSeenSecsAgo != nil:
			nv.StatusLabel, nv.Level = "putus, terakhir terlihat "+humanDuration(time.Duration(*n.LastSeenSecsAgo)*time.Second)+" lalu", "bad"
		default:
			nv.StatusLabel, nv.Level = "putus", "bad"
		}
		if p := n.DataPartition; p != nil && p.Total > 0 {
			used := p.Total - p.Available
			nv.DataPct = pct(used, p.Total)
			nv.DataH = humanBytes(used) + " / " + humanBytes(p.Total)
			nv.DataLevel = levelDisk.of(nv.DataPct)
		}
		if p := n.MetadataPartition; p != nil && p.Total > 0 {
			used := p.Total - p.Available
			nv.MetaPct = pct(used, p.Total)
			nv.MetaH = humanBytes(used) + " / " + humanBytes(p.Total)
			nv.MetaLevel = levelDisk.of(nv.MetaPct)
		}
		v.Nodes = append(v.Nodes, nv)
	}
	return v
}

func (a *App) panelStatus() panelView {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	v := panelView{
		Version:    buildVersion(),
		GoVersion:  runtime.Version(),
		UptimeH:    humanDuration(time.Since(panelStartedAt)),
		Goroutines: runtime.NumGoroutine(),
		HeapH:      humanBytes(int64(ms.HeapAlloc)),
		Listen:     a.cfg.Listen,
	}
	if a.syncDisabled != "" {
		v.SyncDisabled = a.syncDisabled
		return v
	}
	v.SyncEnabled = true
	v.RcloneVersion = a.rcloneVersion()
	for _, j := range a.jobs.List() {
		v.JobsTotal++
		if j.Status == JobRunning {
			v.JobsRunning++
		}
	}
	v.Schedules = len(a.schedules.List())
	v.Remotes = len(a.remotes.List())
	return v
}

// fingerprint lists the keys of every row on the page.
func (v statusView) fingerprint() string {
	var b strings.Builder
	for _, d := range v.Disks {
		b.WriteString("d:" + d.Key + ";")
	}
	for _, d := range v.DiskIO {
		b.WriteString("io:" + d.Key + ";")
	}
	for _, n := range v.Net {
		b.WriteString("n:" + n.Key + ";")
	}
	for _, t := range v.Temps {
		b.WriteString("t:" + t.Key + ";")
	}
	for _, p := range v.Procs {
		b.WriteString("p:" + p.Key + ";")
	}
	for _, n := range v.Garage.Nodes {
		b.WriteString("g:" + n.Key + ";")
	}
	for _, p := range v.Pressure {
		b.WriteString("psi:" + p.Key + ";")
	}
	b.WriteString(fmt.Sprintf("c:%d;h:%t;w:%d", len(v.CPU.PerCore), v.Garage.Health != nil, len(v.Warnings)))
	return b.String()
}

func (a *App) statusView(ctx context.Context) statusView {
	v := buildStatusView(a.sys.Snapshot())
	v.Garage = a.garageStatus(ctx)
	v.Panel = a.panelStatus()
	v.Keys = v.fingerprint()
	return v
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "status.html", map[string]any{
		"Title": "Status server",
		"Page":  "status",
		"S":     a.statusView(r.Context()),
	})
}

// handleStatusJSON feeds the page's polling script.
func (a *App) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.statusView(r.Context()))
}
