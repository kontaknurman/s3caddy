package main

import (
	"strings"
	"testing"
)

func TestValidateBucketName(t *testing.T) {
	good := []string{"media", "abc", "my-bucket-1", strings.Repeat("a", 63)}
	bad := []string{
		"",                      // empty
		"ab",                    // too short
		strings.Repeat("a", 64), // too long
		"-lead",                 // leading dash
		"trail-",                // trailing dash
		"UPPER",                 // uppercase
		"has space",             // space
		"has_underscore",        // underscore
		"a/b",                   // path separator
		"..",                    // dot segment
		"a..b",                  // traversal
		"dots.in.name",          // dots are not allowed by the rule
		"with\nnewline",         // control character
	}
	for _, s := range good {
		if err := ValidateBucketName(s); err != nil {
			t.Errorf("ValidateBucketName(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range bad {
		if err := ValidateBucketName(s); err == nil {
			t.Errorf("ValidateBucketName(%q) = nil, want an error", s)
		}
	}
}

func TestValidateDomain(t *testing.T) {
	good := []string{"example.com", "cdn.example.com", "a.b.c.d.example.co.id", "x1-y2.example.com"}
	bad := []string{
		"",
		"nodot",
		"UPPER.com",
		"-lead.example.com",
		"trail-.example.com",
		"a..example.com",
		"../../etc/passwd",
		"exa mple.com",
		"example.com/path",
		"exam\nple.com",
		".example.com",
	}
	for _, s := range good {
		if err := ValidateDomain(s); err != nil {
			t.Errorf("ValidateDomain(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range bad {
		if err := ValidateDomain(s); err == nil {
			t.Errorf("ValidateDomain(%q) = nil, want an error", s)
		}
	}
}

func TestValidateIPOrCIDR(t *testing.T) {
	cases := []struct{ in, want string }{
		{"203.0.113.10", "203.0.113.10"},
		{" 203.0.113.10 ", "203.0.113.10"},
		{"198.51.100.0/24", "198.51.100.0/24"},
		{"198.51.100.5/24", "198.51.100.0/24"}, // host bits masked off
		{"2001:db8::1", "2001:db8::1"},
		{"2001:db8::/32", "2001:db8::/32"},
	}
	for _, tc := range cases {
		got, err := ValidateIPOrCIDR(tc.in)
		if err != nil {
			t.Errorf("ValidateIPOrCIDR(%q) = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ValidateIPOrCIDR(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Anything that is not a real address must be refused, including strings
	// crafted to break out of the Caddyfile line.
	bad := []string{
		"", "not-an-ip", "203.0.113.999", "203.0.113.10/33", "10.0.0.1 10.0.0.2",
		"203.0.113.10\n}\nevil.com {", "1.2.3.4;", "*",
	}
	for _, s := range bad {
		if _, err := ValidateIPOrCIDR(s); err == nil {
			t.Errorf("ValidateIPOrCIDR(%q) = nil, want an error", s)
		}
	}
}

func TestValidateLabel(t *testing.T) {
	if err := ValidateLabel("server-app jakarta_1"); err != nil {
		t.Errorf("valid label rejected: %v", err)
	}
	bad := []string{
		"",
		strings.Repeat("a", 41),
		"pipe|inside",
		"newline\nhere",
		"quote\"here",
		"hash#here",
	}
	for _, s := range bad {
		if err := ValidateLabel(s); err == nil {
			t.Errorf("ValidateLabel(%q) = nil, want an error", s)
		}
	}
	if err := ValidateLabel(strings.Repeat("é", 40)); err != nil {
		// 40 runes is fine on length, but é is not in the allowed set.
		if !strings.Contains(err.Error(), "hanya boleh") {
			t.Errorf("unexpected error for non-ASCII label: %v", err)
		}
	}
}

func TestValidateObjectKey(t *testing.T) {
	good := []string{"a.jpg", "folder/a.jpg", "deep/folder/name with space.png", "plus+file.pdf"}
	for _, s := range good {
		if err := ValidateObjectKey(s); err != nil {
			t.Errorf("ValidateObjectKey(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{
		"",
		"/leading-slash.jpg",
		"../escape.jpg",
		"folder/../../escape.jpg",
		"a\x00b.jpg",
		"a\nb.jpg",
		strings.Repeat("a", MaxObjectKeyLen+1),
	}
	for _, s := range bad {
		if err := ValidateObjectKey(s); err == nil {
			t.Errorf("ValidateObjectKey(%q) = nil, want an error", s)
		}
	}
	if err := ValidatePrefix(""); err != nil {
		t.Errorf("an empty prefix means bucket root and must be allowed: %v", err)
	}
}

func TestSafeFilenameComponent(t *testing.T) {
	bad := []string{"", ".", "..", "a/b", `a\b`, "a..b", ".hidden", "a\x00b", strings.Repeat("a", 201)}
	for _, s := range bad {
		if err := SafeFilenameComponent(s, "x"); err == nil {
			t.Errorf("SafeFilenameComponent(%q) = nil, want an error", s)
		}
	}
	if err := SafeFilenameComponent("cdn.example.com", "domain"); err != nil {
		t.Errorf("valid component rejected: %v", err)
	}
}

func TestValidateUploadName(t *testing.T) {
	cases := []struct{ in, wantExt, wantType string }{
		{"photo.JPG", ".jpg", "image/jpeg"},
		{"a.b.png", ".png", "image/png"},
		{`C:\Users\me\doc.pdf`, ".pdf", "application/pdf"},
		{"evil.php.jpg", ".jpg", "image/jpeg"},
	}
	for _, tc := range cases {
		ext, ct, err := ValidateUploadName(tc.in)
		if err != nil {
			t.Errorf("ValidateUploadName(%q) = %v", tc.in, err)
			continue
		}
		if ext != tc.wantExt || ct != tc.wantType {
			t.Errorf("ValidateUploadName(%q) = (%q, %q), want (%q, %q)", tc.in, ext, ct, tc.wantExt, tc.wantType)
		}
	}
	for _, s := range []string{"noext", "script.php", "shell.sh", "binary.exe", ".jpg.", "a.bin"} {
		if _, _, err := ValidateUploadName(s); err == nil {
			t.Errorf("ValidateUploadName(%q) = nil, want an error", s)
		}
	}
}

func TestIsImageKey(t *testing.T) {
	for _, s := range []string{"a.jpg", "a.JPEG", "a.png", "a.webp", "a.gif", "a.avif"} {
		if !IsImageKey(s) {
			t.Errorf("IsImageKey(%q) = false, want true", s)
		}
	}
	// SVG and HTML are uploadable but must never be treated as inline images by
	// the panel, because /preview serves from the panel's own origin.
	for _, s := range []string{"a.svg", "a.html", "a.pdf", "a.txt", "noext"} {
		if IsImageKey(s) {
			t.Errorf("IsImageKey(%q) = true, want false", s)
		}
	}
}

func TestCheckLoopbackListen(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8090", "localhost:8090", "[::1]:8090", "127.0.0.2:9000"} {
		if err := checkLoopbackListen(ok); err != nil {
			t.Errorf("checkLoopbackListen(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:8090", ":8090", "192.168.1.10:8090", "[::]:8090", "8090", "127.0.0.1:abc"} {
		if err := checkLoopbackListen(bad); err == nil {
			t.Errorf("checkLoopbackListen(%q) = nil, want an error", bad)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1 << 20, "1.0 MiB"},
		{5 * (1 << 30), "5.0 GiB"},
		{25 * (1 << 40), "25.0 TiB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateObjectKeyRefusesDotSegmentsOnly(t *testing.T) {
	// Real buckets contain keys with consecutive dots; only actual dot
	// segments can escape anything.
	good := []string{"laporan..final.pdf", "a/b..c/d.png", "..hidden", "x/..."}
	for _, s := range good {
		if err := ValidateObjectKey(s); err != nil {
			t.Errorf("ValidateObjectKey(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{"../x", "a/../b", "a/./b", "./a", "/a", "a/.."}
	for _, s := range bad {
		if err := ValidateObjectKey(s); err == nil {
			t.Errorf("ValidateObjectKey(%q) = nil, want an error", s)
		}
	}
}

func TestValidateRemoteEndpoint(t *testing.T) {
	good := map[string]string{
		"https://s3.wasabisys.com":                 "https://s3.wasabisys.com",
		"https://s3.ap-southeast-1.wasabisys.com/": "https://s3.ap-southeast-1.wasabisys.com",
		"https://S3.Example.COM":                   "https://s3.example.com",
		"http://10.0.0.5:3900":                     "http://10.0.0.5:3900",
		"http://127.0.0.1:3900":                    "http://127.0.0.1:3900",
		"http://minio:9000":                        "http://minio:9000",
		"  https://s3.amazonaws.com  ":             "https://s3.amazonaws.com",
	}
	for in, want := range good {
		got, err := ValidateRemoteEndpoint(in)
		if err != nil {
			t.Errorf("ValidateRemoteEndpoint(%q) = %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ValidateRemoteEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
	bad := []string{
		"", "s3.wasabisys.com", "ftp://x.example.com", "file:///etc/passwd",
		"https://user:pw@s3.example.com", "https://s3.example.com/bucket", "https://s3.example.com/?x=1",
		"https://s3.example.com/#frag", "http://169.254.169.254", "http://[fe80::1]:80", "http://0.0.0.0",
		"http://s3.example.com:70000", "http://-bad.example.com", "https://exa mple.com",
	}
	for _, s := range bad {
		if _, err := ValidateRemoteEndpoint(s); err == nil {
			t.Errorf("ValidateRemoteEndpoint(%q) = nil, want an error", s)
		}
	}
}

func TestValidateRemoteNameRegionProviderBucket(t *testing.T) {
	for _, s := range []string{"wasabi", "wasabi-sg", "b2", "a"} {
		if err := ValidateRemoteName(s); err != nil {
			t.Errorf("ValidateRemoteName(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", "-x", "Wasabi", "has space", "a/b", "..", strings.Repeat("a", 33)} {
		if err := ValidateRemoteName(s); err == nil {
			t.Errorf("ValidateRemoteName(%q) = nil, want an error", s)
		}
	}
	for _, s := range []string{"us-east-1", "garage", "ap-southeast-1"} {
		if err := ValidateRegion(s); err != nil {
			t.Errorf("ValidateRegion(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", "US-EAST-1", "a b", strings.Repeat("a", 33)} {
		if err := ValidateRegion(s); err == nil {
			t.Errorf("ValidateRegion(%q) = nil, want an error", s)
		}
	}
	for _, s := range []string{"Wasabi", "AWS", "Minio", "Other"} {
		if err := ValidateProvider(s); err != nil {
			t.Errorf("ValidateProvider(%q) = %v", s, err)
		}
	}
	if err := ValidateProvider("wasabi"); err == nil {
		t.Error("provider names are case-sensitive rclone values")
	}
	for _, s := range []string{"my.bucket", "abc", "my-bucket-1"} {
		if err := ValidateRemoteBucketName(s); err != nil {
			t.Errorf("ValidateRemoteBucketName(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", "ab", "1.2.3.4", "a..b", "a.-b", "-x", "UP", "a/b", strings.Repeat("a", 64)} {
		if err := ValidateRemoteBucketName(s); err == nil {
			t.Errorf("ValidateRemoteBucketName(%q) = nil, want an error", s)
		}
	}
}

func TestValidateJobPrefixFolderAndBaseNames(t *testing.T) {
	for _, s := range []string{"", "photos/", "a/b/"} {
		if err := ValidateJobPrefix(s); err != nil {
			t.Errorf("ValidateJobPrefix(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"photos", "/photos/", "../x/"} {
		if err := ValidateJobPrefix(s); err == nil {
			t.Errorf("ValidateJobPrefix(%q) = nil, want an error", s)
		}
	}
	for _, s := range []string{"Foto 2026", "laporan..final", "ünïcode"} {
		if err := ValidateFolderName(s); err != nil {
			t.Errorf("ValidateFolderName(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", ".", "..", "a/b", `a\b`, " lead", "trail ", "a\nb", "a\x00b", strings.Repeat("a", 256)} {
		if err := ValidateFolderName(s); err == nil {
			t.Errorf("ValidateFolderName(%q) = nil, want an error", s)
		}
	}
	ext, ct, err := ValidateObjectBaseName("Foto Liburan.JPG")
	if err != nil || ext != ".jpg" || ct != "image/jpeg" {
		t.Errorf("ValidateObjectBaseName = (%q, %q, %v)", ext, ct, err)
	}
	for _, s := range []string{".htaccess", "shell.sh", "dir/a.jpg", "noext", "../a.jpg"} {
		if _, _, err := ValidateObjectBaseName(s); err == nil {
			t.Errorf("ValidateObjectBaseName(%q) = nil, want an error", s)
		}
	}
}

func TestValidateJobIDTransfersModeAndSort(t *testing.T) {
	if err := ValidateJobID("0123456789abcdef"); err != nil {
		t.Error(err)
	}
	for _, s := range []string{"", "../x", "0123456789ABCDEF", "0123456789abcde", "0123456789abcdef0"} {
		if err := ValidateJobID(s); err == nil {
			t.Errorf("ValidateJobID(%q) = nil, want an error", s)
		}
	}
	for _, n := range []int{0, -1, MaxTransfers + 1} {
		if err := ValidateTransfers(n); err == nil {
			t.Errorf("ValidateTransfers(%d) = nil, want an error", n)
		}
	}
	if err := ValidateTransfers(4); err != nil {
		t.Error(err)
	}
	for _, m := range []string{"skip-existing", "update-if-different", "overwrite"} {
		if err := ValidateSyncMode(m); err != nil {
			t.Errorf("ValidateSyncMode(%q) = %v", m, err)
		}
	}
	if err := ValidateSyncMode("mirror"); err == nil {
		t.Error("unknown mode accepted")
	}
	if s, d := SortParams("size", "desc"); s != "size" || d != "desc" {
		t.Errorf("SortParams = %s %s", s, d)
	}
	if s, d := SortParams("<script>", "sideways"); s != "name" || d != "asc" {
		t.Errorf("SortParams fallback = %s %s", s, d)
	}
}
