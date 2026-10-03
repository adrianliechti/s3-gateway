package gateway_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/adrianliechti/s3-gateway/gateway"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type cloudTransferProbe struct {
	backend.Backend
	backend.Properties
	backend.NativeHistory
	backend.Composer
	copies atomic.Int64
}

func (p *cloudTransferProbe) Get(ctx context.Context, b, k string, r backend.ReadOptions) (backend.Object, io.ReadCloser, error) {
	if strings.Contains(k, "/parts/") || strings.Contains(k, "/data/") || !strings.HasPrefix(k, backend.InternalPrefix) {
		return backend.Object{}, nil, fmt.Errorf("unexpected payload download during publication: %s", k)
	}
	return p.Backend.Get(ctx, b, k, r)
}
func (p *cloudTransferProbe) Compose(ctx context.Context, b, k string, s []backend.ComposeSource, o backend.PutOptions) (backend.Object, error) {
	p.copies.Add(1)
	return p.Composer.Compose(ctx, b, k, s, o)
}

func TestSDKCompatibilityCloudStreaming(t *testing.T) {
	f := versionSetup(t)
	composer, ok := f.be.(backend.Composer)
	if !ok {
		t.Skip("cloud transfer capability required")
	}
	probe := &cloudTransferProbe{Backend: f.be, Properties: f.be.(backend.Properties), NativeHistory: f.be.(backend.NativeHistory), Composer: composer}
	// Upload requests must succeed even when no staging file can be created.
	g, err := gateway.New(probe, gateway.Options{AccessKey: access, SecretKey: secret, TempDir: filepath.Join(t.TempDir(), "does-not-exist")})
	must(t, err)
	c := client(&transport{handler: g})
	data := bytes.Repeat([]byte("streaming"), 2<<20) // exceeds a native 8 MiB block
	for _, versioned := range []bool{false, true} {
		if versioned {
			f.state(t, types.BucketVersioningStatusEnabled)
		}
		key := fmt.Sprintf("stream-%t", versioned)
		result, err := c.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: bytes.NewReader(data), ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
		must(t, err)
		if got := f.read(t, key, aws.ToString(result.VersionId)); got != string(data) {
			t.Fatal("stream bytes differ")
		}
		_, err = c.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: bytes.NewReader(data), ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(make([]byte, 32)))})
		code(t, err, "BadDigest")
		if f.read(t, key, "") != string(data) {
			t.Fatal("invalid stream replaced committed object")
		}
	}
	// Completion may read small manifests, but every object/part byte must stay
	// inside the provider. Exercise full-object CRC combination and replay.
	g, err = gateway.New(probe, gateway.Options{AccessKey: access, SecretKey: secret})
	must(t, err)
	completeClient := client(&transport{handler: g})
	for _, alg := range []types.ChecksumAlgorithm{types.ChecksumAlgorithmCrc32, types.ChecksumAlgorithmCrc32c, types.ChecksumAlgorithmCrc64nvme} {
		key := "multipart-" + string(alg)
		init, err := f.c.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{Bucket: &f.bucket, Key: &key, ChecksumAlgorithm: alg, ChecksumType: types.ChecksumTypeFullObject})
		must(t, err)
		var parts []types.CompletedPart
		for i, body := range [][]byte{data, []byte("tail")} {
			n := int32(i + 1)
			part, err := c.UploadPart(t.Context(), &s3.UploadPartInput{Bucket: &f.bucket, Key: &key, UploadId: init.UploadId, PartNumber: &n, Body: bytes.NewReader(body), ChecksumAlgorithm: alg})
			must(t, err)
			parts = append(parts, types.CompletedPart{PartNumber: &n, ETag: part.ETag, ChecksumCRC32: part.ChecksumCRC32, ChecksumCRC32C: part.ChecksumCRC32C, ChecksumCRC64NVME: part.ChecksumCRC64NVME})
		}
		req := &s3.CompleteMultipartUploadInput{Bucket: &f.bucket, Key: &key, UploadId: init.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}}
		out, err := completeClient.CompleteMultipartUpload(t.Context(), req)
		must(t, err)
		retried, err := completeClient.CompleteMultipartUpload(t.Context(), req)
		must(t, err)
		if aws.ToString(out.VersionId) != aws.ToString(retried.VersionId) {
			t.Fatal("retry created a new version")
		}
		got, err := f.c.GetObject(t.Context(), &s3.GetObjectInput{Bucket: &f.bucket, Key: &key, ChecksumMode: types.ChecksumModeEnabled})
		must(t, err)
		raw, err := io.ReadAll(got.Body)
		got.Body.Close()
		must(t, err) // SDK independently validates CRC
		if !bytes.Equal(raw, append(append([]byte{}, data...), []byte("tail")...)) {
			t.Fatal("assembled bytes differ")
		}
	}
	if probe.copies.Load() == 0 {
		t.Fatal("native assembly was not used")
	}
}
