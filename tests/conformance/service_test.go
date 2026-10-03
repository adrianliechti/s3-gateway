package conformance_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const xmlns = "http://s3.amazonaws.com/doc/2006-03-01/"

func isoMillis(s string) bool {
	_, err := time.Parse("2006-01-02T15:04:05.000Z", s)
	return err == nil
}
func otherRegion(region string) string {
	if region == "us-east-1" {
		return "eu-west-1"
	}
	return "us-east-1"
}

func TestConformanceErrors(t *testing.T) {
	parallel(t)
	h := begin(t, "errors")
	missing := "s3gw-conf-missing-" + randomHex(4)
	res := h.do("get-missing-bucket", request{method: "GET", bucket: missing})
	h.code(res, 404, "NoSuchBucket", "ErrorResponses")
	h.headerIs(res, "Content-Type", "application/xml", "ErrorResponses")
	h.present(res, "Error.RequestId", true, "ErrorResponses")
	h.eq(res, "header:x-amz-request-id?", res.header("x-amz-request-id") != "", true, "RESTCommonResponseHeaders")
	h.eq(res, "header:x-amz-id-2?", res.header("x-amz-id-2") != "", true, "RESTCommonResponseHeaders")
	h.note(res, "xml:Error.HostId?", res.has("Error.HostId"))
	h.note(res, "xml:Error.BucketName?", res.has("Error.BucketName"))
	h.note(res, "xml:Error.Resource?", res.has("Error.Resource"))
	res = h.do("head-missing-bucket", request{method: "HEAD", bucket: missing})
	h.status(res, 404, "HeadBucket")
	h.eq(res, "body-length", len(res.Body), 0, "HeadBucket")
	res = h.get("get-missing-key", "missing", nil, nil)
	h.code(res, 404, "NoSuchKey", "GetObject")
	h.note(res, "xml:Error.Key?", res.has("Error.Key"))
	res = h.head("head-missing-key", "missing", nil, nil)
	h.status(res, 404, "HeadObject")
	h.eq(res, "body-length", len(res.Body), 0, "HeadObject")
	// Deleting an absent key is idempotent.
	res = h.delete("delete-missing-key", "missing", nil, nil)
	h.status(res, 204, "DeleteObject")
	res = h.do("delete-missing-bucket", request{method: "DELETE", bucket: missing})
	h.code(res, 404, "NoSuchBucket", "DeleteBucket")
	res = h.do("list-missing-bucket", request{method: "GET", bucket: missing, query: q("list-type", "2")})
	h.code(res, 404, "NoSuchBucket", "ListObjectsV2")
	// Outside us-east-1 the location constraint is validated before the name,
	// so CreateBucket carries the endpoint's region.
	var constraint []byte
	if h.tg.region != "us-east-1" {
		constraint = []byte(fmt.Sprintf(`<CreateBucketConfiguration xmlns="%s"><LocationConstraint>%s</LocationConstraint></CreateBucketConfiguration>`, xmlns, h.tg.region))
	}
	for _, name := range []string{"ab", "Upper-Case", "under_score", "192.168.1.1", "trailing-", "-leading", "double..dot", strings.Repeat("a", 64)} {
		res = h.do("invalid-bucket-name:"+name, request{method: "PUT", bucket: name, body: constraint})
		h.code(res, 400, "InvalidBucketName", "bucketnaming")
	}
	// Reserved prefixes and suffixes are checked after authorization; recorded only.
	for _, name := range []string{"xn--punycode", "name-s3alias"} {
		res = h.do("reserved-bucket-name:"+name, request{method: "PUT", bucket: name, body: constraint})
		if res.Status == 200 {
			h.buckets = append(h.buckets, name)
			t.Cleanup(func() { h.removeBucket(name) })
		}
	}
	// Keys may use up to 1024 bytes; components stay within file name limits.
	long := strings.Repeat(strings.Repeat("k", 250)+"/", 4) + strings.Repeat("k", 20)
	h.status(h.put("key-1024-bytes", long, []byte("x"), nil), 200, "object-keys")
	res = h.put("key-1025-bytes", long+"x", []byte("x"), nil)
	h.code(res, 400, "KeyTooLongError", "object-keys")
	res = h.do("unsupported-method", request{method: "PATCH", bucket: h.bucket, key: "missing"})
	h.code(res, 405, "MethodNotAllowed", "ErrorResponses")
}

func TestConformanceAuthentication(t *testing.T) {
	parallel(t)
	h := begin(t, "auth")
	h.seed("key", []byte("content"), nil)
	object := func(method string, r request) request {
		r.method, r.bucket, r.key = method, h.bucket, "key"
		return r
	}
	res := h.do("anonymous-get", object("GET", request{anonymous: true}))
	h.code(res, 403, "AccessDenied", "sigv4")
	res = h.do("anonymous-head", object("HEAD", request{anonymous: true}))
	h.status(res, 403, "sigv4")
	res = h.do("anonymous-put", request{method: "PUT", bucket: h.bucket, key: "anonymous", body: []byte("x"), anonymous: true})
	h.code(res, 403, "AccessDenied", "sigv4")
	res = h.do("wrong-secret", object("GET", request{secret: "wrong-secret"}))
	h.code(res, 403, "SignatureDoesNotMatch", "sigv4")
	res = h.do("unknown-access-key", object("GET", request{accessKey: "AKIAUNKNOWNKEY000000"}))
	h.code(res, 403, "InvalidAccessKeyId", "sigv4")
	res = h.do("clock-skew", object("GET", request{at: time.Now().Add(-20 * time.Minute)}))
	h.code(res, 403, "RequestTimeTooSkewed", "sigv4")
	res = h.do("wrong-region-scope", object("GET", request{region: otherRegion(h.tg.region)}))
	h.code(res, 400, "AuthorizationHeaderMalformed", "sigv4")
	res = h.do("presigned-get", object("GET", request{presign: time.Minute}))
	h.status(res, 200, "sigv4-query")
	h.bodyIs(res, "content", "sigv4-query")
	res = h.do("presigned-expired", object("GET", request{presign: time.Minute, at: time.Now().Add(-2 * time.Hour)}))
	h.code(res, 403, "AccessDenied", "sigv4-query")
	res = h.do("presigned-too-long", object("GET", request{presign: 604801 * time.Second}))
	h.code(res, 400, "AuthorizationQueryParametersError", "sigv4-query")
	res = h.do("presigned-plus-header", object("GET", request{presign: time.Minute, header: hdr("Authorization", "AWS4-HMAC-SHA256 Credential=AKIA/20200101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=0000")}))
	h.code(res, 400, "InvalidArgument", "sigv4-query")
	if got := h.raw(object("GET", request{})); string(got.Body) != "content" {
		t.Fatalf("rejected requests changed the object: %q", got.Body)
	}
}

func TestConformanceBuckets(t *testing.T) {
	parallel(t)
	h := begin(t, "buckets")
	var res *response
	if !h.fixed("create-again") {
		res = h.do("create-again", request{method: "PUT", bucket: h.bucket})
		if h.tg.region == "us-east-1" {
			// Legacy us-east-1 behavior: recreating your own bucket succeeds.
			h.status(res, 200, "CreateBucket")
		} else {
			h.code(res, 409, "BucketAlreadyOwnedByYou", "CreateBucket")
		}
	}
	res = h.do("head", request{method: "HEAD", bucket: h.bucket})
	h.status(res, 200, "HeadBucket")
	h.headerIs(res, "x-amz-bucket-region", h.tg.region, "HeadBucket")
	res = h.do("location", request{method: "GET", bucket: h.bucket, query: q("location", "")})
	h.status(res, 200, "GetBucketLocation")
	want := h.tg.region
	if want == "us-east-1" {
		want = ""
	}
	h.xmlIs(res, "LocationConstraint", want, "GetBucketLocation")
	h.xmlIs(res, "LocationConstraint.@xmlns", xmlns, "GetBucketLocation")
	if !h.fixed("list-buckets") {
		res = h.do("list-buckets", request{method: "GET", query: q("prefix", h.bucket, "max-buckets", "1")})
		h.status(res, 200, "ListBuckets")
		h.xmlIs(res, "ListAllMyBucketsResult.Buckets.Bucket[0].Name", h.bucket, "ListBuckets")
		h.present(res, "ListAllMyBucketsResult.Owner.ID", true, "ListBuckets")
		h.eq(res, "creation-date-format", isoMillis(res.xml("ListAllMyBucketsResult.Buckets.Bucket[0].CreationDate")), true, "ListBuckets")
		h.xmlIs(res, "ListAllMyBucketsResult.Prefix", h.bucket, "ListBuckets")
		h.present(res, "ListAllMyBucketsResult.ContinuationToken", false, "ListBuckets")
		h.note(res, "xml:ListAllMyBucketsResult.Buckets.Bucket[0].BucketRegion", res.xml("ListAllMyBucketsResult.Buckets.Bucket[0].BucketRegion"))
		res = h.do("list-buckets-all", request{method: "GET"})
		h.status(res, 200, "ListBuckets")
		h.present(res, "ListAllMyBucketsResult.Buckets", true, "ListBuckets")
	}
	h.seed("object", []byte("x"), nil)
	if !h.fixed("delete-nonempty") {
		res = h.do("delete-nonempty", request{method: "DELETE", bucket: h.bucket})
		h.code(res, 409, "BucketNotEmpty", "DeleteBucket")
	}
	constraint := func(region string) []byte {
		return []byte(fmt.Sprintf(`<CreateBucketConfiguration xmlns="%s"><LocationConstraint>%s</LocationConstraint></CreateBucketConfiguration>`, xmlns, region))
	}
	for _, tc := range []struct{ step, region, code string }{{"create-other-region", otherRegion(h.tg.region), "IllegalLocationConstraintException"}, {"create-invalid-region", "mars-north-1", "InvalidLocationConstraint"}} {
		if h.fixed(tc.step) {
			continue
		}
		name := "s3gw-conf-" + randomHex(6)
		res = h.do(tc.step, request{method: "PUT", bucket: name, body: constraint(tc.region)})
		if res.Status == 200 {
			h.buckets = append(h.buckets, name)
			t.Cleanup(func() { h.removeBucket(name) })
		}
		h.code(res, 400, tc.code, "CreateBucket")
	}
}

// New buckets carry documented defaults: Block Public Access on, ACLs
// disabled through BucketOwnerEnforced ownership, SSE-S3 encryption, and
// explicit "not found" codes for absent configurations.
func TestConformanceBucketDefaults(t *testing.T) {
	parallel(t)
	h := begin(t, "bucket-defaults")
	get := func(step, sub string) *response {
		return h.do(step, request{method: "GET", bucket: h.bucket, query: q(sub, "")})
	}
	if h.tg.bucket != "" {
		// An existing bucket carries its owner's configuration, not defaults.
		t.Logf("recording configuration of %s without asserting new-bucket defaults", h.bucket)
		for _, sub := range []string{"tagging", "cors", "lifecycle", "publicAccessBlock", "ownershipControls", "acl", "notification", "encryption", "policy", "requestPayment", "accelerate", "logging", "website", "replication", "object-lock", "policyStatus"} {
			get(sub, sub)
		}
		return
	}
	h.code(get("tagging", "tagging"), 404, "NoSuchTagSet", "GetBucketTagging")
	h.code(get("cors", "cors"), 404, "NoSuchCORSConfiguration", "GetBucketCors")
	h.code(get("lifecycle", "lifecycle"), 404, "NoSuchLifecycleConfiguration", "GetBucketLifecycleConfiguration")
	res := get("public-access-block", "publicAccessBlock")
	h.status(res, 200, "block-public-access")
	for _, f := range []string{"BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets"} {
		h.xmlIs(res, "PublicAccessBlockConfiguration."+f, "true", "block-public-access")
	}
	res = get("ownership-controls", "ownershipControls")
	h.status(res, 200, "object-ownership")
	h.xmlIs(res, "OwnershipControls.Rule.ObjectOwnership", "BucketOwnerEnforced", "object-ownership")
	res = get("acl", "acl")
	h.status(res, 200, "GetBucketAcl")
	h.xmlIs(res, "AccessControlPolicy.AccessControlList.Grant.Permission", "FULL_CONTROL", "GetBucketAcl")
	h.xmlIs(res, "AccessControlPolicy.AccessControlList.Grant.Grantee.@type", "CanonicalUser", "GetBucketAcl")
	h.eq(res, "owner-is-grantee", res.xml("AccessControlPolicy.Owner.ID") != "" && res.xml("AccessControlPolicy.Owner.ID") == res.xml("AccessControlPolicy.AccessControlList.Grant.Grantee.ID"), true, "GetBucketAcl")
	h.present(res, "AccessControlPolicy.AccessControlList.Grant[1]", false, "GetBucketAcl")
	res = get("notification", "notification")
	h.status(res, 200, "GetBucketNotificationConfiguration")
	h.present(res, "NotificationConfiguration", true, "GetBucketNotificationConfiguration")
	res = get("encryption", "encryption")
	h.status(res, 200, "default-encryption")
	h.xmlIs(res, "ServerSideEncryptionConfiguration.Rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm", "AES256", "default-encryption")
	h.code(get("policy", "policy"), 404, "NoSuchBucketPolicy", "GetBucketPolicy")
	res = get("request-payment", "requestPayment")
	h.status(res, 200, "GetBucketRequestPayment")
	h.xmlIs(res, "RequestPaymentConfiguration.Payer", "BucketOwner", "GetBucketRequestPayment")
	res = get("accelerate", "accelerate")
	h.status(res, 200, "GetBucketAccelerateConfiguration")
	h.present(res, "AccelerateConfiguration.Status", false, "GetBucketAccelerateConfiguration")
	res = get("logging", "logging")
	h.status(res, 200, "GetBucketLogging")
	h.present(res, "BucketLoggingStatus.LoggingEnabled", false, "GetBucketLogging")
	h.code(get("website", "website"), 404, "NoSuchWebsiteConfiguration", "GetBucketWebsite")
	h.code(get("replication", "replication"), 404, "ReplicationConfigurationNotFoundError", "GetBucketReplication")
	h.code(get("object-lock", "object-lock"), 404, "ObjectLockConfigurationNotFoundError", "GetObjectLockConfiguration")
	h.code(get("policy-status", "policyStatus"), 404, "NoSuchBucketPolicy", "GetBucketPolicyStatus")
}
