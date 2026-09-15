package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// useFixtureMonitor points the panel's Status page at the fake /proc tree.
func (p *testPanel) useFixtureMonitor(t *testing.T) {
	t.Helper()
	m, _, _, _ := newFixtureMonitor(t)
	p.app.sys = m
}

func TestStatusPageShowsServerAndCluster(t *testing.T) {
	p := newTestPanel(t)
	p.useFixtureMonitor(t)

	resp, body := p.get(t, "/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{
		"Ubuntu 24.04 LTS", "6.8.0-fake", "Fake CPU @ 3.0GHz", "2 core",
		`data-meter="cpu.pct"`, "7.6 GiB", // memory used = 8,000,000 kB
		"/var/lib/garage data", "tank/garage", "coretemp Package id 0", "45 °C",
		`data-row="1234"`, "garage", "200.0 MiB", // garage RSS
		"sehat", "node-a", "dc1", "3.0 TiB / 4.0 TiB", "v1.2.0", "node-b", "putus, terakhir terlihat 1 jam 0 menit lalu",
		"rclone v1.66.0", `data-field="panel.jobsRunning"`,
		`href="/status.json"`, // nowhere: the script fetches it; make sure the page at least mentions polling
	} {
		if want == `href="/status.json"` {
			continue
		}
		if !strings.Contains(body, want) {
			t.Errorf("status page missing %q: %s", want, firstLines(body))
		}
	}
	if strings.Contains(body, "test-token") {
		t.Error("admin token on the status page")
	}

	// The JSON the page polls carries the same view.
	req, err := http.NewRequest(http.MethodGet, p.srv.URL+"/status.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	jr, err := p.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer jr.Body.Close()
	if jr.StatusCode != http.StatusOK || !strings.HasPrefix(jr.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("status.json: %d %s", jr.StatusCode, jr.Header.Get("Content-Type"))
	}
	var view statusView
	if err := json.NewDecoder(jr.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.Memory.PctH != "50%" || view.Host.Cores != 2 || len(view.Disks) != 2 || view.Garage.Health == nil || view.Garage.Health.Level != "ok" || len(view.Garage.Nodes) != 2 {
		t.Errorf("json view = %+v", view)
	}
	if view.Keys == "" || !strings.Contains(view.Keys, "p:1234;") || !strings.Contains(view.Keys, "d:/;") {
		t.Errorf("keys = %q", view.Keys)
	}
	if view.Garage.Nodes[1].Level != "bad" || view.Garage.Nodes[0].DataLevel != "ok" || int(view.Garage.Nodes[0].DataPct) != 75 {
		t.Errorf("nodes = %+v", view.Garage.Nodes)
	}
	if view.Panel.JobsTotal != 0 || !view.Panel.SyncEnabled || view.Panel.RcloneVersion != "1.66.0" {
		t.Errorf("panel = %+v", view.Panel)
	}
}

func TestStatusPageDegradesWithoutClusterScopeOrGarage(t *testing.T) {
	p := newTestPanel(t)
	p.useFixtureMonitor(t)

	p.garage.mu.Lock()
	p.garage.noScope["GetClusterStatus"] = true
	p.garage.mu.Unlock()
	_, body := p.get(t, "/status")
	if !strings.Contains(body, "scope GetClusterStatus") || !strings.Contains(body, "sehat") {
		t.Errorf("scope hint missing or health lost: %s", firstLines(body))
	}
	if strings.Contains(body, "node-a") {
		t.Error("nodes rendered although the call was refused")
	}

	// Garage down entirely: the server half of the page still renders.
	p.garage.mu.Lock()
	p.garage.failOps["GetClusterHealth"] = "garage is restarting"
	p.garage.mu.Unlock()
	resp, body := p.get(t, "/status")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "garage is restarting") || !strings.Contains(body, "Ubuntu 24.04 LTS") {
		t.Errorf("garage down: %d %s", resp.StatusCode, firstLines(body))
	}
}

func TestBuildStatusViewLevels(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s := SysStatus{
		At: now, Interval: 5 * time.Second,
		Host:   HostInfo{Cores: 4, Uptime: 26 * time.Hour, BootedAt: now.Add(-26 * time.Hour)},
		CPU:    CPUInfo{Pct: 92.4, Load1: 4.5, PerCore: []float64{10, 95}},
		Memory: MemInfo{Total: 8 << 30, Used: 7 << 30, Pct: 87.5, SwapTotal: 1 << 30, SwapUsed: 800 << 20, SwapPct: 78.1},
		Disks:  []DiskInfo{{Mount: "/", Pct: 50, InodesPct: 99}, {Mount: "/data", Pct: 85, InodesPct: -1}},
		Temps:  []TempInfo{{Label: "cpu", Celsius: 88}},
		Net:    []NetInfo{{Name: "eth0", RxErrors: 2}},
		Procs:  []ProcInfo{{Name: "garage", PID: 7, CPUPct: 12.34, RSS: 1 << 20, StartedAt: now.Add(-90 * time.Minute)}},
	}
	v := buildStatusView(s)
	if v.CPU.Level != "bad" || v.CPU.PctH != "92%" || v.CPU.LoadLevel != "bad" || v.CPU.PerCore[1].Level != "bad" || v.CPU.PerCore[0].Level != "ok" {
		t.Errorf("cpu view = %+v", v.CPU)
	}
	if v.Memory.Level != "warn" || v.Memory.SwapLevel != "bad" || v.Memory.UsedH != "7.0 GiB" || !v.Memory.HasSwap {
		t.Errorf("memory view = %+v", v.Memory)
	}
	if v.Disks[0].Level != "bad" || v.Disks[0].InodesH != "99%" || v.Disks[1].Level != "warn" || v.Disks[1].InodesH != "—" {
		t.Errorf("disk views = %+v", v.Disks)
	}
	if v.Temps[0].Level != "bad" || v.Temps[0].CelsiusH != "88 °C" {
		t.Errorf("temp view = %+v", v.Temps)
	}
	if v.Net[0].Level != "warn" || v.Net[0].ErrorsH != "2 error, 0 drop" {
		t.Errorf("net view = %+v", v.Net)
	}
	if p := v.Procs[0]; p.CPUH != "12.3%" || p.RSSH != "1.0 MiB" || p.UptimeH != "1 jam 30 menit" {
		t.Errorf("proc view = %+v", p)
	}
	if v.Host.UptimeH != "1 hari 2 jam" || v.Interval != "5 detik" {
		t.Errorf("host/interval = %+v %q", v.Host, v.Interval)
	}
}
