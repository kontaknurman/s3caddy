package main

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// AWS's published "Example: PUT Object" vector. It carries a Date header and
// an x-amz-storage-class header, so it proves that sign() covers every
// x-amz-* header plus date, not only the three the client always sets.
func TestSignMatchesAWSPutObjectExample(t *testing.T) {
	const (
		accessKey   = "AKIAIOSFODNN7EXAMPLE"
		secretKey   = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		host        = "examplebucket.s3.amazonaws.com"
		payloadHash = "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072" // sha256("Welcome to Amazon S3.")
		want        = "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"
	)
	fixed := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	c := &S3{accessKey: accessKey, secretKey: secretKey, region: "us-east-1"}
	req, err := http.NewRequest(http.MethodPut, "https://"+host+"/test%24file.text", strings.NewReader("Welcome to Amazon S3."))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	req.Header.Set("x-amz-storage-class", "REDUCED_REDUNDANCY")
	c.sign(req, "/test%24file.text", "", payloadHash, fixed)

	auth := req.Header.Get("Authorization")
	if got := signatureFrom(auth); got != want {
		t.Errorf("signature\n got %s\nwant %s\n%s", got, want, auth)
	}
	if !strings.Contains(auth, "SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class,") {
		t.Errorf("signed header set is wrong: %s", auth)
	}
}

func TestSignedHeadersCoverEveryAmzHeaderButNeverAuthorization(t *testing.T) {
	c := &S3{accessKey: "GK", secretKey: "s", region: "garage"}
	req, err := http.NewRequest(http.MethodPut, "http://127.0.0.1:3900/b/k", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Amz-Copy-Source", "/src/key")
	req.Header.Set("X-Amz-Metadata-Directive", "COPY")
	req.Header.Set("Content-MD5", "abc=")
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("User-Agent", "not-signed")
	req.Header.Set("Authorization", "stale")
	c.sign(req, "/b/k", "", emptyPayloadHash, time.Now())

	auth := req.Header.Get("Authorization")
	_, signed, _ := strings.Cut(auth, "SignedHeaders=")
	signed, _, _ = strings.Cut(signed, ",")
	want := "content-md5;content-type;host;x-amz-content-sha256;x-amz-copy-source;x-amz-date;x-amz-metadata-directive"
	if signed != want {
		t.Errorf("SignedHeaders = %q, want %q", signed, want)
	}
}

func TestListObjectsV2SendsStartAfterOnlyWithoutToken(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		io.WriteString(w, `<ListBucketResult><KeyCount>0</KeyCount></ListBucketResult>`)
	}))
	defer srv.Close()
	c := newTestS3(t, srv.URL)

	if _, err := c.ListObjectsV2(context.Background(), "b", ListOptions{StartAfter: "photos/a b.jpg", MaxKeys: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListObjectsV2(context.Background(), "b", ListOptions{StartAfter: "x", ContinuationToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(queries[0], "start-after=photos%2Fa%20b.jpg") || strings.Contains(queries[0], "delimiter") {
		t.Errorf("first query = %q", queries[0])
	}
	if strings.Contains(queries[1], "start-after") || !strings.Contains(queries[1], "continuation-token=tok") {
		t.Errorf("with a token, start-after must not be sent: %q", queries[1])
	}
}

// AWS-style providers encode a space as "+" under encoding-type=url; Garage
// never emits a raw "+", so query unescaping is right for both dialects.
func TestURLDecodeKeyHandlesBothDialects(t *testing.T) {
	cases := map[string]string{
		"hello+world.jpg":   "hello world.jpg", // AWS / Wasabi / MinIO
		"hello%20world.jpg": "hello world.jpg", // Garage
		"plus%2Bfile.pdf":   "plus+file.pdf",   // both
		"a%2Fb%2Fc.txt":     "a/b/c.txt",
		"100%":              "100%", // malformed stays as-is
	}
	for in, want := range cases {
		if got := urlDecodeKey(in); got != want {
			t.Errorf("urlDecodeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeadObjectReportsMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path == "/b/missing.jpg" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", "1234")
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := newTestS3(t, srv.URL)

	info, exists, err := c.HeadObject(context.Background(), "b", "a.png")
	if err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	if info.Size != 1234 || info.ContentType != "image/png" || info.ETag != "abc123" {
		t.Errorf("info = %+v", info)
	}
	if info.LastModified.Year() != 2015 {
		t.Errorf("Last-Modified not parsed: %v", info.LastModified)
	}
	if _, exists, err := c.HeadObject(context.Background(), "b", "missing.jpg"); err != nil || exists {
		t.Errorf("missing object: exists=%v err=%v", exists, err)
	}
}

func TestCopyObjectSendsSignedCopySource(t *testing.T) {
	var gotSource, gotDirective, gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSource = r.Header.Get("X-Amz-Copy-Source")
		gotDirective = r.Header.Get("X-Amz-Metadata-Directive")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.EscapedPath()
		io.WriteString(w, `<CopyObjectResult><ETag>"x"</ETag></CopyObjectResult>`)
	}))
	defer srv.Close()
	c := newTestS3(t, srv.URL)

	if err := c.CopyObject(context.Background(), "src", "folder/a b+c.jpg", "dst", "new/name.jpg"); err != nil {
		t.Fatal(err)
	}
	if gotSource != "/src/folder/a%20b%2Bc.jpg" {
		t.Errorf("x-amz-copy-source = %q", gotSource)
	}
	if gotDirective != "COPY" {
		t.Errorf("metadata directive = %q", gotDirective)
	}
	if gotPath != "/dst/new/name.jpg" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotAuth, "x-amz-copy-source") {
		t.Errorf("x-amz-copy-source must be signed: %s", gotAuth)
	}
}

// AWS and Wasabi can answer HTTP 200 with an <Error> body for CopyObject.
func TestCopyObjectDetectsErrorBodyBehind200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<Error><Code>InternalError</Code><Message>We encountered an internal error. Please try again.</Message></Error>`)
	}))
	defer srv.Close()
	c := newTestS3(t, srv.URL)

	err := c.CopyObject(context.Background(), "src", "a.jpg", "dst", "b.jpg")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "InternalError") || !strings.Contains(err.Error(), "internal error") {
		t.Errorf("error = %v", err)
	}
}

func TestDeleteObjectsSendsContentMD5AndReportsPerKeyErrors(t *testing.T) {
	var gotBody []byte
	var gotMD5, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotMD5 = r.Header.Get("Content-MD5")
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `<DeleteResult>
  <Error><Key>locked.jpg</Key><Code>AccessDenied</Code><Message>no</Message></Error>
</DeleteResult>`)
	}))
	defer srv.Close()
	c := newTestS3(t, srv.URL)

	failed, err := c.DeleteObjects(context.Background(), "b", []string{"a.jpg", "locked.jpg", "dir/c d.png"})
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "delete=" {
		t.Errorf("query = %q, want delete=", gotQuery)
	}
	sum := md5.Sum(gotBody)
	if want := base64.StdEncoding.EncodeToString(sum[:]); gotMD5 != want {
		t.Errorf("Content-MD5 = %q, want %q", gotMD5, want)
	}
	if !strings.Contains(gotAuth, "content-md5") {
		t.Errorf("Content-MD5 must be signed: %s", gotAuth)
	}
	body := string(gotBody)
	for _, want := range []string{"<Quiet>true</Quiet>", "<Key>a.jpg</Key>", "<Key>locked.jpg</Key>", "<Key>dir/c d.png</Key>", `xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s:\n%s", want, body)
		}
	}
	if len(failed) != 1 || failed[0].Key != "locked.jpg" || failed[0].Code != "AccessDenied" {
		t.Errorf("failed = %+v", failed)
	}

	if _, err := c.DeleteObjects(context.Background(), "b", make([]string, maxDeleteObjects+1)); err == nil {
		t.Error("more than 1000 keys must be refused before any request is sent")
	}
	if _, err := c.DeleteObjects(context.Background(), "b", []string{"../x"}); err == nil {
		t.Error("an invalid key must be refused")
	}
}

func TestPutObjectWithEmptyBodyCreatesFolderMarker(t *testing.T) {
	var gotLen int64 = -1
	var gotHash string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLen = r.ContentLength
		gotHash = r.Header.Get("X-Amz-Content-Sha256")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := newTestS3(t, srv.URL)
	if err := c.PutObject(context.Background(), "b", "folder/", []byte{}, "application/x-directory"); err != nil {
		t.Fatal(err)
	}
	if gotLen != 0 || gotHash != emptyPayloadHash {
		t.Errorf("Content-Length = %d, hash = %s", gotLen, gotHash)
	}
}

// A remote's 403 must not send the operator to garage.toml.
func TestRemoteForbiddenHintIsNotGarageSpecific(t *testing.T) {
	c, err := NewS3With("https://s3.ap-southeast-1.wasabisys.com", "AK", "SK", "ap-southeast-1", S3Options{Label: "wasabi-sg"})
	if err != nil {
		t.Fatal(err)
	}
	sig := c.forbiddenHint(&S3Error{StatusCode: http.StatusForbidden, Code: "SignatureDoesNotMatch", Message: "The request signature we calculated does not match"})
	for _, want := range []string{"wasabi-sg", `"ap-southeast-1"`, "s3.wasabisys.com", "BUKAN soal izin"} {
		if !strings.Contains(sig, want) {
			t.Errorf("remote signature hint missing %q:\n%s", want, sig)
		}
	}
	for _, forbidden := range []string{"garage.toml", "GARAGE_S3_SECRET_KEY", "systemctl restart garagepanel"} {
		if strings.Contains(sig, forbidden) {
			t.Errorf("remote hint must not mention %q", forbidden)
		}
	}
	perm := c.forbiddenHint(&S3Error{StatusCode: http.StatusForbidden, Code: "AccessDenied", Message: "Access Denied"})
	if !strings.Contains(perm, "wasabi-sg") || strings.Contains(perm, "garage bucket allow") {
		t.Errorf("remote permission hint wrong:\n%s", perm)
	}
}

func TestNewS3WithRejectsBadEndpointWithoutNamingGarage(t *testing.T) {
	if _, err := NewS3With("s3.wasabisys.com", "a", "b", "us-east-1", S3Options{Label: "w"}); err == nil {
		t.Fatal("scheme-less endpoint must be rejected")
	} else if strings.Contains(err.Error(), "GARAGE_S3_URL") {
		t.Errorf("remote error must not mention GARAGE_S3_URL: %v", err)
	}
	if _, err := NewS3("nope", "a", "b", "garage"); err == nil || !strings.Contains(err.Error(), "GARAGE_S3_URL") {
		t.Errorf("Garage error should name GARAGE_S3_URL: %v", err)
	}
}
