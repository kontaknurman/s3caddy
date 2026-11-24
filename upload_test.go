package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (f *fakeS3) pendingUploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

// smallParts makes every file above 1 KiB a multipart upload, with the given
// limit on the panel.
func smallParts(t *testing.T, limit int64) *testPanel {
	t.Helper()
	old, oldDelay := uploadPartSize, uploadRetryDelay
	uploadPartSize, uploadRetryDelay = 1<<10, time.Millisecond
	t.Cleanup(func() { uploadPartSize, uploadRetryDelay = old, oldDelay })
	p := newTestPanel(t, func(c *Config) { c.UploadMax = limit })
	p.garage.addBucket("media", 0, 0, true)
	return p
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func TestParseByteSize(t *testing.T) {
	for in, want := range map[string]int64{
		"5G": 5 << 30, "5g": 5 << 30, "5GB": 5 << 30, "5GiB": 5 << 30, "500M": 500 << 20, "500 MiB": 500 << 20,
		"512k": 512 << 10, "1T": 1 << 40, "1048576": 1048576, "100B": 100, " 2G ": 2 << 30,
	} {
		if got, err := ParseByteSize(in); err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "G", "5X", "-5G", "5.5G", "5 G B", "iB", "5iB", "99999999999999999999", "9000000000T"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Errorf("ParseByteSize(%q) accepted", bad)
		}
	}
}

func TestUploadMaxSizeComesFromTheEnvironment(t *testing.T) {
	t.Setenv("GARAGE_ADMIN_TOKEN", "x")
	t.Setenv("UPLOAD_MAX_SIZE", "")
	cfg, err := loadConfig()
	if err != nil || cfg.uploadMax() != MaxUploadSize {
		t.Fatalf("default: %v, %v", cfg, err)
	}
	t.Setenv("UPLOAD_MAX_SIZE", "5G")
	if cfg, err = loadConfig(); err != nil || cfg.uploadMax() != 5<<30 {
		t.Fatalf("5G: %d, %v", cfg.uploadMax(), err)
	}
	for _, bad := range []string{"5000", "200G", "lima giga"} {
		t.Setenv("UPLOAD_MAX_SIZE", bad)
		if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "UPLOAD_MAX_SIZE") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestObjectsPageShowsTheConfiguredLimit(t *testing.T) {
	p := newTestPanel(t, func(c *Config) { c.UploadMax = 5 << 30 })
	p.garage.addBucket("media", 0, 0, true)
	_, body := p.get(t, "/objects?bucket=media")
	if !strings.Contains(body, "Maksimal 5.0 GiB per file") || !strings.Contains(body, `data-max="5368709120"`) {
		t.Error("objects page does not show the configured upload limit")
	}
}

func TestLargeUploadUnderItsOwnNameGoesInParts(t *testing.T) {
	p := smallParts(t, 1<<20)
	content := pattern(10*1024 + 300)
	res := p.uploadWith(t, "media", "video.mp4", content, map[string]string{"mode": "name", "prefix": "klip/"})
	if !res.OK || res.Key != "klip/video.mp4" || res.Size != int64(len(content)) {
		t.Fatalf("upload = %+v", res)
	}
	if got, _ := p.s3.get("media", "klip/video.mp4"); !bytes.Equal(got, content) {
		t.Error("stored content differs")
	}
	if ct := p.s3.contentType("media", "klip/video.mp4"); ct != "video/mp4" {
		t.Errorf("content type = %q", ct)
	}
	if n := p.s3.callCount("UploadPart"); n != 11 {
		t.Errorf("UploadPart calls = %d, want 11", n)
	}
	if p.s3.callCount("PutObject") != 0 || p.s3.callCount("CompleteMultipartUpload") != 1 || p.s3.pendingUploads() != 0 {
		t.Error("a large file must go as one completed multipart upload")
	}
}

func TestLargeContentAddressedUploadIsCopiedToItsHash(t *testing.T) {
	p := smallParts(t, 1<<20)
	content := pattern(5000)
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])[:16] + ".zip"

	first := p.upload(t, "media", "Arsip.ZIP", content)
	if !first.OK || first.Key != want || first.Deduped || first.Warning != "" {
		t.Fatalf("first upload = %+v", first)
	}
	if got, _ := p.s3.get("media", want); !bytes.Equal(got, content) {
		t.Error("stored content differs")
	}
	if keys := strings.Join(p.s3.keys("media"), ","); keys != want {
		t.Errorf("bucket holds %s; the temporary object must be gone", keys)
	}
	if p.s3.callCount("CopyObject") != 1 {
		t.Error("the upload must be copied to its name once")
	}

	second := p.upload(t, "media", "salinan.zip", content)
	if !second.OK || second.Key != want || !second.Deduped {
		t.Errorf("second upload = %+v", second)
	}
	if keys := strings.Join(p.s3.keys("media"), ","); keys != want || p.s3.callCount("CopyObject") != 1 {
		t.Errorf("identical content: bucket holds %s after %d copies", keys, p.s3.callCount("CopyObject"))
	}
}

func TestSmallUploadsStillUseOnePut(t *testing.T) {
	p := newTestPanel(t)
	p.garage.addBucket("media", 0, 0, true)
	res := p.upload(t, "media", "a.png", pattern(4096))
	if !res.OK || p.s3.callCount("PutObject") != 1 || p.s3.callCount("CreateMultipartUpload") != 0 {
		t.Errorf("upload = %+v, PutObject %d, multipart %d", res, p.s3.callCount("PutObject"), p.s3.callCount("CreateMultipartUpload"))
	}
}

func TestLargeUploadOverTheLimitLeavesNothing(t *testing.T) {
	p := smallParts(t, 4<<10)
	res := p.uploadWith(t, "media", "big.mp4", pattern(10<<10), map[string]string{"mode": "name"})
	if res.OK || !strings.Contains(res.Error, "lebih besar dari batas 4.0 KiB") {
		t.Fatalf("upload = %+v", res)
	}
	if p.s3.count("media") != 0 || p.s3.pendingUploads() != 0 {
		t.Error("an oversize upload must leave neither an object nor an open multipart upload")
	}
}

func TestUploadWithATooLongBodyIsRefusedBeforeReading(t *testing.T) {
	p := newTestPanel(t, func(c *Config) { c.UploadMax = 64 << 10 })
	p.garage.addBucket("media", 0, 0, true)
	res := p.uploadWith(t, "media", "big.mp4", pattern(2<<20), map[string]string{"mode": "name"})
	if res.OK || !strings.Contains(res.Error, "lebih besar dari batas 64.0 KiB") {
		t.Fatalf("upload = %+v", res)
	}
	if p.s3.callCount("HeadObject") != 0 {
		t.Error("an oversize request must be refused before anything else")
	}

	// A declared length over the limit is answered before the body is sent:
	// the panel must not wait for gigabytes it will refuse.
	u, _ := url.Parse(p.srv.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	token := p.csrf(t)
	var cookies []string
	for _, c := range p.client.Jar.Cookies(u) {
		cookies = append(cookies, c.Name+"="+c.Value)
	}
	io.WriteString(conn, "POST /upload HTTP/1.1\r\nHost: "+u.Host+"\r\nContent-Type: multipart/form-data; boundary=x"+
		"\r\nContent-Length: 10737418240\r\nX-CSRF-Token: "+token+"\r\nCookie: "+strings.Join(cookies, "; ")+"\r\n\r\n--x\r\n")
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	status := make([]byte, 12)
	if _, err := io.ReadFull(conn, status); err != nil || string(status) != "HTTP/1.1 413" {
		t.Errorf("answer to a 10 GiB declaration: %q, %v", status, err)
	}
}

func TestLargeUploadRefusesATakenNameBeforeSendingParts(t *testing.T) {
	p := smallParts(t, 1<<20)
	p.s3.put("media", "video.mp4", []byte("lama"), "video/mp4")
	res := p.uploadWith(t, "media", "video.mp4", pattern(8<<10), map[string]string{"mode": "name"})
	if res.OK || !strings.Contains(res.Error, "sudah ada") {
		t.Fatalf("upload = %+v", res)
	}
	if p.s3.callCount("CreateMultipartUpload") != 0 {
		t.Error("no part may be sent for a name that is taken")
	}

	res = p.uploadWith(t, "media", "video.mp4", pattern(8<<10), map[string]string{"mode": "name", "overwrite": "1"})
	if !res.OK || !res.Overwritten {
		t.Fatalf("overwrite = %+v", res)
	}
	if got, _ := p.s3.get("media", "video.mp4"); !bytes.Equal(got, pattern(8<<10)) {
		t.Error("overwrite did not store the new content")
	}
}

func TestPartThatFailsOnceIsSentAgain(t *testing.T) {
	p := smallParts(t, 1<<20)
	p.s3.failOps["UploadPart"] = 1
	content := pattern(8 << 10)
	res := p.uploadWith(t, "media", "video.mp4", content, map[string]string{"mode": "name"})
	if !res.OK {
		t.Fatalf("upload = %+v", res)
	}
	if got, _ := p.s3.get("media", "video.mp4"); !bytes.Equal(got, content) {
		t.Error("stored content differs")
	}
	if n := p.s3.callCount("UploadPart"); n != 9 {
		t.Errorf("UploadPart calls = %d, want 8 parts and one repeat", n)
	}
}

func TestPartThatKeepsFailingAbortsTheUpload(t *testing.T) {
	p := smallParts(t, 1<<20)
	p.s3.failOps["UploadPart"] = uploadPartAttempts
	res := p.uploadWith(t, "media", "video.mp4", pattern(8<<10), map[string]string{"mode": "name"})
	if res.OK || !strings.Contains(res.Error, "UploadPart") {
		t.Fatalf("upload = %+v", res)
	}
	if p.s3.callCount("UploadPart") != uploadPartAttempts {
		t.Errorf("UploadPart calls = %d, want %d", p.s3.callCount("UploadPart"), uploadPartAttempts)
	}
	if p.s3.count("media") != 0 || p.s3.pendingUploads() != 0 || p.s3.callCount("AbortMultipartUpload") != 1 {
		t.Error("a part that keeps failing must abort the upload")
	}
}

func TestRetryablePartError(t *testing.T) {
	for err, want := range map[error]bool{
		&S3Error{StatusCode: 500}: true,
		&S3Error{StatusCode: 503}: true,
		&S3Error{StatusCode: 429}: true,
		&S3Error{StatusCode: 408}: true,
		&S3Error{StatusCode: 403}: false,
		&S3Error{StatusCode: 400}: false,
		io.ErrUnexpectedEOF:       true,
	} {
		if got := retryablePartError(err); got != want {
			t.Errorf("retryablePartError(%v) = %v, want %v", err, got, want)
		}
	}
}

// truncatedUpload sends the fields and the first n bytes of a file, then
// breaks the connection, the way a closed tab or a dropped tunnel does.
func truncatedUpload(t *testing.T, p *testPanel, mode string, size, n int) {
	t.Helper()
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	_ = mw.WriteField("bucket", "media")
	_ = mw.WriteField("mode", mode)
	fw, _ := mw.CreateFormFile("file", "video.mp4")
	content := pattern(size)
	fw.Write(content[:n])

	u, _ := url.Parse(p.srv.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	token := p.csrf(t)
	var cookies []string
	for _, c := range p.client.Jar.Cookies(u) {
		cookies = append(cookies, c.Name+"="+c.Value)
	}
	// The declared length is the whole body; only part of it is sent.
	total := head.Len() + (size - n) + len("\r\n--"+mw.Boundary()+"--\r\n")
	req := "POST /upload HTTP/1.1\r\nHost: " + u.Host + "\r\nContent-Type: " + mw.FormDataContentType() +
		"\r\nContent-Length: " + strconv.Itoa(total) + "\r\nX-CSRF-Token: " + token +
		"\r\nCookie: " + strings.Join(cookies, "; ") + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(head.Bytes()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	conn.Close()
}

func TestUploadCutOffHalfWayStoresNothing(t *testing.T) {
	for _, c := range []struct {
		name          string
		mode          string
		size, sent    int
		wantMultipart bool
	}{
		{"small file", "name", 900, 500, false},
		{"large file", "name", 10 << 10, 6 << 10, true},
		{"large content-addressed file", "hash", 10 << 10, 6 << 10, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := smallParts(t, 1<<20)
			truncatedUpload(t, p, c.mode, c.size, c.sent)
			deadline := time.Now().Add(5 * time.Second)
			for c.wantMultipart && p.s3.callCount("AbortMultipartUpload") == 0 && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			time.Sleep(100 * time.Millisecond)
			if p.s3.count("media") != 0 || p.s3.pendingUploads() != 0 {
				t.Errorf("a cut-off upload left %v and %d open multipart uploads", p.s3.keys("media"), p.s3.pendingUploads())
			}
			if c.wantMultipart && p.s3.callCount("AbortMultipartUpload") != 1 {
				t.Error("the multipart upload was not aborted")
			}
			if p.s3.callCount("PutObject")+p.s3.callCount("CompleteMultipartUpload") != 0 {
				t.Error("part of a file was stored as if it were whole")
			}
		})
	}
}

// The server's WriteTimeout would cut a slow upload's answer off, the
// successful one and the refusal alike; the handler lifts it.
func TestSlowUploadOutlivesTheServerWriteTimeout(t *testing.T) {
	old := uploadPartSize
	uploadPartSize = 1 << 10
	t.Cleanup(func() { uploadPartSize = old })
	p := newTestPanel(t, func(c *Config) { c.UploadMax = 1 << 20 })
	p.garage.addBucket("media", 0, 0, true)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: p.app.routes(), WriteTimeout: 500 * time.Millisecond}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	// slowPost sends body in 1500-byte pieces, 200 ms apart.
	slowPost := func(body []byte, contentType string) uploadResult {
		t.Helper()
		pr, pw := io.Pipe()
		go func() {
			for raw := body; len(raw) > 0; {
				n := min(len(raw), 1500)
				if _, err := pw.Write(raw[:n]); err != nil {
					return
				}
				raw = raw[n:]
				time.Sleep(200 * time.Millisecond)
			}
			pw.Close()
		}()
		req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/upload", pr)
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("X-CSRF-Token", strings.Repeat("a", 64))
		req.AddCookie(&http.Cookie{Name: "garagepanel_csrf", Value: strings.Repeat("a", 64)})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("slow upload: %v", err)
		}
		defer resp.Body.Close()
		var res uploadResult
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			t.Fatalf("slow upload: answer is not JSON: %v", err)
		}
		return res
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("bucket", "media")
	_ = mw.WriteField("mode", "name")
	fw, _ := mw.CreateFormFile("file", "lambat.mp4")
	content := pattern(6 << 10)
	fw.Write(content)
	mw.Close()
	if res := slowPost(body.Bytes(), mw.FormDataContentType()); !res.OK {
		t.Fatalf("slow upload: %+v", res)
	}
	if got, _ := p.s3.get("media", "lambat.mp4"); !bytes.Equal(got, content) {
		t.Error("stored content differs")
	}

	// A slow request without a file is refused, and the refusal arrives too.
	body.Reset()
	mw = multipart.NewWriter(&body)
	_ = mw.WriteField("bucket", "media")
	for _, name := range []string{"catatan", "keterangan", "label"} {
		_ = mw.WriteField(name, strings.Repeat("x", 4000))
	}
	mw.Close()
	if res := slowPost(body.Bytes(), mw.FormDataContentType()); res.OK || res.Error != "tidak ada file pada request" {
		t.Errorf("slow request without a file: %+v", res)
	}
}

func TestStalledUploadIsDropped(t *testing.T) {
	oldPart, oldStall := uploadPartSize, uploadStallTimeout
	uploadPartSize, uploadStallTimeout = 1<<10, 300*time.Millisecond
	t.Cleanup(func() { uploadPartSize, uploadStallTimeout = oldPart, oldStall })
	p := newTestPanel(t, func(c *Config) { c.UploadMax = 1 << 20 })
	p.garage.addBucket("media", 0, 0, true)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("bucket", "media")
	_ = mw.WriteField("mode", "name")
	fw, _ := mw.CreateFormFile("file", "macet.mp4")
	fw.Write(pattern(6 << 10))
	mw.Close()
	raw := body.Bytes()

	pr, pw := io.Pipe()
	go func() {
		pw.Write(raw[:len(raw)/2])
		time.Sleep(3 * time.Second) // stalls far longer than the timeout
		pw.CloseWithError(io.ErrClosedPipe)
	}()
	req, _ := http.NewRequest(http.MethodPost, p.srv.URL+"/upload", pr)
	req.ContentLength = int64(len(raw))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", p.csrf(t))
	start := time.Now()
	resp, err := p.client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	if time.Since(start) > 2500*time.Millisecond {
		t.Errorf("the stalled upload was held for %v", time.Since(start))
	}
	deadline := time.Now().Add(3 * time.Second)
	for p.s3.callCount("AbortMultipartUpload") == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if p.s3.count("media") != 0 || p.s3.pendingUploads() != 0 {
		t.Error("a stalled upload must leave nothing behind")
	}
}

func TestUploadCheckAnswersBeforeTheFileIsSent(t *testing.T) {
	p := newTestPanel(t, func(c *Config) { c.UploadMax = 10 << 20 })
	p.garage.addBucket("media", 0, 0, true)
	p.s3.put("media", "foto/ada.jpg", []byte("x"), "image/jpeg")
	check := func(q string) (int, uploadResult) {
		t.Helper()
		resp, body := p.get(t, "/upload/check?"+q)
		var res uploadResult
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			t.Fatalf("%s: not JSON: %s", q, body)
		}
		return resp.StatusCode, res
	}
	cases := []struct {
		q    string
		code int
		want string
	}{
		{"bucket=media&prefix=foto/&mode=name&name=baru.jpg&size=1000", 200, ""},
		{"bucket=media&prefix=foto/&mode=hash&name=ada.jpg&size=1000", 200, ""},
		{"bucket=media&prefix=foto/&mode=name&name=ada.jpg&size=1000", 409, "sudah ada"},
		{"bucket=media&prefix=foto/&mode=name&name=ada.jpg&size=1000&overwrite=1", 200, ""},
		{"bucket=media&mode=name&name=a.php&size=10", 400, "tidak diizinkan"},
		{"bucket=media&mode=name&name=a.jpg&size=" + strconv.Itoa(11<<20), 413, "lebih besar dari batas 10.0 MiB"},
		{"bucket=media&mode=name&name=a.jpg&size=0", 400, "file kosong"},
		{"bucket=media&mode=name&name=a.jpg&size=x", 400, "ukuran file tidak valid"},
		{"bucket=Media!&mode=name&name=a.jpg&size=1", 400, "nama bucket"},
	}
	for _, c := range cases {
		code, res := check(c.q)
		if code != c.code || res.OK != (c.code == 200) || !strings.Contains(res.Error, c.want) {
			t.Errorf("%s: %d %+v, want %d %q", c.q, code, res, c.code, c.want)
		}
	}
	if p.s3.callCount("PutObject")+p.s3.callCount("CreateMultipartUpload") != 0 {
		t.Error("a check must not store anything")
	}
}

func TestUploadCheckWithoutSessionAnswersJSON(t *testing.T) {
	p := newTestPanel(t, withLogin(t))
	resp, body := p.get(t, "/upload/check?bucket=media&mode=name&name=a.jpg&size=1")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("Content-Type"), "application/json") ||
		!strings.Contains(body, "session habis") {
		t.Errorf("status %d, %s", resp.StatusCode, body)
	}
}
