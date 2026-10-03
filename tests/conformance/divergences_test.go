package conformance_test

import (
	"regexp"
	"strings"
)

// ignoredBaselineField hides fields that cannot be compared between two
// targets: DisplayName appears only in some AWS regions, and version fields
// are meaningless when one target's bucket was versioned and the other's not.
var versionField = regexp.MustCompile(`x-amz-version-id|x-amz-copy-source-version-id|x-amz-delete-marker|VersionId|DeleteMarker|IsLatest|ListVersionsResult\.Version\[`)

func ignoredBaselineField(field string, versioningDiffers bool) bool {
	if strings.HasSuffix(field, ".DisplayName[0]") {
		return true
	}
	return versioningDiffers && versionField.MatchString(field)
}

// Baseline comparisons touch many steps with the same cause; these patterns
// classify them once. Matching is on "scenario/step#field" plus the values.
var knownBaselinePatterns = []struct {
	key, got, want *regexp.Regexp
	reason         string
}{
	{regexp.MustCompile(`#header:content-type$`), regexp.MustCompile(`^(application/xml)?$`), regexp.MustCompile(`^(application/xml)?$`),
		"note: AWS omits Content-Type on several XML responses (initiate, tagging, versioning, attributes) and sends it on HEAD bucket"},
	{regexp.MustCompile(`#note:xml:Error\.(HostId|BucketName|Key|Resource)\?$`), nil, nil,
		"note: AWS error documents carry HostId plus Key or BucketName; the gateway carries Resource"},
	{regexp.MustCompile(`#note:header:x-amz-server-side-encryption$`), nil, nil,
		"note: AWS reports its default SSE-S3 encryption on every write"},
	{regexp.MustCompile(`^multipart/(complete|crc32-complete|complete-tiny-single|complete-again)#note:xml:CompleteMultipartUploadResult\.Location`), nil, nil,
		"note: CompleteMultipartUpload on AWS returns an absolute Location URL; the gateway uses a path"},
	{regexp.MustCompile(`^multipart/complete-again#`), nil, nil,
		"note: the gateway answers a repeated CompleteMultipartUpload with the original result; AWS has no upload left to complete"},
	{regexp.MustCompile(`^batch-delete/[^#]+#xml:DeleteResult\.(Deleted|Error)\[`), nil, nil,
		"note: DeleteObjects result entries are unordered; the expectations look them up by key"},
	{regexp.MustCompile(`^buckets/location#xml:LocationConstraint$`), nil, nil,
		"note: the two targets run in different regions"},
	{regexp.MustCompile(`^acl/listing-storage-class#`), nil, nil,
		"design: STANDARD is the only accepted storage class, so the STANDARD_IA object was never stored"},
	{regexp.MustCompile(`^multipart/copy-part[^#]*#header:etag$`), nil, nil,
		"note: UploadPartCopy on AWS returns the ETag only in the XML body"},
	{regexp.MustCompile(`^multipart/abort-again#`), nil, nil,
		"note: AWS answers 204 to a repeated abort of a known upload id; the gateway answers 404 NoSuchUpload as documented"},
	{regexp.MustCompile(`^multipart/initiate-invalid-algorithm#`), nil, nil,
		"note: AWS accepted and echoed an unknown x-amz-checksum-algorithm value; the gateway rejects it"},
	{regexp.MustCompile(`^(acl/(storage-|sse-|object-lock)|bucket-defaults/(accelerate|encryption|logging|request-payment|policy|website|replication|object-lock|policy-status))`), nil, nil,
		"design: features the gateway rejects with NotImplemented or InvalidStorageClass; AWS implements them"},
	{regexp.MustCompile(`^errors/reserved-bucket-name:`), nil, nil,
		"note: reserved bucket suffixes are checked by AWS after authorization; the IAM user may not create buckets"},
	{regexp.MustCompile(`^ranges/range-(start-at-size|beyond-size)#note:header:content-range$`), nil, nil,
		"note: AWS sends no Content-Range on 416; the gateway sends bytes */size as RFC 9110 suggests"},
	{regexp.MustCompile(`^copy/copy-self-unchanged#`), nil, nil,
		"note: AWS accepted an unchanged self-copy in a versioned, SSE-S3 bucket; the gateway rejects it as documented"},
	{regexp.MustCompile(`^acl/(sse-invalid|object-lock-header)#(note:)?code$`), nil, nil,
		"design: encryption and Object Lock headers answer NotImplemented; AWS validates them (InvalidArgument/InvalidRequest)"},
	{regexp.MustCompile(`^batch-delete/delete-reserved-key#`), nil, nil,
		"design: .gateway/ is a reserved prefix on the gateway and is refused; AWS treats it as an ordinary key"},
}

func knownBaselinePattern(key, got, want string) (string, bool) {
	for _, p := range knownBaselinePatterns {
		if p.key.MatchString(key) && (p.got == nil || p.got.MatchString(got)) && (p.want == nil || p.want.MatchString(want)) {
			return p.reason, true
		}
	}
	return "", false
}

// docs resolves short references used in expectations to the AWS pages that
// document the behavior. API operation names resolve automatically.
var docs = map[string]string{
	"ErrorResponses":            "https://docs.aws.amazon.com/AmazonS3/latest/API/ErrorResponses.html",
	"RESTCommonResponseHeaders": "https://docs.aws.amazon.com/AmazonS3/latest/API/RESTCommonResponseHeaders.html",
	"sigv4":                     "https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-authenticating-requests.html",
	"sigv4-query":               "https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-query-string-auth.html",
	"bucketnaming":              "https://docs.aws.amazon.com/AmazonS3/latest/userguide/bucketnamingrules.html",
	"object-keys":               "https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-keys.html",
	"UsingMetadata":             "https://docs.aws.amazon.com/AmazonS3/latest/userguide/UsingMetadata.html",
	"checksums":                 "https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity.html",
	"conditional-requests":      "https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-requests.html",
	"conditional-writes":        "https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html",
	"versioning":                "https://docs.aws.amazon.com/AmazonS3/latest/userguide/Versioning.html",
	"delete-markers":            "https://docs.aws.amazon.com/AmazonS3/latest/userguide/DeleteMarker.html",
	"suspended":                 "https://docs.aws.amazon.com/AmazonS3/latest/userguide/AddingObjectstoVersionSuspendedBuckets.html",
	"mpu-limits":                "https://docs.aws.amazon.com/AmazonS3/latest/userguide/qfacts.html",
	"tagging":                   "https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-tagging.html",
	"object-ownership":          "https://docs.aws.amazon.com/AmazonS3/latest/userguide/about-object-ownership.html",
	"block-public-access":       "https://docs.aws.amazon.com/AmazonS3/latest/userguide/access-control-block-public-access.html",
	"default-encryption":        "https://docs.aws.amazon.com/AmazonS3/latest/userguide/bucket-encryption.html",
	"storage-classes":           "https://docs.aws.amazon.com/AmazonS3/latest/userguide/storage-class-intro.html",
	"listing":                   "https://docs.aws.amazon.com/AmazonS3/latest/userguide/ListingKeysUsingAPIs.html",
	"range":                     "https://www.rfc-editor.org/rfc/rfc9110#section-14",
	"rfc7232":                   "https://www.rfc-editor.org/rfc/rfc7232#section-3.3",
}

func docURL(ref string) string {
	key, _, _ := strings.Cut(ref, " ")
	if key == "" {
		return ""
	}
	if u, ok := docs[key]; ok {
		return u
	}
	if key[0] >= 'A' && key[0] <= 'Z' {
		return "https://docs.aws.amazon.com/AmazonS3/latest/API/API_" + key + ".html"
	}
	return ""
}

// knownDivergences lists gateway behavior that differs from the documented
// AWS behavior, keyed by "scenario/step#field". Entries are tolerated for
// gateway targets and ignored for Amazon S3 itself, so a recorded reason here
// never weakens the oracle. New divergences fail the suite.
var knownDivergences = func() map[string]string {
	m := map[string]string{}
	add := func(reason string, keys ...string) {
		for _, k := range keys {
			m[k] = reason
		}
	}
	// Design decisions documented in docs/compatibility.md.
	add("design: STANDARD is the only accepted storage class; other classes are rejected instead of being stored and reported",
		"acl/storage-standard-ia#status", "acl/storage-standard-ia-head#header:x-amz-storage-class", "acl/storage-reduced-redundancy#status", "acl/listing-storage-class#xml:ListBucketResult.Contents[0].StorageClass")
	add("design: server-side encryption request headers and Object Lock are rejected with NotImplemented rather than honored",
		"acl/sse-s3#status", "acl/sse-s3#header:x-amz-server-side-encryption", "acl/sse-invalid#status", "acl/object-lock-header#status")
	add("design: unsupported bucket subresources answer 501 NotImplemented instead of the AWS not-found code for an absent configuration",
		"bucket-defaults/policy#status", "bucket-defaults/policy#code", "bucket-defaults/policy-status#status", "bucket-defaults/policy-status#code", "bucket-defaults/website#status", "bucket-defaults/website#code", "bucket-defaults/replication#status", "bucket-defaults/replication#code", "bucket-defaults/object-lock#status", "bucket-defaults/object-lock#code")
	add("design: unsupported bucket subresources answer 501 NotImplemented instead of the empty default configuration AWS returns",
		"bucket-defaults/request-payment#status", "bucket-defaults/request-payment#xml:RequestPaymentConfiguration.Payer", "bucket-defaults/accelerate#status", "bucket-defaults/logging#status", "bucket-defaults/encryption#status", "bucket-defaults/encryption#xml:ServerSideEncryptionConfiguration.Rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm")
	// Gaps: behavior that differs from AWS without a documented reason.
	// Confirmed against Amazon S3 (eu-west-1, 2026-10-04).
	// Observed on Amazon S3 (eu-west-1, 2026-10-04) and not yet matched.
	return m
}()

// Provider-specific exceptions must not hide regressions on another backend.
// Keys are target-name/scenario/step#field, using the built-in/Compose names.
var knownProviderDivergences = func() map[string]string {
	m := map[string]string{}
	for _, target := range []string{"azure", "azurite"} {
		for _, field := range []string{"list-encoded#status", "list-encoded#xml:ListBucketResult.Contents[0].Key", "list-versions-encoded#status", "list-versions-encoded#xml:ListVersionsResult.Version[0].Key"} {
			m[target+"/control-characters/"+field] = "provider limitation (Azurite): the emulator returns 500 when listing blob names containing XML control characters; the S3 backend requests URL-encoded listings and does not have this limitation"
		}
	}
	for _, key := range []string{"a//b", "./dot/x", "dot/./inner"} {
		for _, field := range []string{"path-key:" + key + "#status", "path-key-get:" + key + "#body"} {
			m["s3/objects/"+field] = "provider limitation (RustFS): rejects repeated slashes and dot path segments with 400 InvalidArgument; the gateway preserves the client error without rewriting the key"
		}
	}
	return m
}()
