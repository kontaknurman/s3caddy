package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The two vectors below are AWS's own published "Signature Calculations"
// examples for S3 (GET Bucket Lifecycle and GET Bucket / List Objects). They
// are the two examples whose signed header set is exactly the one this client
// uses: host;x-amz-content-sha256;x-amz-date.
func TestSignMatchesAWSExamples(t *testing.T) {
	const (
		accessKey = "AKIAIOSFODNN7EXAMPLE"
		secretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		host      = "examplebucket.s3.amazonaws.com"
	)
	fixed := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		path  string
		query string
		want  string
	}{
		{
			name:  "GET Bucket Lifecycle",
			path:  "/",
			query: "lifecycle=",
			want:  "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name:  "GET Bucket (List Objects)",
			path:  "/",
			query: "max-keys=2&prefix=J",
			want:  "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &S3{accessKey: accessKey, secretKey: secretKey, region: "us-east-1"}
			req, err := http.NewRequest(http.MethodGet, "https://"+host+tc.path+"?"+tc.query, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.sign(req, tc.path, tc.query, emptyPayloadHash, fixed)

			got := signatureFrom(req.Header.Get("Authorization"))
			if got != tc.want {
				t.Errorf("signature\n got %s\nwant %s", got, tc.want)
			}
			if want := "SignedHeaders=host;x-amz-content-sha256;x-amz-date"; !strings.Contains(req.Header.Get("Authorization"), want) {
				t.Errorf("Authorization header missing %q: %s", want, req.Header.Get("Authorization"))
			}
		})
	}
}

func signatureFrom(auth string) string {
	_, sig, found := strings.Cut(auth, "Signature=")
	if !found {
		return ""
	}
	return sig
}

func TestAWSURIEncode(t *testing.T) {
	cases := []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		{"/bucket/plain.txt", false, "/bucket/plain.txt"},
		{"/bucket/with space.txt", false, "/bucket/with%20space.txt"},
		{"/bucket/plus+sign.txt", false, "/bucket/plus%2Bsign.txt"},
		{"/bucket/tilde~dash-dot.under_.txt", false, "/bucket/tilde~dash-dot.under_.txt"},
		{"/bucket/ünïcode.jpg", false, "/bucket/%C3%BCn%C3%AFcode.jpg"},
		{"a/b", true, "a%2Fb"},
		{"100%", false, "100%25"},
		{"a=b&c", true, "a%3Db%26c"},
	}
	for _, tc := range cases {
		if got := awsURIEncode(tc.in, tc.encodeSlash); got != tc.want {
			t.Errorf("awsURIEncode(%q, %v) = %q, want %q", tc.in, tc.encodeSlash, got, tc.want)
		}
	}
}

func TestCanonicalQueryIsSorted(t *testing.T) {
	got := canonicalQuery([][2]string{
		{"prefix", "a b"},
		{"list-type", "2"},
		{"max-keys", "100"},
	})
	want := "list-type=2&max-keys=100&prefix=a%20b"
	if got != want {
		t.Errorf("canonicalQuery = %q, want %q", got, want)
	}
}

// The signed canonical path and the path actually put on the wire must be
// byte-identical, otherwise Garage rejects the signature.
func TestNewRequestWireEncodingMatchesSignature(t *testing.T) {
	c := newTestS3(t, "http://127.0.0.1:3900")
	req, err := c.newRequest(context.Background(), http.MethodGet, "media", "folder/hello world+ü.jpg", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "/media/folder/hello%20world%2B%C3%BC.jpg"
	if got := req.URL.EscapedPath(); got != want {
		t.Errorf("wire path = %q, want %q", got, want)
	}
	if req.Header.Get("X-Amz-Content-Sha256") != emptyPayloadHash {
		t.Errorf("empty GET should carry the empty payload hash")
	}
}

func newTestS3(t *testing.T, endpoint string) *S3 {
	t.Helper()
	c, err := NewS3(endpoint, "GK-test", "secret-test", "garage")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const listXML = `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult>
  <Name>media</Name>
  <Prefix></Prefix>
  <KeyCount>2</KeyCount>
  <MaxKeys>100</MaxKeys>
  <Delimiter>%2F</Delimiter>
  <IsTruncated>true</IsTruncated>
  <NextContinuationToken>abc/def+xyz==</NextContinuationToken>
  <Contents>
    <Key>hello%20world.jpg</Key>
    <LastModified>2026-01-02T03:04:05.000Z</LastModified>
    <ETag>&quot;d41d8cd98f00b204e9800998ecf8427e&quot;</ETag>
    <Size>2048</Size>
  </Contents>
  <Contents>
    <Key>plus%2Bfile.pdf</Key>
    <LastModified>2026-01-03T10:00:00.000Z</LastModified>
    <ETag>&quot;x&quot;</ETag>
    <Size>15</Size>
  </Contents>
  <CommonPrefixes><Prefix>thumbs%2F</Prefix></CommonPrefixes>
</ListBucketResult>`

func TestListObjectsV2(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, listXML)
	}))
	defer srv.Close()

	c := newTestS3(t, srv.URL)
	res, err := c.ListObjectsV2(context.Background(), "media", "", "", "/", 100)
	if err != nil {
		t.Fatal(err)
	}

	if gotPath != "/media" {
		t.Errorf("path = %q, want /media", gotPath)
	}
	for _, want := range []string{"list-type=2", "max-keys=100", "encoding-type=url", "delimiter=%2F"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=GK-test/") {
		t.Errorf("unexpected Authorization: %q", gotAuth)
	}

	if len(res.Objects) != 2 {
		t.Fatalf("got %d objects, want 2", len(res.Objects))
	}
	// encoding-type=url must be decoded, and "+" must survive as a plus sign.
	if res.Objects[0].Key != "hello world.jpg" {
		t.Errorf("key[0] = %q, want %q", res.Objects[0].Key, "hello world.jpg")
	}
	if res.Objects[1].Key != "plus+file.pdf" {
		t.Errorf("key[1] = %q, want %q", res.Objects[1].Key, "plus+file.pdf")
	}
	if res.Objects[0].Size != 2048 {
		t.Errorf("size = %d, want 2048", res.Objects[0].Size)
	}
	if res.Objects[0].LastModified.IsZero() {
		t.Error("LastModified not parsed")
	}
	if len(res.CommonPrefixes) != 1 || res.CommonPrefixes[0] != "thumbs/" {
		t.Errorf("common prefixes = %v, want [thumbs/]", res.CommonPrefixes)
	}
	// The continuation token is opaque and must be handed back untouched.
	if res.NextToken != "abc/def+xyz==" {
		t.Errorf("NextToken = %q, want %q", res.NextToken, "abc/def+xyz==")
	}
	if !res.IsTruncated {
		t.Error("IsTruncated should be true")
	}
}

func TestPutGetStatDeleteObject(t *testing.T) {
	stored := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/media/")
		switch r.Method {
		case http.MethodPut:
			buf := make([]byte, r.ContentLength)
			if _, err := r.Body.Read(buf); err != nil && err.Error() != "EOF" {
				t.Errorf("read body: %v", err)
			}
			stored[key] = buf
			if got := r.Header.Get("Content-Type"); got != "image/png" {
				t.Errorf("content type = %q, want image/png", got)
			}
			if r.Header.Get("X-Amz-Content-Sha256") == emptyPayloadHash {
				t.Error("PUT with a body must not use the empty payload hash")
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			if _, ok := stored[key]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			body, ok := stored[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(body)
		case http.MethodDelete:
			delete(stored, key)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := newTestS3(t, srv.URL)
	ctx := context.Background()

	exists, _, err := c.StatObject(ctx, "media", "a.png")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("object should not exist yet")
	}

	if err := c.PutObject(ctx, "media", "a.png", []byte("payload"), "image/png"); err != nil {
		t.Fatal(err)
	}
	if exists, _, err = c.StatObject(ctx, "media", "a.png"); err != nil || !exists {
		t.Fatalf("after put: exists=%v err=%v", exists, err)
	}

	body, _, err := c.GetObject(ctx, "media", "a.png")
	if err != nil {
		t.Fatal(err)
	}
	body.Close()

	if err := c.DeleteObject(ctx, "media", "a.png"); err != nil {
		t.Fatal(err)
	}
	if _, ok := stored["a.png"]; ok {
		t.Error("object should be gone")
	}
}

func TestS3ErrorIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>key tidak punya izin write</Message></Error>`)
	}))
	defer srv.Close()

	c := newTestS3(t, srv.URL)
	err := c.PutObject(context.Background(), "media", "a.png", []byte("x"), "image/png")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"AccessDenied", "key tidak punya izin write", "403"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// A transport failure must not leak the signed URL or credentials.
func TestTransportErrorIsStripped(t *testing.T) {
	c := newTestS3(t, "http://127.0.0.1:1")
	err := c.DeleteObject(context.Background(), "media", "a.png")
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "X-Amz") || strings.Contains(err.Error(), "secret-test") {
		t.Errorf("error leaks credentials: %v", err)
	}
}

func TestPublicURLEscapesKey(t *testing.T) {
	got := publicURL("cdn.example.com", "folder/hello world.jpg")
	want := "https://cdn.example.com/folder/hello%20world.jpg"
	if got != want {
		t.Errorf("publicURL = %q, want %q", got, want)
	}
	if _, err := url.Parse(got); err != nil {
		t.Errorf("publicURL is not parseable: %v", err)
	}
}
