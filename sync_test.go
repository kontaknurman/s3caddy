package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readChunk(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l != "" {
			keys = append(keys, l)
		}
	}
	return keys
}

func TestMergeJoinModes(t *testing.T) {
	src := newFakeS3(t, "AK", "SK", "garage")
	dst := newFakeS3(t, "AK", "SK", "garage")
	for _, k := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"} {
		src.put("s", "in/"+k, []byte(strings.Repeat("x", len(k))), "text/plain")
	}
	dst.put("d", "out/b.txt", []byte("xxxxx"), "text/plain")          // same size
	dst.put("d", "out/d.txt", []byte("different-size"), "text/plain") // different size
	dst.put("d", "out/only-here.txt", []byte("z"), "text/plain")      // only at destination

	srcC, _ := NewS3(src.URL(), "AK", "SK", "garage")
	dstC, _ := NewS3(dst.URL(), "AK", "SK", "garage")
	ctx := context.Background()

	cases := map[string]struct {
		mode     string
		withDst  bool
		wantKeys string
		skipped  int64
	}{
		"skip-existing":       {"skip-existing", true, "a.txt,c.txt,e.txt", 2},
		"update-if-different": {"update-if-different", true, "a.txt,c.txt,d.txt,e.txt", 1},
		"overwrite":           {"overwrite", false, "a.txt,b.txt,c.txt,d.txt,e.txt", 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := &chunkBuilder{
				src:  newKeyIterator(srcC, "sumber", "s", "in/", ""),
				mode: tc.mode,
				max:  100,
			}
			if tc.withDst {
				b.dst = newKeyIterator(dstC, "tujuan", "d", "out/", "")
			}
			path := filepath.Join(t.TempDir(), "chunk.txt")
			res, done, err := b.next(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if !done {
				t.Error("source should be exhausted")
			}
			if got := strings.Join(readChunk(t, path), ","); got != tc.wantKeys {
				t.Errorf("chunk = %s, want %s", got, tc.wantKeys)
			}
			if res.Skipped != tc.skipped || res.Listed != 5 || res.Last != "e.txt" {
				t.Errorf("result = %+v", res)
			}
			if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
				t.Errorf("chunk file mode = %o", info.Mode().Perm())
			}
		})
	}
	// Overwrite mode must not have listed the destination at all.
	if n := dst.callCount("ListObjectsV2"); n != 2 {
		t.Errorf("destination listed %d times, want 2 (once per mode that compares)", n)
	}
}

func TestChunkBuilderPagesAndResumesWithStartAfter(t *testing.T) {
	src := newFakeS3(t, "AK", "SK", "garage")
	src.pageLimit = 3 // force paging on every listing
	for i := 0; i < 10; i++ {
		src.put("s", "p/"+string(rune('a'+i))+".txt", []byte("x"), "text/plain")
	}
	srcC, _ := NewS3(src.URL(), "AK", "SK", "garage")
	ctx := context.Background()
	dir := t.TempDir()

	b := &chunkBuilder{src: newKeyIterator(srcC, "sumber", "s", "p/", ""), mode: "overwrite", max: 4}
	var chunks [][]string
	var last string
	for i := 0; ; i++ {
		path := filepath.Join(dir, "c.txt")
		res, done, err := b.next(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, readChunk(t, path))
		last = res.Last
		if done {
			break
		}
		if i > 10 {
			t.Fatal("did not finish")
		}
	}
	if len(chunks) != 3 || strings.Join(chunks[0], ",") != "a.txt,b.txt,c.txt,d.txt" || strings.Join(chunks[2], ",") != "i.txt,j.txt" {
		t.Errorf("chunks = %v", chunks)
	}
	if last != "j.txt" {
		t.Errorf("last = %q", last)
	}

	// Resuming from a checkpoint lists strictly after it.
	b2 := &chunkBuilder{src: newKeyIterator(srcC, "sumber", "s", "p/", "p/g.txt"), mode: "overwrite", max: 100}
	path := filepath.Join(dir, "c2.txt")
	if _, done, err := b2.next(ctx, path); err != nil || !done {
		t.Fatal(err)
	}
	if got := strings.Join(readChunk(t, path), ","); got != "h.txt,i.txt,j.txt" {
		t.Errorf("resumed chunk = %s", got)
	}
	if src.callCount("ListObjectsV2.start-after") == 0 {
		t.Error("resume must use start-after")
	}
}

func TestChunkBuilderRefusesUnorderedListingAndKeysOutsidePrefix(t *testing.T) {
	src := newFakeS3(t, "AK", "SK", "garage")
	for _, k := range []string{"p/a.txt", "p/b.txt", "p/c.txt"} {
		src.put("s", k, []byte("x"), "text/plain")
	}
	src.scrambleListing = true
	srcC, _ := NewS3(src.URL(), "AK", "SK", "garage")
	b := &chunkBuilder{src: newKeyIterator(srcC, "sumber", "s", "p/", ""), mode: "overwrite", max: 100}
	_, _, err := b.next(context.Background(), filepath.Join(t.TempDir(), "c.txt"))
	var orderErr *ListingOrderError
	if !errors.As(err, &orderErr) {
		t.Fatalf("expected ListingOrderError, got %v", err)
	}
	if !strings.Contains(err.Error(), "tidak terurut") {
		t.Errorf("message: %v", err)
	}
}

func TestChunkBuilderRecordsUnsupportedKeysAndMarkers(t *testing.T) {
	src := newFakeS3(t, "AK", "SK", "garage")
	for _, k := range []string{"p/", "p/ok.txt", "p/sub/", "p/bad\nname.txt", "p//lead.txt", "p/a//b.txt", "p/../x.txt"} {
		src.put("s", k, []byte("x"), "text/plain")
	}
	srcC, _ := NewS3(src.URL(), "AK", "SK", "garage")
	var invalid []string
	b := &chunkBuilder{
		src: newKeyIterator(srcC, "sumber", "s", "p/", ""), mode: "overwrite", max: 100,
		onInvalid: func(key, reason string) { invalid = append(invalid, key+" ("+reason+")") },
	}
	path := filepath.Join(t.TempDir(), "c.txt")
	res, done, err := b.next(context.Background(), path)
	if err != nil || !done {
		t.Fatal(err)
	}
	if got := strings.Join(readChunk(t, path), ","); got != "ok.txt" {
		t.Errorf("chunk = %s", got)
	}
	if len(res.Markers) != 1 || res.Markers[0] != "sub/" {
		t.Errorf("markers = %v", res.Markers)
	}
	if res.Invalid != 4 || len(invalid) != 4 {
		t.Errorf("invalid = %d %v", res.Invalid, invalid)
	}
	for _, want := range []string{"baris baru", "//", "segmen"} {
		if !strings.Contains(strings.Join(invalid, "\n"), want) {
			t.Errorf("reasons missing %q: %v", want, invalid)
		}
	}
	if res.Listed != 7 {
		t.Errorf("listed = %d", res.Listed)
	}
}

func TestWithRetryRetriesOnlyTransientFailures(t *testing.T) {
	src := newFakeS3(t, "AK", "SK", "garage")
	src.put("s", "a.txt", []byte("x"), "text/plain")
	src.failOps["ListObjectsV2"] = 2
	srcC, _ := NewS3(src.URL(), "AK", "SK", "garage")
	it := newKeyIterator(srcC, "sumber", "s", "", "")
	if obj, err := it.peek(context.Background()); err != nil || obj == nil || obj.Key != "a.txt" {
		t.Fatalf("two injected 500s should be retried: obj=%v err=%v", obj, err)
	}
	if n := src.callCount("ListObjectsV2"); n != 3 {
		t.Errorf("list calls = %d, want 3", n)
	}

	// A 403 is final.
	bad, _ := NewS3(src.URL(), "AK", "WRONG", "garage")
	it2 := newKeyIterator(bad, "sumber", "s", "", "")
	before := src.callCount("ListObjectsV2")
	if _, err := it2.peek(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if n := src.callCount("ListObjectsV2") - before; n != 1 {
		t.Errorf("403 was retried %d times", n)
	}

	// A cancelled context stops the backoff immediately.
	src.failOps["ListObjectsV2"] = 10
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	it3 := newKeyIterator(srcC, "sumber", "s", "", "")
	if _, err := it3.peek(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
