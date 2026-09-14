package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Minimal S3 client with hand-written AWS Signature Version 4.
//
// No SDK: signing is crypto/hmac + crypto/sha256, requests are net/http, and
// responses are parsed with encoding/xml. Garage is addressed path-style
// (http://host:3900/<bucket>/<key>).

const (
	// emptyPayloadHash is sha256("").
	emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	iso8601BasicFmt  = "20060102T150405Z"
	dateStampFmt     = "20060102"
)

// S3 is a tiny S3 client for the Garage S3 endpoint.
type S3 struct {
	endpoint  *url.URL
	accessKey string
	secretKey string
	region    string
	http      *http.Client
}

// NewS3 builds a client. It returns nil when credentials are not configured, so
// callers can report "S3 credentials belum diatur" instead of failing at
// request time.
func NewS3(endpoint, accessKey, secretKey, region string) (*S3, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("GARAGE_S3_URL tidak valid: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("GARAGE_S3_URL harus berupa URL lengkap, contoh http://127.0.0.1:3900")
	}
	return &S3{
		endpoint:  u,
		accessKey: accessKey,
		secretKey: secretKey,
		region:    region,
		http: &http.Client{
			Timeout: 5 * time.Minute, // uploads of up to 50 MB
		},
	}, nil
}

// S3Error is a parsed S3 error document.
type S3Error struct {
	Op         string
	StatusCode int
	Code       string
	Message    string
	Body       string
}

func (e *S3Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "S3 %s gagal (HTTP %d)", e.Op, e.StatusCode)
	switch {
	case e.Code != "" && e.Message != "":
		fmt.Fprintf(&b, ": %s: %s", e.Code, e.Message)
	case e.Code != "":
		fmt.Fprintf(&b, ": %s", e.Code)
	case e.Body != "":
		fmt.Fprintf(&b, ": %s", e.Body)
	}
	return b.String()
}

// NotFound reports whether the object or bucket does not exist.
func (e *S3Error) NotFound() bool {
	return e.StatusCode == http.StatusNotFound || e.Code == "NoSuchKey" || e.Code == "NoSuchBucket"
}

// --- SigV4 ----------------------------------------------------------------

// awsURIEncode percent-encodes per RFC 3986 as required by SigV4: only
// A-Z a-z 0-9 - _ . ~ are left alone. url.QueryEscape cannot be used because it
// encodes a space as "+".
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		case c == '/':
			if encodeSlash {
				b.WriteString("%2F")
			} else {
				b.WriteByte('/')
			}
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalQuery builds the canonical query string: sorted, AWS-encoded pairs.
func canonicalQuery(params [][2]string) string {
	encoded := make([]string, 0, len(params))
	for _, kv := range params {
		encoded = append(encoded, awsURIEncode(kv[0], true)+"="+awsURIEncode(kv[1], true))
	}
	sort.Strings(encoded)
	return strings.Join(encoded, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sign adds the SigV4 Authorization header to req. canonicalPath must be the
// already-encoded path that will appear on the wire, and payloadHash the hex
// sha256 of the body.
func (c *S3) sign(req *http.Request, canonicalPath, canonicalQueryString, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format(iso8601BasicFmt)
	dateStamp := now.UTC().Format(dateStampFmt)

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	// Sign host, x-amz-* and content-type when present.
	type hdr struct{ name, value string }
	headers := []hdr{
		{"host", req.Host},
		{"x-amz-content-sha256", payloadHash},
		{"x-amz-date", amzDate},
	}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		headers = append(headers, hdr{"content-type", ct})
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].name < headers[j].name })

	var canonicalHeaders strings.Builder
	signedNames := make([]string, 0, len(headers))
	for _, h := range headers {
		canonicalHeaders.WriteString(h.name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.TrimSpace(h.value))
		canonicalHeaders.WriteByte('\n')
		signedNames = append(signedNames, h.name)
	}
	signedHeaders := strings.Join(signedNames, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalPath,
		canonicalQueryString,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + c.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+c.secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, c.region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.accessKey, scope, signedHeaders, signature))
}

// newRequest builds and signs a request against <endpoint>/<bucket>[/<key>].
func (c *S3) newRequest(ctx context.Context, method, bucket, key string, params [][2]string, body []byte, contentType string) (*http.Request, error) {
	rawPath := "/" + bucket
	if key != "" {
		rawPath += "/" + key
	}
	// The wire path and the signed canonical path must be byte-identical, so
	// set RawPath explicitly instead of letting net/url pick its own escaping.
	encodedPath := awsURIEncode(rawPath, false)
	cq := canonicalQuery(params)

	u := *c.endpoint
	u.Path = rawPath
	u.RawPath = encodedPath
	u.RawQuery = cq

	var reader io.Reader
	payloadHash := emptyPayloadHash
	if body != nil {
		reader = bytes.NewReader(body)
		payloadHash = sha256Hex(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.sign(req, encodedPath, cq, payloadHash, time.Now())
	return req, nil
}

// do executes a signed request and turns a non-2xx answer into an *S3Error.
func (c *S3) do(req *http.Request, op string) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tidak bisa menghubungi S3 API di %s (%s): %v", c.endpoint.String(), op, unwrapURLError(err))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		s3err := &S3Error{Op: op, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
		var doc struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		if xml.Unmarshal(raw, &doc) == nil {
			s3err.Code = doc.Code
			s3err.Message = doc.Message
		}
		if len(s3err.Body) > 500 {
			s3err.Body = s3err.Body[:500] + "…"
		}
		if resp.StatusCode == http.StatusForbidden && s3err.Message == "" {
			s3err.Message = "akses ditolak — periksa GARAGE_S3_ACCESS_KEY/GARAGE_S3_SECRET_KEY dan izin key pada bucket ini"
		}
		return nil, s3err
	}
	return resp, nil
}

// --- ListObjectsV2 --------------------------------------------------------

// ObjectEntry is one object in a listing.
type ObjectEntry struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
}

// ListResult is one page of ListObjectsV2.
type ListResult struct {
	Objects        []ObjectEntry
	CommonPrefixes []string
	IsTruncated    bool
	NextToken      string
	KeyCount       int
}

type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Name                  string   `xml:"Name"`
	Prefix                string   `xml:"Prefix"`
	Delimiter             string   `xml:"Delimiter"`
	MaxKeys               int      `xml:"MaxKeys"`
	KeyCount              int      `xml:"KeyCount"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

// ListObjectsV2 lists one page of objects. delimiter may be "" (flat listing)
// or "/" (folder-style). The returned NextToken is passed back verbatim to get
// the next page.
func (c *S3) ListObjectsV2(ctx context.Context, bucket, prefix, continuationToken, delimiter string, maxKeys int) (*ListResult, error) {
	if maxKeys <= 0 {
		maxKeys = 100
	}
	params := [][2]string{
		{"list-type", "2"},
		{"max-keys", strconv.Itoa(maxKeys)},
		// Ask for URL-encoded keys so names with newlines or other awkward
		// bytes survive the XML round-trip.
		{"encoding-type", "url"},
	}
	if prefix != "" {
		params = append(params, [2]string{"prefix", prefix})
	}
	if continuationToken != "" {
		params = append(params, [2]string{"continuation-token", continuationToken})
	}
	if delimiter != "" {
		params = append(params, [2]string{"delimiter", delimiter})
	}

	req, err := c.newRequest(ctx, http.MethodGet, bucket, "", params, nil, "")
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req, "ListObjectsV2")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var doc listBucketResult
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("ListObjectsV2: response tidak bisa dibaca: %w", err)
	}

	out := &ListResult{
		IsTruncated: doc.IsTruncated,
		// The continuation token is opaque and is not affected by
		// encoding-type, so it is passed through untouched.
		NextToken: doc.NextContinuationToken,
		KeyCount:  doc.KeyCount,
	}
	for _, cp := range doc.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, urlDecodeKey(cp.Prefix))
	}
	for _, item := range doc.Contents {
		entry := ObjectEntry{
			Key:  urlDecodeKey(item.Key),
			Size: item.Size,
			ETag: strings.Trim(item.ETag, `"`),
		}
		entry.LastModified = parseS3Time(item.LastModified)
		out.Objects = append(out.Objects, entry)
	}
	return out, nil
}

// urlDecodeKey reverses encoding-type=url. PathUnescape is used rather than
// QueryUnescape so that "+" inside a key stays a plus sign.
func urlDecodeKey(s string) string {
	if decoded, err := url.PathUnescape(s); err == nil {
		return decoded
	}
	return s
}

func parseS3Time(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// --- object operations ----------------------------------------------------

// PutObject stores body under key.
func (c *S3) PutObject(ctx context.Context, bucket, key string, body []byte, contentType string) error {
	if err := ValidateObjectKey(key); err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPut, bucket, key, nil, body, contentType)
	if err != nil {
		return err
	}
	resp, err := c.do(req, "PutObject")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return nil
}

// GetObject fetches an object. The caller must close the returned body.
func (c *S3) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, http.Header, error) {
	if err := ValidateObjectKey(key); err != nil {
		return nil, nil, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, bucket, key, nil, nil, "")
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.do(req, "GetObject")
	if err != nil {
		return nil, nil, err
	}
	return resp.Body, resp.Header, nil
}

// StatObject issues a HEAD and reports whether the object already exists. It is
// what makes content-addressed uploads skip work for a file that is already
// stored.
func (c *S3) StatObject(ctx context.Context, bucket, key string) (exists bool, size int64, err error) {
	if err := ValidateObjectKey(key); err != nil {
		return false, 0, err
	}
	req, err := c.newRequest(ctx, http.MethodHead, bucket, key, nil, nil, "")
	if err != nil {
		return false, 0, err
	}
	resp, err := c.do(req, "HeadObject")
	if err != nil {
		var s3err *S3Error
		if errors.As(err, &s3err) && s3err.NotFound() {
			return false, 0, nil
		}
		return false, 0, err
	}
	defer resp.Body.Close()
	return true, resp.ContentLength, nil
}

// DeleteObject removes an object.
func (c *S3) DeleteObject(ctx context.Context, bucket, key string) error {
	if err := ValidateObjectKey(key); err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodDelete, bucket, key, nil, nil, "")
	if err != nil {
		return err
	}
	resp, err := c.do(req, "DeleteObject")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return nil
}
