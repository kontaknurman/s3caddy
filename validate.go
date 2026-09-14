package main

import (
	"errors"
	"fmt"
	"net"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Every string that ends up in a file name or inside a Caddyfile passes through
// this file first. Nothing here trusts the caller.

const (
	// MaxUploadSize is the hard per-file limit for uploads (50 MB).
	MaxUploadSize = 50 << 20
	// MaxLabelLen bounds whitelist labels.
	MaxLabelLen = 40
	// MaxObjectKeyLen is the S3 limit for object keys.
	MaxObjectKeyLen = 1024
)

var (
	// Bucket names: 3..63 chars, lowercase alphanumeric and dashes, no leading
	// or trailing dash.
	bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

	// Domains: lowercase labels separated by dots, at least two labels.
	domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

	// Labels for whitelist entries. The pipe character is deliberately excluded
	// because it separates value from label in the Caddyfile comment.
	labelRe = regexp.MustCompile(`^[a-zA-Z0-9 _-]+$`)
)

// ValidateBucketName checks a bucket name before it is used in an API call, a
// Caddyfile body or a file name.
func ValidateBucketName(s string) error {
	if s == "" {
		return errors.New("nama bucket tidak boleh kosong")
	}
	if len(s) > 63 {
		return errors.New("nama bucket maksimal 63 karakter")
	}
	if !bucketNameRe.MatchString(s) {
		return fmt.Errorf("nama bucket %q tidak valid: gunakan 3-63 karakter huruf kecil, angka, dan tanda hubung; tidak boleh diawali/diakhiri tanda hubung", s)
	}
	// Defence in depth: the regexp already excludes these, but the value is
	// used as a path component so check explicitly.
	return SafeFilenameComponent(s, "nama bucket")
}

// ValidateDomain checks a domain before it is used as "<domain>.caddy" and as a
// site address inside a Caddyfile.
func ValidateDomain(s string) error {
	if s == "" {
		return errors.New("domain tidak boleh kosong")
	}
	if len(s) > 253 {
		return errors.New("domain maksimal 253 karakter")
	}
	if !domainRe.MatchString(s) {
		return fmt.Errorf("domain %q tidak valid: gunakan huruf kecil, angka, tanda hubung, dan minimal satu titik (contoh: cdn.example.com)", s)
	}
	return SafeFilenameComponent(s, "domain")
}

// ValidateIPOrCIDR parses an IP address or CIDR block and returns its canonical
// textual form. Parsing is done with net.ParseIP / net.ParseCIDR, never with a
// regexp, and the canonical form is what gets written to the Caddyfile.
func ValidateIPOrCIDR(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("IP atau CIDR tidak boleh kosong")
	}
	if strings.Contains(s, "/") {
		_, ipnet, err := net.ParseCIDR(s)
		if err != nil {
			return "", fmt.Errorf("CIDR %q tidak valid: %v", s, err)
		}
		// ipnet.String() masks off host bits, so 198.51.100.5/24 becomes
		// 198.51.100.0/24.
		return ipnet.String(), nil
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return "", fmt.Errorf("IP %q tidak valid", s)
	}
	return ip.String(), nil
}

// ValidateLabel checks a human label used in Caddyfile comments.
func ValidateLabel(s string) error {
	if s == "" {
		return errors.New("label tidak boleh kosong")
	}
	if utf8.RuneCountInString(s) > MaxLabelLen {
		return fmt.Errorf("label maksimal %d karakter", MaxLabelLen)
	}
	if !labelRe.MatchString(s) {
		return errors.New("label hanya boleh berisi huruf, angka, spasi, garis bawah, dan tanda hubung")
	}
	return nil
}

// ValidateObjectKey rejects keys that could escape the bucket or break the
// request line.
func ValidateObjectKey(s string) error {
	if s == "" {
		return errors.New("object key tidak boleh kosong")
	}
	if len(s) > MaxObjectKeyLen {
		return fmt.Errorf("object key maksimal %d karakter", MaxObjectKeyLen)
	}
	if strings.HasPrefix(s, "/") {
		return errors.New("object key tidak boleh diawali \"/\"")
	}
	if strings.Contains(s, "..") {
		return errors.New("object key tidak boleh mengandung \"..\"")
	}
	if !utf8.ValidString(s) {
		return errors.New("object key bukan UTF-8 yang valid")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return errors.New("object key mengandung karakter kontrol")
		}
	}
	return nil
}

// ValidatePrefix checks a listing prefix. Same rules as a key, but empty is
// allowed (it means "bucket root").
func ValidatePrefix(s string) error {
	if s == "" {
		return nil
	}
	return ValidateObjectKey(s)
}

// SafeFilenameComponent rejects anything that must never be used as part of a
// file path: separators, dot segments, control characters and NUL.
func SafeFilenameComponent(s, what string) error {
	if s == "" || s == "." || s == ".." {
		return fmt.Errorf("%s tidak valid", what)
	}
	if strings.Contains(s, "..") {
		return fmt.Errorf("%s tidak boleh mengandung \"..\"", what)
	}
	if strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("%s tidak boleh mengandung \"/\" atau \"\\\"", what)
	}
	if strings.HasPrefix(s, ".") {
		return fmt.Errorf("%s tidak boleh diawali titik", what)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s mengandung karakter kontrol", what)
		}
	}
	// The file name is built as "<component>.caddy"; keep it well below any
	// filesystem limit.
	if len(s) > 200 {
		return fmt.Errorf("%s terlalu panjang", what)
	}
	return nil
}

// allowedExtensions is the upload whitelist: extension -> content type.
// Anything not listed here is rejected.
var allowedExtensions = map[string]string{
	// raster images
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".bmp":  "image/bmp",
	".ico":  "image/x-icon",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	// vector / web assets
	".svg":  "image/svg+xml",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".htm":  "text/html; charset=utf-8",
	".map":  "application/json",
	// fonts
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".eot":   "application/vnd.ms-fontobject",
	// documents
	".pdf":  "application/pdf",
	".txt":  "text/plain; charset=utf-8",
	".md":   "text/markdown; charset=utf-8",
	".csv":  "text/csv; charset=utf-8",
	".json": "application/json",
	".xml":  "application/xml",
	".yml":  "text/plain; charset=utf-8",
	".yaml": "text/plain; charset=utf-8",
	// media
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".mp3":  "audio/mpeg",
	".ogg":  "audio/ogg",
	".wav":  "audio/wav",
	".m4a":  "audio/mp4",
	// archives
	".zip": "application/zip",
	".gz":  "application/gzip",
	".tar": "application/x-tar",
	".7z":  "application/x-7z-compressed",
}

// imageExtensions are rendered as thumbnails in the grid and are the only
// content types the /preview endpoint will serve inline.
var imageExtensions = map[string]bool{
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".webp": true,
	".gif":  true,
	".avif": true,
}

// AllowedExtensionList returns the upload whitelist, sorted, for display.
func AllowedExtensionList() []string {
	out := make([]string, 0, len(allowedExtensions))
	for ext := range allowedExtensions {
		out = append(out, strings.TrimPrefix(ext, "."))
	}
	sort.Strings(out)
	return out
}

// ValidateUploadName checks the client-supplied file name and returns the
// lowercase extension (with leading dot) and its content type.
func ValidateUploadName(name string) (ext, contentType string, err error) {
	// Only the extension survives: the stored key is sha256(content)[:16]+ext.
	base := path.Base(strings.ReplaceAll(name, `\`, `/`))
	ext = strings.ToLower(path.Ext(base))
	if ext == "" {
		return "", "", fmt.Errorf("file %q tidak punya ekstensi", name)
	}
	ct, ok := allowedExtensions[ext]
	if !ok {
		return "", "", fmt.Errorf("ekstensi %q tidak diizinkan", ext)
	}
	return ext, ct, nil
}

// IsImageKey reports whether a key should be shown as a thumbnail.
func IsImageKey(key string) bool {
	return imageExtensions[strings.ToLower(path.Ext(key))]
}

// ContentTypeForKey returns the content type for a stored key, or the empty
// string when the extension is not on the whitelist.
func ContentTypeForKey(key string) string {
	return allowedExtensions[strings.ToLower(path.Ext(key))]
}
