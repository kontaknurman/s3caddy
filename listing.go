package main

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The merge-join needs both sides in byte order. An S3 listing already comes
// that way, page by page. A folder on an SFTP server does not: rclone lists
// it directory by directory, in no global order. So the panel lists the
// folder once per run with `rclone lsjson -R`, sorts the result on disk in
// runs of sortRunEntries (an external merge sort, so memory stays flat for
// millions of files), and walks the sorted file like a listing.

// sortRunEntries is how many entries are sorted in memory at a time (about
// 20 MB with typical path lengths). A variable so tests can force many runs.
var sortRunEntries = 100_000

// listingProgressEvery is how often the job's phase line is refreshed.
const listingProgressEvery = 5_000

// listing is one side of the merge-join: an S3 keyIterator or a sorted
// folder listing.
type listing interface {
	peek(ctx context.Context) (*ObjectEntry, error)
	advance()
	keyPrefix() string
}

func (it *keyIterator) keyPrefix() string { return it.prefix }

// listingEntry is one file of a folder listing, its path relative to the
// listed folder.
type listingEntry struct {
	rel  string
	size int64
}

// listFiles runs `rclone lsjson -R --files-only` on remotePath and calls fn
// for every file, as rclone finds them. It returns rclone's exit code and
// last log lines; a cancelled ctx stops rclone with SIGINT.
func (r *rcloneRunner) listFiles(ctx context.Context, env []string, remotePath string, fn func(rel string, size int64) error) (int, []string, error) {
	args := []string{"lsjson", remotePath,
		"-R", "--files-only", "--no-mimetype", "--no-modtime",
		"--config", "",
		"--contimeout", "15s", "--timeout", "5m",
		"--retries", "3", "--low-level-retries", "10",
	}
	args = append(args, r.sharedArgs()...)
	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Env = append(minimalEnv(), env...)
	var stderr cappedBuffer
	stderr.max = 256 << 10
	cmd.Stderr = &stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = rcloneStopGrace
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, nil, err
	}
	if err := cmd.Start(); err != nil {
		return -1, nil, fmt.Errorf("rclone gagal dijalankan: %w", err)
	}

	// lsjson prints "[", then one JSON object per line (all but the last
	// followed by a comma), then "]". JSON escaping keeps every name on one
	// line whatever characters it holds.
	var fnErr error
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSuffix(strings.TrimSpace(scanner.Text()), ",")
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var item struct {
			Path  string `json:"Path"`
			Size  int64  `json:"Size"`
			IsDir bool   `json:"IsDir"`
		}
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			fnErr = fmt.Errorf("output rclone lsjson tidak bisa dibaca: %w", err)
			break
		}
		if item.IsDir || item.Path == "" {
			continue
		}
		if err := fn(item.Path, item.Size); err != nil {
			fnErr = err
			break
		}
	}
	if fnErr == nil {
		fnErr = scanner.Err()
	}
	if fnErr != nil {
		_ = cmd.Process.Signal(os.Interrupt)
	}
	// Drain what is left so rclone is never blocked on a full pipe.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	lines := rcloneLogTail(stderr.String())
	if ctx.Err() != nil {
		return -1, lines, ctx.Err()
	}
	if fnErr != nil {
		return -1, lines, fnErr
	}
	if waitErr == nil {
		return 0, lines, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode(), lines, nil
	}
	return -1, lines, fmt.Errorf("rclone: %w", waitErr)
}

// sortedListingWriter collects entries and writes them, sorted, to disk.
type sortedListingWriter struct {
	dir, name string
	buf       []listingEntry
	runs      []string
	count     int64
}

func newSortedListingWriter(dir, name string) *sortedListingWriter {
	return &sortedListingWriter{dir: dir, name: name}
}

func (w *sortedListingWriter) add(rel string, size int64) error {
	w.buf = append(w.buf, listingEntry{rel: rel, size: size})
	w.count++
	if len(w.buf) >= sortRunEntries {
		return w.flushRun()
	}
	return nil
}

// flushRun sorts the buffered entries into a run file.
func (w *sortedListingWriter) flushRun() error {
	if len(w.buf) == 0 {
		return nil
	}
	sort.Slice(w.buf, func(i, j int) bool { return w.buf[i].rel < w.buf[j].rel })
	path := filepath.Join(w.dir, w.name+".run"+strconv.Itoa(len(w.runs)))
	if err := writeListingFile(path, func(emit func(listingEntry) error) error {
		for i, e := range w.buf {
			if i > 0 && e.rel == w.buf[i-1].rel {
				continue // one entry per path, also when there is a single run
			}
			if err := emit(e); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	w.runs = append(w.runs, path)
	w.buf = w.buf[:0]
	return nil
}

// finish merges the runs into one sorted file, without duplicates, and
// returns its path.
func (w *sortedListingWriter) finish() (string, error) {
	if err := w.flushRun(); err != nil {
		return "", err
	}
	out := filepath.Join(w.dir, w.name+".sorted")
	defer w.removeRuns()
	switch len(w.runs) {
	case 0:
		return out, writeListingFile(out, func(func(listingEntry) error) error { return nil })
	case 1:
		return out, os.Rename(w.runs[0], out)
	}
	readers := make([]*listingReader, 0, len(w.runs))
	defer func() {
		for _, r := range readers {
			r.Close()
		}
	}()
	h := &entryHeap{}
	for i, path := range w.runs {
		r, err := openListingReader(path)
		if err != nil {
			return "", err
		}
		readers = append(readers, r)
		e, ok, err := r.next()
		if err != nil {
			return "", err
		}
		if ok {
			heap.Push(h, heapItem{entry: e, run: i})
		}
	}
	err := writeListingFile(out, func(emit func(listingEntry) error) error {
		last, first := "", true
		for h.Len() > 0 {
			item := heap.Pop(h).(heapItem)
			if first || item.entry.rel != last {
				if err := emit(item.entry); err != nil {
					return err
				}
				last, first = item.entry.rel, false
			}
			e, ok, err := readers[item.run].next()
			if err != nil {
				return err
			}
			if ok {
				heap.Push(h, heapItem{entry: e, run: item.run})
			}
		}
		return nil
	})
	return out, err
}

func (w *sortedListingWriter) removeRuns() {
	for _, p := range w.runs {
		_ = os.Remove(p)
	}
}

// writeListingFile writes entries as uvarint(len) path varint(size) records.
func writeListingFile(path string, fill func(emit func(listingEntry) error) error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("file listing: %w", err)
	}
	bw := bufio.NewWriterSize(f, 256<<10)
	var tmp [binary.MaxVarintLen64]byte
	err = fill(func(e listingEntry) error {
		n := binary.PutUvarint(tmp[:], uint64(len(e.rel)))
		if _, err := bw.Write(tmp[:n]); err != nil {
			return err
		}
		if _, err := bw.WriteString(e.rel); err != nil {
			return err
		}
		n = binary.PutVarint(tmp[:], e.size)
		_, err := bw.Write(tmp[:n])
		return err
	})
	if err == nil {
		err = bw.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// listingReader reads a listing file record by record.
type listingReader struct {
	f *os.File
	r *bufio.Reader
}

func openListingReader(path string) (*listingReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &listingReader{f: f, r: bufio.NewReaderSize(f, 256<<10)}, nil
}

func (l *listingReader) next() (listingEntry, bool, error) {
	n, err := binary.ReadUvarint(l.r)
	if err == io.EOF {
		return listingEntry{}, false, nil
	}
	if err != nil || n > 1<<20 {
		return listingEntry{}, false, errors.New("file listing rusak")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(l.r, b); err != nil {
		return listingEntry{}, false, errors.New("file listing rusak")
	}
	size, err := binary.ReadVarint(l.r)
	if err != nil {
		return listingEntry{}, false, errors.New("file listing rusak")
	}
	return listingEntry{rel: string(b), size: size}, true, nil
}

func (l *listingReader) Close() error { return l.f.Close() }

type heapItem struct {
	entry listingEntry
	run   int
}

type entryHeap []heapItem

func (h entryHeap) Len() int           { return len(h) }
func (h entryHeap) Less(i, j int) bool { return h[i].entry.rel < h[j].entry.rel }
func (h entryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *entryHeap) Push(x any)        { *h = append(*h, x.(heapItem)) }
func (h *entryHeap) Pop() any {
	old := *h
	item := old[len(old)-1]
	*h = old[:len(old)-1]
	return item
}

// fileIterator walks a sorted listing file as a merge-join side. Keys are
// prefix + relative path, like an S3 listing of the same folder, so the
// checkpoint arithmetic is the same for both kinds of side.
type fileIterator struct {
	r          *listingReader
	prefix     string
	startAfter string
	cur        *ObjectEntry
	done       bool
}

func openFileIterator(path, prefix, startAfter string) (*fileIterator, error) {
	r, err := openListingReader(path)
	if err != nil {
		return nil, err
	}
	return &fileIterator{r: r, prefix: prefix, startAfter: startAfter}, nil
}

func (it *fileIterator) peek(ctx context.Context) (*ObjectEntry, error) {
	for it.cur == nil && !it.done {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e, ok, err := it.r.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			it.done = true
			break
		}
		key := it.prefix + e.rel
		if it.startAfter != "" && key <= it.startAfter {
			continue
		}
		it.cur = &ObjectEntry{Key: key, Size: e.size}
	}
	return it.cur, nil
}

func (it *fileIterator) advance()          { it.cur = nil }
func (it *fileIterator) keyPrefix() string { return it.prefix }
func (it *fileIterator) Close() error      { return it.r.Close() }
