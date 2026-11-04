package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"testing"
)

// realGarage returns an Admin API client and the S3 endpoint and region of a
// real Garage to test against, or skips. Set GARAGEPANEL_TEST_GARAGE_ADMIN_URL,
// GARAGEPANEL_TEST_GARAGE_TOKEN and GARAGEPANEL_TEST_GARAGE_S3_URL (region:
// GARAGEPANEL_TEST_GARAGE_REGION, default "garage"). The test makes its own
// buckets and keys and removes them again.
func realGarage(t *testing.T) (g *Garage, s3URL, region string) {
	t.Helper()
	admin, token := os.Getenv("GARAGEPANEL_TEST_GARAGE_ADMIN_URL"), os.Getenv("GARAGEPANEL_TEST_GARAGE_TOKEN")
	s3URL = os.Getenv("GARAGEPANEL_TEST_GARAGE_S3_URL")
	if admin == "" || token == "" || s3URL == "" {
		t.Skip("no real Garage (set GARAGEPANEL_TEST_GARAGE_ADMIN_URL, _TOKEN and _S3_URL)")
	}
	region = os.Getenv("GARAGEPANEL_TEST_GARAGE_REGION")
	if region == "" {
		region = "garage"
	}
	return NewGarage(admin, token), s3URL, region
}

func randomSuffix(t *testing.T) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// A reset against the real thing: the old credentials stop working, the new
// ones can do exactly what the old ones could, on every bucket.
func TestRealGarageKeyReset(t *testing.T) {
	g, s3URL, region := realGarage(t)
	ctx := t.Context()
	suffix := randomSuffix(t)

	media, err := g.CreateBucket(ctx, "rt-media-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := g.CreateBucket(ctx, "rt-backup-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, id := range []string{media.ID, backup.ID} {
			if err := g.DeleteBucket(context.Background(), id); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})

	old, err := g.CreateKeyLike(ctx, "rt-key-"+suffix, KeyPerm{CreateBucket: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := g.DeleteKey(context.Background(), old.AccessKeyID); err != nil && !isNotFound(err) {
			t.Errorf("cleanup: %v", err)
		}
	})
	if err := g.AllowBucketKey(ctx, media.ID, old.AccessKeyID, BucketKeyPerm{Read: true, Write: true}); err != nil {
		t.Fatal(err)
	}
	if err := g.AllowBucketKey(ctx, backup.ID, old.AccessKeyID, BucketKeyPerm{Write: true}); err != nil {
		t.Fatal(err)
	}
	oldS3, err := NewS3(s3URL, old.AccessKeyID, old.Secret(), region)
	if err != nil {
		t.Fatal(err)
	}
	if err := oldS3.PutObject(ctx, media.Name(), "a.txt", []byte("lama"), "text/plain"); err != nil {
		t.Fatalf("old key before the reset: %v", err)
	}

	a := &App{cfg: &Config{S3AccessKey: "GK-not-this-one"}, garage: g}
	info, err := g.GetKeyInfo(ctx, old.AccessKeyID, false)
	if err != nil {
		t.Fatal(err)
	}
	if why := a.resetBlocker(info); why != "" {
		t.Fatal(why)
	}
	res := a.resetKey(ctx, info)
	if !res.Done || res.New == nil || res.Err != nil {
		t.Fatalf("reset: done=%v err=%v steps=%+v", res.Done, res.Err, res.Steps)
	}
	nk := res.New
	newS3, err := NewS3(s3URL, nk.AccessKeyID, nk.Secret(), region)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, o := range []struct{ bucket, key string }{{media.Name(), "a.txt"}, {media.Name(), "b.txt"}, {backup.Name(), "c.txt"}} {
			_ = newS3.DeleteObject(context.Background(), o.bucket, o.key)
		}
		if err := g.DeleteKey(context.Background(), nk.AccessKeyID); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	if _, err := g.GetKeyInfo(ctx, old.AccessKeyID, false); !isNotFound(err) {
		t.Errorf("old key after the reset: %v, want not found", err)
	}
	if err := oldS3.PutObject(ctx, media.Name(), "b.txt", []byte("x"), "text/plain"); err == nil {
		t.Error("the old credentials still write")
	}

	got, err := g.GetKeyInfo(ctx, nk.AccessKeyID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != old.Name || !got.Permissions.CreateBucket || got.Expiration != nil {
		t.Errorf("new key = %+v", got)
	}
	perms := map[string]BucketKeyPerm{}
	for _, b := range got.Buckets {
		perms[b.ID] = b.Permissions
	}
	if perms[media.ID] != (BucketKeyPerm{Read: true, Write: true}) || perms[backup.ID] != (BucketKeyPerm{Write: true}) || len(perms) != 2 {
		t.Errorf("new key permissions = %+v", perms)
	}

	if err := newS3.PutObject(ctx, media.Name(), "b.txt", []byte("baru"), "text/plain"); err != nil {
		t.Errorf("new key writes media: %v", err)
	}
	body, _, err := newS3.GetObject(ctx, media.Name(), "a.txt")
	if err != nil {
		t.Errorf("new key reads media: %v", err)
	} else {
		data, _ := io.ReadAll(body)
		body.Close()
		if string(data) != "lama" {
			t.Errorf("read %q", data)
		}
	}
	if err := newS3.PutObject(ctx, backup.Name(), "c.txt", []byte("x"), "text/plain"); err != nil {
		t.Errorf("new key writes backup: %v", err)
	}
	if body, _, err := newS3.GetObject(ctx, backup.Name(), "c.txt"); err == nil {
		body.Close()
		t.Error("the new key reads backup, where the old one could only write")
	}
}
