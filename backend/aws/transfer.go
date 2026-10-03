package aws

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func objectDefaults(o backend.Object, k string, size int64, digest string, preserve bool) backend.Object {
	o.Key, o.Size = k, size
	if o.ETag == "" {
		o.ETag = digest
	}
	if o.ContentType == "" {
		o.ContentType = "application/octet-stream"
	}
	if !preserve {
		o.Modified = time.Now().UTC().Truncate(time.Second)
	}
	return o
}

func (s *Store) startUpload(ctx context.Context, b, k string, o backend.Object, m map[string]string) (*s3.CreateMultipartUploadOutput, error) {
	var expires *time.Time
	if o.Expires != "" {
		t, err := http.ParseTime(o.Expires)
		if err != nil {
			return nil, err
		}
		expires = &t
	}
	return s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &b, Key: &k, ContentType: optional(o.ContentType), CacheControl: optional(o.CacheControl), ContentDisposition: optional(o.ContentDisposition), ContentEncoding: optional(o.ContentEncoding), ContentLanguage: optional(o.ContentLanguage), WebsiteRedirectLocation: optional(o.WebsiteRedirectLocation), Expires: expires, Metadata: m})
}

func (s *Store) abortUpload(ctx context.Context, b, k string, id *string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, _ = s.client.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{Bucket: &b, Key: &k, UploadId: id})
}

// Small bodies use PutObject; larger streams upload four retryable parts at a
// time. Metadata becomes known at EOF, so MPU references a write-once receipt
// saved before completion. No object bytes are spooled or copied back through
// the gateway, and failed validation never publishes the MPU.
func (s *Store) put(ctx context.Context, b, k string, body io.Reader, size int64, o backend.Object, match, none *string, preserve bool) (backend.Object, error) {
	capacity := uploadPartSize + 1
	if size >= 0 {
		capacity = min(capacity, size+1)
	}
	first := make([]byte, capacity)
	n, err := io.ReadFull(body, first)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return o, err
	}
	if err != nil {
		if size >= 0 && int64(n) != size {
			return o, io.ErrUnexpectedEOF
		}
		sum := md5.Sum(first[:n])
		o = objectDefaults(backend.UploadedObject(body, o), k, int64(n), hex.EncodeToString(sum[:]), preserve)
		m, err := s.encodeMetadata(ctx, b, o)
		if err != nil {
			return o, err
		}
		var expires *time.Time
		if o.Expires != "" {
			t, e := http.ParseTime(o.Expires)
			if e != nil {
				return o, e
			}
			expires = &t
		}
		out, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: &k, Body: bytes.NewReader(first[:n]), ContentLength: &o.Size, ContentType: &o.ContentType, CacheControl: optional(o.CacheControl), ContentDisposition: optional(o.ContentDisposition), ContentEncoding: optional(o.ContentEncoding), ContentLanguage: optional(o.ContentLanguage), WebsiteRedirectLocation: optional(o.WebsiteRedirectLocation), Expires: expires, Metadata: m, IfMatch: match, IfNoneMatch: none})
		if err != nil {
			return o, s.objectError(ctx, b, err)
		}
		o.Revision = aws.ToString(out.ETag)
		return o, nil
	}
	var nonce [32]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return o, err
	}
	reference := detailsPrefix + hex.EncodeToString(nonce[:])
	pointer, _ := json.Marshal(metadata{Version: 2, Reference: reference})
	o = objectDefaults(o, k, size, "", preserve)
	init, err := s.startUpload(ctx, b, k, o, map[string]string{"gateway": base64.StdEncoding.EncodeToString(pointer)})
	if err != nil {
		return o, translate(err)
	}
	committed := false
	defer func() {
		if !committed {
			s.abortUpload(ctx, b, k, init.UploadId)
		}
	}()
	var mu sync.Mutex
	parts := make([]types.CompletedPart, 10000)
	count := 0
	blockSize := int(max(uploadPartSize, (size+9999)/10000))
	input := io.MultiReader(bytes.NewReader(first[:n]), body)
	var stream io.Reader = input
	if closer, ok := body.(io.Closer); ok {
		stream = struct {
			io.Reader
			io.Closer
		}{input, closer}
	}
	total, digest, err := backend.Transfer(ctx, stream, blockSize, func(ctx context.Context, i int, data []byte) error {
		if i >= len(parts) {
			return fmt.Errorf("S3 upload exceeds 10000 native parts")
		}
		number, length := int32(i+1), int64(len(data))
		part, err := s.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: &b, Key: &k, UploadId: init.UploadId, PartNumber: &number, Body: bytes.NewReader(data), ContentLength: &length})
		if err != nil {
			return translate(err)
		}
		parts[i] = types.CompletedPart{PartNumber: &number, ETag: part.ETag}
		mu.Lock()
		count = max(count, i+1)
		mu.Unlock()
		return nil
	})
	if err != nil {
		return o, err
	}
	if size >= 0 && total != size {
		return o, io.ErrUnexpectedEOF
	}
	o = objectDefaults(backend.UploadedObject(body, o), k, total, digest, preserve)
	raw, err := json.Marshal(metadata{Version: 1, Object: &o})
	if err != nil {
		return o, err
	}
	if len(raw) > maxDetailsSize {
		return o, backend.ErrMetadataTooLarge
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: &reference, Body: bytes.NewReader(raw), ContentType: aws.String("application/json"), IfNoneMatch: aws.String("*")})
	if err != nil {
		return o, translate(err)
	}
	out, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &b, Key: &k, UploadId: init.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts[:count]}, IfMatch: match, IfNoneMatch: none})
	if err != nil {
		return o, s.objectError(ctx, b, err)
	}
	committed, o.Revision = true, aws.ToString(out.ETag)
	return o, nil
}

var _ backend.Composer = (*Store)(nil)

func (s *Store) Compose(ctx context.Context, b, k string, sources []backend.ComposeSource, p backend.PutOptions) (backend.Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.compose(ctx, b, k, sources, p, false)
}

func (s *Store) compose(ctx context.Context, b, k string, sources []backend.ComposeSource, p backend.PutOptions, preserve bool) (backend.Object, error) {
	match, none, _, err := s.conditions(ctx, b, k, p.Conditions, false)
	if err != nil {
		return p.Object, err
	}
	type piece struct {
		source         backend.ComposeSource
		offset, length int64
	}
	var pieces []piece
	var total int64
	for _, source := range sources {
		if source.Size < 0 {
			return p.Object, fmt.Errorf("invalid composition size")
		}
		total += source.Size
		for offset := int64(0); offset < source.Size; offset += 5 << 30 {
			pieces = append(pieces, piece{source, offset, min(5<<30, source.Size-offset)})
		}
	}
	if len(pieces) == 0 {
		return s.put(ctx, b, k, bytes.NewReader(nil), 0, p.Object, match, none, preserve)
	}
	if len(pieces) > 10000 {
		return p.Object, fmt.Errorf("composition exceeds 10000 native parts")
	}
	o := objectDefaults(p.Object, k, total, "", preserve)
	m, err := s.encodeMetadata(ctx, b, o)
	if err != nil {
		return o, err
	}
	init, err := s.startUpload(ctx, b, k, o, m)
	if err != nil {
		return o, translate(err)
	}
	committed := false
	defer func() {
		if !committed {
			s.abortUpload(ctx, b, k, init.UploadId)
		}
	}()
	parts := make([]types.CompletedPart, len(pieces))
	err = backend.Parallel(ctx, len(pieces), func(ctx context.Context, i int) error {
		piece := pieces[i]
		key, version, ref, err := backend.ParseVersionReference(piece.source.Key)
		if err != nil {
			return err
		}
		if !ref {
			key = piece.source.Key
		}
		source := url.PathEscape(b + "/" + key)
		if version != "" {
			source += "?versionId=" + url.QueryEscape(version)
		}
		number := int32(i + 1)
		rangeHeader := fmt.Sprintf("bytes=%d-%d", piece.offset, piece.offset+piece.length-1)
		out, err := s.client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: &b, Key: &k, UploadId: init.UploadId, PartNumber: &number, CopySource: &source, CopySourceRange: &rangeHeader, CopySourceIfMatch: optional(piece.source.Revision)})
		if err != nil {
			return translate(err)
		}
		if out.CopyPartResult == nil || out.CopyPartResult.ETag == nil {
			return fmt.Errorf("S3 copy returned no part ETag")
		}
		parts[i] = types.CompletedPart{PartNumber: &number, ETag: out.CopyPartResult.ETag}
		return nil
	})
	if err != nil {
		return o, err
	}
	out, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &b, Key: &k, UploadId: init.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}, IfMatch: match, IfNoneMatch: none})
	if err != nil {
		return o, s.objectError(ctx, b, err)
	}
	committed, o.Revision = true, aws.ToString(out.ETag)
	return o, nil
}
