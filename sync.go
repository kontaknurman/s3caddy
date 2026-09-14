package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"time"
)

// The panel decides *what* to copy; rclone copies it. Both sides of a job are
// listed page by page in key order and merged like a merge sort, which needs
// two listing requests per 1000 objects and never a HEAD per object. Keys
// that must be transferred are written to a chunk file for one rclone run.
// After a chunk finishes, the last key of that chunk is the checkpoint: a
// restart lists both sides again from `start-after = checkpoint`.

const (
	// syncPageSize is the listing page size on both sides.
	syncPageSize = 1000
	// syncChunkKeys is how many keys one rclone run gets. Smaller chunks
	// checkpoint more often; larger ones spawn fewer processes.
	syncChunkKeys = 10000
	// listRetries is how often a failing listing request is retried.
	listRetries = 5
)

// ListingOrderError reports a provider that returned keys out of order. The
// merge-join depends on byte order, so this stops the job rather than
// silently mis-comparing (or looping forever on a resume).
type ListingOrderError struct {
	Side, Prev, Got string
}

func (e *ListingOrderError) Error() string {
	return fmt.Sprintf("listing %s tidak terurut: %q datang setelah %q. S3 wajib mengembalikan key terurut byte-wise; job dihentikan supaya tidak salah banding", e.Side, e.Got, e.Prev)
}

// keyIterator walks one bucket/prefix in key order, one page at a time, and
// verifies the order it gets back.
type keyIterator struct {
	c          *S3
	side       string // "sumber" or "tujuan", for messages
	bucket     string
	prefix     string
	startAfter string
	token      string
	buf        []ObjectEntry
	pos        int
	exhausted  bool
	last       string
	pages      int64
}

func newKeyIterator(c *S3, side, bucket, prefix, startAfter string) *keyIterator {
	return &keyIterator{c: c, side: side, bucket: bucket, prefix: prefix, startAfter: startAfter, last: startAfter}
}

// fill loads the next page. It returns false once the listing is exhausted.
func (it *keyIterator) fill(ctx context.Context) (bool, error) {
	for {
		if it.exhausted {
			return false, nil
		}
		var res *ListResult
		err := withRetry(ctx, func(ctx context.Context) error {
			r, err := it.c.ListObjectsV2(ctx, it.bucket, ListOptions{
				Prefix:            it.prefix,
				StartAfter:        it.startAfter,
				ContinuationToken: it.token,
				MaxKeys:           syncPageSize,
			})
			if err != nil {
				return err
			}
			res = r
			return nil
		})
		if err != nil {
			return false, fmt.Errorf("listing %s (%s): %w", it.side, it.bucket, err)
		}
		for _, o := range res.Objects {
			if !strings.HasPrefix(o.Key, it.prefix) {
				return false, fmt.Errorf("listing %s: key %q di luar prefix %q — provider mengabaikan parameter prefix", it.side, o.Key, it.prefix)
			}
			if o.Key <= it.last {
				return false, &ListingOrderError{Side: it.side, Prev: it.last, Got: o.Key}
			}
			it.last = o.Key
		}
		it.pages++
		it.buf = res.Objects
		it.pos = 0
		it.token = res.NextToken
		if !res.IsTruncated || res.NextToken == "" {
			it.exhausted = true
		}
		if len(it.buf) > 0 {
			return true, nil
		}
		if it.exhausted {
			return false, nil
		}
		// An empty page that is still truncated: ask for the next one.
	}
}

// peek returns the current entry without consuming it, or nil at the end.
func (it *keyIterator) peek(ctx context.Context) (*ObjectEntry, error) {
	for it.pos >= len(it.buf) {
		ok, err := it.fill(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
	}
	return &it.buf[it.pos], nil
}

func (it *keyIterator) advance() { it.pos++ }

// withRetry runs fn with exponential backoff for the failures that are worth
// retrying: transport errors and 5xx/429/408 answers. Any other S3 error
// (403, 404, 400) is returned at once.
func withRetry(ctx context.Context, fn func(context.Context) error) error {
	var err error
	for attempt := 0; attempt < listRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * time.Second
			delay += time.Duration(rand.Int64N(int64(delay / 2)))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		err = fn(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retryable(err) {
			return err
		}
	}
	return err
}

func retryable(err error) bool {
	var s3err *S3Error
	if !errors.As(err, &s3err) {
		return true // transport-level: connection reset, timeout, DNS
	}
	switch s3err.StatusCode {
	case http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	switch s3err.Code {
	case "SlowDown", "RequestTimeout", "InternalError", "ServiceUnavailable":
		return true
	}
	return false
}

// chunkResult describes one chunk file that was built.
type chunkResult struct {
	Keys    int    // keys written to the file
	Bytes   int64  // their total size on the source
	Last    string // relative key of the last source object consumed
	Listed  int64  // source objects examined
	Skipped int64  // already present at the destination
	Invalid int64  // keys rclone cannot be given (reported through onInvalid)
	Markers []string
}

// chunkBuilder runs the merge-join and writes chunk files.
type chunkBuilder struct {
	src *keyIterator
	dst *keyIterator // nil: copy everything (overwrite mode, prefix jobs)
	// mode is skip-existing, update-if-different or overwrite.
	mode string
	// max keys per chunk.
	max int
	// onInvalid is told about keys that cannot be handed to rclone.
	onInvalid func(key, reason string)
}

// unsupportedKeyReason explains why a key cannot go into a chunk file. rclone
// reads the file line by line and cleans paths, so a key with a newline, a
// leading slash, an empty segment or a dot segment would be read as a
// different key.
func unsupportedKeyReason(rel string) string {
	switch {
	case strings.ContainsAny(rel, "\r\n"):
		return "key mengandung baris baru; rclone membaca daftar key per baris"
	case strings.HasPrefix(rel, "/"):
		return "key relatif diawali \"/\" (ada \"//\" di dalam key); rclone akan membacanya sebagai key lain"
	case strings.Contains(rel, "//"):
		return "key mengandung \"//\"; rclone akan menormalkannya jadi key lain"
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "." || seg == ".." {
			return "key mengandung segmen \".\" atau \"..\""
		}
	}
	return ""
}

// next builds the next chunk into path. done is true when the source is
// exhausted (the chunk may still hold keys). A chunk with zero keys and
// done=false happens when a whole page was skipped; callers just loop.
func (b *chunkBuilder) next(ctx context.Context, path string) (chunkResult, bool, error) {
	var res chunkResult
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return res, false, fmt.Errorf("tidak bisa menulis file chunk: %w", err)
	}
	w := bufio.NewWriter(f)
	done := false
	for res.Keys+len(res.Markers) < b.max {
		obj, err := b.src.peek(ctx)
		if err != nil {
			f.Close()
			return res, false, err
		}
		if obj == nil {
			done = true
			break
		}
		rel := obj.Key[len(b.src.prefix):]
		b.src.advance()
		res.Listed++
		res.Last = rel
		if rel == "" {
			continue // the prefix's own marker object
		}
		if strings.HasSuffix(rel, "/") {
			// A folder marker: zero bytes, not a file for rclone. Handled by
			// the job itself.
			res.Markers = append(res.Markers, rel)
			continue
		}
		if reason := unsupportedKeyReason(rel); reason != "" {
			res.Invalid++
			if b.onInvalid != nil {
				b.onInvalid(obj.Key, reason)
			}
			continue
		}
		if b.dst != nil {
			skip, err := b.presentAtDestination(ctx, rel, obj)
			if err != nil {
				f.Close()
				return res, false, err
			}
			if skip {
				res.Skipped++
				continue
			}
		}
		if _, err := w.WriteString(rel + "\n"); err != nil {
			f.Close()
			return res, false, fmt.Errorf("tidak bisa menulis file chunk: %w", err)
		}
		res.Keys++
		res.Bytes += obj.Size
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return res, false, err
	}
	if err := f.Close(); err != nil {
		return res, false, err
	}
	return res, done, nil
}

// presentAtDestination advances the destination cursor up to rel and reports
// whether the object should be skipped under the job's mode.
func (b *chunkBuilder) presentAtDestination(ctx context.Context, rel string, obj *ObjectEntry) (bool, error) {
	for {
		d, err := b.dst.peek(ctx)
		if err != nil {
			return false, err
		}
		if d == nil {
			return false, nil
		}
		drel := d.Key[len(b.dst.prefix):]
		switch {
		case drel < rel:
			b.dst.advance()
		case drel == rel:
			b.dst.advance()
			if b.mode == "update-if-different" {
				return d.Size == obj.Size, nil
			}
			return true, nil
		default:
			return false, nil
		}
	}
}
