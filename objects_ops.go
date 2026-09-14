package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// Rename, move and copy of single objects. S3 has no rename, so each is a
// server-side CopyObject followed (for rename and move) by a DeleteObject.
// Whole folders go through the job runner instead (handleFolderJob), because
// a folder can hold millions of objects.

// keyFolder returns the folder part of a key, "" for the root.
func keyFolder(key string) string {
	i := strings.LastIndex(key, "/")
	if i < 0 {
		return ""
	}
	return key[:i+1]
}

// renameTarget checks a new base name for an existing key. The extension
// must stay the same or be on the upload whitelist, so a rename cannot turn
// an image into something the web endpoint would serve as a script.
func renameTarget(key, name string) (string, error) {
	if err := ValidateFolderName(name); err != nil {
		return "", fmt.Errorf("nama baru: %w", err)
	}
	if strings.HasPrefix(name, ".") {
		return "", errors.New("nama baru tidak boleh diawali titik")
	}
	oldExt := strings.ToLower(path.Ext(key))
	newExt := strings.ToLower(path.Ext(name))
	if newExt != oldExt {
		if _, ok := allowedExtensions[newExt]; !ok {
			return "", fmt.Errorf("ekstensi %q tidak diizinkan; pertahankan %q atau pakai ekstensi dari daftar upload", newExt, oldExt)
		}
	}
	newKey := keyFolder(key) + name
	if err := ValidateObjectKey(newKey); err != nil {
		return "", err
	}
	if newKey == key {
		return "", errors.New("nama baru sama dengan nama lama")
	}
	return newKey, nil
}

// copyThenMaybeDelete does the copy, and the delete when move is set. The
// error text tells the operator exactly which half failed.
func (a *App) copyThenMaybeDelete(r *http.Request, srcBucket, srcKey, dstBucket, dstKey string, overwrite, move bool) error {
	ctx := r.Context()
	if a.s3 == nil {
		return errors.New("kredensial S3 panel belum diatur")
	}
	if srcBucket == dstBucket && srcKey == dstKey {
		return errors.New("sumber dan tujuan sama")
	}
	if _, exists, err := a.s3.HeadObject(ctx, dstBucket, dstKey); err != nil {
		return err
	} else if exists && !overwrite {
		return fmt.Errorf("%s sudah ada di %s — centang \"timpa\" kalau memang mau menggantinya", dstKey, dstBucket)
	}
	if err := a.s3.CopyObject(ctx, srcBucket, srcKey, dstBucket, dstKey); err != nil {
		return fmt.Errorf("gagal menyalin: %w", err)
	}
	if !move {
		return nil
	}
	if err := a.s3.DeleteObject(ctx, srcBucket, srcKey); err != nil {
		return fmt.Errorf("objek sudah disalin ke %s/%s, tapi yang lama (%s) gagal dihapus: %w", dstBucket, dstKey, srcKey, err)
	}
	return nil
}

func (a *App) handleObjectRename(w http.ResponseWriter, r *http.Request) {
	bucket := strings.TrimSpace(r.PostFormValue("bucket"))
	prefix := r.PostFormValue("prefix")
	key := r.PostFormValue("key")
	name := strings.TrimSpace(r.PostFormValue("name"))
	overwrite := r.PostFormValue("overwrite") == "1"

	if err := ValidateBucketName(bucket); err != nil {
		a.redirectErr(w, r, "/objects", err)
		return
	}
	back := objectsBack(bucket, prefix)
	if err := ValidatePrefix(prefix); err != nil {
		a.redirectErr(w, r, back, err)
		return
	}
	if err := ValidateObjectKey(key); err != nil {
		a.redirectErr(w, r, back, err)
		return
	}
	newKey, err := renameTarget(key, name)
	if err != nil {
		a.redirectErr(w, r, back, err)
		return
	}
	if err := a.copyThenMaybeDelete(r, bucket, key, bucket, newKey, overwrite, true); err != nil {
		a.redirectErr(w, r, back, err)
		return
	}
	log.Printf("bucket %q: objek diganti nama", bucket)
	a.redirectOK(w, r, back, fmt.Sprintf("%s diganti nama menjadi %s.", path.Base(key), name))
}

// handleObjectTransfer serves /objects/move and /objects/copy: the object
// keeps its base name and lands in dst_bucket/dst_prefix.
func (a *App) handleObjectTransfer(move bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bucket := strings.TrimSpace(r.PostFormValue("bucket"))
		prefix := r.PostFormValue("prefix")
		key := r.PostFormValue("key")
		dstBucket := strings.TrimSpace(r.PostFormValue("dst_bucket"))
		dstPrefix := strings.TrimSpace(r.PostFormValue("dst_prefix"))
		overwrite := r.PostFormValue("overwrite") == "1"

		if err := ValidateBucketName(bucket); err != nil {
			a.redirectErr(w, r, "/objects", err)
			return
		}
		back := objectsBack(bucket, prefix)
		if err := ValidatePrefix(prefix); err != nil {
			a.redirectErr(w, r, back, err)
			return
		}
		if err := ValidateObjectKey(key); err != nil {
			a.redirectErr(w, r, back, err)
			return
		}
		if dstBucket == "" {
			dstBucket = bucket
		}
		if err := ValidateBucketName(dstBucket); err != nil {
			a.redirectErr(w, r, back, fmt.Errorf("bucket tujuan: %w", err))
			return
		}
		if dstPrefix != "" && !strings.HasSuffix(dstPrefix, "/") {
			dstPrefix += "/"
		}
		if err := ValidateJobPrefix(dstPrefix); err != nil {
			a.redirectErr(w, r, back, fmt.Errorf("folder tujuan: %w", err))
			return
		}
		dstKey := dstPrefix + path.Base(key)
		if err := ValidateObjectKey(dstKey); err != nil {
			a.redirectErr(w, r, back, err)
			return
		}
		if err := a.copyThenMaybeDelete(r, bucket, key, dstBucket, dstKey, overwrite, move); err != nil {
			a.redirectErr(w, r, back, err)
			return
		}
		verb := "disalin"
		if move {
			verb = "dipindah"
			log.Printf("bucket %q: objek dipindah ke bucket %q", bucket, dstBucket)
		} else {
			log.Printf("bucket %q: objek disalin ke bucket %q", bucket, dstBucket)
		}
		a.redirectOK(w, r, back, fmt.Sprintf("%s %s ke %s/%s.", path.Base(key), verb, dstBucket, dstKey))
	}
}

// handleFolderJob turns a folder operation into a background job: deleting,
// copying or moving a prefix can mean millions of objects, which no request
// should wait for.
func (a *App) handleFolderJob(w http.ResponseWriter, r *http.Request) {
	bucket := strings.TrimSpace(r.PostFormValue("bucket"))
	prefix := r.PostFormValue("prefix")
	action := r.PostFormValue("action")

	if err := ValidateBucketName(bucket); err != nil {
		a.redirectErr(w, r, "/objects", err)
		return
	}
	back := objectsBack(bucket, prefix)
	if err := ValidateJobPrefix(prefix); err != nil || prefix == "" {
		a.redirectErr(w, r, back, errors.New("operasi folder hanya untuk folder, bukan akar bucket"))
		return
	}
	if a.syncDisabled != "" {
		a.redirectErr(w, r, back, fmt.Errorf("operasi folder butuh fitur Sync yang sedang nonaktif: %s", a.syncDisabled))
		return
	}
	folderName := path.Base(strings.TrimSuffix(prefix, "/"))
	transfers := 4
	if v := strings.TrimSpace(r.PostFormValue("transfers")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			a.redirectErr(w, r, back, errors.New("transfer paralel harus angka"))
			return
		}
		transfers = n
	}

	spec := JobSpec{Transfers: transfers, Src: Endpoint{Bucket: bucket, Prefix: prefix}}
	switch action {
	case "delete":
		if strings.TrimSpace(r.PostFormValue("confirm")) != folderName {
			a.redirectErr(w, r, back, fmt.Errorf("konfirmasi tidak cocok: ketik ulang persis %q untuk menghapus folder ini beserta seluruh isinya", folderName))
			return
		}
		spec.Kind = JobDeletePrefix
	case "copy", "move":
		dstBucket := strings.TrimSpace(r.PostFormValue("dst_bucket"))
		if dstBucket == "" {
			dstBucket = bucket
		}
		if err := ValidateBucketName(dstBucket); err != nil {
			a.redirectErr(w, r, back, fmt.Errorf("bucket tujuan: %w", err))
			return
		}
		dstPrefix := strings.TrimSpace(r.PostFormValue("dst_prefix"))
		if dstPrefix != "" && !strings.HasSuffix(dstPrefix, "/") {
			dstPrefix += "/"
		}
		if err := ValidateJobPrefix(dstPrefix); err != nil {
			a.redirectErr(w, r, back, fmt.Errorf("folder tujuan: %w", err))
			return
		}
		// The folder keeps its own name under the destination.
		spec.Dst = Endpoint{Bucket: dstBucket, Prefix: dstPrefix + folderName + "/"}
		spec.Kind = JobCopyPrefix
		if action == "move" {
			spec.Kind = JobMovePrefix
		}
	default:
		a.redirectErr(w, r, back, errors.New("aksi folder tidak dikenal"))
		return
	}
	a.startJob(w, r, spec, back)
}
