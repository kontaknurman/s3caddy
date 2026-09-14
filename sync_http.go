package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// --- page 5: sync with other S3 providers ---------------------------------

// jobView is a JobState dressed up for the template and the polling JSON.
type jobView struct {
	JobState
	KindLabel   string `json:"kindLabel"`
	Direction   string `json:"direction"`
	StatusLabel string `json:"statusLabel"`
	Active      bool   `json:"active"`
	CanPause    bool   `json:"canPause"`
	CanResume   bool   `json:"canResume"`
	CanCancel   bool   `json:"canCancel"`
	CanRerun    bool   `json:"canRerun"`
	CanDelete   bool   `json:"canDelete"`
	SrcLabel    string `json:"srcLabel"`
	DstLabel    string `json:"dstLabel"`
	BytesH      string `json:"bytesH"`
	SpeedH      string `json:"speedH"`
	UpdatedH    string `json:"updatedH"`
	StartedH    string `json:"startedH"`
	FinishedH   string `json:"finishedH"`
	ErrorShort  string `json:"errorShort"`
	LastLogText string `json:"lastLogText"`
}

func newJobView(s JobState) jobView {
	v := jobView{
		JobState:    s,
		KindLabel:   s.Kind.Label(),
		Direction:   s.Direction(),
		StatusLabel: s.Status.Label(),
		Active:      !s.Status.Terminal(),
		CanPause:    s.Status == JobRunning,
		CanResume:   s.Status == JobPaused || s.Status == JobFailed,
		CanCancel:   !s.Status.Terminal(),
		CanRerun:    s.Status.Terminal(),
		CanDelete:   s.Status.Terminal(),
		SrcLabel:    s.Src.String(),
		BytesH:      humanBytes(s.Counters.Bytes),
		UpdatedH:    humanTime(s.UpdatedAt),
		StartedH:    humanTime(s.StartedAt),
		LastLogText: strings.Join(s.LastLog, "\n"),
	}
	if s.Kind != JobDeletePrefix {
		v.DstLabel = s.Dst.String()
	}
	if s.Status == JobRunning && s.Speed > 0 {
		v.SpeedH = humanBytes(int64(s.Speed)) + "/s"
	}
	if s.FinishedAt != nil {
		v.FinishedH = humanTime(*s.FinishedAt)
	}
	v.ErrorShort = s.Error
	if len(v.ErrorShort) > 300 {
		v.ErrorShort = v.ErrorShort[:300] + "…"
	}
	return v
}

func (a *App) jobViews() []jobView {
	if a.jobs == nil {
		return nil
	}
	states := a.jobs.List()
	out := make([]jobView, 0, len(states))
	for _, s := range states {
		out = append(out, newJobView(s))
	}
	return out
}

func (a *App) handleSync(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Title":        "Sync dengan S3 lain",
		"Page":         "sync",
		"Providers":    providers,
		"MaxTransfers": MaxTransfers,
		"StateDir":     a.cfg.StateDir,
	}
	if a.syncDisabled != "" {
		data["Disabled"] = a.syncDisabled
		a.render(w, r, "sync.html", data)
		return
	}
	buckets, bucketsErr := a.garage.ListBuckets(r.Context())
	if bucketsErr != nil {
		data["BucketsWarning"] = bucketsErr.Error()
	}
	jobs := a.jobViews()
	active := 0
	for _, j := range jobs {
		if j.Active {
			active++
		}
	}
	data["Buckets"] = bucketNamesFrom(buckets)
	data["Remotes"] = a.remotes.List()
	data["Jobs"] = jobs
	data["Schedules"] = a.scheduleViews()
	data["HasActive"] = active > 0
	data["ActiveCount"] = active
	data["RcloneVersion"] = a.rcloneVersion()
	data["ChunkKeys"] = syncChunkKeys
	a.render(w, r, "sync.html", data)
}

func (a *App) rcloneVersion() string {
	if a.jobs == nil || a.jobs.rclone == nil {
		return ""
	}
	return a.jobs.rclone.version
}

// handleSyncJobsJSON feeds the page's polling script.
func (a *App) handleSyncJobsJSON(w http.ResponseWriter, r *http.Request) {
	if a.syncDisabled != "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": a.syncDisabled})
		return
	}
	views := a.jobViews()
	if views == nil {
		views = []jobView{}
	}
	writeJSON(w, http.StatusOK, views)
}

// syncGuard answers the disabled notice for every mutating handler.
func (a *App) syncGuard(w http.ResponseWriter, r *http.Request) bool {
	if a.syncDisabled != "" {
		a.redirectErr(w, r, "/sync", errors.New(a.syncDisabled))
		return false
	}
	return true
}

// remoteFromForm reads the add/edit form.
func remoteFromForm(r *http.Request) Remote {
	return Remote{
		Name:       strings.TrimSpace(r.PostFormValue("name")),
		Provider:   strings.TrimSpace(r.PostFormValue("provider")),
		Endpoint:   strings.TrimSpace(r.PostFormValue("endpoint")),
		Region:     strings.TrimSpace(r.PostFormValue("region")),
		AccessKey:  r.PostFormValue("access_key"),
		SecretKey:  r.PostFormValue("secret_key"),
		Addressing: strings.TrimSpace(r.PostFormValue("addressing")),
	}
}

func (a *App) handleRemoteAdd(w http.ResponseWriter, r *http.Request) {
	if !a.syncGuard(w, r) {
		return
	}
	remote := remoteFromForm(r)
	if err := a.remotes.Add(remote); err != nil {
		a.redirectErr(w, r, "/sync", fmt.Errorf("remote tidak disimpan: %w", err))
		return
	}
	log.Printf("remote %q ditambahkan (%s)", remote.Name, remote.Endpoint)
	msg := fmt.Sprintf("Remote %q disimpan di %s (mode 0600). Klik \"Tes koneksi\" dengan nama bucket untuk memastikan endpoint, region, kredensial, dan izin tulisnya benar.", remote.Name, filepath.Join(a.cfg.StateDir, remotesFileName))
	if strings.HasPrefix(remote.Endpoint, "http://") {
		msg += " Catatan: endpoint http tanpa TLS — isi objek lewat jaringan tanpa enkripsi."
	}
	a.redirectOK(w, r, "/sync", msg)
}

func (a *App) handleRemoteUpdate(w http.ResponseWriter, r *http.Request) {
	if !a.syncGuard(w, r) {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if err := ValidateRemoteName(name); err != nil {
		a.redirectErr(w, r, "/sync", err)
		return
	}
	if a.jobs.UsesRemote(name) {
		a.redirectErr(w, r, "/sync", fmt.Errorf("remote %q sedang dipakai job yang belum selesai; jeda atau batalkan job itu dulu sebelum mengubah kredensialnya", name))
		return
	}
	remote := remoteFromForm(r)
	if err := a.remotes.Update(name, remote); err != nil {
		a.redirectErr(w, r, "/sync", fmt.Errorf("remote tidak diubah: %w", err))
		return
	}
	log.Printf("remote %q diubah (%s)", name, remote.Endpoint)
	a.redirectOK(w, r, "/sync", fmt.Sprintf("Remote %q diperbarui. Jalankan \"Tes koneksi\" lagi untuk memastikan setelan barunya bekerja.", name))
}

func (a *App) handleRemoteDelete(w http.ResponseWriter, r *http.Request) {
	if !a.syncGuard(w, r) {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if err := ValidateRemoteName(name); err != nil {
		a.redirectErr(w, r, "/sync", err)
		return
	}
	if a.jobs.UsesRemote(name) {
		a.redirectErr(w, r, "/sync", fmt.Errorf("remote %q masih dipakai job yang belum selesai; batalkan atau tunggu job itu dulu", name))
		return
	}
	if a.schedules.UsesRemote(name) {
		a.redirectErr(w, r, "/sync", fmt.Errorf("remote %q masih dipakai sebuah jadwal; hapus jadwalnya dulu", name))
		return
	}
	if err := a.remotes.Delete(name); err != nil {
		a.redirectErr(w, r, "/sync", err)
		return
	}
	log.Printf("remote %q dihapus", name)
	a.redirectOK(w, r, "/sync", fmt.Sprintf("Remote %q dihapus.", name))
}

func (a *App) handleRemoteTest(w http.ResponseWriter, r *http.Request) {
	if !a.syncGuard(w, r) {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	bucket := strings.TrimSpace(r.PostFormValue("bucket"))
	write := r.PostFormValue("write") != "0"
	if err := ValidateRemoteName(name); err != nil {
		a.redirectErr(w, r, "/sync", err)
		return
	}
	report, err := a.remotes.Probe(r.Context(), name, bucket, write)
	if err != nil {
		msg := fmt.Errorf("tes koneksi gagal: %w", err)
		if report.Switched {
			msg = fmt.Errorf("tes koneksi gagal (gaya alamat remote diganti ke %s karena itu yang diterima): %w", addressingLabel(report.Virtual), err)
		}
		a.redirectErr(w, r, "/sync", msg)
		return
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("Remote %q bisa membaca bucket %q", name, bucket))
	if write {
		parts = append(parts, "menulis dan menghapus objek uji juga berhasil")
	}
	msg := strings.Join(parts, "; ") + ". Endpoint, region, dan kredensialnya benar."
	if report.Switched {
		msg += fmt.Sprintf(" Gaya alamat yang diterima adalah %s — setelan remote sudah diganti ke situ.", addressingLabel(report.Virtual))
	}
	a.redirectOK(w, r, "/sync", msg)
}

// syncSpecFromForm reads the shared job/schedule form.
func syncSpecFromForm(r *http.Request) (JobSpec, error) {
	transfers, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("transfers")))
	if err != nil {
		return JobSpec{}, errors.New("jumlah transfer paralel harus angka")
	}
	remoteSide := Endpoint{
		Remote: strings.TrimSpace(r.PostFormValue("remote")),
		Bucket: strings.TrimSpace(r.PostFormValue("remote_bucket")),
		Prefix: folderPrefix(r.PostFormValue("remote_prefix")),
	}
	garageSide := Endpoint{
		Bucket: strings.TrimSpace(r.PostFormValue("garage_bucket")),
		Prefix: folderPrefix(r.PostFormValue("garage_prefix")),
	}
	spec := JobSpec{Kind: JobSync, Mode: strings.TrimSpace(r.PostFormValue("mode")), Transfers: transfers}
	switch r.PostFormValue("direction") {
	case "import":
		spec.Src, spec.Dst = remoteSide, garageSide
	case "export":
		spec.Src, spec.Dst = garageSide, remoteSide
	default:
		return JobSpec{}, errors.New("pilih arah: impor atau ekspor")
	}
	return spec, nil
}

func (a *App) handleJobCreate(w http.ResponseWriter, r *http.Request) {
	if !a.syncGuard(w, r) {
		return
	}
	spec, err := syncSpecFromForm(r)
	if err != nil {
		a.redirectErr(w, r, "/sync", err)
		return
	}
	a.startJob(w, r, spec, "/sync")
}

// --- schedules ---------------------------------------------------------------

// scheduleView is a Schedule dressed up for the template.
type scheduleView struct {
	Schedule
	Direction string
	SrcLabel  string
	DstLabel  string
	EveryH    string
	NextRunH  string
	Overdue   bool
	LastRun   *ScheduleRun
}

func (a *App) scheduleViews() []scheduleView {
	if a.schedules == nil {
		return nil
	}
	items := a.schedules.List()
	now := time.Now()
	out := make([]scheduleView, 0, len(items))
	for _, it := range items {
		v := scheduleView{
			Schedule:  it,
			Direction: it.Spec.Direction(),
			SrcLabel:  it.Spec.Src.String(),
			DstLabel:  it.Spec.Dst.String(),
			EveryH:    it.Every.Human(),
			NextRunH:  humanTime(it.NextRun),
			Overdue:   it.Enabled && it.NextRun.Before(now),
		}
		if len(it.Runs) > 0 {
			run := it.Runs[0]
			v.LastRun = &run
		}
		out = append(out, v)
	}
	return out
}

func (a *App) handleScheduleCreate(w http.ResponseWriter, r *http.Request) {
	if !a.syncGuard(w, r) {
		return
	}
	spec, err := syncSpecFromForm(r)
	if err != nil {
		a.redirectErr(w, r, "/sync", err)
		return
	}
	every, err := ParseEvery(r.PostFormValue("every"))
	if err != nil {
		a.redirectErr(w, r, "/sync", fmt.Errorf("jadwal tidak dibuat: %w", err))
		return
	}
	var first time.Time
	if raw := strings.TrimSpace(r.PostFormValue("first_run")); raw != "" {
		// <input type="datetime-local"> gives local wall time without a zone.
		t, err := time.ParseInLocation("2006-01-02T15:04", raw, time.Local)
		if err != nil {
			a.redirectErr(w, r, "/sync", errors.New("waktu mulai pertama tidak bisa dibaca"))
			return
		}
		first = t
	}
	item, err := a.schedules.Add(spec, every, first)
	if err != nil {
		a.redirectErr(w, r, "/sync", fmt.Errorf("jadwal tidak dibuat: %w", err))
		return
	}
	a.redirectOK(w, r, "/sync", fmt.Sprintf("Jadwal %s dibuat: %s, %s. Run pertama %s. Setiap run memeriksa kedua sisi dulu; kalau bucket-nya masih dipakai job lain, run itu dilewati dan dicoba lagi pada jadwal berikutnya.",
		item.ID, jobSummary(JobState{JobSpec: item.Spec}), item.Every.Human(), humanTime(item.NextRun)))
}

func (a *App) scheduleAction(fn func(id string) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.syncGuard(w, r) {
			return
		}
		id := strings.TrimSpace(r.PostFormValue("id"))
		if err := ValidateJobID(id); err != nil {
			a.redirectErr(w, r, "/sync", err)
			return
		}
		msg, err := fn(id)
		if err != nil {
			a.redirectErr(w, r, "/sync", err)
			return
		}
		a.redirectOK(w, r, "/sync", msg)
	}
}

func (a *App) handleScheduleToggle(w http.ResponseWriter, r *http.Request) {
	enable := r.PostFormValue("enabled") == "1"
	a.scheduleAction(func(id string) (string, error) {
		if err := a.schedules.SetEnabled(id, enable); err != nil {
			return "", err
		}
		if enable {
			return fmt.Sprintf("Jadwal %s diaktifkan.", id), nil
		}
		return fmt.Sprintf("Jadwal %s dijeda; tidak ada run baru sampai diaktifkan lagi.", id), nil
	})(w, r)
}

func (a *App) handleScheduleDelete(w http.ResponseWriter, r *http.Request) {
	a.scheduleAction(func(id string) (string, error) {
		if err := a.schedules.Delete(id); err != nil {
			return "", err
		}
		return fmt.Sprintf("Jadwal %s dihapus. Job yang sudah dimulainya tetap ada di daftar job.", id), nil
	})(w, r)
}

func (a *App) handleScheduleRun(w http.ResponseWriter, r *http.Request) {
	a.scheduleAction(func(id string) (string, error) {
		run, err := a.schedules.RunNow(r.Context(), id)
		if err != nil {
			return "", err
		}
		if !run.OK {
			return "", fmt.Errorf("jadwal %s tidak bisa dijalankan sekarang: %s", id, strings.TrimPrefix(run.Result, "dilewati: "))
		}
		return fmt.Sprintf("Jadwal %s dijalankan: %s.", id, run.Result), nil
	})(w, r)
}

// jobSummary is "Impor src → dst" or "Hapus folder src", for messages.
func jobSummary(s JobState) string {
	if s.Kind == JobDeletePrefix {
		return s.Direction() + " " + s.Src.String()
	}
	return s.Direction() + " " + s.Src.String() + " → " + s.Dst.String()
}

// folderPrefix normalises a typed prefix: trimmed, and with the trailing
// slash people tend to leave out. Validation still happens in JobManager.
func folderPrefix(s string) string {
	s = strings.TrimSpace(s)
	if s != "" && !strings.HasSuffix(s, "/") {
		s += "/"
	}
	return s
}

// startJob creates a job and redirects with the outcome. Probing both sides
// can take a while against a slow remote, so it gets its own deadline.
func (a *App) startJob(w http.ResponseWriter, r *http.Request, spec JobSpec, back string) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	state, err := a.jobs.Create(ctx, spec)
	if err != nil {
		a.redirectErr(w, r, back, fmt.Errorf("job tidak dimulai: %w", err))
		return
	}
	a.redirectOK(w, r, "/sync", fmt.Sprintf("Job %s (%s) dimulai. Halaman ini boleh ditutup; job jalan di server dan dilanjutkan otomatis setelah restart.",
		state.ID, jobSummary(state)))
}

// jobAction wraps the pause/resume/rerun/cancel/delete handlers.
func (a *App) jobAction(verb string, fn func(id string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.syncGuard(w, r) {
			return
		}
		id := strings.TrimSpace(r.PostFormValue("id"))
		if err := ValidateJobID(id); err != nil {
			a.redirectErr(w, r, "/sync", err)
			return
		}
		if err := fn(id); err != nil {
			a.redirectErr(w, r, "/sync", err)
			return
		}
		a.redirectOK(w, r, "/sync", fmt.Sprintf("Job %s: %s.", id, verb))
	}
}

// handleJobFailed shows the tail of a job's failed-key log, or streams the
// whole file as a download.
func (a *App) handleJobFailed(w http.ResponseWriter, r *http.Request) {
	if a.syncDisabled != "" {
		a.renderError(w, r, http.StatusServiceUnavailable, "Sync nonaktif", errors.New(a.syncDisabled))
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if err := ValidateJobID(id); err != nil {
		a.renderError(w, r, http.StatusBadRequest, "ID job tidak valid", err)
		return
	}
	job, ok := a.jobs.Get(id)
	if !ok {
		a.renderError(w, r, http.StatusNotFound, "Job tidak ditemukan", fmt.Errorf("job %s tidak ada", id))
		return
	}
	if r.URL.Query().Get("download") == "1" {
		path, err := a.jobs.FailedKeysPath(id)
		if err != nil {
			a.renderError(w, r, http.StatusNotFound, "Job tidak ditemukan", err)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.Error(w, "tidak ada key gagal", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "job-"+id+"-failed.jsonl"))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, "", time.Time{}, f)
		return
	}
	const tail = 500
	keys, total, err := a.jobs.FailedKeys(id, tail)
	if err != nil {
		a.renderError(w, r, http.StatusInternalServerError, "Tidak bisa membaca daftar key gagal", err)
		return
	}
	a.render(w, r, "sync_failed.html", map[string]any{
		"Title": "Key gagal — job " + id,
		"Page":  "sync",
		"Job":   newJobView(job),
		"Keys":  keys,
		"Total": total,
		"Tail":  tail,
	})
}
