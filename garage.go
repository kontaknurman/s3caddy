package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client for the Garage Admin API v2.
//
// Verified against the OpenAPI description published at
// https://garagehq.deuxfleurs.fr/api/garage-admin-v2.json (spec version
// v2.3.0). Every operation is addressed as /v2/<OperationName>; reads use GET
// with query parameters and writes use POST with a JSON body. Authentication is
// a bearer token.
//
// Note that there is no dedicated "enable website" endpoint: website access is
// part of POST /v2/UpdateBucket?id=<id>.

// Garage talks to the Garage Admin API.
type Garage struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewGarage builds a client. baseURL is e.g. http://127.0.0.1:3903.
func NewGarage(baseURL, token string) *Garage {
	return &Garage{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError carries what Garage reported so the panel can show it verbatim
// instead of a generic "internal server error".
type APIError struct {
	Op         string
	StatusCode int
	Code       string
	Message    string
	Body       string
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Garage %s gagal (HTTP %d)", e.Op, e.StatusCode)
	switch {
	case e.Code != "" && e.Message != "":
		fmt.Fprintf(&b, ": %s: %s", e.Code, e.Message)
	case e.Message != "":
		fmt.Fprintf(&b, ": %s", e.Message)
	case e.Body != "":
		fmt.Fprintf(&b, ": %s", e.Body)
	}
	return b.String()
}

// NotFound reports whether the API answered 404.
func (e *APIError) NotFound() bool { return e.StatusCode == http.StatusNotFound }

// do performs one Admin API call. body may be nil for GET requests; out may be
// nil when the response carries no payload.
func (g *Garage) do(ctx context.Context, method, op string, query url.Values, body, out any) error {
	u := g.baseURL + "/v2/" + op
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s: menyusun request: %w", op, err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return fmt.Errorf("%s: menyusun request: %w", op, err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.http.Do(req)
	if err != nil {
		// Never echo the request (it carries the bearer token) into the error.
		return fmt.Errorf("tidak bisa menghubungi Garage Admin API di %s (%s): %v", g.baseURL, op, unwrapURLError(err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: membaca response: %w", op, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Op: op, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
		var parsed struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &parsed) == nil {
			apiErr.Code = parsed.Code
			apiErr.Message = parsed.Message
		}
		if apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden {
			if apiErr.Message == "" {
				apiErr.Message = "token admin ditolak — periksa GARAGE_ADMIN_TOKEN"
			}
		}
		if len(apiErr.Body) > 500 {
			apiErr.Body = apiErr.Body[:500] + "…"
		}
		return apiErr
	}

	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: response Garage tidak bisa dibaca: %w", op, err)
	}
	return nil
}

// unwrapURLError strips the URL from transport errors so neither the bearer
// token nor a signed URL can leak through an error string.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// --- response types -------------------------------------------------------

// BucketListItem is one entry of GET /v2/ListBuckets.
type BucketListItem struct {
	ID            string   `json:"id"`
	Created       string   `json:"created"`
	GlobalAliases []string `json:"globalAliases"`
	LocalAliases  []struct {
		AccessKeyID string `json:"accessKeyId"`
		Alias       string `json:"alias"`
	} `json:"localAliases"`
}

// Name returns the first global alias, or an empty string for an unnamed bucket.
func (b BucketListItem) Name() string {
	if len(b.GlobalAliases) > 0 {
		return b.GlobalAliases[0]
	}
	return ""
}

// BucketKeyPerm mirrors ApiBucketKeyPerm.
type BucketKeyPerm struct {
	Read  bool `json:"read"`
	Write bool `json:"write"`
	Owner bool `json:"owner"`
}

// BucketInfo is GET /v2/GetBucketInfo (also returned by CreateBucket).
type BucketInfo struct {
	ID            string   `json:"id"`
	Created       string   `json:"created"`
	GlobalAliases []string `json:"globalAliases"`
	WebsiteAccess bool     `json:"websiteAccess"`
	WebsiteConfig *struct {
		IndexDocument string  `json:"indexDocument"`
		ErrorDocument *string `json:"errorDocument"`
	} `json:"websiteConfig"`
	Objects int64 `json:"objects"`
	Bytes   int64 `json:"bytes"`
	Keys    []struct {
		AccessKeyID string        `json:"accessKeyId"`
		Name        string        `json:"name"`
		Permissions BucketKeyPerm `json:"permissions"`
	} `json:"keys"`
	UnfinishedUploads int64 `json:"unfinishedUploads"`
}

// Name returns the first global alias, or an empty string.
func (b *BucketInfo) Name() string {
	if b != nil && len(b.GlobalAliases) > 0 {
		return b.GlobalAliases[0]
	}
	return ""
}

// KeyInfo is GET /v2/GetKeyInfo and the response of POST /v2/CreateKey.
// SecretAccessKey is only populated when the key is created (or when
// showSecretKey=true is requested).
type KeyInfo struct {
	AccessKeyID     string  `json:"accessKeyId"`
	Name            string  `json:"name"`
	SecretAccessKey *string `json:"secretAccessKey"`
	Expired         bool    `json:"expired"`
}

// Secret returns the secret access key, or "" when Garage did not send one.
func (k *KeyInfo) Secret() string {
	if k == nil || k.SecretAccessKey == nil {
		return ""
	}
	return *k.SecretAccessKey
}

// --- operations -----------------------------------------------------------

// ListBuckets returns every bucket known to the cluster.
func (g *Garage) ListBuckets(ctx context.Context) ([]BucketListItem, error) {
	var out []BucketListItem
	if err := g.do(ctx, http.MethodGet, "ListBuckets", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetBucketInfo looks a bucket up by its id.
func (g *Garage) GetBucketInfo(ctx context.Context, id string) (*BucketInfo, error) {
	var out BucketInfo
	q := url.Values{"id": {id}}
	if err := g.do(ctx, http.MethodGet, "GetBucketInfo", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetBucketByName looks a bucket up by its global alias.
func (g *Garage) GetBucketByName(ctx context.Context, name string) (*BucketInfo, error) {
	if err := ValidateBucketName(name); err != nil {
		return nil, err
	}
	var out BucketInfo
	q := url.Values{"globalAlias": {name}}
	if err := g.do(ctx, http.MethodGet, "GetBucketInfo", q, nil, &out); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			return nil, fmt.Errorf("bucket %q tidak ditemukan di Garage", name)
		}
		return nil, err
	}
	return &out, nil
}

// CreateBucket creates a bucket with the given global alias.
func (g *Garage) CreateBucket(ctx context.Context, name string) (*BucketInfo, error) {
	body := map[string]any{"globalAlias": name}
	var out BucketInfo
	if err := g.do(ctx, http.MethodPost, "CreateBucket", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteBucket removes a bucket. Garage answers 400 when the bucket still holds
// objects.
func (g *Garage) DeleteBucket(ctx context.Context, id string) error {
	q := url.Values{"id": {id}}
	err := g.do(ctx, http.MethodPost, "DeleteBucket", q, nil, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest {
		return fmt.Errorf("Garage menolak menghapus bucket: bucket harus kosong dulu (%s)", apiErr.messageOrBody())
	}
	return err
}

func (e *APIError) messageOrBody() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Body
}

// SetWebsiteAccess turns Garage's website serving on or off for a bucket. This
// is what makes the bucket reachable through the web endpoint on port 3902.
func (g *Garage) SetWebsiteAccess(ctx context.Context, id string, enabled bool, indexDocument string) error {
	access := map[string]any{"enabled": enabled}
	if enabled {
		access["indexDocument"] = indexDocument
	}
	body := map[string]any{"websiteAccess": access}
	q := url.Values{"id": {id}}
	return g.do(ctx, http.MethodPost, "UpdateBucket", q, body, nil)
}

// CreateKey creates an S3 access key. The response is the only time Garage
// reveals the secret.
func (g *Garage) CreateKey(ctx context.Context, name string) (*KeyInfo, error) {
	body := map[string]any{"name": name, "neverExpires": true}
	var out KeyInfo
	if err := g.do(ctx, http.MethodPost, "CreateKey", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AllowBucketKey grants permissions on a bucket to an access key.
func (g *Garage) AllowBucketKey(ctx context.Context, bucketID, accessKeyID string, perm BucketKeyPerm) error {
	body := map[string]any{
		"bucketId":    bucketID,
		"accessKeyId": accessKeyID,
		"permissions": perm,
	}
	return g.do(ctx, http.MethodPost, "AllowBucketKey", nil, body, nil)
}

// GetKeyInfo looks an access key up. showSecret asks Garage to include the
// secret, which is what makes it possible to tell a wrong GARAGE_S3_SECRET_KEY
// apart from every other reason a signature can be refused.
func (g *Garage) GetKeyInfo(ctx context.Context, accessKeyID string, showSecret bool) (*KeyInfo, error) {
	q := url.Values{"id": {accessKeyID}}
	if showSecret {
		q.Set("showSecretKey", "true")
	}
	var out KeyInfo
	if err := g.do(ctx, http.MethodGet, "GetKeyInfo", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Health checks that the Admin API answers and the token is accepted.
func (g *Garage) Health(ctx context.Context) error {
	_, err := g.GetClusterHealth(ctx)
	return err
}

// ClusterHealth is the answer of GetClusterHealth.
type ClusterHealth struct {
	Status           string `json:"status"` // healthy, degraded, unavailable
	KnownNodes       int    `json:"knownNodes"`
	ConnectedNodes   int    `json:"connectedNodes"`
	StorageNodes     int    `json:"storageNodes"`
	StorageNodesUp   int    `json:"storageNodesUp"`
	Partitions       int    `json:"partitions"`
	PartitionsQuorum int    `json:"partitionsQuorum"`
	PartitionsAllOK  int    `json:"partitionsAllOk"`
}

// GetClusterHealth reports how the cluster is doing from this node's view.
func (g *Garage) GetClusterHealth(ctx context.Context) (*ClusterHealth, error) {
	var out ClusterHealth
	if err := g.do(ctx, http.MethodGet, "GetClusterHealth", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClusterStatus is the answer of GetClusterStatus: every node the cluster
// knows, with the disks behind it.
type ClusterStatus struct {
	LayoutVersion int           `json:"layoutVersion"`
	Nodes         []ClusterNode `json:"nodes"`
}

// ClusterNode is one node in ClusterStatus. Nullable strings decode to "".
type ClusterNode struct {
	ID                string          `json:"id"`
	Hostname          string          `json:"hostname"`
	Addr              string          `json:"addr"`
	IsUp              bool            `json:"isUp"`
	LastSeenSecsAgo   *int64          `json:"lastSeenSecsAgo"`
	Draining          bool            `json:"draining"`
	GarageVersion     string          `json:"garageVersion"`
	Role              *NodeRole       `json:"role"`
	DataPartition     *PartitionUsage `json:"dataPartition"`
	MetadataPartition *PartitionUsage `json:"metadataPartition"`
}

// NodeRole is what the layout assigns to a node; nil for nodes outside it.
type NodeRole struct {
	Zone     string   `json:"zone"`
	Capacity *int64   `json:"capacity"` // nil for gateway nodes
	Tags     []string `json:"tags"`
}

// PartitionUsage is the free/total bytes of a node's data or metadata disk.
type PartitionUsage struct {
	Available int64 `json:"available"`
	Total     int64 `json:"total"`
}

// GetClusterStatus lists the nodes. It needs its own scope on a restricted
// admin token; the Status page says so when the call is refused.
func (g *Garage) GetClusterStatus(ctx context.Context) (*ClusterStatus, error) {
	var out ClusterStatus
	if err := g.do(ctx, http.MethodGet, "GetClusterStatus", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BucketDetails is a bucket plus the per-bucket statistics that ListBuckets
// does not return.
type BucketDetails struct {
	Item BucketListItem
	Info *BucketInfo
	Err  error
}

// ListBucketsWithInfo lists buckets and fills in size/object counts by calling
// GetBucketInfo for each one, with bounded concurrency. A failure on one bucket
// is reported on that bucket instead of failing the whole page.
func (g *Garage) ListBucketsWithInfo(ctx context.Context) ([]BucketDetails, error) {
	items, err := g.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]BucketDetails, len(items))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, item := range items {
		out[i].Item = item
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			info, err := g.GetBucketInfo(ctx, id)
			out[i].Info = info
			out[i].Err = err
		}(i, item.ID)
	}
	wg.Wait()
	return out, nil
}
