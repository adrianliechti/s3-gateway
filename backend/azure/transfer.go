package azure

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/adrianliechti/s3-gateway/backend"
)

var _ backend.Composer = (*Store)(nil)

// CommitBlockList may discard other uncommitted blocks at the same key. Keep
// writes to that key together; unrelated uploads still transfer concurrently.
func (s *Store) lockWrite(b, k string) func() {
	h := sha256.Sum256([]byte(b + "/" + k))
	s.writes[h[0]].Lock()
	return s.writes[h[0]].Unlock
}

func (s *Store) Compose(ctx context.Context, b, k string, sources []backend.ComposeSource, p backend.PutOptions) (backend.Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	unlock := s.lockWrite(b, k)
	defer unlock()
	cond, err := s.conditions(ctx, b, k, p.Conditions, false)
	if err != nil {
		return p.Object, err
	}
	type piece struct {
		url          string
		auth         *string
		revision     string
		offset, size int64
	}
	var pieces []piece
	var total int64
	for _, source := range sources {
		if source.Size < 0 {
			return p.Object, fmt.Errorf("invalid composition size")
		}
		c, err := s.versionClient(b, source.Key)
		if err != nil {
			return p.Object, err
		}
		sourceURL := c.URL()
		var auth *string
		if s.sharedKey {
			// The SDK blob helper appends '?' and drops version signing when the
			// client URL already has versionid. A read-only container SAS covers
			// the historical blob while preserving its version query parameter.
			if _, _, historical, _ := backend.ParseVersionReference(source.Key); historical {
				containerURL, e := s.cc(b).GetSASURL(sas.ContainerPermissions{Read: true}, time.Now().UTC().Add(time.Hour), nil)
				if e != nil {
					return p.Object, e
				}
				signed, e := url.Parse(containerURL)
				if e != nil {
					return p.Object, e
				}
				u, e := url.Parse(sourceURL)
				if e != nil {
					return p.Object, e
				}
				q := u.Query()
				for key, values := range signed.Query() {
					q[key] = values
				}
				u.RawQuery = q.Encode()
				sourceURL = u.String()
			} else {
				sourceURL, err = c.GetSASURL(sas.BlobPermissions{Read: true}, time.Now().UTC().Add(time.Hour), nil)
				if err != nil {
					return p.Object, err
				}
			}
		} else if s.credential != nil {
			token, err := s.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://storage.azure.com/.default"}})
			if err != nil {
				return p.Object, err
			}
			auth = ptr("Bearer " + token.Token)
		}
		total += source.Size
		// Azure's maximum copied block is 4000 MiB. S3 parts can be 5 GiB.
		for offset := int64(0); offset < source.Size; offset += 4000 << 20 {
			pieces = append(pieces, piece{sourceURL, auth, source.Revision, offset, min(4000<<20, source.Size-offset)})
		}
	}
	if len(pieces) > 50000 {
		return p.Object, fmt.Errorf("composition exceeds 50000 Azure blocks")
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return p.Object, err
	}
	ids := make([]string, len(pieces))
	err = backend.Parallel(ctx, len(pieces), func(ctx context.Context, i int) error {
		piece := pieces[i]
		ids[i] = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%x%08d", nonce, i)))
		options := &blockblob.StageBlockFromURLOptions{CopySourceAuthorization: piece.auth, Range: blob.HTTPRange{Offset: piece.offset, Count: piece.size}}
		if piece.revision != "" {
			options.SourceModifiedAccessConditions = &blob.SourceModifiedAccessConditions{SourceIfMatch: ptr(azcore.ETag(piece.revision))}
		}
		_, err := s.bc(b, k).StageBlockFromURL(ctx, ids[i], piece.url, options)
		return translate(err)
	})
	if bloberror.HasCode(err, "APINotImplemented") {
		// Azurite does not implement Put Block From URL. Its compatibility
		// fallback still streams with bounded buffers; real Azure uses copies.
		reader := &compositionReader{ctx: ctx, store: s, bucket: b, sources: sources}
		defer reader.Close()
		return s.put(ctx, b, k, reader, total, p, false)
	}
	if err != nil {
		return p.Object, err
	}
	o := p.Object
	o.Key, o.Size = k, total
	if o.ContentType == "" {
		o.ContentType = "application/octet-stream"
	}
	// Composite MD5 cannot be derived from individual MD5s. Leave native
	// Content-MD5 unset and retain the frontend ETag/checksums in the envelope.
	m, err := s.encodeMetadata(ctx, b, o, "", false)
	if err != nil {
		return o, err
	}
	out, err := s.bc(b, k).CommitBlockList(ctx, ids, &blockblob.CommitBlockListOptions{AccessConditions: cond, Metadata: m, HTTPHeaders: &blob.HTTPHeaders{BlobContentType: ptr(o.ContentType), BlobCacheControl: ptr(o.CacheControl), BlobContentDisposition: ptr(o.ContentDisposition), BlobContentEncoding: ptr(o.ContentEncoding), BlobContentLanguage: ptr(o.ContentLanguage)}})
	if err != nil {
		return o, translate(err)
	}
	o.Modified, o.Revision = val(out.LastModified), string(val(out.ETag))
	return o, nil
}

// Used only by providers explicitly returning APINotImplemented (Azurite).
type compositionReader struct {
	ctx     context.Context
	store   *Store
	bucket  string
	sources []backend.ComposeSource
	body    io.ReadCloser
}

func (r *compositionReader) Read(p []byte) (int, error) {
	for {
		if r.body == nil {
			if len(r.sources) == 0 {
				return 0, io.EOF
			}
			source := r.sources[0]
			r.sources = r.sources[1:]
			_, body, err := r.store.get(r.ctx, r.bucket, source.Key, backend.ReadOptions{Length: source.Size, Revision: source.Revision})
			if err != nil {
				return 0, err
			}
			r.body = body
		}
		n, err := r.body.Read(p)
		if err == io.EOF {
			r.body.Close()
			r.body = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}
func (r *compositionReader) Close() error {
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}
