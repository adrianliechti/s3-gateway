package gateway_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/adrianliechti/s3-gateway/backend/aws"
	"github.com/adrianliechti/s3-gateway/backend/azure"
	"github.com/adrianliechti/s3-gateway/backend/disk"
	"github.com/adrianliechti/s3-gateway/gateway"
	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type versionFixture struct {
	c      *s3.Client
	be     backend.Backend
	bucket string
}

func versionSetup(t *testing.T) versionFixture {
	t.Helper()
	var be backend.Backend
	var err error
	if endpoint := os.Getenv("GATEWAY_TEST_STORAGE_ENDPOINT"); endpoint != "" {
		be, err = aws.New(t.Context(), aws.Options{Endpoint: endpoint, Region: "us-east-1", UsePathStyle: true})
	} else if os.Getenv("GATEWAY_TEST_AZURE") == "1" {
		be, err = azure.New(azure.Options{Account: os.Getenv("AZURE_STORAGE_ACCOUNT"), AccountKey: os.Getenv("AZURE_STORAGE_KEY"), ServiceURL: os.Getenv("AZURE_STORAGE_SERVICE_URL"), SASToken: os.Getenv("AZURE_STORAGE_SAS_TOKEN")})
	} else if endpoint := os.Getenv("GATEWAY_TEST_AZURITE_URL"); endpoint != "" {
		be, err = azure.New(azure.Options{Account: "gateway", AccountKey: "Y29tcGF0aWJpbGl0eS10ZXN0LWtleS1vbmx5", ServiceURL: endpoint})
	} else {
		be, err = disk.New(disk.Options{Root: t.TempDir()})
	}
	must(t, err)
	t.Cleanup(func() { be.Close() })
	g, err := gateway.New(be, gateway.Options{AccessKey: access, SecretKey: secret})
	must(t, err)
	f := versionFixture{client(&transport{handler: g}), be, fmt.Sprintf("versions-%d", time.Now().UnixNano())}
	_, err = f.c.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: &f.bucket})
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for {
			p, e := f.c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &f.bucket})
			if e != nil {
				t.Error(e)
				return
			}
			objects := []types.ObjectIdentifier{}
			for _, v := range p.Versions {
				objects = append(objects, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
			}
			for _, v := range p.DeleteMarkers {
				objects = append(objects, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
			}
			if len(objects) == 0 {
				break
			}
			out, e := f.c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &f.bucket, Delete: &types.Delete{Objects: objects}})
			if e != nil {
				t.Error(e)
				return
			}
			if len(out.Errors) > 0 {
				for _, problem := range out.Errors {
					t.Errorf("cleanup key=%q version=%q: %s %s", sdk.ToString(problem.Key), sdk.ToString(problem.VersionId), sdk.ToString(problem.Code), sdk.ToString(problem.Message))
				}
				return
			}
		}
		_, e := f.c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &f.bucket})
		if e != nil {
			t.Error(e)
		}
	})
	return f
}

func TestSDKCompatibilityNativeHistory(t *testing.T) {
	f := versionSetup(t)
	native, ok := f.be.(backend.NativeHistory)
	if !ok {
		t.Skip("backend has no native version storage")
	}
	f.state(t, types.BucketVersioningStatusEnabled)
	first := f.put(t, "native-history", "first")
	ref, err := native.NativeVersion(t.Context(), f.bucket, "native-history")
	must(t, err)
	if ref == "" {
		t.Skip("provider versioning is not enabled; shared immutable data fallback is in use")
	}
	f.put(t, "native-history", "second")
	if f.read(t, "native-history", sdk.ToString(first.VersionId)) != "first" {
		t.Fatal("native historical bytes lost")
	}
	_, body, err := f.be.Get(t.Context(), f.bucket, ref, backend.ReadOptions{Length: -1})
	must(t, err)
	raw, err := io.ReadAll(body)
	body.Close()
	must(t, err)
	if string(raw) != "first" {
		t.Fatal("provider version was overwritten")
	}
	objects, _, err := f.be.List(t.Context(), f.bucket, backend.InternalPrefix+"versions/", "", 1000)
	must(t, err)
	for _, o := range objects {
		if strings.Contains(o.Key, "/data/") {
			t.Fatalf("persistent duplicate version bytes: %s", o.Key)
		}
	}
	t.Log("retained bytes are stored in native provider versions; only logical indexes remain")
}

// A null version can be removed before the first managed history index exists.
// Native versioned providers must still permit deleting the now-empty bucket.
func TestSDKCompatibilityDeleteUnindexedNullVersion(t *testing.T) {
	f := versionSetup(t)
	f.put(t, "null-before-index", "body")
	f.state(t, types.BucketVersioningStatusEnabled)
	_, err := f.c.DeleteObject(t.Context(), &s3.DeleteObjectInput{Bucket: &f.bucket, Key: sdk.String("null-before-index"), VersionId: sdk.String("null")})
	must(t, err)
	out, err := f.c.ListObjectVersions(t.Context(), &s3.ListObjectVersionsInput{Bucket: &f.bucket})
	must(t, err)
	if len(out.Versions) != 0 || len(out.DeleteMarkers) != 0 {
		t.Fatal("null version remained visible")
	}
	// versionSetup cleanup also asserts DeleteBucket succeeds.
}
func (f versionFixture) state(t *testing.T, status types.BucketVersioningStatus) {
	t.Helper()
	_, err := f.c.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{Bucket: &f.bucket, VersioningConfiguration: &types.VersioningConfiguration{Status: status}})
	must(t, err)
}
func (f versionFixture) put(t *testing.T, key, data string) *s3.PutObjectOutput {
	t.Helper()
	o, err := f.c.PutObject(t.Context(), &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: strings.NewReader(data)})
	must(t, err)
	return o
}
func (f versionFixture) read(t *testing.T, key, version string) string {
	t.Helper()
	in := &s3.GetObjectInput{Bucket: &f.bucket, Key: &key}
	if version != "" {
		in.VersionId = &version
	}
	o, err := f.c.GetObject(t.Context(), in)
	must(t, err)
	defer o.Body.Close()
	raw, err := io.ReadAll(o.Body)
	must(t, err)
	return string(raw)
}
func (f versionFixture) versions(t *testing.T, key string) *s3.ListObjectVersionsOutput {
	t.Helper()
	o, err := f.c.ListObjectVersions(t.Context(), &s3.ListObjectVersionsInput{Bucket: &f.bucket, Prefix: &key})
	must(t, err)
	return o
}

func TestSDKVersioningTransitions(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	key := "dir/key+雪"
	// Existing native content becomes a null version without an import step.
	_, err := f.be.Put(ctx, f.bucket, key, strings.NewReader("native"), 6, backend.PutOptions{})
	must(t, err)
	f.state(t, types.BucketVersioningStatusEnabled)
	// The us-east-1 legacy recreate operation resets ACLs, not version history.
	_, err = f.c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &f.bucket})
	must(t, err)
	_, err = f.c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: &f.bucket, VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled, MFADelete: types.MFADeleteDisabled}})
	must(t, err)
	a, b := f.put(t, key, "first"), f.put(t, key, "second")
	if sdk.ToString(a.VersionId) == "" || sdk.ToString(a.VersionId) == "null" || sdk.ToString(a.VersionId) == sdk.ToString(b.VersionId) {
		t.Fatal("version IDs must be unique")
	}
	if f.read(t, key, "null") != "native" || f.read(t, key, sdk.ToString(a.VersionId)) != "first" || f.read(t, key, "") != "second" {
		t.Fatal("historical content differs")
	}
	_, native, err := f.be.Get(ctx, f.bucket, key, backend.ReadOptions{Length: -1})
	must(t, err)
	raw, err := io.ReadAll(native)
	native.Close()
	must(t, err)
	if string(raw) != "second" {
		t.Fatal("native latest path differs")
	}
	first, err := f.c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &f.bucket, MaxKeys: sdk.Int32(1)})
	must(t, err)
	if len(first.Versions) != 1 || !sdk.ToBool(first.IsTruncated) || !sdk.ToBool(first.Versions[0].IsLatest) || sdk.ToString(first.Versions[0].VersionId) != sdk.ToString(b.VersionId) {
		t.Fatalf("bad first version page: %+v", first)
	}
	second, err := f.c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &f.bucket, MaxKeys: sdk.Int32(1), KeyMarker: first.NextKeyMarker, VersionIdMarker: first.NextVersionIdMarker})
	must(t, err)
	if len(second.Versions) != 1 || sdk.ToString(second.Versions[0].VersionId) != sdk.ToString(a.VersionId) || sdk.ToBool(second.Versions[0].IsLatest) {
		t.Fatalf("bad second page: %+v", second)
	}
	grouped, err := f.c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &f.bucket, Delimiter: sdk.String("/")})
	must(t, err)
	if len(grouped.CommonPrefixes) != 1 || len(grouped.Versions) != 0 {
		t.Fatalf("bad delimiter grouping: %+v", grouped)
	}
	hidden, err := f.c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &f.bucket, Prefix: sdk.String(".gateway/")})
	must(t, err)
	if len(hidden.Versions) != 0 || len(hidden.DeleteMarkers) != 0 {
		t.Fatal("version listing exposed gateway helper state")
	}
	_, err = f.c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: &f.bucket, VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled, MFADelete: types.MFADeleteEnabled}})
	code(t, err, "NotImplemented")
	del, err := f.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: &key})
	must(t, err)
	if !sdk.ToBool(del.DeleteMarker) || sdk.ToString(del.VersionId) == "" {
		t.Fatalf("missing delete marker: %+v", del)
	}
	_, err = f.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: &key})
	code(t, err, "NoSuchKey")
	_, err = f.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: &key, VersionId: del.VersionId})
	code(t, err, "MethodNotAllowed")
	listed, err := f.c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &f.bucket})
	must(t, err)
	if len(listed.Contents) != 0 {
		t.Fatal("delete marker appears in ordinary listing")
	}
	_, err = f.c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &f.bucket})
	code(t, err, "BucketNotEmpty")
	revived, err := f.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: strings.NewReader("revived"), IfNoneMatch: sdk.String("*")})
	must(t, err)
	_, err = f.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: strings.NewReader("invalid"), IfMatch: sdk.String(`"wrong"`)})
	code(t, err, "PreconditionFailed")
	if len(f.versions(t, key).Versions) != 4 {
		t.Fatal("failed conditional put changed history")
	}
	_, err = f.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: &key, VersionId: revived.VersionId})
	must(t, err)
	_, err = f.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: &key, VersionId: del.VersionId})
	must(t, err)
	if f.read(t, key, "") != "second" {
		t.Fatal("deleting marker failed to restore previous current version")
	}
	f.state(t, types.BucketVersioningStatusSuspended)
	for _, data := range []string{"null one", "null two"} {
		if v := f.put(t, key, data); v.VersionId != nil {
			t.Fatal("suspended puts must omit a new version ID")
		}
	}
	if len(f.versions(t, key).Versions) != 3 || f.read(t, key, "null") != "null two" {
		t.Fatal("suspended null replacement failed")
	}
	for i := 0; i < 2; i++ {
		v, e := f.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: &key})
		must(t, e)
		if sdk.ToString(v.VersionId) != "null" || !sdk.ToBool(v.DeleteMarker) {
			t.Fatal("suspended delete must create null marker")
		}
	}
	if v := f.versions(t, key); len(v.DeleteMarkers) != 1 || len(v.Versions) != 2 {
		t.Fatal("suspended deletion retained null data or duplicated marker")
	}
	f.state(t, types.BucketVersioningStatusEnabled)
	f.put(t, key, "enabled again")
	// A new handler has no in-memory version index or configuration cache.
	restarted, err := gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret})
	must(t, err)
	f.c = client(&transport{handler: restarted})
	if f.read(t, key, sdk.ToString(a.VersionId)) != "first" || f.read(t, key, "") != "enabled again" {
		t.Fatal("version state did not survive handler restart")
	}
	status, err := f.c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: &f.bucket})
	must(t, err)
	if status.Status != types.BucketVersioningStatusEnabled {
		t.Fatal("version setting not persisted")
	}
	// Malformed version ids are rejected before lookup, as on S3; a
	// well-formed unknown id is NoSuchVersion.
	_, err = f.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: &key, VersionId: sdk.String("missing")})
	code(t, err, "InvalidArgument")
	_, err = f.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: &key, VersionId: sdk.String("00000000000000000000000000000000")})
	code(t, err, "NoSuchVersion")
}

func TestSDKVersionedCopyAndMultipart(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	f.state(t, types.BucketVersioningStatusEnabled)
	old := f.put(t, "source", "old body")
	f.put(t, "source", "new body")
	source := url.PathEscape(f.bucket+"/source") + "?versionId=" + url.QueryEscape(sdk.ToString(old.VersionId))
	copied, err := f.c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &f.bucket, Key: sdk.String("copy"), CopySource: &source})
	must(t, err)
	if sdk.ToString(copied.VersionId) == "" || sdk.ToString(copied.CopySourceVersionId) != sdk.ToString(old.VersionId) || f.read(t, "copy", "") != "old body" {
		t.Fatal("versioned copy differs")
	}
	init, err := f.c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &f.bucket, Key: sdk.String("multipart")})
	must(t, err)
	part, err := f.c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: &f.bucket, Key: sdk.String("multipart"), UploadId: init.UploadId, PartNumber: sdk.Int32(1), CopySource: &source})
	must(t, err)
	if sdk.ToString(part.CopySourceVersionId) != sdk.ToString(old.VersionId) {
		t.Fatal("copy part source version missing")
	}
	input := &s3.CompleteMultipartUploadInput{Bucket: &f.bucket, Key: sdk.String("multipart"), UploadId: init.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: sdk.Int32(1), ETag: part.CopyPartResult.ETag}}}}
	completed, err := f.c.CompleteMultipartUpload(ctx, input)
	must(t, err)
	retry, err := f.c.CompleteMultipartUpload(ctx, input)
	must(t, err)
	if sdk.ToString(completed.VersionId) == "" || sdk.ToString(retry.VersionId) != sdk.ToString(completed.VersionId) || len(f.versions(t, "multipart").Versions) != 1 || f.read(t, "multipart", "") != "old body" {
		t.Fatal("multipart completion version/retry differs")
	}
	deleted, err := f.c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &f.bucket, Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: sdk.String("copy")}, {Key: sdk.String("source"), VersionId: old.VersionId}}}})
	must(t, err)
	if len(deleted.Deleted) != 2 || !sdk.ToBool(deleted.Deleted[0].DeleteMarker) || sdk.ToString(deleted.Deleted[0].DeleteMarkerVersionId) == "" {
		t.Fatalf("incorrect batch delete version response: %+v", deleted)
	}
}

func TestSDKPrivateACL(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	// ACL mutations require explicit opt-in; new buckets enforce ownership.
	enableACL := func(f versionFixture) {
		_, err := f.c.DeleteBucketOwnershipControls(ctx, &s3.DeleteBucketOwnershipControlsInput{Bucket: &f.bucket})
		must(t, err)
		_, err = f.c.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{Bucket: &f.bucket})
		must(t, err)
	}
	enableACL(f)
	f.state(t, types.BucketVersioningStatusEnabled)
	a := f.put(t, "object", "old")
	f.put(t, "object", "current")
	acl, err := f.c.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: &f.bucket})
	must(t, err)
	expected := sha256.Sum256([]byte(access))
	if sdk.ToString(acl.Owner.ID) != hex.EncodeToString(expected[:]) || len(acl.Grants) != 1 || acl.Grants[0].Grantee.Type != types.TypeCanonicalUser || acl.Grants[0].Permission != types.PermissionFullControl {
		t.Fatalf("bad default ACL: %+v", acl)
	}
	_, err = f.c.PutBucketAcl(ctx, &s3.PutBucketAclInput{Bucket: &f.bucket, ACL: types.BucketCannedACLPrivate})
	must(t, err)
	before, err := f.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &f.bucket, Key: sdk.String("object"), VersionId: a.VersionId})
	must(t, err)
	_, err = f.c.PutObjectAcl(ctx, &s3.PutObjectAclInput{Bucket: &f.bucket, Key: sdk.String("object"), VersionId: a.VersionId, AccessControlPolicy: &types.AccessControlPolicy{Owner: acl.Owner, Grants: []types.Grant{{Grantee: &types.Grantee{Type: types.TypeCanonicalUser, ID: acl.Owner.ID}, Permission: types.PermissionRead}}}})
	must(t, err)
	changed, err := f.c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: &f.bucket, Key: sdk.String("object"), VersionId: a.VersionId})
	must(t, err)
	if len(changed.Grants) != 1 || changed.Grants[0].Permission != types.PermissionRead {
		t.Fatalf("version ACL did not persist: %+v", changed)
	}
	after, err := f.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &f.bucket, Key: sdk.String("object"), VersionId: a.VersionId})
	must(t, err)
	if sdk.ToString(before.ETag) != sdk.ToString(after.ETag) || !before.LastModified.Equal(*after.LastModified) || len(f.versions(t, "object").Versions) != 2 {
		t.Fatal("ACL update changed version or content identity")
	}
	current, err := f.c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: &f.bucket, Key: sdk.String("object")})
	must(t, err)
	if len(current.Grants) != 1 || current.Grants[0].Permission != types.PermissionFullControl {
		t.Fatal("historical ACL changed current ACL")
	}
	_, err = f.c.PutObjectAcl(ctx, &s3.PutObjectAclInput{Bucket: &f.bucket, Key: sdk.String("object"), ACL: types.ObjectCannedACLPublicRead})
	code(t, err, "NotImplemented")
	_, err = f.c.PutBucketAcl(ctx, &s3.PutBucketAclInput{Bucket: &f.bucket, GrantRead: sdk.String(`id="someone-else"`)})
	code(t, err, "NotImplemented")
	_, err = f.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: sdk.String("object"), Body: strings.NewReader("public"), ACL: types.ObjectCannedACLPublicRead})
	code(t, err, "NotImplemented")
	if f.read(t, "object", "") != "current" {
		t.Fatal("rejected public ACL mutated object")
	}
	// Default (unversioned) objects also persist ACL changes without rewriting bytes.
	other := versionSetup(t)
	enableACL(other)
	other.put(t, "plain", "bytes")
	plain, err := other.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &other.bucket, Key: sdk.String("plain")})
	must(t, err)
	_, err = other.c.PutObjectAcl(ctx, &s3.PutObjectAclInput{Bucket: &other.bucket, Key: sdk.String("plain"), ACL: types.ObjectCannedACLPrivate})
	must(t, err)
	final, err := other.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &other.bucket, Key: sdk.String("plain")})
	must(t, err)
	if sdk.ToString(plain.ETag) != sdk.ToString(final.ETag) || !plain.LastModified.Equal(*final.LastModified) {
		t.Fatal("unversioned ACL update changed content identity")
	}
	if endpoint := os.Getenv("GATEWAY_TEST_AZURITE_URL"); endpoint != "" {
		credential, e := azblob.NewSharedKeyCredential("gateway", "Y29tcGF0aWJpbGl0eS10ZXN0LWtleS1vbmx5")
		must(t, e)
		native, e := azblob.NewClientWithSharedKeyCredential(endpoint, credential, nil)
		must(t, e)
		bc := native.ServiceClient().NewContainerClient(other.bucket).NewBlobClient("plain")
		// Native tools can create blobs without Content-MD5. Clear it to exercise
		// metadata updates whose only initial content token is the Azure ETag.
		_, e = bc.SetHTTPHeaders(ctx, blob.HTTPHeaders{BlobContentType: sdk.String("text/plain")}, nil)
		must(t, e)
		properties, e := bc.GetProperties(ctx, nil)
		must(t, e)
		if len(properties.ContentMD5) != 0 {
			t.Fatal("native fixture still has a digest")
		}
		before, e := other.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &other.bucket, Key: sdk.String("plain")})
		must(t, e)
		_, e = other.c.PutObjectAcl(ctx, &s3.PutObjectAclInput{Bucket: &other.bucket, Key: sdk.String("plain"), AccessControlPolicy: &types.AccessControlPolicy{Owner: acl.Owner, Grants: []types.Grant{{Grantee: &types.Grantee{Type: types.TypeCanonicalUser, ID: acl.Owner.ID}, Permission: types.PermissionRead}}}})
		must(t, e)
		after, e := other.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &other.bucket, Key: sdk.String("plain")})
		must(t, e)
		saved, e := other.c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: &other.bucket, Key: sdk.String("plain")})
		must(t, e)
		if sdk.ToString(before.ETag) != sdk.ToString(after.ETag) || !before.LastModified.Equal(*after.LastModified) || len(saved.Grants) != 1 || saved.Grants[0].Permission != types.PermissionRead || other.read(t, "plain", "") != "bytes" {
			t.Fatal("native blob ACL/content identity not preserved")
		}
	}
	// Anonymous reads still require authentication even after ACL APIs are used.
	g, err := gateway.New(other.be, gateway.Options{AccessKey: access, SecretKey: secret})
	must(t, err)
	req, _ := http.NewRequest("GET", "http://gateway.test/"+other.bucket+"/plain", nil)
	res, err := (&transport{handler: g}).RoundTrip(req)
	must(t, err)
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("anonymous access was enabled")
	}
}

func TestSDKVersionedConditionalDelete(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	f.state(t, types.BucketVersioningStatusEnabled)
	old := f.put(t, "key", "old")
	f.put(t, "key", "new")
	_, err := f.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: sdk.String("key"), VersionId: old.VersionId, IfMatch: sdk.String("wrong")})
	code(t, err, "PreconditionFailed")
	response, err := f.c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &f.bucket, Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: sdk.String("key"), VersionId: old.VersionId, ETag: sdk.String("wrong")}}}})
	must(t, err)
	if len(response.Errors) != 1 || sdk.ToString(response.Errors[0].Code) != "PreconditionFailed" {
		t.Fatalf("bad conditional batch response: %+v", response)
	}
	_, err = f.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: sdk.String("key"), VersionId: old.VersionId, IfMatch: old.ETag})
	must(t, err)
	if f.read(t, "key", "") != "new" || len(f.versions(t, "key").Versions) != 1 {
		t.Fatal("conditional delete affected wrong version")
	}
	_, err = f.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: sdk.String("key"), VersionId: old.VersionId, IfMatch: sdk.String("*")})
	must(t, err)
}
