package gateway_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/adrianliechti/s3-gateway/gateway"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestSDKCompatibilityReadChecksums(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	key := "checksums"
	original, err := f.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: strings.NewReader("0123456789"), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32})
	must(t, err)
	for _, span := range []string{"bytes=2-5", "bytes=-4"} {
		out, err := f.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: &key, Range: &span, ChecksumMode: types.ChecksumModeEnabled})
		must(t, err)
		data, err := io.ReadAll(out.Body)
		out.Body.Close()
		must(t, err)
		expected := "2345"
		if span == "bytes=-4" {
			expected = "6789"
		}
		if string(data) != expected || out.ChecksumCRC32 != nil {
			t.Fatalf("invalid range or checksum: %q %+v", data, out)
		}
	}
	// Exercise real response headers, including the bodyless 304 path.
	req, err := http.NewRequestWithContext(ctx, "GET", "http://gateway.test/"+f.bucket+"/"+key, nil)
	must(t, err)
	req.Header.Set("X-Amz-Checksum-Mode", "ENABLED")
	req.Header.Set("If-None-Match", aws.ToString(original.ETag))
	digest := fmt.Sprintf("%x", sha256.Sum256(nil))
	req.Header.Set("X-Amz-Content-Sha256", digest)
	must(t, v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: access, SecretAccessKey: secret}, req, digest, "s3", "us-east-1", time.Now()))
	res, err := f.c.Options().HTTPClient.Do(req)
	must(t, err)
	defer res.Body.Close()
	if res.StatusCode != 304 || res.Header.Get("X-Amz-Checksum-Crc32") != "" {
		t.Fatalf("304 included data checksum: %d %+v", res.StatusCode, res.Header)
	}
	out, err := f.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: &key, ChecksumMode: types.ChecksumModeEnabled})
	must(t, err)
	_, err = io.ReadAll(out.Body)
	out.Body.Close()
	must(t, err)
	if out.ChecksumCRC32 == nil {
		t.Fatal("full-object checksum lost")
	}
}

func TestSDKCompatibilityAttributesAndParts(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	f.state(t, types.BucketVersioningStatusEnabled)
	key := "multipart"
	init, err := f.c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &f.bucket, Key: &key, ChecksumAlgorithm: types.ChecksumAlgorithmSha256, Tagging: aws.String("table=events")})
	must(t, err)
	chunks := [][]byte{bytes.Repeat([]byte("a"), 5<<20), []byte("last part")}
	var complete []types.CompletedPart
	for i, data := range chunks {
		sum := sha256.Sum256(data)
		encoded := base64.StdEncoding.EncodeToString(sum[:])
		n := int32(i + 1)
		out, e := f.c.UploadPart(ctx, &s3.UploadPartInput{Bucket: &f.bucket, Key: &key, UploadId: init.UploadId, PartNumber: &n, Body: bytes.NewReader(data), ChecksumSHA256: &encoded})
		must(t, e)
		complete = append(complete, types.CompletedPart{PartNumber: &n, ETag: out.ETag, ChecksumSHA256: out.ChecksumSHA256})
	}
	committed, err := f.c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &f.bucket, Key: &key, UploadId: init.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: complete}})
	must(t, err)
	f.put(t, key, "new generation")
	// New gateway instance verifies durable metadata, after parts were cleaned up.
	reopened, err := gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret})
	must(t, err)
	c := client(&transport{handler: reopened})
	attrs, err := c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{Bucket: &f.bucket, Key: &key, VersionId: committed.VersionId, ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesEtag, types.ObjectAttributesChecksum, types.ObjectAttributesObjectParts, types.ObjectAttributesObjectSize}, MaxParts: aws.Int32(1)})
	must(t, err)
	if aws.ToInt64(attrs.ObjectSize) != int64(len(chunks[0])+len(chunks[1])) || attrs.ObjectParts == nil || !aws.ToBool(attrs.ObjectParts.IsTruncated) || len(attrs.ObjectParts.Parts) != 1 || attrs.Checksum == nil || aws.ToString(attrs.Checksum.ChecksumSHA256) != aws.ToString(committed.ChecksumSHA256) {
		t.Fatalf("incorrect attributes: %+v", attrs)
	}
	next, err := c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{Bucket: &f.bucket, Key: &key, VersionId: committed.VersionId, ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesObjectParts}, PartNumberMarker: attrs.ObjectParts.NextPartNumberMarker})
	must(t, err)
	if next.ETag != nil || next.ObjectSize != nil || len(next.ObjectParts.Parts) != 1 || aws.ToInt32(next.ObjectParts.Parts[0].PartNumber) != 2 {
		t.Fatalf("bad attribute selection/page: %+v", next)
	}
	for i, data := range chunks {
		n := int32(i + 1)
		out, e := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: &key, VersionId: committed.VersionId, PartNumber: &n, ChecksumMode: types.ChecksumModeEnabled})
		must(t, e)
		raw, e := io.ReadAll(out.Body)
		out.Body.Close()
		must(t, e)
		if !bytes.Equal(raw, data) || aws.ToInt32(out.PartsCount) != 2 || aws.ToString(out.ETag) != aws.ToString(committed.ETag) || out.ChecksumType != types.ChecksumTypeComposite {
			t.Fatal("wrong version/part bytes")
		}
		head, e := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &f.bucket, Key: &key, VersionId: committed.VersionId, PartNumber: &n})
		must(t, e)
		if aws.ToInt64(head.ContentLength) != int64(len(data)) {
			t.Fatal("wrong part size")
		}
	}
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: &key, VersionId: committed.VersionId, PartNumber: aws.Int32(3)})
	code(t, err, "InvalidPart")
	single, err := c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{Bucket: &f.bucket, Key: &key, ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesObjectParts}})
	must(t, err)
	if single.ObjectParts != nil {
		t.Fatal("overwriting retained stale multipart boundaries")
	}
}

func TestSDKCompatibilityTags(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	key := "tags"
	f.state(t, types.BucketVersioningStatusEnabled)
	first, err := f.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: strings.NewReader("first"), Tagging: aws.String("table=events&note=hello+world")})
	must(t, err)
	f.put(t, key, "second")
	before, err := f.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &f.bucket, Key: &key})
	must(t, err)
	tagset := []types.Tag{{Key: aws.String("namespace"), Value: aws.String("analytics")}, {Key: aws.String("unicode"), Value: aws.String("雪")}}
	_, err = f.c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: &f.bucket, Key: &key, Tagging: &types.Tagging{TagSet: tagset}})
	must(t, err)
	after, err := f.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &f.bucket, Key: &key})
	must(t, err)
	if aws.ToString(before.ETag) != aws.ToString(after.ETag) || !before.LastModified.Equal(*after.LastModified) || aws.ToString(before.VersionId) != aws.ToString(after.VersionId) {
		t.Fatal("tag update changed object identity")
	}
	got, err := f.c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: &f.bucket, Key: &key})
	must(t, err)
	if !reflect.DeepEqual(got.TagSet, tagset) {
		t.Fatalf("tags differ: %+v", got.TagSet)
	}
	old, err := f.c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: &f.bucket, Key: &key, VersionId: first.VersionId})
	must(t, err)
	if len(old.TagSet) != 2 || aws.ToString(old.TagSet[0].Key) != "note" || aws.ToString(old.TagSet[0].Value) != "hello world" {
		t.Fatal("historical tags changed")
	}
	source := f.bucket + "/" + key
	for _, directive := range []types.TaggingDirective{types.TaggingDirectiveCopy, types.TaggingDirectiveReplace} {
		dest := "copy-" + string(directive)
		in := &s3.CopyObjectInput{Bucket: &f.bucket, Key: &dest, CopySource: &source, TaggingDirective: directive}
		if directive == types.TaggingDirectiveReplace {
			in.Tagging = aws.String("replacement=yes")
		}
		_, err = f.c.CopyObject(ctx, in)
		must(t, err)
		out, e := f.c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: &f.bucket, Key: &dest})
		must(t, e)
		count := 2
		if directive == types.TaggingDirectiveReplace {
			count = 1
		}
		if len(out.TagSet) != count {
			t.Fatal("copy tagging directive ignored")
		}
	}
	_, err = f.c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: &f.bucket, Key: &key, Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("duplicate"), Value: aws.String("a")}, {Key: aws.String("duplicate"), Value: aws.String("b")}}}})
	code(t, err, "InvalidTag")
	_, err = f.c.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{Bucket: &f.bucket, Key: &key, VersionId: first.VersionId})
	must(t, err)
	old, err = f.c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: &f.bucket, Key: &key, VersionId: first.VersionId})
	must(t, err)
	if len(old.TagSet) != 0 {
		t.Fatal("version tags not deleted")
	}
	got, err = f.c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: &f.bucket, Key: &key})
	must(t, err)
	if len(got.TagSet) != 2 {
		t.Fatal("deleting old tags changed latest")
	}
}

func TestSDKCompatibilityLargeMetadata(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	key := "large-manifest"
	// Seed a legal 10,000-part descriptor directly to exercise metadata capacity
	// without uploading 50 GiB just to test Azure's 8 KiB metadata boundary.
	parts := make([]backend.ObjectPart, 10000)
	for i := range parts {
		parts[i] = backend.ObjectPart{Number: i + 1, Size: 1}
	}
	_, err := f.be.Put(ctx, f.bucket, key, strings.NewReader(strings.Repeat("a", len(parts))), int64(len(parts)), backend.PutOptions{Object: backend.Object{Parts: parts}})
	must(t, err)
	var tags []types.Tag
	for i := 0; i < 10; i++ {
		tags = append(tags, types.Tag{Key: aws.String(fmt.Sprint(i) + strings.Repeat("雪", 127)), Value: aws.String(strings.Repeat("山", 256))})
	}
	_, err = f.c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: &f.bucket, Key: &key, Tagging: &types.Tagging{TagSet: tags}})
	must(t, err)
	out, err := f.c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{Bucket: &f.bucket, Key: &key, ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesObjectParts}, PartNumberMarker: aws.String("9998")})
	must(t, err)
	if len(out.ObjectParts.Parts) != 2 || aws.ToInt32(out.ObjectParts.TotalPartsCount) != 10000 {
		t.Fatal("large part manifest lost")
	}
	read, err := f.c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: &f.bucket, Key: &key})
	must(t, err)
	if !reflect.DeepEqual(read.TagSet, tags) {
		t.Fatal("large tag set lost")
	}
	listed, err := f.c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &f.bucket})
	must(t, err)
	if len(listed.Contents) != 1 {
		t.Fatal("metadata helpers exposed through S3")
	}
}

func TestSDKCompatibilityConditionalMutations(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	key := "conditional"
	for _, status := range []types.BucketVersioningStatus{"", types.BucketVersioningStatusEnabled, types.BucketVersioningStatusSuspended} {
		if status != "" {
			f.state(t, status)
		}
		_, err := f.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: strings.NewReader("missing"), IfMatch: aws.String("*")})
		code(t, err, "NoSuchKey")
		original := f.put(t, key, "content")
		_, err = f.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: strings.NewReader("wrong"), IfMatch: aws.String("wrong")})
		code(t, err, "PreconditionFailed")
		_, err = f.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: &key, IfMatch: original.ETag})
		must(t, err)
		_, err = f.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: strings.NewReader("deleted"), IfMatch: original.ETag})
		code(t, err, "NoSuchKey")
		// General-purpose S3 existence conditions reject a current delete marker.
		_, err = f.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: &key, IfMatch: aws.String("*")})
		want := "PreconditionFailed"
		if status == "" {
			want = "NoSuchKey"
		}
		code(t, err, want)
	}
}

// This reader blocks before yielding bytes, modelling a client whose retry
// arrives while an earlier UploadPart request is still transmitting.
type pausedBody struct {
	started chan struct{}
	release chan struct{}
	sent    bool
}

func (b *pausedBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.EOF
	}
	close(b.started)
	<-b.release
	b.sent = true
	return copy(p, "first"), nil
}
func signedPart(ctx context.Context, f versionFixture, uid, key string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "PUT", "http://gateway.test/"+f.bucket+"/"+key+"?uploadId="+url.QueryEscape(uid)+"&partNumber=1", body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	err = v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: access, SecretAccessKey: secret}, req, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now())
	if err != nil {
		return nil, err
	}
	return f.c.Options().HTTPClient.Do(req)
}
func TestSDKCompatibilityOverlappingPartRetries(t *testing.T) {
	f := versionSetup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	for _, abort := range []bool{false, true} {
		key := fmt.Sprintf("overlap-%v", abort)
		init, err := f.c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &f.bucket, Key: &key})
		must(t, err)
		body := &pausedBody{started: make(chan struct{}), release: make(chan struct{})}
		type result struct {
			res *http.Response
			err error
		}
		done := make(chan result, 1)
		go func() {
			res, e := signedPart(ctx, f, aws.ToString(init.UploadId), key, body, 5)
			done <- result{res, e}
		}()
		<-body.started
		// Always unblock on failure to keep test cleanup bounded.
		released := false
		defer func() {
			if !released {
				close(body.release)
			}
		}()
		if abort {
			_, err = f.c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &f.bucket, Key: &key, UploadId: init.UploadId})
			must(t, err)
		} else {
			retry, e := f.c.UploadPart(ctx, &s3.UploadPartInput{Bucket: &f.bucket, Key: &key, UploadId: init.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("retry")})
			must(t, e)
			if retry.ETag == nil {
				t.Fatal("retry did not finish while first body was blocked")
			}
		}
		close(body.release)
		released = true
		first := <-done
		must(t, first.err)
		first.res.Body.Close()
		if abort {
			if first.res.StatusCode != 404 {
				t.Fatal("in-flight upload resurrected aborted state")
			}
			continue
		}
		if first.res.StatusCode != 200 {
			t.Fatalf("first upload: %d", first.res.StatusCode)
		}
		etag := first.res.Header.Get("ETag")
		_, err = f.c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &f.bucket, Key: &key, UploadId: init.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: &etag}}}})
		must(t, err)
		if f.read(t, key, "") != "first" {
			t.Fatal("last completed retry was not published")
		}
	}
}
