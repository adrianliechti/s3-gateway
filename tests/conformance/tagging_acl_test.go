package conformance_test

import (
	"fmt"
	"strings"
	"testing"
)

func tagging(tags ...[2]string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `<Tagging xmlns="%s"><TagSet>`, xmlns)
	for _, t := range tags {
		fmt.Fprintf(&b, "<Tag><Key>%s</Key><Value>%s</Value></Tag>", t[0], t[1])
	}
	b.WriteString("</TagSet></Tagging>")
	return []byte(b.String())
}

func TestConformanceTagging(t *testing.T) {
	parallel(t)
	h := begin(t, "tagging")
	h.seed("o", []byte("o"), nil)
	putTags := func(step string, body []byte) *response {
		return h.do(step, request{method: "PUT", bucket: h.bucket, key: "o", query: q("tagging", ""), body: body, contentMD5: true})
	}
	res := h.get("get-untagged", "o", nil, q("tagging", ""))
	h.status(res, 200, "GetObjectTagging")
	h.present(res, "Tagging.TagSet", true, "GetObjectTagging")
	h.present(res, "Tagging.TagSet.Tag", false, "GetObjectTagging")
	res = putTags("put-two", tagging([2]string{"alpha", "1"}, [2]string{"beta", "two words"}))
	h.status(res, 200, "PutObjectTagging")
	res = h.get("get-two", "o", nil, q("tagging", ""))
	h.xmlIs(res, "Tagging.TagSet.Tag[0].Key", "alpha", "GetObjectTagging")
	h.xmlIs(res, "Tagging.TagSet.Tag[1].Key", "beta", "GetObjectTagging")
	h.xmlIs(res, "Tagging.TagSet.Tag[1].Value", "two words", "GetObjectTagging")
	res = h.head("head-tag-count", "o", nil, nil)
	h.headerIs(res, "x-amz-tagging-count", "2", "GetObject")
	h.note(res, "header:x-amz-tagging-count", res.header("x-amz-tagging-count"))
	res = h.get("get-tag-count", "o", nil, nil)
	h.headerIs(res, "x-amz-tagging-count", "2", "GetObject")
	var eleven [][2]string
	for i := 0; i < 11; i++ {
		eleven = append(eleven, [2]string{fmt.Sprintf("k%02d", i), "v"})
	}
	res = putTags("put-eleven", tagging(eleven...))
	h.code(res, 400, "BadRequest", "tagging")
	h.code(putTags("put-duplicate-key", tagging([2]string{"dup", "1"}, [2]string{"dup", "2"})), 400, "InvalidTag", "PutObjectTagging")
	h.code(putTags("put-key-too-long", tagging([2]string{strings.Repeat("k", 129), "v"})), 400, "InvalidTag", "tagging")
	h.code(putTags("put-value-too-long", tagging([2]string{"k", strings.Repeat("v", 257)})), 400, "InvalidTag", "tagging")
	h.code(putTags("put-invalid-character", tagging([2]string{"bang!", "v"})), 400, "InvalidTag", "tagging")
	h.code(putTags("put-aws-prefix", tagging([2]string{"aws:reserved", "v"})), 400, "InvalidTag", "tagging")
	h.code(putTags("put-empty-key", tagging([2]string{"", "v"})), 400, "InvalidTag", "tagging")
	h.status(putTags("put-allowed-characters", tagging([2]string{"k+-=._:/@ x", "v+-=._:/@ x"})), 200, "tagging")
	h.status(putTags("put-unicode", tagging([2]string{"schlüssel", "wert 雪"})), 200, "tagging")
	h.status(putTags("put-empty-value", tagging([2]string{"empty", ""})), 200, "tagging")
	h.code(putTags("put-malformed", []byte("<Tagging")), 400, "MalformedXML", "PutObjectTagging")
	res = h.get("get-after-rejections", "o", nil, q("tagging", ""))
	h.xmlIs(res, "Tagging.TagSet.Tag[0].Key", "empty", "PutObjectTagging")
	res = h.delete("delete-tags", "o", nil, q("tagging", ""))
	h.status(res, 204, "DeleteObjectTagging")
	res = h.get("get-after-delete", "o", nil, q("tagging", ""))
	h.present(res, "Tagging.TagSet.Tag", false, "DeleteObjectTagging")
	res = h.head("head-after-delete", "o", nil, nil)
	h.headerIs(res, "x-amz-tagging-count", "", "DeleteObjectTagging")
	res = h.put("put-with-header", "h", []byte("h"), hdr("X-Amz-Tagging", "k1=v1&k2=v%202"))
	h.status(res, 200, "PutObject")
	res = h.get("get-header-tags", "h", nil, q("tagging", ""))
	h.xmlIs(res, "Tagging.TagSet.Tag[1].Value", "v 2", "PutObject")
	h.headerIs(h.get("get-header-count", "h", nil, nil), "x-amz-tagging-count", "2", "PutObject")
	h.code(h.put("put-with-bad-header", "h2", []byte("h"), hdr("X-Amz-Tagging", "k1=v1&k1=v2")), 400, "InvalidArgument", "PutObject")
	h.code(h.get("get-tags-missing-key", "absent", nil, q("tagging", "")), 404, "NoSuchKey", "GetObjectTagging")

	if h.fixed("bucket-tagging") {
		return
	}
	bucketTags := func(step string, body []byte) *response {
		return h.do(step, request{method: "PUT", bucket: h.bucket, query: q("tagging", ""), body: body, contentMD5: true})
	}
	res = bucketTags("bucket-put", tagging([2]string{"team", "blue"}, [2]string{"tier", "gold"}))
	h.status(res, 204, "PutBucketTagging")
	res = h.do("bucket-get", request{method: "GET", bucket: h.bucket, query: q("tagging", "")})
	h.status(res, 200, "GetBucketTagging")
	h.xmlIs(res, "Tagging.TagSet.Tag[0].Key", "team", "GetBucketTagging")
	h.xmlIs(res, "Tagging.TagSet.Tag[1].Value", "gold", "GetBucketTagging")
	var fiftyOne [][2]string
	for i := 0; i < 51; i++ {
		fiftyOne = append(fiftyOne, [2]string{fmt.Sprintf("k%02d", i), "v"})
	}
	res = bucketTags("bucket-put-fifty-one", tagging(fiftyOne...))
	h.status(res, 400, "PutBucketTagging")
	h.note(res, "code", res.Code)
	h.code(bucketTags("bucket-put-duplicate", tagging([2]string{"d", "1"}, [2]string{"d", "2"})), 400, "InvalidTag", "PutBucketTagging")
	res = h.do("bucket-delete", request{method: "DELETE", bucket: h.bucket, query: q("tagging", "")})
	h.status(res, 204, "DeleteBucketTagging")
	h.code(h.do("bucket-get-after-delete", request{method: "GET", bucket: h.bucket, query: q("tagging", "")}), 404, "NoSuchTagSet", "GetBucketTagging")
}

// Object ownership defaults to BucketOwnerEnforced on AWS: ACL writes other
// than bucket-owner-full-control fail, reads still return the owner grant.
func TestConformanceACLAndStorage(t *testing.T) {
	parallel(t)
	h := begin(t, "acl")
	h.seed("o", []byte("o"), nil)
	res := h.get("object-acl", "o", nil, q("acl", ""))
	h.status(res, 200, "GetObjectAcl")
	h.xmlIs(res, "AccessControlPolicy.@xmlns", xmlns, "GetObjectAcl")
	h.xmlIs(res, "AccessControlPolicy.AccessControlList.Grant.Permission", "FULL_CONTROL", "GetObjectAcl")
	h.xmlIs(res, "AccessControlPolicy.AccessControlList.Grant.Grantee.@type", "CanonicalUser", "GetObjectAcl")
	h.eq(res, "owner-is-grantee", res.xml("AccessControlPolicy.Owner.ID") != "" && res.xml("AccessControlPolicy.Owner.ID") == res.xml("AccessControlPolicy.AccessControlList.Grant.Grantee.ID"), true, "GetObjectAcl")
	h.code(h.get("object-acl-missing-key", "absent", nil, q("acl", "")), 404, "NoSuchKey", "GetObjectAcl")
	res = h.put("put-public-read", "public", []byte("p"), hdr("X-Amz-Acl", "public-read"))
	h.code(res, 400, "AccessControlListNotSupported", "object-ownership")
	// private is accepted under BucketOwnerEnforced (it changes nothing).
	res = h.put("put-private", "private", []byte("p"), hdr("X-Amz-Acl", "private"))
	h.status(res, 200, "object-ownership")
	res = h.put("put-bucket-owner-full-control", "bofc", []byte("p"), hdr("X-Amz-Acl", "bucket-owner-full-control"))
	h.status(res, 200, "object-ownership")
	res = h.put("put-invalid-canned-acl", "invalid", []byte("p"), hdr("X-Amz-Acl", "not-an-acl"))
	h.code(res, 400, "InvalidArgument", "PutObject")
	res = h.do("put-object-acl-private", request{method: "PUT", bucket: h.bucket, key: "o", query: q("acl", ""), header: hdr("X-Amz-Acl", "private")})
	h.code(res, 400, "AccessControlListNotSupported", "object-ownership")
	res = h.do("put-bucket-acl-private", request{method: "PUT", bucket: h.bucket, query: q("acl", ""), header: hdr("X-Amz-Acl", "private")})
	h.code(res, 400, "AccessControlListNotSupported", "object-ownership")
	res = h.put("grant-header", "granted", []byte("g"), hdr("X-Amz-Grant-Read", `uri="http://acs.amazonaws.com/groups/global/AllUsers"`))
	h.code(res, 400, "AccessControlListNotSupported", "object-ownership")
	h.code(h.get("public-not-created", "public", nil, nil), 404, "NoSuchKey", "object-ownership")

	res = h.put("storage-standard", "sc", []byte("s"), hdr("X-Amz-Storage-Class", "STANDARD"))
	h.status(res, 200, "storage-classes")
	h.headerIs(h.head("storage-standard-head", "sc", nil, nil), "x-amz-storage-class", "", "HeadObject")
	res = h.put("storage-standard-ia", "sc-ia", []byte("s"), hdr("X-Amz-Storage-Class", "STANDARD_IA"))
	h.status(res, 200, "storage-classes")
	h.headerIs(h.head("storage-standard-ia-head", "sc-ia", nil, nil), "x-amz-storage-class", "STANDARD_IA", "HeadObject")
	res = h.put("storage-reduced-redundancy", "sc-rr", []byte("s"), hdr("X-Amz-Storage-Class", "REDUCED_REDUNDANCY"))
	h.status(res, 200, "storage-classes")
	res = h.put("storage-invalid", "sc-bad", []byte("s"), hdr("X-Amz-Storage-Class", "NOT_A_CLASS"))
	h.code(res, 400, "InvalidStorageClass", "PutObject")
	res = h.do("listing-storage-class", request{method: "GET", bucket: h.bucket, query: q("list-type", "2", "prefix", "sc-ia")})
	h.xmlIs(res, "ListBucketResult.Contents[0].StorageClass", "STANDARD_IA", "ListObjectsV2")

	res = h.put("sse-s3", "sse", []byte("e"), hdr("X-Amz-Server-Side-Encryption", "AES256"))
	h.status(res, 200, "PutObject")
	h.headerIs(res, "x-amz-server-side-encryption", "AES256", "PutObject")
	res = h.put("sse-invalid", "sse-bad", []byte("e"), hdr("X-Amz-Server-Side-Encryption", "ROT13"))
	h.status(res, 400, "PutObject")
	h.note(res, "code", res.Code)
	res = h.put("object-lock-header", "locked", []byte("l"), hdr("X-Amz-Object-Lock-Mode", "GOVERNANCE", "X-Amz-Object-Lock-Retain-Until-Date", "2099-01-01T00:00:00Z"))
	h.status(res, 400, "PutObject")
	h.note(res, "code", res.Code)
	res = h.put("website-redirect", "redirect", []byte("r"), hdr("X-Amz-Website-Redirect-Location", "/elsewhere"))
	h.status(res, 200, "PutObject")
	h.headerIs(h.head("website-redirect-head", "redirect", nil, nil), "x-amz-website-redirect-location", "/elsewhere", "HeadObject")
}
