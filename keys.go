package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Access keys per bucket: a page listing every key that can reach a bucket,
// and key reset.
//
// Garage cannot give an existing key a new secret, so a reset creates a new key
// with the same name, the same key-level settings and the same permissions on
// every bucket the old key could reach, and then deletes the old key.
// Everything that can be checked is checked before the first change, and a
// failure half-way is undone, so the old key keeps working unless its
// replacement is complete.

// keyResetOps are the Admin API operations a reset calls on top of
// GetBucketInfo, which every bucket page needs anyway.
var keyResetOps = []string{"GetKeyInfo", "CreateKey", "AllowBucketKey", "DeleteKey"}

// keyResetTimeout bounds one reset. The reset runs detached from the request,
// so a closed tab cannot stop it between creating the new key and deleting the
// old one.
const keyResetTimeout = 2 * time.Minute

// keyRow is one key on a bucket's key page.
type keyRow struct {
	Name        string
	AccessKeyID string
	Perm        string   // on this bucket: "read + write", "write", ...
	Others      []string // the other buckets the key reaches, with its permissions there
	Panel       bool     // GARAGE_S3_ACCESS_KEY, the panel's own key
	Blocker     string   // why the panel will not reset this key
}

// keyGrant is one bucket permission of a new key, for the result page.
type keyGrant struct {
	Bucket string
	Perm   string
}

// keyReset is what one reset did.
type keyReset struct {
	Steps  []step
	Grants []keyGrant
	// New is the new key when it may be handed out: it has every permission
	// the old key had. Done tells whether the old key is gone; when it is not,
	// Err says why.
	New  *KeyInfo
	Done bool
	Err  error
}

// permLabel spells a bucket permission for people.
func permLabel(p BucketKeyPerm) string {
	var parts []string
	if p.Read {
		parts = append(parts, "read")
	}
	if p.Write {
		parts = append(parts, "write")
	}
	if p.Owner {
		parts = append(parts, "owner")
	}
	if len(parts) == 0 {
		return "tanpa izin"
	}
	return strings.Join(parts, " + ")
}

func (p BucketKeyPerm) granted() bool { return p.Read || p.Write || p.Owner }

func isNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.NotFound()
}

// keyAPIError adds the likely cause to a refused key operation: on a token with
// a scope, a 403 means that scope is missing, whatever Garage's message says
// ("Invalid bearer token").
func keyAPIError(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w. Kalau token admin panel ber-scope, tambahkan scope %s (README, Langkah 0c)", err, apiErr.Op)
	}
	return err
}

// keyScopeHint names the scopes the admin token lacks for a reset. It returns
// "" when the token has them all, or when Garage does not say.
func (a *App) keyScopeHint(ctx context.Context) string {
	tok, err := a.garage.CurrentAdminToken(ctx)
	if err != nil {
		return ""
	}
	missing := tok.Missing(keyResetOps...)
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("Token admin panel tidak punya scope %s, jadi key tidak bisa di-reset dari panel. "+
		"Buat ulang token dengan scope itu (README, Langkah 0c), atau pakai admin_token dari garage.toml.",
		strings.Join(missing, ", "))
}

// resetBlocker says why the panel will not reset a key, or returns "".
func (a *App) resetBlocker(k *KeyInfo) string {
	if k.AccessKeyID == a.cfg.S3AccessKey {
		return "Key ini dipakai panel sendiri (GARAGE_S3_ACCESS_KEY). Reset lewat CLI Garage, lalu perbarui file environment panel."
	}
	if k.Expired {
		return "Key ini sudah kedaluwarsa, dan key penggantinya akan mewarisi tanggal itu. Perpanjang dulu lewat CLI Garage, atau buat key baru."
	}
	for _, b := range k.Buckets {
		if len(b.LocalAliases) > 0 {
			return fmt.Sprintf("Key ini punya alias lokal %s untuk bucket %s. Alias itu melekat pada key lama dan tidak ikut dipindahkan, jadi reset key ini lewat CLI Garage.",
				strings.Join(b.LocalAliases, ", "), b.Label())
		}
	}
	return ""
}

// keyReaches reports whether the key has some permission on the bucket.
func keyReaches(k *KeyInfo, bucketID string) bool {
	for _, b := range k.Buckets {
		if b.ID == bucketID && b.Permissions.granted() {
			return true
		}
	}
	return false
}

func bucketHasKey(b *BucketInfo, accessKeyID string) bool {
	for _, k := range b.Keys {
		if k.AccessKeyID == accessKeyID {
			return true
		}
	}
	return false
}

func (a *App) handleBucketKeys(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.URL.Query().Get("bucket"))
	if err := ValidateBucketName(name); err != nil {
		a.redirectErr(w, r, "/buckets", err)
		return
	}
	bucket, err := a.garage.GetBucketByName(ctx, name)
	if err != nil {
		a.redirectErr(w, r, "/buckets", err)
		return
	}
	hint := a.keyScopeHint(ctx)

	rows := make([]keyRow, 0, len(bucket.Keys))
	for _, k := range bucket.Keys {
		row := keyRow{
			Name:        k.Name,
			AccessKeyID: k.AccessKeyID,
			Perm:        permLabel(k.Permissions),
			Panel:       k.AccessKeyID == a.cfg.S3AccessKey,
		}
		// The panel's own key is never reset here, and without the scopes
		// there is no reset button to explain: both skip the lookup.
		if !row.Panel && hint == "" {
			info, err := a.garage.GetKeyInfo(ctx, k.AccessKeyID, false)
			if err != nil {
				row.Blocker = "Key ini tidak bisa dibaca: " + keyAPIError(err).Error()
			} else {
				for _, b := range info.Buckets {
					if b.ID != bucket.ID && b.Permissions.granted() {
						row.Others = append(row.Others, b.Label()+" ("+permLabel(b.Permissions)+")")
					}
				}
				row.Blocker = a.resetBlocker(info)
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].AccessKeyID < rows[j].AccessKeyID
	})

	a.render(w, r, "bucket_keys.html", map[string]any{
		"Title":     "Key bucket " + name,
		"Page":      "buckets",
		"Bucket":    name,
		"Keys":      rows,
		"ScopeHint": hint,
	})
}

func (a *App) handleBucketKeyReset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("bucket"))
	keyID := strings.TrimSpace(r.PostFormValue("key"))
	if err := ValidateBucketName(name); err != nil {
		a.redirectErr(w, r, "/buckets", err)
		return
	}
	back := "/buckets/keys?bucket=" + url.QueryEscape(name)
	if err := ValidateAccessKeyID(keyID); err != nil {
		a.redirectErr(w, r, back, err)
		return
	}
	if keyID == a.cfg.S3AccessKey {
		a.redirectErr(w, r, back, errors.New("key ini dipakai panel sendiri (GARAGE_S3_ACCESS_KEY) dan tidak di-reset dari panel"))
		return
	}

	a.keyResetMu.Lock()
	defer a.keyResetMu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), keyResetTimeout)
	defer cancel()

	if hint := a.keyScopeHint(ctx); hint != "" {
		a.redirectFlash(w, r, back, "error", hint)
		return
	}
	bucket, err := a.garage.GetBucketByName(ctx, name)
	if err != nil {
		a.redirectErr(w, r, "/buckets", err)
		return
	}
	// Only a key that reaches this bucket can be reset from its page. The same
	// form sent again after a reset (a refresh) names the old key, which is
	// gone by then.
	if !bucketHasKey(bucket, keyID) {
		a.redirectErr(w, r, back, fmt.Errorf("key %s tidak punya akses ke bucket %q, mungkin karena sudah di-reset. Tidak ada yang diubah.", keyID, name))
		return
	}
	old, err := a.garage.GetKeyInfo(ctx, keyID, false)
	if err != nil {
		a.redirectErr(w, r, back, fmt.Errorf("key %s tidak bisa dibaca: %w", keyID, keyAPIError(err)))
		return
	}
	if why := a.resetBlocker(old); why != "" {
		a.redirectErr(w, r, back, errors.New(why))
		return
	}
	if !keyReaches(old, bucket.ID) {
		a.redirectErr(w, r, back, fmt.Errorf("izin key %s di bucket %q berubah saat diperiksa. Muat ulang daftar key lalu coba lagi.", keyID, name))
		return
	}

	res := a.resetKey(ctx, old)
	if res.New == nil {
		log.Printf("reset key %q (%s) gagal: %v", old.Name, old.AccessKeyID, res.Err)
		a.redirectErr(w, r, back, res.Err)
		return
	}
	errText := ""
	if res.Done {
		log.Printf("key %q di-reset: %s diganti %s (%d bucket)", old.Name, old.AccessKeyID, res.New.AccessKeyID, len(res.Grants))
	} else {
		errText = res.Err.Error()
		log.Printf("reset key %q belum tuntas, %s dan %s sama-sama aktif: %v", old.Name, old.AccessKeyID, res.New.AccessKeyID, res.Err)
	}
	expiration := ""
	if old.Expiration != nil {
		expiration = *old.Expiration
	}

	publicURL, exposed := a.s3PublicEndpoint()
	a.render(w, r, "key_reset.html", map[string]any{
		"Title":         "Reset key " + old.Name,
		"Page":          "buckets",
		"Bucket":        name,
		"KeyName":       old.Name,
		"OldID":         old.AccessKeyID,
		"Done":          res.Done,
		"Err":           errText,
		"Steps":         res.Steps,
		"Grants":        res.Grants,
		"CreateBucket":  old.Permissions.CreateBucket,
		"Expiration":    expiration,
		"AccessKey":     res.New.AccessKeyID,
		"SecretKey":     res.New.Secret(),
		"S3PublicURL":   publicURL,
		"S3PublicReady": exposed,
		"S3InternalURL": a.cfg.S3URL,
		"Region":        a.cfg.S3Region,
	})
}

// resetKey replaces old with a new key of the same name, settings and
// permissions, then deletes old. A failure before old is gone deletes the new
// key again, so either the old key or a complete new key works afterwards.
func (a *App) resetKey(ctx context.Context, old *KeyInfo) keyReset {
	var res keyReset
	ok := func(label string) { res.Steps = append(res.Steps, step{Name: label, OK: true}) }
	fail := func(label string, err error) { res.Steps = append(res.Steps, step{Name: label, Err: err.Error()}) }

	created, err := a.garage.CreateKeyLike(ctx, old.Name, old.Permissions, old.Expiration)
	if err != nil {
		fail(fmt.Sprintf("CreateKey %q", old.Name), err)
		res.Err = fmt.Errorf("key baru gagal dibuat, key lama tetap aktif dan tidak ada yang berubah: %w", keyAPIError(err))
		return res
	}
	ok(fmt.Sprintf("CreateKey %q: %s", old.Name, created.AccessKeyID))

	for _, b := range old.Buckets {
		if !b.Permissions.granted() {
			continue
		}
		label := fmt.Sprintf("AllowBucketKey %s (%s)", b.Label(), permLabel(b.Permissions))
		if err := a.garage.AllowBucketKey(ctx, b.ID, created.AccessKeyID, b.Permissions); err != nil {
			fail(label, err)
			res.Err = a.undoReset(ctx, &res, created, false,
				fmt.Errorf("izin bucket %s gagal diberikan ke key baru: %w", b.Label(), keyAPIError(err)))
			return res
		}
		ok(label)
		res.Grants = append(res.Grants, keyGrant{Bucket: b.Label(), Perm: permLabel(b.Permissions)})
	}

	label := fmt.Sprintf("DeleteKey %s (key lama)", old.AccessKeyID)
	if err := a.garage.DeleteKey(ctx, old.AccessKeyID); err != nil {
		// The answer can get lost after Garage did delete the key: only a key
		// that is still there makes this a failure.
		if _, gerr := a.garage.GetKeyInfo(ctx, old.AccessKeyID, false); !isNotFound(gerr) {
			fail(label, err)
			res.Err = a.undoReset(ctx, &res, created, true,
				fmt.Errorf("key lama gagal dihapus: %w", keyAPIError(err)))
			return res
		}
	}
	ok(label)
	res.New = created
	res.Done = true
	return res
}

// undoReset deletes the new key after a failed step and returns the error to
// report. When that fails too, a complete new key is handed out anyway (both
// keys then work, and the page says to delete the old one), while an
// incomplete one is named for manual removal.
func (a *App) undoReset(ctx context.Context, res *keyReset, created *KeyInfo, complete bool, cause error) error {
	label := fmt.Sprintf("DeleteKey %s (membatalkan key baru)", created.AccessKeyID)
	err := a.garage.DeleteKey(ctx, created.AccessKeyID)
	if err == nil || isNotFound(err) {
		res.Steps = append(res.Steps, step{Name: label, OK: true})
		return fmt.Errorf("reset dibatalkan, key lama tetap aktif dan tidak ada yang berubah: %w", cause)
	}
	res.Steps = append(res.Steps, step{Name: label, Err: err.Error()})
	if complete {
		res.New = created
		return fmt.Errorf("%w. Membatalkan key baru juga gagal (%v), jadi key lama dan key baru sama-sama aktif", cause, err)
	}
	return fmt.Errorf("%w. Membatalkan juga gagal (%v): key baru %s tertinggal dengan izin yang belum lengkap. Hapus manual dengan: garage key delete --yes %s",
		cause, err, created.AccessKeyID, created.AccessKeyID)
}
