// Package azure maps buckets to containers and keys to native block blobs.
package azure

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
	"github.com/adrianliechti/s3-gateway/backend"
)

type Options struct {
	Account, AccountKey, ServiceURL, SASToken string
}
type Store struct {
	client     *azblob.Client
	credential azcore.TokenCredential
	sharedKey  bool
	transport  *http.Transport
	writes     [256]sync.Mutex
	// Keep bucket deletion separate from object mutations in this process.
	mu sync.RWMutex
}

var _ backend.Backend = (*Store)(nil)

func New(o Options) (*Store, error) {
	endpoint := o.ServiceURL
	if endpoint == "" {
		if o.Account == "" {
			return nil, fmt.Errorf("Azure account or service URL is required")
		}
		endpoint = "https://" + o.Account + ".blob.core.windows.net"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid Azure service URL; configure SAS separately")
	}
	if o.AccountKey != "" && o.SASToken != "" {
		return nil, fmt.Errorf("configure either an Azure key or SAS")
	}
	var c *azblob.Client
	var credential azcore.TokenCredential
	// Content-Encoding describes the stored object, not an SDK response format.
	// Preserve the exact bytes, Content-Length and checksums on every download.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.MaxIdleConnsPerHost = 10 // Match the Azure SDK pool for parallel block transfers.
	options := &azblob.ClientOptions{ClientOptions: policy.ClientOptions{Transport: &http.Client{Transport: transport}}}
	switch {
	case o.AccountKey != "":
		cred, e := azblob.NewSharedKeyCredential(o.Account, o.AccountKey)
		if e != nil {
			return nil, fmt.Errorf("invalid Azure shared key")
		}
		c, err = azblob.NewClientWithSharedKeyCredential(endpoint, cred, options)
	case o.SASToken != "":
		u.RawQuery = strings.TrimPrefix(o.SASToken, "?")
		c, err = azblob.NewClientWithNoCredential(u.String(), options)
	default:
		cred, e := azidentity.NewDefaultAzureCredential(nil)
		if e != nil {
			return nil, e
		}
		credential = cred
		c, err = azblob.NewClient(endpoint, cred, options)
	}
	if err != nil {
		return nil, err
	}
	return &Store{client: c, credential: credential, sharedKey: o.AccountKey != "", transport: transport}, nil
}
func (s *Store) Close() error {
	if s.transport != nil {
		s.transport.CloseIdleConnections()
	}
	return nil
}
func (s *Store) cc(b string) *container.Client { return s.client.ServiceClient().NewContainerClient(b) }
func (s *Store) bc(b, k string) *blockblob.Client {
	return s.cc(b).NewBlockBlobClient(k)
}
func ptr[T any](v T) *T { return &v }
func val[T any](v *T) (z T) {
	if v != nil {
		return *v
	}
	return z
}
func translate(err error) error {
	if err == nil {
		return nil
	}
	var e *azcore.ResponseError
	if errors.As(err, &e) {
		switch e.ErrorCode {
		case "ContainerNotFound":
			return backend.ErrBucketNotFound
		case "BlobNotFound":
			return backend.ErrNotFound
		case "ContainerAlreadyExists":
			return backend.ErrBucketExists
		case "ConditionNotMet", "TargetConditionNotMet":
			return backend.ErrPrecondition
		case "InvalidResourceName", "OutOfRangeInput":
			return backend.ErrInvalidKey
		}
		if e.StatusCode == 412 {
			return backend.ErrPrecondition
		}
	}
	return err
}
func (s *Store) ListBuckets(ctx context.Context) ([]backend.Bucket, error) {
	pager := s.client.NewListContainersPager(&azblob.ListContainersOptions{Include: service.ListContainersInclude{Metadata: true}})
	out := []backend.Bucket{}
	for pager.More() {
		p, err := pager.NextPage(ctx)
		if err != nil {
			return nil, translate(err)
		}
		for _, c := range p.ContainerItems {
			created := val(c.Properties.LastModified)
			if t, e := time.Parse(time.RFC3339Nano, val(c.Metadata["gateway_created"])); e == nil {
				created = t
			}
			out = append(out, backend.Bucket{Name: val(c.Name), Created: created})
		}
	}
	return out, nil
}
func (s *Store) CreateBucket(ctx context.Context, b string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, err := s.cc(b).Create(ctx, &container.CreateOptions{Metadata: map[string]*string{"gateway_created": ptr(time.Now().UTC().Format(time.RFC3339Nano))}})
	return translate(err)
}
func (s *Store) HeadBucket(ctx context.Context, b string) error {
	_, err := s.cc(b).GetProperties(ctx, nil)
	return translate(err)
}
func (s *Store) DeleteBucket(ctx context.Context, b string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, _, err := s.list(ctx, b, "", "", 1)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return backend.ErrBucketNotEmpty
	}
	_, err = s.cc(b).Delete(ctx, nil)
	return translate(err)
}

// Native-compatible user metadata remains directly readable by Azure clients.
// Only S3-specific fields and incompatible metadata names use the envelope.
type metadata struct {
	Tags                    []backend.Tag        `json:"tags,omitempty"`
	Parts                   []backend.ObjectPart `json:"parts,omitempty"`
	Reference               string               `json:"ref,omitempty"`
	Version                 int                  `json:"v"`
	MD5                     string               `json:"md5"`
	Size                    int64                `json:"size"`
	ETag                    string               `json:"etag"`
	Expires                 string               `json:"expires,omitempty"`
	WebsiteRedirectLocation string               `json:"website_redirect,omitempty"`
	Extra                   map[string]string    `json:"extra,omitempty"`
	Checksums               map[string]string    `json:"checksums,omitempty"`
	ChecksumType            string               `json:"checksum_type,omitempty"`
	VersionID               string               `json:"version_id,omitempty"`
	ACL                     backend.ACL          `json:"acl,omitempty"`
	Modified                time.Time            `json:"modified,omitempty"`
}

func nativeMetadata(k, v string) bool {
	if k == "" || strings.EqualFold(k, "gateway") {
		return false
	}
	for i, c := range []byte(k) {
		if c != '_' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	for _, c := range []byte(v) {
		if c < 32 || c > 126 {
			return false
		}
	}
	return true
}
func encode(o backend.Object, digest string) (map[string]*string, error) {
	saved := metadata{Version: 1, MD5: digest, Size: o.Size, ETag: o.ETag, Expires: o.Expires, Extra: map[string]string{}, Checksums: o.Checksums, ChecksumType: o.ChecksumType}
	if digest == "" {
		saved.Version = 2
	}
	saved.VersionID, saved.ACL = o.VersionID, o.ACL
	saved.WebsiteRedirectLocation = o.WebsiteRedirectLocation
	saved.Tags, saved.Parts = o.Tags, o.Parts
	m := map[string]*string{}
	for k, v := range o.Metadata {
		if nativeMetadata(k, v) {
			m[strings.ToLower(k)] = ptr(v)
		} else {
			saved.Extra[k] = v
		}
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return nil, err
	}
	m["gateway"] = ptr(base64.StdEncoding.EncodeToString(data))
	size := 0
	for k, v := range m {
		size += len(k) + len(val(v)) + len("x-ms-meta-: \r\n")
	}
	if size > 8000 {
		return m, backend.ErrMetadataTooLarge
	}
	return m, nil
}
func decode(o backend.Object, m map[string]*string) (backend.Object, error) {
	o.Metadata = map[string]string{}
	for k, v := range m {
		o.Metadata[strings.ToLower(k)] = val(v)
	}
	data, err := base64.StdEncoding.DecodeString(o.Metadata["gateway"])
	var saved metadata
	if err != nil || json.Unmarshal(data, &saved) != nil || (saved.Version != 1 && saved.Version != 2) {
		return o, nil // An unrelated native metadata field is not gateway state.
	}
	delete(o.Metadata, "gateway")
	// Ignore stale gateway state after a native writer replaces content while
	// retaining metadata. Native Content-MD5 and size identify the saved bytes.
	if saved.Size != o.Size || saved.Version == 1 && saved.MD5 != o.ETag {
		return o, nil
	}
	o.ETag, o.Expires = saved.ETag, saved.Expires
	o.WebsiteRedirectLocation = saved.WebsiteRedirectLocation
	o.Checksums, o.ChecksumType = saved.Checksums, saved.ChecksumType
	o.VersionID, o.ACL = saved.VersionID, saved.ACL
	o.Tags, o.Parts = saved.Tags, saved.Parts
	if !saved.Modified.IsZero() {
		o.Modified = saved.Modified
	}
	for k, v := range saved.Extra {
		o.Metadata[k] = v
	}
	return o, nil
}
func (s *Store) Head(ctx context.Context, b, k string) (backend.Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.head(ctx, b, k)
}
func (s *Store) head(ctx context.Context, b, k string) (backend.Object, error) {
	c, err := s.versionClient(b, k)
	if err != nil {
		return backend.Object{}, err
	}
	p, err := c.GetProperties(ctx, nil)
	if err != nil {
		return backend.Object{}, translate(err)
	}
	o := backend.Object{Key: k, Size: val(p.ContentLength), Modified: val(p.LastModified), Revision: string(val(p.ETag)), ETag: strings.Trim(string(val(p.ETag)), "\""), ContentType: val(p.ContentType), CacheControl: val(p.CacheControl), ContentDisposition: val(p.ContentDisposition), ContentEncoding: val(p.ContentEncoding), ContentLanguage: val(p.ContentLanguage)}
	if len(p.ContentMD5) > 0 {
		o.ETag = hex.EncodeToString(p.ContentMD5)
	}
	return s.decodeMetadata(ctx, b, o, p.Metadata)
}
func (s *Store) Get(ctx context.Context, b, k string, r backend.ReadOptions) (backend.Object, io.ReadCloser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.get(ctx, b, k, r)
}
func (s *Store) get(ctx context.Context, b, k string, r backend.ReadOptions) (backend.Object, io.ReadCloser, error) {
	var o backend.Object
	if r.Offset < 0 || r.Length < -1 || r.Length > 0 && r.Offset+r.Length-1 < r.Offset {
		return o, nil, fmt.Errorf("invalid read range")
	}
	if r.Length == 0 {
		o, err := s.head(ctx, b, k)
		if err != nil {
			return o, nil, err
		}
		if r.Revision != "" && r.Revision != o.Revision {
			return o, nil, backend.ErrPrecondition
		}
		return o, io.NopCloser(bytes.NewReader(nil)), nil
	}
	n := r.Length
	if n < 0 {
		n = 0
	}
	c, err := s.versionClient(b, k)
	if err != nil {
		return o, nil, err
	}
	options := &blob.DownloadStreamOptions{Range: blob.HTTPRange{Offset: r.Offset, Count: n}}
	if r.Revision != "" {
		options.AccessConditions = &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: ptr(azcore.ETag(r.Revision))}}
	}
	p, err := c.DownloadStream(ctx, options)
	if err != nil {
		return o, nil, translate(err)
	}
	size, err := backend.ReadObjectSize(val(p.ContentLength), val(p.ContentRange))
	if err != nil {
		p.Body.Close()
		return o, nil, err
	}
	o = backend.Object{Key: k, Size: size, Modified: val(p.LastModified), Revision: string(val(p.ETag)), ETag: strings.Trim(string(val(p.ETag)), "\""), ContentType: val(p.ContentType), CacheControl: val(p.CacheControl), ContentDisposition: val(p.ContentDisposition), ContentEncoding: val(p.ContentEncoding), ContentLanguage: val(p.ContentLanguage)}
	if r.Revision != "" && r.Revision != o.Revision {
		p.Body.Close()
		return o, nil, backend.ErrPrecondition
	}
	digest := p.BlobContentMD5
	if len(digest) == 0 && (p.ContentRange == nil || size == val(p.ContentLength)) {
		digest = p.ContentMD5
	}
	if len(digest) > 0 {
		o.ETag = hex.EncodeToString(digest)
	}
	if !r.DataOnly {
		o, err = s.decodeMetadata(ctx, b, o, p.Metadata)
		if err != nil {
			p.Body.Close()
			return o, nil, err
		}
	}
	return o, p.Body, nil
}
func (s *Store) conditions(ctx context.Context, b, k string, c backend.Conditions, deleting bool) (*blob.AccessConditions, error) {
	if c.IfMatch == "" && c.IfNoneMatch == "" {
		return nil, nil
	}
	o, err := s.head(ctx, b, k)
	if err != nil && !errors.Is(err, backend.ErrNotFound) {
		return nil, err
	}
	check := backend.CheckWriteConditions
	if deleting {
		check = backend.CheckDeleteConditions
	}
	if e := check(o, err == nil, c); e != nil {
		return nil, e
	}
	m := &blob.ModifiedAccessConditions{}
	if err == nil {
		m.IfMatch = ptr(azcore.ETag(o.Revision))
	} else {
		m.IfNoneMatch = ptr(azcore.ETag("*"))
	}
	return &blob.AccessConditions{ModifiedAccessConditions: m}, nil
}
func (s *Store) Put(ctx context.Context, b, k string, body io.Reader, size int64, p backend.PutOptions) (backend.Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	unlock := s.lockWrite(b, k)
	defer unlock()
	return s.put(ctx, b, k, body, size, p, false)
}
func (s *Store) put(ctx context.Context, b, k string, body io.Reader, size int64, p backend.PutOptions, preserveModified bool) (backend.Object, error) {
	cond, err := s.conditions(ctx, b, k, p.Conditions, false)
	if err != nil {
		return backend.Object{}, err
	}
	// Stage native Azure blocks. Only CommitBlockList publishes bytes + metadata;
	// failing a body digest never replaces the currently committed blob.
	var upload [16]byte
	_, _ = rand.Read(upload[:])
	ids := make([]string, 50000)
	total, digest, err := backend.Transfer(ctx, body, backend.TransferBlockSize, func(ctx context.Context, part int, data []byte) error {
		if part >= len(ids) {
			return fmt.Errorf("Azure upload exceeds 50000 blocks")
		}
		id := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%x%08d", upload, part)))
		ids[part] = id
		_, err := s.bc(b, k).StageBlock(ctx, id, streaming.NopCloser(bytes.NewReader(data)), nil)
		return translate(err)
	})
	if err != nil {
		return backend.Object{}, err
	}
	ids = ids[:(total+backend.TransferBlockSize-1)/backend.TransferBlockSize]
	digestBytes, _ := hex.DecodeString(digest)
	if size >= 0 && size != total {
		return backend.Object{}, io.ErrUnexpectedEOF
	}
	o := backend.UploadedObject(body, p.Object)
	o.Key = k
	o.Size = total
	if o.ETag == "" {
		o.ETag = digest
	}
	if o.ContentType == "" {
		o.ContentType = "application/octet-stream"
	}
	m, err := s.encodeMetadata(ctx, b, o, digest, preserveModified)
	if err != nil {
		return o, err
	}
	res, err := s.bc(b, k).CommitBlockList(ctx, ids, &blockblob.CommitBlockListOptions{AccessConditions: cond, Metadata: m, HTTPHeaders: &blob.HTTPHeaders{BlobContentType: ptr(o.ContentType), BlobCacheControl: ptr(o.CacheControl), BlobContentDisposition: ptr(o.ContentDisposition), BlobContentEncoding: ptr(o.ContentEncoding), BlobContentLanguage: ptr(o.ContentLanguage), BlobContentMD5: digestBytes}})
	if err != nil {
		return o, translate(err)
	}
	if !preserveModified {
		o.Modified = val(res.LastModified)
	}
	o.Revision = string(val(res.ETag))
	return o, nil
}
func (s *Store) Delete(ctx context.Context, b, k string, c backend.Conditions) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.HeadBucket(ctx, b); err != nil {
		return err
	}
	cond, err := s.conditions(ctx, b, k, c, true)
	if err != nil {
		return err
	}
	client, err := s.versionClient(b, k)
	if err != nil {
		return err
	}
	_, err = client.Delete(ctx, &blob.DeleteOptions{AccessConditions: cond})
	err = translate(err)
	if errors.Is(err, backend.ErrNotFound) {
		return nil
	}
	return err
}
func (s *Store) List(ctx context.Context, b, prefix, after string, limit int) ([]backend.Object, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.list(ctx, b, prefix, after, limit)
}
func (s *Store) list(ctx context.Context, b, prefix, after string, limit int) ([]backend.Object, string, error) {
	if err := s.HeadBucket(ctx, b); err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		return []backend.Object{}, "", nil
	}
	pager := s.cc(b).NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: &prefix, Include: container.ListBlobsInclude{Metadata: true}})
	out := []backend.Object{}
	internal := strings.HasPrefix(prefix, backend.InternalPrefix)
	for pager.More() {
		p, err := pager.NextPage(ctx)
		if err != nil {
			return nil, "", translate(err)
		}
		for _, v := range p.Segment.BlobItems {
			k := val(v.Name)
			if k <= after || (!internal && strings.HasPrefix(k, backend.InternalPrefix)) {
				continue
			}
			if len(out) == limit {
				return out, out[len(out)-1].Key, nil
			}
			prop := v.Properties
			o := backend.Object{Key: k, Size: val(prop.ContentLength), Modified: val(prop.LastModified), Revision: string(val(prop.ETag)), ETag: strings.Trim(string(val(prop.ETag)), "\""), ContentType: val(prop.ContentType)}
			if len(prop.ContentMD5) > 0 {
				o.ETag = hex.EncodeToString(prop.ContentMD5)
			}
			o, err = s.decodeMetadata(ctx, b, o, v.Metadata)
			if err != nil {
				return nil, "", err
			}
			out = append(out, o)
		}
	}
	return out, "", nil
}
