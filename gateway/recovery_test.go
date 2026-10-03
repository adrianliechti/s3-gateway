package gateway_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/adrianliechti/s3-gateway/backend/managed"
	"github.com/adrianliechti/s3-gateway/gateway"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Fail before or after a durable backend operation; the latter models a lost
// acknowledgement. Replacing the Gateway discards all in-memory upload state.
type publicationFault struct {
	backend.Backend
	backend.Properties
	mu    sync.Mutex
	match func(string, string) bool
	after bool
	fired bool
}

func (f *publicationFault) NativeVersion(ctx context.Context, b, k string) (string, error) {
	if native, ok := f.Backend.(backend.NativeHistory); ok {
		return native.NativeVersion(ctx, b, k)
	}
	return "", nil
}

func (f *publicationFault) hit(method, key string, after bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.fired && f.match != nil && f.after == after && f.match(method, key) {
		f.fired = true
		return true
	}
	return false
}
func (f *publicationFault) Put(ctx context.Context, b, k string, r io.Reader, n int64, p backend.PutOptions) (backend.Object, error) {
	if f.hit("put", k, false) {
		return backend.Object{}, errors.New("injected publication failure")
	}
	o, err := f.Backend.Put(ctx, b, k, r, n, p)
	if err == nil && f.hit("put", k, true) {
		err = errors.New("injected lost publication acknowledgement")
	}
	return o, err
}
func (f *publicationFault) Delete(ctx context.Context, b, k string, c backend.Conditions) error {
	if f.hit("delete", k, false) {
		return errors.New("injected deletion failure")
	}
	err := f.Backend.Delete(ctx, b, k, c)
	if err == nil && f.hit("delete", k, true) {
		err = errors.New("injected lost deletion acknowledgement")
	}
	return err
}

func multipartRequest(t *testing.T, f versionFixture, key, data string) *s3.CompleteMultipartUploadInput {
	t.Helper()
	init, err := f.c.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{Bucket: &f.bucket, Key: &key, ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
	must(t, err)
	part, err := f.c.UploadPart(t.Context(), &s3.UploadPartInput{Bucket: &f.bucket, Key: &key, UploadId: init.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader(data), ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
	must(t, err)
	return &s3.CompleteMultipartUploadInput{Bucket: &f.bucket, Key: &key, UploadId: init.UploadId, IfNoneMatch: aws.String("*"), MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag, ChecksumSHA256: part.ChecksumSHA256}}}}
}

func TestSDKCompatibilityMultipartRecovery(t *testing.T) {
	for _, mode := range []types.BucketVersioningStatus{"", types.BucketVersioningStatusEnabled, types.BucketVersioningStatusSuspended} {
		for _, stage := range []string{"data", "pending", "native", "index", "manifest", "remove-intent", "cleanup"} {
			if stage == "index" && mode == "" {
				continue
			}
			for _, after := range []bool{false, true} {
				name := string(mode) + "/" + stage + "/before"
				if after {
					name = string(mode) + "/" + stage + "/after"
				}
				t.Run(name, func(t *testing.T) {
					f := versionSetup(t)
					if _, native := f.be.(backend.Composer); native && stage == "data" {
						t.Skip("native multipart assembly publishes directly; there is no staged data write")
					}
					if mode != "" {
						f.state(t, mode)
					}
					req := multipartRequest(t, f, "recovered", "multipart value")
					fault := &publicationFault{Backend: f.be, Properties: f.be.(backend.Properties), after: after}
					fault.match = func(method, key string) bool {
						switch stage {
						case "data":
							return method == "put" && strings.Contains(key, "/data/")
						case "pending", "index", "manifest":
							return method == "put" && strings.HasSuffix(key, "/"+stage)
						case "native":
							return method == "put" && key == "recovered"
						case "remove-intent":
							return method == "delete" && strings.HasSuffix(key, "/pending")
						case "cleanup":
							return method == "delete" && strings.Contains(key, "/parts/")
						}
						return false
					}
					var instrumented backend.Backend = fault
					if composer, ok := f.be.(backend.Composer); ok {
						instrumented = &composedPublicationFault{publicationFault: fault, composer: composer}
					}
					g, err := gateway.New(instrumented, gateway.Options{AccessKey: access, SecretKey: secret})
					must(t, err)
					c := client(&transport{handler: g})
					initial, err := c.CompleteMultipartUpload(t.Context(), req)
					if stage == "cleanup" {
						must(t, err)
					} else {
						code(t, err, "InternalError")
					}
					if !fault.fired {
						t.Fatal("fault was not reached")
					}
					g, err = gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret})
					must(t, err)
					if after {
						must(t, g.RunMaintenanceOnce(t.Context(), time.Now()))
					}
					c = client(&transport{handler: g})
					result, err := c.CompleteMultipartUpload(t.Context(), req)
					must(t, err)
					if initial != nil && (aws.ToString(initial.VersionId) != aws.ToString(result.VersionId) || aws.ToString(initial.ETag) != aws.ToString(result.ETag)) {
						t.Fatal("acknowledged result changed")
					}
					if len(f.versions(t, "recovered").Versions) != 1 || f.read(t, "recovered", "") != "multipart value" {
						t.Fatal("duplicate publication or lost bytes")
					}
					f.put(t, "recovered", "later write")
					retry, err := c.CompleteMultipartUpload(t.Context(), req)
					must(t, err)
					if aws.ToString(retry.VersionId) != aws.ToString(result.VersionId) || aws.ToString(retry.ETag) != aws.ToString(result.ETag) || aws.ToString(retry.ChecksumSHA256) != aws.ToString(result.ChecksumSHA256) || f.read(t, "recovered", "") != "later write" {
						t.Fatal("retry changed original result or overwrote later write")
					}
					_, err = c.DeleteObject(t.Context(), &s3.DeleteObjectInput{Bucket: &f.bucket, Key: req.Key})
					must(t, err)
					_, err = c.CompleteMultipartUpload(t.Context(), req)
					must(t, err)
					_, err = c.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: &f.bucket, Key: req.Key})
					if err == nil {
						t.Fatal("retry resurrected deleted object")
					}
					changed := *req
					changed.IfNoneMatch = nil
					_, err = c.CompleteMultipartUpload(t.Context(), &changed)
					code(t, err, "NoSuchUpload")
					_, err = c.AbortMultipartUpload(t.Context(), &s3.AbortMultipartUploadInput{Bucket: &f.bucket, Key: req.Key, UploadId: req.UploadId})
					code(t, err, "NoSuchUpload")
					must(t, g.RunMaintenanceOnce(t.Context(), time.Now().Add(48*time.Hour)))
					_, err = c.CompleteMultipartUpload(t.Context(), req)
					must(t, err) // Retained receipts outlive both object and cleanup grace.
				})
			}
		}
	}
}

func TestSDKCompatibilityMultipartRejectedCondition(t *testing.T) {
	f := versionSetup(t)
	f.state(t, types.BucketVersioningStatusEnabled)
	old := f.put(t, "key", "old")
	req := multipartRequest(t, f, "key", "new")
	_, err := f.c.CompleteMultipartUpload(t.Context(), req)
	code(t, err, "PreconditionFailed")
	parts, err := f.c.ListParts(t.Context(), &s3.ListPartsInput{Bucket: &f.bucket, Key: req.Key, UploadId: req.UploadId})
	must(t, err)
	if len(parts.Parts) != 1 {
		t.Fatal("rejected request consumed upload")
	}
	req.IfNoneMatch, req.IfMatch = nil, old.ETag
	_, err = f.c.CompleteMultipartUpload(t.Context(), req)
	must(t, err)
	if f.read(t, "key", "") != "new" || len(f.versions(t, "key").Versions) != 2 {
		t.Fatal("conditional completion failed")
	}
}

func TestSDKCompatibilityHelperCollection(t *testing.T) {
	f := versionSetup(t)
	f.state(t, types.BucketVersioningStatusEnabled)
	ctx := t.Context()
	store := managed.New(f.be)
	large := backend.Object{Parts: make([]backend.ObjectPart, 500)}
	for i := range large.Parts {
		large.Parts[i] = backend.ObjectPart{Number: i + 1, Size: 1, Checksums: map[string]string{"SHA256": strings.Repeat("a", 44)}}
	}
	first, err := store.Put(ctx, f.bucket, "history", strings.NewReader("old"), 3, backend.PutOptions{Object: large})
	must(t, err)
	_, err = store.Put(ctx, f.bucket, "history", strings.NewReader("new"), 3, backend.PutOptions{})
	must(t, err)
	// Track the helper for a uniquely tagged temporary object so collection
	// can prove it removes orphans while preserving all retained references.
	before, _, err := f.be.List(ctx, f.bucket, ".gateway/metadata/", "", 1000)
	must(t, err)
	knownHelpers := make(map[string]bool)
	for _, o := range before {
		knownHelpers[o.Key] = true
	}
	temporary := large
	temporary.Tags = []backend.Tag{{Key: "gc-test", Value: "orphan"}}
	_, err = f.be.Put(ctx, f.bucket, "temporary", strings.NewReader("value"), 5, backend.PutOptions{Object: temporary})
	must(t, err)
	after, _, err := f.be.List(ctx, f.bucket, ".gateway/metadata/", "", 1000)
	must(t, err)
	orphanHelpers := make(map[string]bool)
	for _, o := range after {
		if !knownHelpers[o.Key] {
			orphanHelpers[o.Key] = true
		}
	}
	must(t, f.be.Delete(ctx, f.bucket, "temporary", backend.Conditions{}))
	p := f.be.(backend.Properties)
	settings, err := p.GetBucketProperties(ctx, f.bucket)
	must(t, err)
	settings.Tags = []backend.Tag{{Key: "large", Value: strings.Repeat("x", 10000)}}
	must(t, p.SetBucketProperties(ctx, f.bucket, settings))
	settings.Tags = nil
	must(t, p.SetBucketProperties(ctx, f.bucket, settings))
	// Unpublished version staging has no index reference.
	orphan := ".gateway/versions/" + strings.Repeat("0", 64) + "/data/" + strings.Repeat("0", 32)
	_, err = f.be.Put(ctx, f.bucket, orphan, strings.NewReader("abandoned"), 9, backend.PutOptions{})
	must(t, err)
	active := multipartRequest(t, f, "active", "active part")
	_, err = f.be.Put(ctx, f.bucket, ".gateway/"+strings.Repeat("f", 32)+"/parts/00001", strings.NewReader("aborted"), 7, backend.PutOptions{})
	must(t, err)
	g, err := gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret})
	must(t, err)
	must(t, g.RunMaintenanceOnce(ctx, time.Now()))
	_, err = f.be.Head(ctx, f.bucket, orphan)
	must(t, err) // Young staging retains its grace period.
	must(t, g.RunMaintenanceOnce(ctx, time.Now().Add(48*time.Hour)))
	_, err = f.be.Head(ctx, f.bucket, orphan)
	if !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("orphan survived: %v", err)
	}
	if f.read(t, "history", first.VersionID) != "old" {
		t.Fatal("history collected")
	}
	historical, err := store.HeadVersion(ctx, f.bucket, "history", first.VersionID)
	must(t, err)
	if len(historical.Parts) != 500 {
		t.Fatal("retained metadata collected")
	}
	parts, err := f.c.ListParts(ctx, &s3.ListPartsInput{Bucket: &f.bucket, Key: active.Key, UploadId: active.UploadId})
	must(t, err)
	if len(parts.Parts) != 1 {
		t.Fatal("active upload collected")
	}
	_, err = f.c.CompleteMultipartUpload(ctx, active)
	must(t, err)
	objects, _, err := f.be.List(ctx, f.bucket, ".gateway/", "", 1000)
	must(t, err)
	for _, o := range objects {
		if strings.HasPrefix(o.Key, ".gateway/"+strings.Repeat("f", 32)+"/") {
			t.Fatalf("unreachable helper survived: %s", o.Key)
		}
		if orphanHelpers[o.Key] {
			t.Fatalf("orphan metadata helper survived: %s", o.Key)
		}
	}

}

// Preserve the optional native capability so cloud recovery tests exercise the
// same server-side publication path used in production.
type composedPublicationFault struct {
	*publicationFault
	composer backend.Composer
}

func (f *composedPublicationFault) Compose(ctx context.Context, b, k string, sources []backend.ComposeSource, p backend.PutOptions) (backend.Object, error) {
	if f.hit("put", k, false) {
		return backend.Object{}, errors.New("injected publication failure")
	}
	o, err := f.composer.Compose(ctx, b, k, sources, p)
	if err == nil && f.hit("put", k, true) {
		err = errors.New("injected lost publication acknowledgement")
	}
	return o, err
}
