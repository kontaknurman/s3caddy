package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestManualServe is not a test: it keeps the fake Garage, the two fake S3
// servers and a panel alive so a person (or a browser automation) can click
// through the real UI, and so the real binary can be pointed at the fakes.
//
//	GARAGEPANEL_MANUAL_URLFILE=/tmp/urls GARAGEPANEL_MANUAL_STOPFILE=/tmp/stop \
//	  GARAGEPANEL_TEST_RCLONE=/usr/bin/rclone go test -run TestManualServe -timeout 60m ./...
func TestManualServe(t *testing.T) {
	urlFile := os.Getenv("GARAGEPANEL_MANUAL_URLFILE")
	stopFile := os.Getenv("GARAGEPANEL_MANUAL_STOPFILE")
	if urlFile == "" || stopFile == "" {
		t.Skip("manual harness; set GARAGEPANEL_MANUAL_URLFILE and GARAGEPANEL_MANUAL_STOPFILE")
	}
	rclone := realRcloneBin()
	p := newTestPanel(t, func(c *Config) {
		if rclone != "" {
			c.RcloneBin = rclone
		}
	})
	p.garage.addBucket("media", 0, 0, true)
	p.garage.addBucket("arsip", 0, 0, false)
	p.s3.put("media", "foto/", []byte{}, "application/x-directory")
	for i := 0; i < 12; i++ {
		p.s3.put("media", fmt.Sprintf("foto/img-%02d.png", i), pngPixel(), "image/png")
	}
	p.s3.put("media", "foto/catatan.txt", []byte("catatan"), "text/plain")
	p.s3.put("media", "laporan.pdf", []byte("%PDF-1.4 fake"), "application/pdf")
	p.s3.put("media", "arsip lama/data.csv", []byte("a,b\n1,2\n"), "text/csv")
	for i := 0; i < 130; i++ {
		p.s3.put("media", fmt.Sprintf("banyak/item-%03d.txt", i), []byte(fmt.Sprintf("%0*d", i%50+1, 0)), "text/plain")
	}
	// A key rclone cannot be handed (newline) and one only at the remote.
	p.remote.put("backup", "in/nama\nsalah.txt", []byte("x"), "text/plain")
	for i := 0; i < 2000; i++ {
		p.remote.put("backup", fmt.Sprintf("in/file-%05d.txt", i), []byte(fmt.Sprintf("remote %d", i)), "text/plain")
	}
	big := make([]byte, 20<<20)
	for i := range big {
		big[i] = byte(i)
	}
	p.remote.put("backup", "in/besar.bin", big, "application/octet-stream")

	info := fmt.Sprintf("PANEL=%s\nGARAGE_ADMIN_URL=%s\nGARAGE_S3_URL=%s\nGARAGE_WEB_URL=%s\nREMOTE_URL=%s\nSITES_DIR=%s\nSTATE_DIR=%s\nRCLONE=%s\n",
		p.srv.URL, p.app.cfg.AdminURL, p.app.cfg.S3URL, p.app.cfg.WebURL, p.remote.URL(), p.sitesDir, p.app.cfg.StateDir, rclone)
	if err := os.WriteFile(urlFile, []byte(info), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(55 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stopFile); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// pngPixel is a valid 1×1 PNG so thumbnails really render in a browser.
func pngPixel() []byte {
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, 0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x03, 0x01, 0x01, 0x00, 0x18, 0xdd, 0x8d, 0xb0, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
		0x44, 0xae, 0x42, 0x60, 0x82,
	}
}
