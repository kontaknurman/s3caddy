package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Uploads from the Objek page.
//
// The browser sends one file per request as multipart/form-data, the form
// fields first and the file last, and the body is read as a stream. A file
// that fits in one part (50 MiB) is held in memory and stored with a single
// PutObject, as uploads always were. A larger one goes to Garage as a
// multipart upload, one part at a time, so the panel holds one part in memory
// whatever the size of the file, and writes nothing to disk. A
// content-addressed name needs the sha256 of the whole file, so such a file is
// uploaded under a temporary key and copied to its name at the end.

var (
	// uploadPartSize is the part size of large uploads, and the largest file
	// stored with a single PutObject. Tests lower it.
	uploadPartSize = 50 << 20
	// uploadStallTimeout is how long an upload may send nothing before the
	// panel gives up on it.
	uploadStallTimeout = 2 * time.Minute
	// uploadPartTimeout bounds one attempt at sending a part to Garage, and
	// uploadRetryDelay is the pause before the next attempt, growing with
	// each one.
	uploadPartTimeout = 2 * time.Minute
	uploadRetryDelay  = time.Second
)

// uploadPartAttempts is how often a part is sent before the upload fails.
const uploadPartAttempts = 3

const (
	// uploadFieldMax bounds one form field sent before the file.
	uploadFieldMax = 4 << 10
	// uploadOverhead is what the multipart framing and the fields may add to
	// the size of the file in the request body.
	uploadOverhead = 1 << 20
	// uploadFinishTimeout bounds what happens after the last byte arrived:
	// completing the upload and copying it to its name.
	uploadFinishTimeout = 10 * time.Minute
	// maxUploadParts is the most parts S3 allows in one multipart upload.
	maxUploadParts = 10000
	// uploadTempPrefix names the temporary object of a large
	// content-addressed upload, at the root of the bucket.
	uploadTempPrefix = ".garagepanel-upload-"
)

// errUploadTooLarge is a file that turned out larger than the limit.
var errUploadTooLarge = errors.New("file lebih besar dari batas")

// clientError is a failure on the browser's side of an upload: the body ended
// early, stalled, or could not be read.
type clientError struct{ err error }

func (e clientError) Error() string { return "gagal membaca file: " + e.err.Error() }
func (e clientError) Unwrap() error { return e.err }

// uploadRequest is what the form fields and the file name of an upload say.
type uploadRequest struct {
	Bucket, Prefix, Mode string
	Overwrite            bool
	BaseName, Ext        string
	ContentType          string
}

// parseUploadRequest validates the fields of an upload, read through get, and
// the name of its file.
func parseUploadRequest(get func(string) string, filename string) (uploadRequest, error) {
	u := uploadRequest{
		Bucket:    strings.TrimSpace(get("bucket")),
		Prefix:    get("prefix"),
		Mode:      get("mode"),
		Overwrite: get("overwrite") == "1",
	}
	if err := ValidateBucketName(u.Bucket); err != nil {
		return u, err
	}
	// Files land in the folder being browsed. An empty prefix is the root.
	if err := ValidateJobPrefix(u.Prefix); err != nil {
		return u, err
	}
	if u.Mode == "" {
		u.Mode = "hash"
	}
	if u.Mode != "hash" && u.Mode != "name" {
		return u, errors.New("mode penamaan harus hash atau name")
	}
	// Only the base name is ever looked at; directories in the client's
	// path are dropped before any validation.
	u.BaseName = path.Base(strings.ReplaceAll(filename, `\`, `/`))
	var err error
	if u.Mode == "name" {
		u.Ext, u.ContentType, err = ValidateObjectBaseName(u.BaseName)
	} else {
		u.Ext, u.ContentType, err = ValidateUploadName(u.BaseName)
	}
	return u, err
}

// nameKey is the key of an upload that keeps the file's own name.
func (u uploadRequest) nameKey() string { return u.Prefix + u.BaseName }

// hashKey is the key of a content-addressed upload with the given sha256.
func (u uploadRequest) hashKey(sum []byte) string {
	return u.Prefix + hex.EncodeToString(sum)[:16] + u.Ext
}

func existsMessage(name string) string {
	return fmt.Sprintf("%s sudah ada di folder ini — centang \"timpa yang sudah ada\" kalau memang mau menggantinya", name)
}

func tooLargeMessage(max int64) string {
	return fmt.Sprintf("file lebih besar dari batas %s", humanBytes(max))
}

// stallReader renews the connection's read deadline as the body is read, so
// an upload may take as long as it needs while data keeps coming, but one
// that stalls is dropped after timeout.
type stallReader struct {
	io.ReadCloser
	rc      *http.ResponseController
	timeout time.Duration
	renewed time.Time
}

func (s *stallReader) Read(p []byte) (int, error) {
	if now := time.Now(); now.Sub(s.renewed) > s.timeout/4 {
		_ = s.rc.SetReadDeadline(now.Add(s.timeout))
		s.renewed = now
	}
	return s.ReadCloser.Read(p)
}

// fill reads into buf until it is full or src ends. It returns io.EOF only
// when src ended cleanly; any other error, io.ErrUnexpectedEOF included,
// means the file did not arrive whole. (io.ReadFull cannot tell the two
// apart.)
func fill(src io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := src.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	if a.s3 == nil {
		writeJSON(w, http.StatusServiceUnavailable, uploadResult{Error: "kredensial S3 panel belum diatur"})
		return
	}
	max := a.cfg.uploadMax()
	if r.ContentLength > max+uploadOverhead {
		writeJSON(w, http.StatusRequestEntityTooLarge, uploadResult{Error: tooLargeMessage(max)})
		return
	}

	// The server's WriteTimeout counts from the request headers, and a file of
	// gigabytes takes longer than that to arrive. Instead the upload may stall
	// for uploadStallTimeout at most, renewed while data flows.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		log.Printf("upload: batas waktu koneksi tidak bisa diatur (%v); upload yang lebih lama dari 10 menit akan terputus", err)
	}
	r.Body = &stallReader{ReadCloser: http.MaxBytesReader(w, r.Body, max+uploadOverhead), rc: rc, timeout: uploadStallTimeout}

	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, uploadResult{Error: "upload ditolak: " + err.Error()})
		return
	}
	fields := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			writeJSON(w, http.StatusBadRequest, uploadResult{Error: "tidak ada file pada request"})
			return
		}
		if err != nil {
			writeUploadFailure(w, "", clientError{err}, max)
			return
		}
		if part.FormName() == "file" {
			res, code := a.storeUpload(r, fields, part, max)
			_ = rc.SetWriteDeadline(time.Now().Add(time.Minute))
			writeJSON(w, code, res)
			return
		}
		value, err := io.ReadAll(io.LimitReader(part, uploadFieldMax+1))
		if err != nil {
			writeUploadFailure(w, "", clientError{err}, max)
			return
		}
		if len(value) > uploadFieldMax {
			writeJSON(w, http.StatusBadRequest, uploadResult{Error: fmt.Sprintf("field %q terlalu panjang", part.FormName())})
			return
		}
		fields[part.FormName()] = string(value)
	}
}

// uploadFailure turns a failed upload into its answer.
func uploadFailure(key string, err error, max int64) (uploadResult, int) {
	var tooBig *http.MaxBytesError
	var client clientError
	switch {
	case errors.As(err, &tooBig) || errors.Is(err, errUploadTooLarge):
		return uploadResult{Error: tooLargeMessage(max)}, http.StatusRequestEntityTooLarge
	case errors.As(err, &client):
		return uploadResult{Error: err.Error()}, http.StatusBadRequest
	}
	return uploadResult{Key: key, Error: err.Error()}, http.StatusBadGateway
}

func writeUploadFailure(w http.ResponseWriter, key string, err error, max int64) {
	res, code := uploadFailure(key, err, max)
	writeJSON(w, code, res)
}

// storeUpload stores the file part of an upload, once the fields sent before
// it are known.
func (a *App) storeUpload(r *http.Request, fields map[string]string, file *multipart.Part, max int64) (uploadResult, int) {
	ctx := r.Context()
	u, err := parseUploadRequest(func(k string) string { return fields[k] }, file.FileName())
	if err != nil {
		return uploadResult{Error: err.Error()}, http.StatusBadRequest
	}

	// The request's length bounds the file, so a small file never gets a
	// whole part's buffer.
	size := int64(uploadPartSize)
	if r.ContentLength > 0 && r.ContentLength < size {
		size = r.ContentLength
	}
	buf := make([]byte, size)
	n, err := fill(file, buf)
	more := false
	switch {
	case err == io.EOF:
	case err != nil:
		return uploadFailure("", clientError{err}, max)
	default:
		// The buffer is full: one more byte tells whether the file goes on.
		var peek [1]byte
		m, err := fill(file, peek[:])
		switch {
		case err == io.EOF:
		case err != nil:
			return uploadFailure("", clientError{err}, max)
		default:
			more = m == 1
		}
		if more {
			if int64(len(buf)) >= max {
				return uploadResult{Error: tooLargeMessage(max)}, http.StatusRequestEntityTooLarge
			}
			res, code := a.storeLarge(ctx, u, buf, io.MultiReader(bytes.NewReader(peek[:]), file), max)
			return a.withLinks(u.Bucket, res), code
		}
	}
	if n == 0 {
		return uploadResult{Error: "file kosong"}, http.StatusBadRequest
	}
	if int64(n) > max {
		return uploadResult{Error: tooLargeMessage(max)}, http.StatusRequestEntityTooLarge
	}
	res, code := a.storeSmall(ctx, u, buf[:n])
	return a.withLinks(u.Bucket, res), code
}

// withLinks adds the public URL and the preview link to a stored upload.
func (a *App) withLinks(bucket string, res uploadResult) uploadResult {
	if !res.OK {
		return res
	}
	sites, _ := a.caddy.ListSites()
	if domain := FirstDomainForBucket(sites, bucket); domain != "" {
		res.PublicURL = publicURL(domain, res.Key)
	}
	res.Preview = "/preview?bucket=" + url.QueryEscape(bucket) + "&key=" + url.QueryEscape(res.Key)
	return res
}

// storeSmall stores a file that fits in one part with a single PutObject, the
// way uploads always worked.
func (a *App) storeSmall(ctx context.Context, u uploadRequest, content []byte) (uploadResult, int) {
	key := u.nameKey()
	if u.Mode == "hash" {
		// Content addressing: the name is derived from the bytes, so
		// uploading the same file twice cannot create a duplicate.
		sum := sha256.Sum256(content)
		key = u.hashKey(sum[:])
	}
	if err := ValidateObjectKey(key); err != nil {
		return uploadResult{Error: err.Error()}, http.StatusBadRequest
	}

	res := uploadResult{Key: key, Size: int64(len(content))}
	exists, _, err := a.s3.StatObject(ctx, u.Bucket, key)
	if err != nil {
		return uploadResult{Key: key, Error: err.Error()}, http.StatusBadGateway
	}
	switch {
	case exists && u.Mode == "hash":
		// Same bytes, same name: nothing to store.
		res.Deduped = true
	case exists && !u.Overwrite:
		return uploadResult{Key: key, Error: existsMessage(u.BaseName)}, http.StatusConflict
	default:
		if err := a.s3.PutObject(ctx, u.Bucket, key, content, u.ContentType); err != nil {
			return uploadResult{Key: key, Error: err.Error()}, http.StatusBadGateway
		}
		res.Overwritten = exists
	}
	res.OK = true
	return res, http.StatusOK
}

// storeLarge stores a file larger than one part as a multipart upload. buf
// holds the first part and rest the remainder of the file.
func (a *App) storeLarge(ctx context.Context, u uploadRequest, buf []byte, rest io.Reader, max int64) (uploadResult, int) {
	if u.Mode == "name" {
		key := u.nameKey()
		if err := ValidateObjectKey(key); err != nil {
			return uploadResult{Error: err.Error()}, http.StatusBadRequest
		}
		exists, _, err := a.s3.StatObject(ctx, u.Bucket, key)
		if err != nil {
			return uploadResult{Key: key, Error: err.Error()}, http.StatusBadGateway
		}
		if exists && !u.Overwrite {
			return uploadResult{Key: key, Error: existsMessage(u.BaseName)}, http.StatusConflict
		}
		size, _, err := a.putMultipart(ctx, u.Bucket, key, u.ContentType, buf, rest, max)
		if err != nil {
			return uploadFailure(key, err, max)
		}
		return uploadResult{OK: true, Key: key, Size: size, Overwritten: exists}, http.StatusOK
	}

	// The name comes from the sha256 of the whole file, known only at the
	// end: upload under a temporary key, then copy it to its name.
	tmp := uploadTempPrefix + randomHex(8)
	size, sum, err := a.putMultipart(ctx, u.Bucket, tmp, u.ContentType, buf, rest, max)
	if err != nil {
		return uploadFailure("", err, max)
	}
	// The whole file is in; the rest must not stop because the browser gave up
	// waiting.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uploadFinishTimeout)
	defer cancel()

	key := u.hashKey(sum)
	res, code := uploadResult{OK: true, Key: key, Size: size}, http.StatusOK
	if err := ValidateObjectKey(key); err != nil {
		res, code = uploadResult{Error: err.Error()}, http.StatusBadRequest
	} else if exists, _, err := a.s3.StatObject(fctx, u.Bucket, key); err != nil {
		res, code = uploadResult{Key: key, Error: err.Error()}, http.StatusBadGateway
	} else if exists {
		// Same bytes, same name: the copy just uploaded is not needed.
		res.Deduped = true
	} else if err := a.s3.CopyObject(fctx, u.Bucket, tmp, u.Bucket, key); err != nil {
		res, code = uploadResult{Key: key, Error: err.Error()}, http.StatusBadGateway
	}
	if err := a.s3.DeleteObject(fctx, u.Bucket, tmp); err != nil {
		log.Printf("upload ke bucket %q: objek sementara %s tidak terhapus: %v", u.Bucket, tmp, err)
		res.Warning = fmt.Sprintf("objek sementara %s di root bucket tidak terhapus (%v); hapus manual", tmp, err)
	}
	return res, code
}

// putMultipart stores buf, which holds the first part, followed by rest as a
// multipart upload of bucket/key, reusing buf for every part. It returns the
// size and sha256 of what it stored. Unless the upload completes it is
// aborted, so a failure leaves nothing behind.
func (a *App) putMultipart(ctx context.Context, bucket, key, contentType string, buf []byte, rest io.Reader, max int64) (int64, []byte, error) {
	id, err := a.s3.CreateMultipartUpload(ctx, bucket, key, contentType)
	if err != nil {
		return 0, nil, err
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := a.s3.AbortMultipartUpload(actx, bucket, key, id); err != nil {
			log.Printf("upload ke bucket %q: multipart upload %s untuk %s tidak bisa dibatalkan: %v", bucket, id, key, err)
		}
	}()

	hash := sha256.New()
	var parts []CompletedPart
	var size int64
	for n := len(buf); n > 0; {
		size += int64(n)
		if size > max || len(parts) == maxUploadParts {
			return 0, nil, errUploadTooLarge
		}
		hash.Write(buf[:n])
		etag, err := a.uploadPart(ctx, bucket, key, id, len(parts)+1, buf[:n])
		if err != nil {
			return 0, nil, err
		}
		parts = append(parts, CompletedPart{PartNumber: len(parts) + 1, ETag: etag})
		n, err = fill(rest, buf)
		if err != nil && err != io.EOF {
			return 0, nil, clientError{err}
		}
	}

	// Every byte is in: completing must not fail because the browser stopped
	// waiting.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uploadFinishTimeout)
	defer cancel()
	if err := a.s3.CompleteMultipartUpload(cctx, bucket, key, id, parts); err != nil {
		return 0, nil, err
	}
	completed = true
	return size, hash.Sum(nil), nil
}

// uploadPart sends one part, again when the failure may pass: no answer in
// time, a lost connection, a 5xx. The part is still in memory, and sending a
// part number again replaces it, so one hiccup does not cost the gigabytes
// already sent.
func (a *App) uploadPart(ctx context.Context, bucket, key, id string, num int, data []byte) (string, error) {
	for attempt := 1; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, uploadPartTimeout)
		etag, err := a.s3.UploadPart(actx, bucket, key, id, num, data)
		cancel()
		if err == nil {
			return etag, nil
		}
		if attempt == uploadPartAttempts || ctx.Err() != nil || !retryablePartError(err) {
			return "", err
		}
		log.Printf("upload ke bucket %q: bagian %d dari %s gagal, dicoba lagi: %v", bucket, num, key, err)
		select {
		case <-ctx.Done():
			return "", err
		case <-time.After(time.Duration(attempt) * uploadRetryDelay):
		}
	}
}

// retryablePartError reports whether sending a part again might work.
func retryablePartError(err error) bool {
	var s3err *S3Error
	if errors.As(err, &s3err) {
		return s3err.StatusCode >= 500 || s3err.StatusCode == http.StatusRequestTimeout || s3err.StatusCode == http.StatusTooManyRequests
	}
	return true // no answer at all: a timeout or a lost connection
}

// handleUploadCheck tells the uploader, before it sends a file, whether
// /upload would take it: the same checks of the fields and the file name, the
// size limit, and for an upload under the file's own name without overwrite,
// whether that name is taken. Finding any of that out only after sending
// gigabytes would waste the upload. /upload checks everything again.
func (a *App) handleUploadCheck(w http.ResponseWriter, r *http.Request) {
	if a.s3 == nil {
		writeJSON(w, http.StatusServiceUnavailable, uploadResult{Error: "kredensial S3 panel belum diatur"})
		return
	}
	q := r.URL.Query()
	u, err := parseUploadRequest(q.Get, q.Get("name"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, uploadResult{Error: err.Error()})
		return
	}
	size, err := strconv.ParseInt(q.Get("size"), 10, 64)
	if err != nil || size < 0 {
		writeJSON(w, http.StatusBadRequest, uploadResult{Error: "ukuran file tidak valid"})
		return
	}
	if size == 0 {
		writeJSON(w, http.StatusBadRequest, uploadResult{Error: "file kosong"})
		return
	}
	if max := a.cfg.uploadMax(); size > max {
		writeJSON(w, http.StatusRequestEntityTooLarge, uploadResult{Error: tooLargeMessage(max)})
		return
	}
	res := uploadResult{OK: true}
	if u.Mode == "name" {
		key := u.nameKey()
		if err := ValidateObjectKey(key); err != nil {
			writeJSON(w, http.StatusBadRequest, uploadResult{Error: err.Error()})
			return
		}
		exists, _, err := a.s3.StatObject(r.Context(), u.Bucket, key)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, uploadResult{Key: key, Error: err.Error()})
			return
		}
		if exists && !u.Overwrite {
			writeJSON(w, http.StatusConflict, uploadResult{Key: key, Error: existsMessage(u.BaseName)})
			return
		}
		res.Key = key
	}
	writeJSON(w, http.StatusOK, res)
}
