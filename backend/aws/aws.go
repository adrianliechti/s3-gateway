// Package aws maps buckets and keys to an S3-compatible service using the AWS SDK.
package aws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type Options struct {
	Endpoint, Region string
	UsePathStyle     bool
}

type Store struct {
	client *s3.Client
	// Exclude bucket deletion and helper collection while publishing or reading.
	mu sync.RWMutex
}

var _ backend.Backend = (*Store)(nil)
var _ backend.Properties = (*Store)(nil)

func New(ctx context.Context, o Options) (*Store, error) {
	if o.Endpoint != "" {
		u, err := url.Parse(o.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("invalid S3 storage endpoint; configure credentials separately")
		}
	}
	var options []func(*config.LoadOptions) error
	if o.Region != "" {
		options = append(options, config.WithRegion(o.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	client := s3.NewFromConfig(cfg, func(c *s3.Options) {
		if o.Endpoint != "" {
			c.BaseEndpoint = aws.String(o.Endpoint)
		}
		c.UsePathStyle = o.UsePathStyle
		// Avoid optional streaming checksum extensions on compatible providers.
		c.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		c.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &Store{client: client}, nil
}

func (s *Store) Close() error { return nil }

func translate(err error) error {
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchBucket":
			return backend.ErrBucketNotFound
		case "NoSuchKey", "NotFound":
			return backend.ErrNotFound
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return backend.ErrBucketExists
		case "BucketNotEmpty":
			return backend.ErrBucketNotEmpty
		case "PreconditionFailed":
			return backend.ErrPrecondition
		case "ConditionalRequestConflict", "OperationAborted":
			return backend.ErrConflict
		case "InvalidObjectName", "InvalidBucketName", "KeyTooLongError", "InvalidArgument":
			return backend.ErrInvalidKey
		case "InvalidRequest":
			return backend.ErrInvalidRequest
		case "MetadataTooLarge":
			return backend.ErrMetadataTooLarge
		}
	}
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) {
		switch response.HTTPStatusCode() {
		case http.StatusNotFound:
			return backend.ErrNotFound
		case http.StatusPreconditionFailed:
			return backend.ErrPrecondition
		}
	}
	return err
}

// HEAD has no XML error body, so a 404 must be disambiguated at bucket level.
func (s *Store) objectError(ctx context.Context, b string, err error) error {
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() == "NoSuchKey" {
		// GET errors identify the missing resource. Only bodyless/ambiguous
		// 404 responses need a separate bucket-existence check.
		return backend.ErrNotFound
	}
	err = translate(err)
	if errors.Is(err, backend.ErrNotFound) {
		if e := s.HeadBucket(ctx, b); e != nil {
			return e
		}
	}
	return err
}

func (s *Store) ListBuckets(ctx context.Context) ([]backend.Bucket, error) {
	var out []backend.Bucket
	pager := s3.NewListBucketsPaginator(s.client, &s3.ListBucketsInput{})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, translate(err)
		}
		for _, b := range page.Buckets {
			out = append(out, backend.Bucket{Name: aws.ToString(b.Name), Created: aws.ToTime(b.CreationDate)})
		}
	}
	return out, nil
}

func (s *Store) CreateBucket(ctx context.Context, b string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// AWS us-east-1 otherwise treats creation of an owned bucket as success.
	if err := s.HeadBucket(ctx, b); err == nil {
		return backend.ErrBucketExists
	} else if !errors.Is(err, backend.ErrBucketNotFound) {
		return err
	}
	in := &s3.CreateBucketInput{Bucket: &b}
	if region := s.client.Options().Region; region != "us-east-1" {
		in.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}
	}
	_, err := s.client.CreateBucket(ctx, in)
	return translate(err)
}

func (s *Store) HeadBucket(ctx context.Context, b string) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &b})
	err = translate(err)
	if errors.Is(err, backend.ErrNotFound) {
		return backend.ErrBucketNotFound
	}
	return err
}

func (s *Store) DeleteBucket(ctx context.Context, b string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	objects, _, err := s.list(ctx, b, "", "", 1)
	if err != nil {
		return err
	}
	if len(objects) != 0 {
		return backend.ErrBucketNotEmpty
	}
	if err = s.purgeEmptyBucket(ctx, b); err != nil {
		return err
	}
	_, err = s.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &b})
	return translate(err)
}

func (s *Store) Head(ctx context.Context, b, k string) (backend.Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.head(ctx, b, k)
}

func (s *Store) head(ctx context.Context, b, k string) (backend.Object, error) {
	key, version, _, err := backend.ParseVersionReference(k)
	if err != nil {
		return backend.Object{}, err
	}
	if key == "" {
		key = k
	}
	p, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b, Key: aws.String(key), VersionId: optional(version)})
	if err != nil {
		return backend.Object{}, s.objectError(ctx, b, err)
	}
	o := backend.Object{Key: k, Size: aws.ToInt64(p.ContentLength), Modified: aws.ToTime(p.LastModified), ETag: strings.Trim(aws.ToString(p.ETag), "\""), Revision: aws.ToString(p.ETag), ContentType: aws.ToString(p.ContentType), CacheControl: aws.ToString(p.CacheControl), ContentDisposition: aws.ToString(p.ContentDisposition), ContentEncoding: aws.ToString(p.ContentEncoding), ContentLanguage: aws.ToString(p.ContentLanguage), WebsiteRedirectLocation: aws.ToString(p.WebsiteRedirectLocation), Metadata: p.Metadata}
	if p.Expires != nil {
		o.Expires = p.Expires.UTC().Format(http.TimeFormat)
	}
	return s.decodeMetadata(ctx, b, o)
}

func (s *Store) Get(ctx context.Context, b, k string, r backend.ReadOptions) (backend.Object, io.ReadCloser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.get(ctx, b, k, r)
}

func (s *Store) get(ctx context.Context, b, k string, r backend.ReadOptions) (backend.Object, io.ReadCloser, error) {
	var o backend.Object
	if r.Offset < 0 || r.Length < -1 {
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
	key, version, _, err := backend.ParseVersionReference(k)
	if err != nil {
		return o, nil, err
	}
	if key == "" {
		key = k
	}
	in := &s3.GetObjectInput{Bucket: &b, Key: aws.String(key), VersionId: optional(version), IfMatch: optional(r.Revision)}
	if r.Length > 0 {
		end := r.Offset + r.Length - 1
		if end < r.Offset {
			return o, nil, fmt.Errorf("invalid read range")
		}
		in.Range = aws.String(fmt.Sprintf("bytes=%d-%d", r.Offset, end))
	} else if r.Offset > 0 {
		in.Range = aws.String(fmt.Sprintf("bytes=%d-", r.Offset))
	}
	p, err := s.client.GetObject(ctx, in)
	if err != nil {
		return o, nil, s.objectError(ctx, b, err)
	}
	size, err := backend.ReadObjectSize(aws.ToInt64(p.ContentLength), aws.ToString(p.ContentRange))
	if err != nil {
		p.Body.Close()
		return o, nil, err
	}
	o = backend.Object{Key: k, Size: size, Modified: aws.ToTime(p.LastModified), ETag: strings.Trim(aws.ToString(p.ETag), "\""), Revision: aws.ToString(p.ETag), ContentType: aws.ToString(p.ContentType), CacheControl: aws.ToString(p.CacheControl), ContentDisposition: aws.ToString(p.ContentDisposition), ContentEncoding: aws.ToString(p.ContentEncoding), ContentLanguage: aws.ToString(p.ContentLanguage), WebsiteRedirectLocation: aws.ToString(p.WebsiteRedirectLocation), Metadata: p.Metadata}
	if r.Revision != "" && r.Revision != o.Revision {
		p.Body.Close()
		return o, nil, backend.ErrPrecondition
	}
	if p.Expires != nil {
		o.Expires = p.Expires.UTC().Format(http.TimeFormat)
	}
	if !r.DataOnly {
		o, err = s.decodeMetadata(ctx, b, o)
		if err != nil {
			p.Body.Close()
			return o, nil, err
		}
	}
	return o, p.Body, nil
}

// Gateway ETags may describe a frontend multipart upload, rather than the
// upstream representation. Check those first, then guard the mutation with
// the native ETag (or If-None-Match: * for an absent object).
func (s *Store) conditions(ctx context.Context, b, k string, c backend.Conditions, deleting bool) (match, none *string, missing bool, err error) {
	if c.IfMatch == "" && c.IfNoneMatch == "" {
		return
	}
	o, e := s.head(ctx, b, k)
	if e != nil && !errors.Is(e, backend.ErrNotFound) {
		err = e
		return
	}
	missing = e != nil
	check := backend.CheckWriteConditions
	if deleting {
		check = backend.CheckDeleteConditions
	}
	if err = check(o, !missing, c); err != nil {
		return
	}
	if missing {
		none = aws.String("*")
	} else {
		match = &o.Revision
	}
	return
}

func (s *Store) Put(ctx context.Context, b, k string, body io.Reader, size int64, p backend.PutOptions) (backend.Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	match, none, _, err := s.conditions(ctx, b, k, p.Conditions, false)
	if err != nil {
		return backend.Object{}, err
	}
	return s.put(ctx, b, k, body, size, p.Object, match, none, false)
}

const uploadPartSize int64 = 8 << 20

func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func (s *Store) Delete(ctx context.Context, b, k string, c backend.Conditions) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.HeadBucket(ctx, b); err != nil {
		return err
	}
	match, _, missing, err := s.conditions(ctx, b, k, c, true)
	if err != nil || missing {
		return err
	}
	key, version, reference, err := backend.ParseVersionReference(k)
	if err != nil {
		return err
	}
	if reference {
		_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b, Key: aws.String(key), VersionId: &version, IfMatch: match})
		return translate(err)
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b, Key: aws.String(k), IfMatch: match})
	if err == nil && strings.HasPrefix(k, backend.InternalPrefix) {
		return s.purgeKey(ctx, b, k)
	}
	return translate(err)
}

func (s *Store) List(ctx context.Context, b, prefix, after string, limit int) ([]backend.Object, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.list(ctx, b, prefix, after, limit)
}
func (s *Store) list(ctx context.Context, b, prefix, after string, limit int) ([]backend.Object, string, error) {
	if limit <= 0 {
		return []backend.Object{}, "", s.HeadBucket(ctx, b)
	}
	in := &s3.ListObjectsV2Input{Bucket: &b, Prefix: aws.String(prefix), StartAfter: optional(after), MaxKeys: aws.Int32(int32(min(limit, 999) + 1))}
	pager := s3.NewListObjectsV2Paginator(listingClient{s.client}, in)
	out := []backend.Object{}
	internal := strings.HasPrefix(prefix, backend.InternalPrefix)
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, "", translate(err)
		}
		for _, item := range page.Contents {
			key := aws.ToString(item.Key)
			if key <= after || (!internal && strings.HasPrefix(key, backend.InternalPrefix)) {
				continue
			}
			// ListObjects does not include metadata needed for gateway ETags/tags.
			o, err := s.head(ctx, b, key)
			if errors.Is(err, backend.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, "", err
			}
			if len(out) == limit {
				return out, out[len(out)-1].Key, nil
			}
			out = append(out, o)
		}
	}
	return out, "", nil
}
