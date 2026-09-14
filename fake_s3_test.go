package main

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is an in-memory S3 server for tests: sorted listings with prefix,
// delimiter, start-after and continuation tokens; HEAD/GET/PUT/DELETE;
// CopyObject; DeleteObjects; and a real SigV4 check so a request whose wire
// form drifts from its signed form is refused like Garage would.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]map[string]*fakeObject // bucket -> key -> object
	uploads map[string]*fakeUpload            // uploadId -> multipart upload in progress

	accessKey, secretKey, region string

	// knobs
	deny             string         // when set, every request gets 403 with this body
	failOps          map[string]int // op -> remaining injected HTTP 500s
	scrambleListing  bool           // emit one page out of order
	awsStyleEncoding bool           // "+" for space under encoding-type=url
	undeletable      map[string]bool
	pageLimit        int    // caps max-keys, to force paging in tests
	denyWrites       bool   // PUT/DELETE/COPY answer 403 AccessDenied (read-only key)
	virtualOnly      bool   // path-style requests answer 403, like Hetzner
	badKeyBody       string // when set, every request answers 403 with this body (InvalidAccessKeyId)
	lastHost         string // Host header of the last request, for assertions
	virtualBase      string // extra base host under which <bucket>.<base> is virtual-hosted

	calls map[string]int
	srv   *httptest.Server
}

type fakeObject struct {
	data        []byte
	contentType string
	etag        string
	modified    time.Time
}

type fakeUpload struct {
	bucket, key, contentType string
	parts                    map[int][]byte
}

func newFakeS3(t *testing.T, accessKey, secretKey, region string) *fakeS3 {
	t.Helper()
	f := &fakeS3{
		objects:     map[string]map[string]*fakeObject{},
		uploads:     map[string]*fakeUpload{},
		accessKey:   accessKey,
		secretKey:   secretKey,
		region:      region,
		failOps:     map[string]int{},
		undeletable: map[string]bool{},
		calls:       map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeS3) URL() string { return f.srv.URL }

// hostOnly is the server's host:port, what a virtual-host request appends
// the bucket to.
func (f *fakeS3) hostOnly() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeS3) put(bucket, key string, data []byte, contentType string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putLocked(bucket, key, data, contentType)
}

func (f *fakeS3) putLocked(bucket, key string, data []byte, contentType string) {
	if f.objects[bucket] == nil {
		f.objects[bucket] = map[string]*fakeObject{}
	}
	sum := md5.Sum(data)
	f.objects[bucket][key] = &fakeObject{
		data:        append([]byte{}, data...),
		contentType: contentType,
		etag:        hex.EncodeToString(sum[:]),
		modified:    time.Now().UTC().Truncate(time.Second),
	}
}

func (f *fakeS3) get(bucket, key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[bucket][key]
	if !ok {
		return nil, false
	}
	return append([]byte{}, o.data...), true
}

func (f *fakeS3) contentType(bucket, key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o, ok := f.objects[bucket][key]; ok {
		return o.contentType
	}
	return ""
}

func (f *fakeS3) count(bucket string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.objects[bucket])
}

func (f *fakeS3) keys(bucket string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sortedKeysLocked(bucket)
}

func (f *fakeS3) sortedKeysLocked(bucket string) []string {
	out := make([]string, 0, len(f.objects[bucket]))
	for k := range f.objects[bucket] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (f *fakeS3) callCount(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

// --- server ---------------------------------------------------------------

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	body, _ := io.ReadAll(r.Body)

	if f.deny != "" {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, f.deny)
		return
	}

	// Virtual-hosted requests carry the bucket in the Host header:
	// <bucket>.<server host>; path-style ones put it in the path.
	f.lastHost = r.Host
	var bucket, key string
	virtual := false
	for _, base := range []string{f.hostOnly(), f.virtualBase} {
		if base == "" {
			continue
		}
		if suffix := "." + base; strings.HasSuffix(r.Host, suffix) {
			bucket = strings.TrimSuffix(r.Host, suffix)
			key = strings.TrimPrefix(r.URL.Path, "/")
			virtual = true
			break
		}
	}
	if !virtual {
		bucket, key = splitBucketKey(r.URL.Path)
	}
	q := r.URL.Query()
	op := f.classify(r, key, q)
	f.calls[op]++
	if virtual {
		f.calls[op+".virtual"]++
	}
	if f.badKeyBody != "" {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, f.badKeyBody)
		return
	}
	if f.virtualOnly && !virtual {
		s3Fail(w, http.StatusForbidden, "AccessDenied", "")
		return
	}
	if op == "ListObjectsV2" && q.Get("start-after") != "" {
		f.calls["ListObjectsV2.start-after"]++
	}

	if code, msg := f.checkSignature(r, body); code != 0 {
		s3Fail(w, code, "SignatureDoesNotMatch", msg)
		return
	}
	if n := f.failOps[op]; n > 0 {
		f.failOps[op] = n - 1
		s3Fail(w, http.StatusInternalServerError, "InternalError", "injected failure for "+op)
		return
	}
	if f.denyWrites {
		switch op {
		case "PutObject", "DeleteObject", "DeleteObjects", "CopyObject", "CreateMultipartUpload", "UploadPart", "CompleteMultipartUpload":
			s3Fail(w, http.StatusForbidden, "AccessDenied", "")
			return
		}
	}

	switch op {
	case "ListObjectsV2":
		f.list(w, bucket, q)
	case "HeadObject":
		o, ok := f.objects[bucket][key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		setObjectHeaders(w, o)
		w.WriteHeader(http.StatusOK)
	case "GetObject":
		o, ok := f.objects[bucket][key]
		if !ok {
			s3Fail(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		setObjectHeaders(w, o)
		w.Write(o.data)
	case "CopyObject":
		src := r.Header.Get("X-Amz-Copy-Source")
		srcBucket, srcKey := splitBucketKey(src)
		decoded, err := url.PathUnescape(srcKey)
		if err != nil {
			s3Fail(w, http.StatusBadRequest, "InvalidArgument", "bad copy source")
			return
		}
		o, ok := f.objects[srcBucket][decoded]
		if !ok {
			s3Fail(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		f.putLocked(bucket, key, o.data, o.contentType)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<CopyObjectResult><ETag>"%s"</ETag></CopyObjectResult>`, f.objects[bucket][key].etag)
	case "PutObject":
		f.putLocked(bucket, key, body, r.Header.Get("Content-Type"))
		w.Header().Set("ETag", `"`+f.objects[bucket][key].etag+`"`)
		w.WriteHeader(http.StatusOK)
	case "DeleteObject":
		if f.undeletable[key] {
			s3Fail(w, http.StatusForbidden, "AccessDenied", "undeletable in this test")
			return
		}
		delete(f.objects[bucket], key)
		w.WriteHeader(http.StatusNoContent)
	case "DeleteObjects":
		sum := md5.Sum(body)
		if r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(sum[:]) {
			s3Fail(w, http.StatusBadRequest, "InvalidDigest", "Content-MD5 missing or wrong")
			return
		}
		var req struct {
			Objects []struct {
				Key string `xml:"Key"`
			} `xml:"Object"`
		}
		if err := xml.Unmarshal(body, &req); err != nil {
			s3Fail(w, http.StatusBadRequest, "MalformedXML", err.Error())
			return
		}
		var out strings.Builder
		out.WriteString(`<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
		for _, o := range req.Objects {
			if f.undeletable[o.Key] {
				fmt.Fprintf(&out, `<Error><Key>%s</Key><Code>AccessDenied</Code><Message>undeletable in this test</Message></Error>`, xmlEscape(o.Key))
				continue
			}
			delete(f.objects[bucket], o.Key)
		}
		out.WriteString(`</DeleteResult>`)
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, out.String())
	case "CreateMultipartUpload":
		id := fmt.Sprintf("upload-%d", len(f.uploads)+1)
		f.uploads[id] = &fakeUpload{bucket: bucket, key: key, contentType: r.Header.Get("Content-Type"), parts: map[int][]byte{}}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`,
			xmlEscape(bucket), xmlEscape(key), id)
	case "UploadPart":
		up, ok := f.uploads[q.Get("uploadId")]
		if !ok || up.bucket != bucket || up.key != key {
			s3Fail(w, http.StatusNotFound, "NoSuchUpload", "no such upload")
			return
		}
		n, err := strconv.Atoi(q.Get("partNumber"))
		if err != nil || n < 1 || n > 10000 {
			s3Fail(w, http.StatusBadRequest, "InvalidArgument", "bad part number")
			return
		}
		up.parts[n] = append([]byte{}, body...)
		sum := md5.Sum(body)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.WriteHeader(http.StatusOK)
	case "CompleteMultipartUpload":
		up, ok := f.uploads[q.Get("uploadId")]
		if !ok || up.bucket != bucket || up.key != key {
			s3Fail(w, http.StatusNotFound, "NoSuchUpload", "no such upload")
			return
		}
		var req struct {
			Parts []struct {
				PartNumber int    `xml:"PartNumber"`
				ETag       string `xml:"ETag"`
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &req); err != nil {
			s3Fail(w, http.StatusBadRequest, "MalformedXML", err.Error())
			return
		}
		var data []byte
		last := 0
		for _, p := range req.Parts {
			part, ok := up.parts[p.PartNumber]
			if !ok || p.PartNumber <= last {
				s3Fail(w, http.StatusBadRequest, "InvalidPart", "part missing or out of order")
				return
			}
			last = p.PartNumber
			data = append(data, part...)
		}
		f.putLocked(bucket, key, data, up.contentType)
		delete(f.uploads, q.Get("uploadId"))
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>"%s-%d"</ETag></CompleteMultipartUploadResult>`,
			xmlEscape(bucket), xmlEscape(key), f.objects[bucket][key].etag, len(req.Parts))
	case "AbortMultipartUpload":
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Fail(w, http.StatusNotImplemented, "NotImplemented", "fake S3 does not implement "+r.Method+" "+r.URL.Path)
	}
}

func (f *fakeS3) classify(r *http.Request, key string, q url.Values) string {
	_, uploadID := q["uploadId"]
	switch r.Method {
	case http.MethodGet:
		if key == "" {
			return "ListObjectsV2"
		}
		return "GetObject"
	case http.MethodHead:
		return "HeadObject"
	case http.MethodPut:
		if uploadID {
			return "UploadPart"
		}
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			return "CopyObject"
		}
		return "PutObject"
	case http.MethodDelete:
		if uploadID {
			return "AbortMultipartUpload"
		}
		return "DeleteObject"
	case http.MethodPost:
		if _, ok := q["delete"]; ok {
			return "DeleteObjects"
		}
		if _, ok := q["uploads"]; ok {
			return "CreateMultipartUpload"
		}
		if uploadID {
			return "CompleteMultipartUpload"
		}
	}
	return "Unknown"
}

// checkSignature recomputes the SigV4 signature from the request as received.
// It catches a client whose signed canonical form drifts from its wire form,
// and any x-amz-* header that was sent but left out of SignedHeaders — the two
// mistakes real providers refuse with 403.
func (f *fakeS3) checkSignature(r *http.Request, body []byte) (int, string) {
	auth := r.Header.Get("Authorization")
	const prefix = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(auth, prefix) {
		return http.StatusForbidden, "missing SigV4 Authorization header"
	}
	fields := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(auth, prefix), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		fields[k] = v
	}
	credParts := strings.Split(fields["Credential"], "/")
	if len(credParts) != 5 || credParts[0] != f.accessKey {
		return http.StatusForbidden, "unknown access key"
	}
	if credParts[2] != f.region {
		return http.StatusForbidden, "region mismatch: signed with " + credParts[2]
	}
	signedNames := strings.Split(fields["SignedHeaders"], ";")
	signed := map[string]bool{}
	for _, n := range signedNames {
		signed[n] = true
	}
	for name := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") && !signed[lower] {
			return http.StatusForbidden, "header " + lower + " sent but not signed"
		}
	}
	// rclone's SDK sends UNSIGNED-PAYLOAD for single PUTs (over any scheme
	// when the provider is not AWS) and relies on Content-MD5 instead.
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "UNSIGNED-PAYLOAD" {
		if md5hdr := r.Header.Get("Content-MD5"); md5hdr != "" {
			sum := md5.Sum(body)
			if md5hdr != base64.StdEncoding.EncodeToString(sum[:]) {
				return http.StatusBadRequest, "Content-MD5 mismatch"
			}
		}
	} else if payloadHash != sha256Hex(body) {
		return http.StatusBadRequest, "payload hash mismatch"
	}

	var canonicalHeaders strings.Builder
	for _, n := range signedNames {
		value := ""
		if n == "host" {
			value = r.Host
		} else {
			value = strings.Join(r.Header.Values(n), ",")
		}
		canonicalHeaders.WriteString(n + ":" + strings.TrimSpace(value) + "\n")
	}
	queryParts := []string{}
	if r.URL.RawQuery != "" {
		queryParts = strings.Split(r.URL.RawQuery, "&")
	}
	sort.Strings(queryParts)
	canonicalRequest := strings.Join([]string{
		r.Method,
		r.URL.EscapedPath(),
		strings.Join(queryParts, "&"),
		canonicalHeaders.String(),
		fields["SignedHeaders"],
		payloadHash,
	}, "\n")
	amzDate := r.Header.Get("X-Amz-Date")
	scope := strings.Join(credParts[1:], "/")
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonicalRequest))}, "\n")
	kDate := hmacSHA256([]byte("AWS4"+f.secretKey), credParts[1])
	kRegion := hmacSHA256(kDate, credParts[2])
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	want := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))
	if fields["Signature"] != want {
		return http.StatusForbidden, "signature mismatch"
	}
	return 0, ""
}

type fakeListEntry struct {
	key      string
	obj      *fakeObject
	isPrefix bool
}

func (f *fakeS3) list(w http.ResponseWriter, bucket string, q url.Values) {
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	maxKeys := 1000
	if v := q.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxKeys = n
		}
	}
	if f.pageLimit > 0 && maxKeys > f.pageLimit {
		maxKeys = f.pageLimit
	}
	after := q.Get("start-after")
	if tok := q.Get("continuation-token"); tok != "" {
		raw, err := base64.StdEncoding.DecodeString(tok)
		if err != nil {
			s3Fail(w, http.StatusBadRequest, "InvalidArgument", "bad continuation token")
			return
		}
		after = string(raw)
	}

	keys := f.sortedKeysLocked(bucket)
	var entries []fakeListEntry
	lastConsumed := ""
	truncated := false
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || (after != "" && k <= after) {
			continue
		}
		entry := fakeListEntry{key: k, obj: f.objects[bucket][k]}
		if delimiter != "" {
			rest := k[len(prefix):]
			if i := strings.Index(rest, delimiter); i >= 0 {
				entry = fakeListEntry{key: prefix + rest[:i+len(delimiter)], isPrefix: true}
				if n := len(entries); n > 0 && entries[n-1].isPrefix && entries[n-1].key == entry.key {
					lastConsumed = k // same common prefix, no new entry
					continue
				}
			}
		}
		if len(entries) == maxKeys {
			truncated = true
			break
		}
		entries = append(entries, entry)
		lastConsumed = k
	}

	if f.scrambleListing && len(entries) >= 2 {
		entries[0], entries[1] = entries[1], entries[0]
	}

	encode := func(s string) string {
		if q.Get("encoding-type") != "url" {
			return s
		}
		if f.awsStyleEncoding {
			return url.QueryEscape(s)
		}
		return awsURIEncode(s, true)
	}

	var out strings.Builder
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	fmt.Fprintf(&out, "<Name>%s</Name><Prefix>%s</Prefix><MaxKeys>%d</MaxKeys><KeyCount>%d</KeyCount><IsTruncated>%v</IsTruncated>",
		xmlEscape(bucket), xmlEscape(encode(prefix)), maxKeys, len(entries), truncated)
	if delimiter != "" {
		fmt.Fprintf(&out, "<Delimiter>%s</Delimiter>", xmlEscape(encode(delimiter)))
	}
	if truncated {
		fmt.Fprintf(&out, "<NextContinuationToken>%s</NextContinuationToken>", base64.StdEncoding.EncodeToString([]byte(lastConsumed)))
	}
	for _, e := range entries {
		if e.isPrefix {
			fmt.Fprintf(&out, "<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>", xmlEscape(encode(e.key)))
			continue
		}
		fmt.Fprintf(&out, `<Contents><Key>%s</Key><LastModified>%s</LastModified><ETag>&quot;%s&quot;</ETag><Size>%d</Size></Contents>`,
			xmlEscape(encode(e.key)), e.obj.modified.Format("2006-01-02T15:04:05.000Z"), e.obj.etag, len(e.obj.data))
	}
	out.WriteString("</ListBucketResult>")
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, out.String())
}

func setObjectHeaders(w http.ResponseWriter, o *fakeObject) {
	w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
	if o.contentType != "" {
		w.Header().Set("Content-Type", o.contentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("ETag", `"`+o.etag+`"`)
	w.Header().Set("Last-Modified", o.modified.Format(http.TimeFormat))
}

func splitBucketKey(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	bucket, key, _ = strings.Cut(p, "/")
	return bucket, key
}

func s3Fail(w http.ResponseWriter, code int, s3code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, s3code, xmlEscape(msg))
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// The fake is the ground truth for the merge-join tests, so its listing
// semantics are pinned down here: byte order, prefix, delimiter roll-up,
// start-after (exclusive) and continuation tokens that resume after a whole
// common-prefix group.
func TestFakeS3ListingSemantics(t *testing.T) {
	f := newFakeS3(t, "GK", "SK", "garage")
	for _, k := range []string{"a.txt", "b/1.txt", "b/2.txt", "b/c/3.txt", "d.txt", "e.txt"} {
		f.put("bk", k, []byte(k), "text/plain")
	}
	c, err := NewS3(f.URL(), "GK", "SK", "garage")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	flat, err := c.ListObjectsV2(ctx, "bk", ListOptions{MaxKeys: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range flat.Objects {
		keys = append(keys, o.Key)
	}
	if got := strings.Join(keys, ","); got != "a.txt,b/1.txt,b/2.txt,b/c/3.txt,d.txt,e.txt" {
		t.Errorf("flat listing = %s", got)
	}

	folders, err := c.ListObjectsV2(ctx, "bk", ListOptions{Delimiter: "/", MaxKeys: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(folders.Objects) != 3 || len(folders.CommonPrefixes) != 1 || folders.CommonPrefixes[0] != "b/" {
		t.Errorf("delimiter listing: objects=%d prefixes=%v", len(folders.Objects), folders.CommonPrefixes)
	}

	after, err := c.ListObjectsV2(ctx, "bk", ListOptions{StartAfter: "b/2.txt", MaxKeys: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Objects) != 3 || after.Objects[0].Key != "b/c/3.txt" {
		t.Errorf("start-after must be exclusive: %+v", after.Objects)
	}

	// Page size 2 with a delimiter: page 1 = a.txt + b/ (the whole group),
	// page 2 = d.txt + e.txt.
	page1, err := c.ListObjectsV2(ctx, "bk", ListOptions{Delimiter: "/", MaxKeys: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !page1.IsTruncated || len(page1.Objects) != 1 || len(page1.CommonPrefixes) != 1 {
		t.Fatalf("page1 = %+v", page1)
	}
	page2, err := c.ListObjectsV2(ctx, "bk", ListOptions{Delimiter: "/", MaxKeys: 2, ContinuationToken: page1.NextToken})
	if err != nil {
		t.Fatal(err)
	}
	if page2.IsTruncated || len(page2.Objects) != 2 || page2.Objects[0].Key != "d.txt" {
		t.Errorf("page2 = %+v", page2)
	}

	// A wrong secret is refused like a real provider would.
	bad, _ := NewS3(f.URL(), "GK", "WRONG", "garage")
	if _, err := bad.ListObjectsV2(ctx, "bk", ListOptions{}); err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("wrong secret: err = %v", err)
	}
	// So is a wrong region.
	badRegion, _ := NewS3(f.URL(), "GK", "SK", "us-east-1")
	if _, err := badRegion.ListObjectsV2(ctx, "bk", ListOptions{}); err == nil {
		t.Error("wrong region must be refused")
	}
}
