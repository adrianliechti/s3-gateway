package gateway_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/gateway"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestSDKCompatibilityProtocolPolish(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	extra := f.bucket + "-next"
	_, err := f.c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &extra})
	must(t, err)
	t.Cleanup(func() {
		_, e := f.c.DeleteBucket(context.Background(), &s3.DeleteBucketInput{Bucket: &extra})
		must(t, e)
	})
	page, err := f.c.ListBuckets(ctx, &s3.ListBucketsInput{Prefix: &f.bucket, MaxBuckets: aws.Int32(1)})
	must(t, err)
	if len(page.Buckets) != 1 || aws.ToString(page.Buckets[0].Name) != f.bucket || page.ContinuationToken == nil {
		t.Fatalf("bad first page: %+v", page)
	}
	next, err := f.c.ListBuckets(ctx, &s3.ListBucketsInput{Prefix: &f.bucket, MaxBuckets: aws.Int32(1), ContinuationToken: page.ContinuationToken})
	must(t, err)
	if len(next.Buckets) != 1 || aws.ToString(next.Buckets[0].Name) != extra || next.ContinuationToken != nil {
		t.Fatalf("bad next page: %+v", next)
	}
	_, err = f.c.ListBuckets(ctx, &s3.ListBucketsInput{Prefix: aws.String("different"), ContinuationToken: page.ContinuationToken})
	code(t, err, "InvalidArgument")
	key := "metadata"
	value := "Hello é 世界"
	_, err = f.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: strings.NewReader("payload"), Metadata: map[string]string{"unicode": mime.BEncoding.Encode("UTF-8", value)}, ContentEncoding: aws.String("deflate, gzip"), ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
	must(t, err)
	h, err := f.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &f.bucket, Key: &key})
	must(t, err)
	decoded, err := new(mime.WordDecoder).DecodeHeader(h.Metadata["unicode"])
	must(t, err)
	if decoded != value || aws.ToString(h.ContentEncoding) != "deflate, gzip" {
		t.Fatalf("metadata changed: %+v", h)
	}
	_, err = f.c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &f.bucket, Key: &key, CopySource: aws.String(f.bucket + "/" + key)})
	code(t, err, "InvalidRequest")
	cp, err := f.c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &f.bucket, Key: aws.String("copied"), CopySource: aws.String(f.bucket + "/" + key), ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
	must(t, err)
	sum := sha256.Sum256([]byte("payload"))
	want := base64.StdEncoding.EncodeToString(sum[:])
	if cp.CopyObjectResult == nil || aws.ToString(cp.CopyObjectResult.ChecksumSHA256) != want {
		t.Fatalf("copy checksum missing: %+v", cp)
	}
	h, err = f.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &f.bucket, Key: aws.String("copied"), ChecksumMode: types.ChecksumModeEnabled})
	must(t, err)
	if aws.ToString(h.ChecksumSHA256) != want {
		t.Fatal("copy checksum not persisted")
	}
}

func TestSDKCompatibilityBucketConfiguration(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	_, err := f.c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: &f.bucket})
	code(t, err, "NoSuchTagSet")
	var tags []types.Tag
	for i := 0; i < 50; i++ {
		tags = append(tags, types.Tag{Key: aws.String(fmt.Sprintf("key%02d", i)), Value: aws.String(strings.Repeat("value", 40))})
	}
	_, err = f.c.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: &f.bucket, Tagging: &types.Tagging{TagSet: tags}})
	must(t, err)
	cors := &types.CORSConfiguration{CORSRules: []types.CORSRule{{AllowedMethods: []string{"GET", "PUT"}, AllowedOrigins: []string{"https://*.example.test"}, AllowedHeaders: []string{"x-amz-*", "content-type"}, ExposeHeaders: []string{"ETag"}, MaxAgeSeconds: aws.Int32(600)}}}
	_, err = f.c.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: &f.bucket, CORSConfiguration: cors})
	must(t, err)
	pab := &types.PublicAccessBlockConfiguration{BlockPublicAcls: aws.Bool(true), IgnorePublicAcls: aws.Bool(true), BlockPublicPolicy: aws.Bool(true), RestrictPublicBuckets: aws.Bool(true)}
	_, err = f.c.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{Bucket: &f.bucket, PublicAccessBlockConfiguration: pab})
	must(t, err)
	_, err = f.c.PutBucketOwnershipControls(ctx, &s3.PutBucketOwnershipControlsInput{Bucket: &f.bucket, OwnershipControls: &types.OwnershipControls{Rules: []types.OwnershipControlsRule{{ObjectOwnership: types.ObjectOwnershipBucketOwnerPreferred}}}})
	must(t, err)
	g, err := gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret})
	must(t, err)
	c := client(&transport{handler: g})
	out, err := c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: &f.bucket})
	must(t, err)
	if !reflect.DeepEqual(out.TagSet, tags) {
		t.Fatal("bucket tags lost on restart")
	}
	co, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: &f.bucket})
	must(t, err)
	if !reflect.DeepEqual(co.CORSRules, cors.CORSRules) {
		t.Fatalf("CORS changed: %+v", co)
	}
	po, err := c.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: &f.bucket})
	must(t, err)
	if !reflect.DeepEqual(po.PublicAccessBlockConfiguration, pab) {
		t.Fatal("public access block changed")
	}
	own, err := c.GetBucketOwnershipControls(ctx, &s3.GetBucketOwnershipControlsInput{Bucket: &f.bucket})
	must(t, err)
	if own.OwnershipControls.Rules[0].ObjectOwnership != types.ObjectOwnershipBucketOwnerPreferred {
		t.Fatal("ownership lost")
	}
	listed, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &f.bucket})
	must(t, err)
	if len(listed.Contents) != 0 {
		t.Fatal("private settings leaked into listing")
	}
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: aws.String("public"), ACL: types.ObjectCannedACLPublicRead})
	code(t, err, "AccessDenied")
	_, err = c.DeleteBucketCors(ctx, &s3.DeleteBucketCorsInput{Bucket: &f.bucket})
	must(t, err)
	_, err = c.DeleteBucketTagging(ctx, &s3.DeleteBucketTaggingInput{Bucket: &f.bucket})
	must(t, err)
	_, err = c.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{Bucket: &f.bucket})
	must(t, err)
	_, err = c.DeleteBucketOwnershipControls(ctx, &s3.DeleteBucketOwnershipControlsInput{Bucket: &f.bucket})
	must(t, err)
	_, err = c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: &f.bucket})
	code(t, err, "NoSuchCORSConfiguration")
	_, err = c.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: &f.bucket})
	code(t, err, "NoSuchPublicAccessBlockConfiguration")
	_, err = c.GetBucketOwnershipControls(ctx, &s3.GetBucketOwnershipControlsInput{Bucket: &f.bucket})
	code(t, err, "OwnershipControlsNotFoundError")
}

func TestSDKCompatibilityCORS(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	key := "cors"
	f.put(t, key, "private")
	_, err := f.c.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: &f.bucket, CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{{AllowedMethods: []string{"GET", "PUT"}, AllowedOrigins: []string{"https://*.example.test"}, AllowedHeaders: []string{"x-amz-*"}, ExposeHeaders: []string{"ETag"}}}}})
	must(t, err)
	for _, tc := range []struct {
		method, origin, headers string
		status                  int
	}{{"PUT", "https://app.example.test", "X-Amz-Meta-Test", 200}, {"DELETE", "https://app.example.test", "", 403}, {"PUT", "https://other.test", "", 403}, {"PUT", "https://app.example.test", "X-Other", 403}} {
		req, _ := http.NewRequestWithContext(ctx, "OPTIONS", "http://gateway.test/"+f.bucket+"/"+key, nil)
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Access-Control-Request-Method", tc.method)
		req.Header.Set("Access-Control-Request-Headers", tc.headers)
		res, e := f.c.Options().HTTPClient.Do(req)
		must(t, e)
		res.Body.Close()
		if res.StatusCode != tc.status || tc.status == 200 && res.Header.Get("Access-Control-Allow-Origin") != tc.origin {
			t.Fatalf("bad preflight: %d %v", res.StatusCode, res.Header)
		}
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://gateway.test/"+f.bucket+"/"+key, nil)
	req.Header.Set("Origin", "https://app.example.test")
	res, err := f.c.Options().HTTPClient.Do(req)
	must(t, err)
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("CORS granted anonymous access")
	}
	signed, err := s3.NewPresignClient(f.c).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: &key})
	must(t, err)
	req, _ = http.NewRequestWithContext(ctx, "GET", signed.URL, nil)
	req.Header.Set("Origin", "https://app.example.test")
	res, err = f.c.Options().HTTPClient.Do(req)
	must(t, err)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Access-Control-Allow-Origin") != "https://app.example.test" {
		t.Fatal("signed CORS GET failed")
	}
}

func TestSDKCompatibilityOwnership(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	key := "owned"
	_, err := f.c.PutBucketOwnershipControls(ctx, &s3.PutBucketOwnershipControlsInput{Bucket: &f.bucket, OwnershipControls: &types.OwnershipControls{Rules: []types.OwnershipControlsRule{{ObjectOwnership: types.ObjectOwnershipBucketOwnerEnforced}}}})
	must(t, err)
	_, err = f.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: strings.NewReader("private"), ACL: types.ObjectCannedACLPrivate})
	must(t, err) // AWS accepts a private canned ACL on PutObject under enforced ownership.
	_, err = f.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: strings.NewReader("owned"), ACL: types.ObjectCannedACLBucketOwnerFullControl})
	must(t, err)
	_, err = f.c.PutObjectAcl(ctx, &s3.PutObjectAclInput{Bucket: &f.bucket, Key: &key, ACL: types.ObjectCannedACLPrivate})
	code(t, err, "AccessControlListNotSupported")
	_, err = f.c.PutBucketAcl(ctx, &s3.PutBucketAclInput{Bucket: &f.bucket, ACL: types.BucketCannedACLPrivate})
	code(t, err, "AccessControlListNotSupported")
	acl, err := f.c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: &f.bucket, Key: &key})
	must(t, err)
	if len(acl.Grants) != 1 || acl.Grants[0].Permission != types.PermissionFullControl || aws.ToString(acl.Grants[0].Grantee.ID) != aws.ToString(acl.Owner.ID) {
		t.Fatal("enforced owner ACL is incorrect")
	}
	obj, err := f.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: &key})
	must(t, err)
	_, err = io.ReadAll(obj.Body)
	obj.Body.Close()
	must(t, err)
}

func lifecycleGateway(t *testing.T, f versionFixture) *gateway.Gateway {
	t.Helper()
	g, err := gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret})
	must(t, err)
	return g
}
func lifecyclePut(t *testing.T, f versionFixture, rules ...types.LifecycleRule) {
	t.Helper()
	_, err := f.c.PutBucketLifecycleConfiguration(t.Context(), &s3.PutBucketLifecycleConfigurationInput{Bucket: &f.bucket, LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: rules}})
	must(t, err)
}

func TestSDKCompatibilityLifecycle(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	_, err := f.c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: &f.bucket})
	code(t, err, "NoSuchLifecycleConfiguration")
	lifecyclePut(t, f, types.LifecycleRule{ID: aws.String("tagged"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{Prefix: aws.String("expire/"), Tags: []types.Tag{{Key: aws.String("temporary"), Value: aws.String("yes")}}, ObjectSizeGreaterThan: aws.Int64(2), ObjectSizeLessThan: aws.Int64(10)}}, Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)}})
	for _, key := range []string{"expire/selected", "keep/prefix", "expire/tag", "expire/small"} {
		body := "payload"
		tag := "temporary=yes"
		if key == "expire/small" {
			body = "xx"
		}
		if key == "expire/tag" {
			tag = "temporary=no"
		}
		out, e := f.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, Body: strings.NewReader(body), Tagging: &tag})
		must(t, e)
		if (out.Expiration != nil) != (key == "expire/selected") {
			t.Fatalf("incorrect expiration header for %s", key)
		}
	}
	g := lifecycleGateway(t, f)
	must(t, g.RunLifecycleOnce(ctx, time.Now()))
	if f.read(t, "expire/selected", "") != "payload" {
		t.Fatal("object expired prematurely")
	}
	must(t, g.RunLifecycleOnce(ctx, time.Now().Add(72*time.Hour)))
	_, err = f.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &f.bucket, Key: aws.String("expire/selected")})
	if err == nil {
		t.Fatal("eligible object survived")
	}
	for _, key := range []string{"keep/prefix", "expire/tag", "expire/small"} {
		if f.read(t, key, "") == "" {
			t.Fatal("unmatched object removed")
		}
	}
	_, err = f.c.DeleteBucketLifecycle(ctx, &s3.DeleteBucketLifecycleInput{Bucket: &f.bucket})
	must(t, err)
	_, err = f.c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: &f.bucket})
	code(t, err, "NoSuchLifecycleConfiguration")
}

func TestSDKCompatibilityLifecycleVersions(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	f.state(t, types.BucketVersioningStatusEnabled)
	for i := 0; i < 5; i++ {
		f.put(t, "history", fmt.Sprint(i))
	}
	lifecyclePut(t, f, types.LifecycleRule{ID: aws.String("history"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")}, NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(1), NewerNoncurrentVersions: aws.Int32(2)}})
	g := lifecycleGateway(t, f)
	future := time.Now().Add(72 * time.Hour)
	must(t, g.RunLifecycleOnce(ctx, future))
	if v := f.versions(t, "history"); len(v.Versions) != 3 {
		t.Fatalf("retention count: %d", len(v.Versions))
	}
	if f.read(t, "history", "") != "4" {
		t.Fatal("current version was deleted")
	}
	lifecyclePut(t, f, types.LifecycleRule{ID: aws.String("all"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")}, Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)}, NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(1)}})
	must(t, g.RunLifecycleOnce(ctx, future))
	v := f.versions(t, "history")
	if len(v.Versions) != 1 || len(v.DeleteMarkers) != 1 {
		t.Fatalf("expiration did not preserve current history: %+v", v)
	}
	must(t, g.RunLifecycleOnce(ctx, future.Add(72*time.Hour)))
	v = f.versions(t, "history")
	if len(v.Versions)+len(v.DeleteMarkers) != 0 {
		t.Fatal("expired history/delete marker survived")
	}
	// Suspended expiration removes null content but retains numbered versions.
	_, err := f.c.DeleteBucketLifecycle(ctx, &s3.DeleteBucketLifecycleInput{Bucket: &f.bucket})
	must(t, err)
	f.put(t, "suspended", "numbered")
	f.state(t, types.BucketVersioningStatusSuspended)
	f.put(t, "suspended", "null")
	lifecyclePut(t, f, types.LifecycleRule{ID: aws.String("current"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")}, Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)}})
	must(t, g.RunLifecycleOnce(ctx, future))
	v = f.versions(t, "suspended")
	if len(v.Versions) != 1 || len(v.DeleteMarkers) != 1 || aws.ToString(v.DeleteMarkers[0].VersionId) != "null" {
		t.Fatalf("suspended expiration: %+v", v)
	}
}

func TestSDKCompatibilityLifecycleUploads(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	key := "abort/pending"
	lifecyclePut(t, f, types.LifecycleRule{ID: aws.String("abort"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("abort/")}, AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(1)}})
	init, err := f.c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &f.bucket, Key: &key})
	must(t, err)
	_, err = f.c.UploadPart(ctx, &s3.UploadPartInput{Bucket: &f.bucket, Key: &key, UploadId: init.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("pending")})
	must(t, err)
	keep, err := f.c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &f.bucket, Key: aws.String("keep")})
	must(t, err)
	g := lifecycleGateway(t, f)
	future := time.Now().Add(72 * time.Hour)
	ro, err := gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret, ReadOnly: true})
	must(t, err)
	must(t, ro.RunLifecycleOnce(ctx, future))
	_, err = f.c.ListParts(ctx, &s3.ListPartsInput{Bucket: &f.bucket, Key: &key, UploadId: init.UploadId})
	must(t, err)
	must(t, g.RunLifecycleOnce(ctx, future))
	_, err = f.c.ListParts(ctx, &s3.ListPartsInput{Bucket: &f.bucket, Key: &key, UploadId: init.UploadId})
	code(t, err, "NoSuchUpload")
	_, err = f.c.ListParts(ctx, &s3.ListPartsInput{Bucket: &f.bucket, Key: aws.String("keep"), UploadId: keep.UploadId})
	must(t, err)
	_, err = f.c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &f.bucket, Key: aws.String("keep"), UploadId: keep.UploadId})
	must(t, err)
}

func TestSDKCompatibilityLifecycleMarkerAge(t *testing.T) {
	f := versionSetup(t)
	ctx := t.Context()
	f.state(t, types.BucketVersioningStatusEnabled)
	old := f.put(t, "key", "content")
	_, err := f.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: aws.String("key")})
	must(t, err)
	_, err = f.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &f.bucket, Key: aws.String("key"), VersionId: old.VersionId})
	must(t, err)
	lifecyclePut(t, f, types.LifecycleRule{ID: aws.String("marker-age"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")}, Expiration: &types.LifecycleExpiration{Days: aws.Int32(5)}})
	g := lifecycleGateway(t, f)
	must(t, g.RunLifecycleOnce(ctx, time.Now().Add(48*time.Hour)))
	if v := f.versions(t, "key"); len(v.DeleteMarkers) != 1 {
		t.Fatal("marker expired before its deadline")
	}
	must(t, g.RunLifecycleOnce(ctx, time.Now().Add(7*24*time.Hour)))
	if v := f.versions(t, "key"); len(v.DeleteMarkers) != 0 {
		t.Fatal("old marker survived its deadline")
	}
}
