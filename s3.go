package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
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
// responses are parsed with encoding/xml. Every endpoint is addressed
// path-style (http://host:3900/<bucket>/<key>), which Garage, Wasabi, AWS and
// MinIO all accept.
//
// The client is deliberately small: it lists, inspects, copies and deletes.
// Bulk transfers between providers are rclone's job (see rclone.go).

const (
	// emptyPayloadHash is sha256("").
	emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	iso8601BasicFmt  = "20060102T150405Z"
	dateStampFmt     = "20060102"

	// maxDeleteObjects is the S3 limit for one DeleteObjects request.
	maxDeleteObjects = 1000
)

// S3 is a tiny S3 client for one endpoint.
type S3 struct {
	endpoint  *url.URL
	accessKey string
	secretKey string
	region    string
	http      *http.Client

	// label names the endpoint in messages ("Garage" or the remote's name).
	label string
	// garage selects the Garage-specific troubleshooting text.
	garage bool
	// provider is the preset name, for provider-specific hints.
	provider string
	// virtualHost addresses buckets as <bucket>.<host> instead of
	// <host>/<bucket>. Hetzner only accepts the former; Garage, Wasabi, MinIO
	// and AWS accept both.
	virtualHost bool
}

// S3Options tunes a client for an endpoint other than the panel's own Garage.
type S3Options struct {
	Label       string
	Garage      bool
	Provider    string
	VirtualHost bool
	// dialAddr, when set, makes every connection go to this address whatever
	// the URL's host says. Tests use it so <bucket>.127.0.0.1 resolves.
	dialAddr string
}

// NewS3 builds a client for the panel's own Garage. It returns an error when
// the endpoint is not a full URL, so callers can report "GARAGE_S3_URL tidak
// valid" at start-up instead of failing at request time.
func NewS3(endpoint, accessKey, secretKey, region string) (*S3, error) {
	c, err := NewS3With(endpoint, accessKey, secretKey, region, S3Options{Label: "Garage", Garage: true})
	if err != nil {
		return nil, fmt.Errorf("GARAGE_S3_URL %w", err)
	}
	return c, nil
}

// NewS3With builds a client for any S3-compatible endpoint (Wasabi, AWS,
// MinIO, another Garage). The panel uses it to list and probe remotes; the
// transfers themselves run through rclone.
func NewS3With(endpoint, accessKey, secretKey, region string, o S3Options) (*S3, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("tidak valid: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("harus berupa URL lengkap, contoh http://127.0.0.1:3900 (dapat %q)", endpoint)
	}
	label := o.Label
	if label == "" {
		label = u.Host
	}
	client := &http.Client{
		Timeout: 5 * time.Minute, // uploads of up to 50 MB
	}
	if o.dialAddr != "" {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, o.dialAddr)
			},
		}
	}
	return &S3{
		endpoint:    u,
		accessKey:   accessKey,
		secretKey:   secretKey,
		region:      region,
		label:       label,
		garage:      o.Garage,
		provider:    o.Provider,
		virtualHost: o.VirtualHost,
		http:        client,
	}, nil
}

// Label reports the human name of the endpoint.
func (c *S3) Label() string { return c.label }

// VirtualHost reports the addressing style in use.
func (c *S3) VirtualHost() bool { return c.virtualHost }

// S3Error is a parsed S3 error document.
type S3Error struct {
	Op         string
	StatusCode int
	Code       string
	Message    string
	Body       string
	// Hint menerjemahkan penolakan yang mudah disalahartikan menjadi langkah
	// yang bisa ditindaklanjuti.
	Hint string
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
	if e.Hint != "" {
		fmt.Fprintf(&b, "\n\n%s", e.Hint)
	}
	return b.String()
}

// forbiddenHint menjelaskan penolakan 403, yang punya dua sebab yang sangat
// berbeda dan gampang tertukar: tanda tangan yang tidak cocok, versus key yang
// memang belum diberi izin.
func (c *S3) forbiddenHint(e *S3Error) string {
	if e.StatusCode != http.StatusForbidden {
		return ""
	}
	blob := strings.ToLower(e.Message + " " + e.Code + " " + e.Body)
	signature := strings.Contains(blob, "signature")

	if !c.garage {
		if signature {
			return "Tanda tangan SigV4 ditolak oleh " + c.label + " — ini BUKAN soal izin bucket. Yang perlu dicek:\n" +
				"\n" +
				"  1. Secret key remote salah ketik atau terpotong (spasi di ujung ikut merusak).\n" +
				"  2. Region tidak cocok dengan endpoint. Panel menandatangani dengan region " + strconv.Quote(c.region) + ".\n" +
				"     Wasabi: s3.wasabisys.com → us-east-1, s3.<region>.wasabisys.com → <region>.\n" +
				"     AWS: region tempat bucket berada. MinIO/Garage: nilai region di config servernya.\n" +
				"  3. Jam server meleset jauh — tanda tangan memuat stempel waktu."
		}
		return "Kredensial remote " + c.label + " diterima, tapi aksinya ditolak — key itu belum punya izin\n" +
			"pada bucket ini. Periksa policy/izin key di penyedia S3-nya."
	}

	if signature {
		return "Tanda tangan SigV4 ditolak — ini BUKAN soal izin bucket, dan tidak ada\n" +
			"hubungannya dengan status public/private bucket. Yang perlu dicek:\n" +
			"\n" +
			"  1. GARAGE_S3_SECRET_KEY salah ketik atau terpotong. Spasi atau carriage\n" +
			"     return yang ikut terbawa juga menyebabkan ini — file env berakhiran\n" +
			"     CRLF adalah penyebab tersering. Cek dengan:\n" +
			"       sudo cat -A /etc/garagepanel/garagepanel.env | grep SECRET\n" +
			"     Kalau ujung barisnya bukan hanya tanda dolar, ubah ke LF:\n" +
			"       sudo sed -i 's/\\r$//' /etc/garagepanel/garagepanel.env\n" +
			"\n" +
			"  2. Region tidak cocok. Panel menandatangani dengan region " + strconv.Quote(c.region) + ";\n" +
			"     itu harus sama persis dengan s3_region di /etc/garage.toml:\n" +
			"       sudo grep s3_region /etc/garage.toml\n" +
			"\n" +
			"  3. Jam server meleset jauh — tanda tangan memuat stempel waktu:\n" +
			"       timedatectl status | grep 'System clock'\n" +
			"\n" +
			"Setelah diperbaiki: sudo systemctl restart garagepanel"
	}

	return "Kredensialnya diterima, tapi aksinya ditolak — kemungkinan besar key panel\n" +
		"belum diberi izin pada bucket ini. Status public/private bucket tidak\n" +
		"berpengaruh di sini: halaman Objek selalu lewat S3 API, bukan lewat web\n" +
		"endpoint. Beri izin dengan:\n" +
		"  garage bucket allow --read --write <bucket> --key <nama-key-panel>"
}

// redact replaces the client's secret key wherever it appears in text.
func (c *S3) redact(text string) string {
	if c.secretKey == "" {
		return text
	}
	return strings.ReplaceAll(text, c.secretKey, "[secret]")
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

// signedHeaderName reports whether a request header takes part in the
// signature. Host is always signed; every x-amz-* header must be (S3 rejects
// an unsigned x-amz-copy-source, for instance); content-type, content-md5 and
// date are signed when present because AWS's own examples do. Authorization
// itself never is.
func signedHeaderName(lower string) bool {
	if strings.HasPrefix(lower, "x-amz-") {
		return true
	}
	switch lower {
	case "content-type", "content-md5", "date":
		return true
	}
	return false
}

// sign adds the SigV4 Authorization header to req. canonicalPath must be the
// already-encoded path that will appear on the wire, and payloadHash the hex
// sha256 of the body. Every header already present on req that
// signedHeaderName accepts is covered by the signature, so callers set their
// x-amz-* headers before calling sign.
func (c *S3) sign(req *http.Request, canonicalPath, canonicalQueryString, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format(iso8601BasicFmt)
	dateStamp := now.UTC().Format(dateStampFmt)

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	type hdr struct{ name, value string }
	headers := []hdr{{"host", req.Host}}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if !signedHeaderName(lower) {
			continue
		}
		trimmed := make([]string, len(values))
		for i, v := range values {
			trimmed[i] = strings.TrimSpace(v)
		}
		headers = append(headers, hdr{lower, strings.Join(trimmed, ",")})
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].name < headers[j].name })

	var canonicalHeaders strings.Builder
	signedNames := make([]string, 0, len(headers))
	for _, h := range headers {
		canonicalHeaders.WriteString(h.name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(h.value)
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
	return c.newRequestWith(ctx, method, bucket, key, params, body, contentType, nil)
}

// newRequestWith is newRequest with extra headers that must be part of the
// signature (x-amz-copy-source, content-md5, …). A non-nil empty body sends a
// zero-length payload with Content-Length: 0, which is how folder markers and
// other empty objects are created.
func (c *S3) newRequestWith(ctx context.Context, method, bucket, key string, params [][2]string, body []byte, contentType string, extra map[string]string) (*http.Request, error) {
	u := *c.endpoint
	rawPath := "/"
	if c.virtualHost && bucket != "" {
		u.Host = bucket + "." + c.endpoint.Host
		if key != "" {
			rawPath += key
		}
	} else {
		rawPath += bucket
		if key != "" {
			rawPath += "/" + key
		}
	}
	// The wire path and the signed canonical path must be byte-identical, so
	// set RawPath explicitly instead of letting net/url pick its own escaping.
	encodedPath := awsURIEncode(rawPath, false)
	cq := canonicalQuery(params)

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
	for name, value := range extra {
		req.Header.Set(name, value)
	}
	c.sign(req, encodedPath, cq, payloadHash, time.Now())
	return req, nil
}

// do executes a signed request and turns a non-2xx answer into an *S3Error.
func (c *S3) do(req *http.Request, op string) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tidak bisa menghubungi S3 API %s di %s (%s): %v", c.label, c.endpoint.String(), op, unwrapURLError(err))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, c.errorFromBody(op, resp.StatusCode, raw)
	}
	return resp, nil
}

// errorFromBody builds an *S3Error out of an error document. The secret key
// is scrubbed from whatever the provider sent back: no provider echoes it,
// but the message ends up on a page and in the journal, so nothing is
// trusted to keep it out.
func (c *S3) errorFromBody(op string, status int, raw []byte) *S3Error {
	s3err := &S3Error{Op: op, StatusCode: status, Body: c.redact(strings.TrimSpace(string(raw)))}
	var doc struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(raw, &doc) == nil {
		s3err.Code = c.redact(doc.Code)
		s3err.Message = c.redact(doc.Message)
	}
	if len(s3err.Body) > 500 {
		s3err.Body = s3err.Body[:500] + "…"
	}
	s3err.Hint = c.hintFor(s3err)
	return s3err
}

// hintFor turns the answers that are easy to misread into next steps.
func (c *S3) hintFor(e *S3Error) string {
	code := e.Code
	blob := strings.ToLower(e.Message + " " + e.Body)
	switch {
	case code == "InvalidAccessKeyId" || strings.Contains(blob, "malformed access key"):
		hint := "Access key ini tidak dikenal oleh " + c.label + " — ini soal access key ID, bukan secret atau izin.\n"
		switch c.provider {
		case "Backblaze":
			hint += "Backblaze B2: pakai keyID dari application key yang dibuat manual (Application Keys → Add a New Application Key).\n" +
				"Master application key tidak bisa dipakai di S3 API, dan yang dimasukkan harus keyID-nya, bukan nama key."
		case "Hetzner":
			hint += "Hetzner: buat kredensial S3 di Cloud Console → Object Storage → Manage credentials, lalu masukkan Access Key-nya (32 karakter hex)."
		default:
			hint += "Periksa access key ID-nya (bukan nama key atau secret), dan pastikan key itu dibuat di akun/project yang sama dengan bucket."
		}
		return hint
	case code == "AuthorizationHeaderMalformed" || code == "PermanentRedirect" || code == "TemporaryRedirect":
		return "Endpoint atau region tidak cocok dengan lokasi bucket. Pesan di atas biasanya menyebut region yang benar;\n" +
			"ubah endpoint dan region remote ini sesuai region bucket (region yang dipakai sekarang: " + strconv.Quote(c.region) + ")."
	case code == "NoSuchBucket":
		return "Bucket tidak ada di endpoint ini. Periksa ejaannya, dan pastikan endpoint/region sesuai lokasi bucket — bucket di region lain tidak terlihat dari sini."
	case e.StatusCode == http.StatusForbidden:
		return c.forbiddenHint(e)
	}
	return ""
}

// writeDeniedHint explains a write that was refused although reading works.
func (c *S3) writeDeniedHint() string {
	hint := "Bucket bisa dibaca, tapi menulis ke sana ditolak. Penyebab yang paling sering:\n" +
		"  1. Key hanya punya izin baca (read-only) — cek izin/policy key di penyedia.\n" +
		"  2. Bucket milik akun/project lain: bucket public bisa di-list siapa saja, tapi hanya pemiliknya yang boleh menulis."
	switch c.provider {
	case "Hetzner":
		hint += "\n  3. Hetzner: kredensial S3 berlaku per project — bucket dan kredensial harus dari project yang sama,\n" +
			"     dan alamatnya harus virtual-host (panel mencoba kedua gaya alamat saat tes koneksi)."
	case "Backblaze":
		hint += "\n  3. Backblaze: application key perlu capability writeFiles dan deleteFiles, dan kalau dibatasi ke bucket, harus bucket ini."
	case "Garage", "Other":
		if c.garage {
			hint += "\n  3. Garage: beri izin dengan  garage bucket allow --read --write <bucket> --key <nama-key-panel>"
		}
	}
	return hint
}

// decodeXMLResult reads a 2xx response body into out. CopyObject and a few
// other operations can answer HTTP 200 with an <Error> document on AWS and
// Wasabi (the status line is sent before the operation finishes), so the root
// element is checked before trusting the status code.
func (c *S3) decodeXMLResult(resp *http.Response, op string, out any) error {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: response tidak bisa dibaca: %w", op, err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var root struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("%s: response bukan XML yang valid: %w", op, err)
	}
	if root.XMLName.Local == "Error" {
		return c.errorFromBody(op, resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := xml.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: response tidak bisa dibaca: %w", op, err)
	}
	return nil
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

// ListOptions selects what one ListObjectsV2 call returns.
type ListOptions struct {
	Prefix    string
	Delimiter string // "" for a flat listing, "/" for folder-style
	// ContinuationToken continues a paged listing. It is opaque and may
	// expire, so long-running walks use StartAfter instead.
	ContinuationToken string
	// StartAfter lists keys strictly after this one. It is only sent when
	// ContinuationToken is empty: providers ignore it otherwise, and some
	// object to seeing both.
	StartAfter string
	MaxKeys    int // 0 means 100
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

// ListObjectsV2 lists one page of objects. The returned NextToken is passed
// back verbatim (as ContinuationToken) to get the next page.
func (c *S3) ListObjectsV2(ctx context.Context, bucket string, o ListOptions) (*ListResult, error) {
	if o.MaxKeys <= 0 {
		o.MaxKeys = 100
	}
	params := [][2]string{
		{"list-type", "2"},
		{"max-keys", strconv.Itoa(o.MaxKeys)},
		// Ask for URL-encoded keys so names with newlines or other awkward
		// bytes survive the XML round-trip.
		{"encoding-type", "url"},
	}
	if o.Prefix != "" {
		params = append(params, [2]string{"prefix", o.Prefix})
	}
	if o.Delimiter != "" {
		params = append(params, [2]string{"delimiter", o.Delimiter})
	}
	switch {
	case o.ContinuationToken != "":
		params = append(params, [2]string{"continuation-token", o.ContinuationToken})
	case o.StartAfter != "":
		params = append(params, [2]string{"start-after", o.StartAfter})
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

// urlDecodeKey reverses encoding-type=url.
//
// Providers disagree on the dialect: AWS, Wasabi and MinIO use form encoding
// (a space becomes "+", a literal plus "%2B"), while Garage uses RFC 3986 (a
// space becomes "%20" and a plus "%2B", so a raw "+" never appears). Query
// unescaping is right for both — that is why QueryUnescape, not PathUnescape.
func urlDecodeKey(s string) string {
	if decoded, err := url.QueryUnescape(s); err == nil {
		return decoded
	}
	return s
}

func parseS3Time(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", http.TimeFormat} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// --- object operations ----------------------------------------------------

// PutObject stores body under key. A non-nil empty body creates an empty
// object, which is how a folder marker ("prefix/") is made.
func (c *S3) PutObject(ctx context.Context, bucket, key string, body []byte, contentType string) error {
	if err := ValidateObjectKey(key); err != nil {
		return err
	}
	if body == nil {
		body = []byte{}
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

// ObjectInfo is what HeadObject reports.
type ObjectInfo struct {
	Size         int64
	ETag         string
	ContentType  string
	LastModified time.Time
}

// HeadObject reports whether an object exists and, if so, its metadata.
func (c *S3) HeadObject(ctx context.Context, bucket, key string) (*ObjectInfo, bool, error) {
	if err := ValidateObjectKey(key); err != nil {
		return nil, false, err
	}
	req, err := c.newRequest(ctx, http.MethodHead, bucket, key, nil, nil, "")
	if err != nil {
		return nil, false, err
	}
	resp, err := c.do(req, "HeadObject")
	if err != nil {
		var s3err *S3Error
		if errors.As(err, &s3err) && s3err.NotFound() {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer resp.Body.Close()
	info := &ObjectInfo{
		Size:        resp.ContentLength,
		ETag:        strings.Trim(resp.Header.Get("ETag"), `"`),
		ContentType: resp.Header.Get("Content-Type"),
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		info.LastModified = parseS3Time(lm)
	}
	return info, true, nil
}

// StatObject issues a HEAD and reports whether the object already exists. It is
// what makes content-addressed uploads skip work for a file that is already
// stored.
func (c *S3) StatObject(ctx context.Context, bucket, key string) (exists bool, size int64, err error) {
	info, exists, err := c.HeadObject(ctx, bucket, key)
	if err != nil || !exists {
		return false, 0, err
	}
	return true, info.Size, nil
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

// CopyObject copies one object server-side, metadata included. S3 has no
// rename: a rename is this followed by DeleteObject on the source.
func (c *S3) CopyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) error {
	if err := ValidateObjectKey(srcKey); err != nil {
		return fmt.Errorf("sumber: %w", err)
	}
	if err := ValidateObjectKey(dstKey); err != nil {
		return fmt.Errorf("tujuan: %w", err)
	}
	extra := map[string]string{
		// The header is URL-encoded per the S3 API; every provider (Garage
		// included) percent-decodes it.
		"x-amz-copy-source":        "/" + srcBucket + "/" + awsURIEncode(srcKey, false),
		"x-amz-metadata-directive": "COPY",
	}
	req, err := c.newRequestWith(ctx, http.MethodPut, dstBucket, dstKey, nil, nil, "", extra)
	if err != nil {
		return err
	}
	resp, err := c.do(req, "CopyObject")
	if err != nil {
		return err
	}
	return c.decodeXMLResult(resp, "CopyObject", nil)
}

// DeleteError is one key that a DeleteObjects call could not remove.
type DeleteError struct {
	Key     string
	Code    string
	Message string
}

func (e DeleteError) String() string {
	if e.Message != "" {
		return e.Key + ": " + e.Code + ": " + e.Message
	}
	return e.Key + ": " + e.Code
}

type deleteRequest struct {
	XMLName xml.Name `xml:"Delete"`
	Xmlns   string   `xml:"xmlns,attr"`
	Quiet   bool     `xml:"Quiet"`
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
}

type deleteResult struct {
	XMLName xml.Name `xml:"DeleteResult"`
	Errors  []struct {
		Key     string `xml:"Key"`
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Error"`
}

// DeleteObjects removes up to 1000 keys in one request. Keys the server
// refused come back as DeleteError values; the error return is for the request
// as a whole.
func (c *S3) DeleteObjects(ctx context.Context, bucket string, keys []string) ([]DeleteError, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > maxDeleteObjects {
		return nil, fmt.Errorf("DeleteObjects: maksimal %d key per request, dapat %d", maxDeleteObjects, len(keys))
	}
	doc := deleteRequest{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Quiet: true}
	for _, key := range keys {
		if err := ValidateObjectKey(key); err != nil {
			return nil, err
		}
		doc.Objects = append(doc.Objects, struct {
			Key string `xml:"Key"`
		}{Key: key})
	}
	body, err := xml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("DeleteObjects: %w", err)
	}
	body = append([]byte(xml.Header), body...)

	// AWS and Wasabi refuse the request without Content-MD5; Garage ignores
	// it. The header is signed because sign() covers content-md5.
	sum := md5.Sum(body)
	extra := map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(sum[:])}
	req, err := c.newRequestWith(ctx, http.MethodPost, bucket, "", [][2]string{{"delete", ""}}, body, "application/xml", extra)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req, "DeleteObjects")
	if err != nil {
		return nil, err
	}
	var result deleteResult
	if err := c.decodeXMLResult(resp, "DeleteObjects", &result); err != nil {
		return nil, err
	}
	var failed []DeleteError
	for _, e := range result.Errors {
		failed = append(failed, DeleteError{Key: urlDecodeKey(e.Key), Code: e.Code, Message: e.Message})
	}
	return failed, nil
}
