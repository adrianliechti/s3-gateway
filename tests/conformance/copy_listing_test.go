package conformance_test

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestConformanceCopy(t *testing.T) {
	parallel(t)
	h := begin(t, "copy")
	src := []byte("copy me")
	seeded := h.seed("src", src, hdr("Content-Type", "text/csv", "X-Amz-Meta-Origin", "one", "X-Amz-Tagging", "a=1&b=2", "Cache-Control", "max-age=1"))
	etag := seeded.header("ETag")
	source := awsEncode("/"+h.bucket+"/src", true)
	copyHdr := func(kv ...string) http.Header { return hdr(append([]string{"X-Amz-Copy-Source", source}, kv...)...) }
	res := h.put("copy-default", "dst", nil, copyHdr())
	h.status(res, 200, "CopyObject")
	h.xmlIs(res, "CopyObjectResult.ETag", quote(md5Hex(src)), "CopyObject")
	h.present(res, "CopyObjectResult.LastModified", true, "CopyObject")
	h.eq(res, "last-modified-format", isoMillis(res.xml("CopyObjectResult.LastModified")), true, "CopyObject")
	h.xmlIs(res, "CopyObjectResult.@xmlns", xmlns, "CopyObject")
	res = h.get("copy-default-get", "dst", nil, nil)
	h.bodyIs(res, string(src), "CopyObject")
	h.headerIs(res, "Content-Type", "text/csv", "CopyObject")
	h.headerIs(res, "x-amz-meta-origin", "one", "CopyObject")
	h.headerIs(res, "Cache-Control", "max-age=1", "CopyObject")
	h.headerIs(res, "x-amz-tagging-count", "2", "CopyObject")
	res = h.put("copy-replace", "dst2", nil, copyHdr("X-Amz-Metadata-Directive", "REPLACE", "X-Amz-Meta-Origin", "two"))
	h.status(res, 200, "CopyObject")
	res = h.get("copy-replace-get", "dst2", nil, nil)
	h.headerIs(res, "x-amz-meta-origin", "two", "CopyObject")
	h.headerIs(res, "Content-Type", "binary/octet-stream", "CopyObject")
	h.headerIs(res, "Cache-Control", "", "CopyObject")
	h.headerIs(res, "x-amz-tagging-count", "2", "CopyObject")
	res = h.put("copy-tagging-replace", "dst3", nil, copyHdr("X-Amz-Tagging-Directive", "REPLACE", "X-Amz-Tagging", "c=3"))
	h.status(res, 200, "CopyObject")
	res = h.get("copy-tagging-replace-tags", "dst3", nil, q("tagging", ""))
	h.xmlIs(res, "Tagging.TagSet.Tag[0].Key", "c", "CopyObject")
	h.present(res, "Tagging.TagSet.Tag[1]", false, "CopyObject")
	// Recorded only: S3 documents InvalidRequest for an unchanged self-copy,
	// but a versioned bucket with default encryption answered 200.
	h.put("copy-self-unchanged", "src", nil, copyHdr())
	res = h.put("copy-self-replace", "src", nil, copyHdr("X-Amz-Metadata-Directive", "REPLACE", "X-Amz-Meta-Origin", "three", "Content-Type", "text/csv"))
	h.status(res, 200, "CopyObject")
	res = h.head("copy-self-replace-head", "src", nil, nil)
	h.headerIs(res, "x-amz-meta-origin", "three", "CopyObject")
	h.headerIs(res, "ETag", etag, "CopyObject")
	res = h.put("copy-invalid-directive", "dst4", nil, copyHdr("X-Amz-Metadata-Directive", "MERGE"))
	h.code(res, 400, "InvalidArgument", "CopyObject")
	res = h.put("copy-missing-source", "dst4", nil, hdr("X-Amz-Copy-Source", awsEncode("/"+h.bucket+"/absent", true)))
	h.code(res, 404, "NoSuchKey", "CopyObject")
	res = h.put("copy-missing-bucket", "dst4", nil, hdr("X-Amz-Copy-Source", "/s3gw-conf-missing-"+randomHex(4)+"/src"))
	h.code(res, 404, "NoSuchBucket", "CopyObject")
	head := h.raw(request{method: "HEAD", bucket: h.bucket, key: "src"})
	modified, _ := http.ParseTime(head.header("Last-Modified"))
	past := modified.Add(-24 * time.Hour).UTC().Format(http.TimeFormat)
	between, future := settle(modified)
	for _, tc := range []struct{ step, name, value string }{
		{"copy-if-match-wrong", "X-Amz-Copy-Source-If-Match", quote("00000000000000000000000000000000")},
		{"copy-if-none-match-current", "X-Amz-Copy-Source-If-None-Match", etag},
		{"copy-if-unmodified-since-past", "X-Amz-Copy-Source-If-Unmodified-Since", past},
		{"copy-if-modified-since-between", "X-Amz-Copy-Source-If-Modified-Since", between},
	} {
		res = h.put(tc.step, "dst5", nil, copyHdr(tc.name, tc.value))
		h.code(res, 412, "PreconditionFailed", "CopyObject")
	}
	res = h.put("copy-if-modified-since-future", "dst5", nil, copyHdr("X-Amz-Copy-Source-If-Modified-Since", future))
	h.status(res, 200, "rfc7232")
	res = h.put("copy-if-match-current", "dst5", nil, copyHdr("X-Amz-Copy-Source-If-Match", etag))
	h.status(res, 200, "CopyObject")
	res = h.put("copy-source-without-slash", "dst6", nil, hdr("X-Amz-Copy-Source", awsEncode(h.bucket+"/src", true)))
	h.status(res, 200, "CopyObject")
	if h.unversioned("copy-source-null-version") {
		res = h.put("copy-source-null-version", "dst7", nil, hdr("X-Amz-Copy-Source", source+"?versionId=null"))
		h.status(res, 200, "CopyObject")
		h.note(res, "header:x-amz-copy-source-version-id", res.header("x-amz-copy-source-version-id"))
	}
	special := "sp ace+/ü?q"
	h.seed(special, []byte("special"), nil)
	res = h.put("copy-special-source", "dst8", nil, hdr("X-Amz-Copy-Source", awsEncode("/"+h.bucket+"/"+special, true)))
	h.status(res, 200, "CopyObject")
	h.bodyIs(h.get("copy-special-get", "dst8", nil, nil), "special", "CopyObject")
	// CopyObject takes no request body.
	res = h.put("copy-with-body", "dst9", []byte("ignored?"), copyHdr())
	h.code(res, 400, "InvalidRequest", "CopyObject")
}

func keysOf(res *response, root, element, field string) []string {
	var out []string
	for i := 0; res.has(fmt.Sprintf("%s.%s[%d]", root, element, i)); i++ {
		out = append(out, res.xml(fmt.Sprintf("%s.%s[%d].%s", root, element, i, field)))
	}
	return out
}

func TestConformanceListing(t *testing.T) {
	parallel(t)
	h := begin(t, "listing")
	// "b-" sorts before "b/" and avoids a file/directory collision on disk.
	keys := []string{"a", "b-", "b/1", "b/2", "b0", "c", "d/e/f", "s p+&/x", "z"}
	for _, k := range keys {
		h.seed(k, []byte(k), nil)
	}
	const root = "ListBucketResult"
	list := func(step string, kv ...string) *response {
		return h.do(step, request{method: "GET", bucket: h.bucket, query: q(kv...)})
	}
	v2 := func(step string, kv ...string) *response {
		return list(step, append([]string{"list-type", "2"}, kv...)...)
	}
	res := v2("v2-default")
	h.status(res, 200, "ListObjectsV2")
	h.xmlIs(res, root+".@xmlns", xmlns, "ListObjectsV2")
	h.xmlIs(res, root+".Name", h.bucket, "ListObjectsV2")
	h.xmlIs(res, root+".Prefix", "", "ListObjectsV2")
	h.xmlIs(res, root+".KeyCount", "9", "ListObjectsV2")
	h.xmlIs(res, root+".MaxKeys", "1000", "ListObjectsV2")
	h.xmlIs(res, root+".IsTruncated", "false", "ListObjectsV2")
	h.eq(res, "keys", strings.Join(keysOf(res, root, "Contents", "Key"), ","), strings.Join(keys, ","), "ListObjectsV2")
	h.xmlIs(res, root+".Contents[0].ETag", quote(md5Hex([]byte("a"))), "ListObjectsV2")
	h.xmlIs(res, root+".Contents[0].Size", "1", "ListObjectsV2")
	h.xmlIs(res, root+".Contents[0].StorageClass", "STANDARD", "ListObjectsV2")
	h.eq(res, "last-modified-format", isoMillis(res.xml(root+".Contents[0].LastModified")), true, "ListObjectsV2")
	h.present(res, root+".Contents[0].Owner", false, "ListObjectsV2")
	h.present(res, root+".Marker", false, "ListObjectsV2")
	h.present(res, root+".NextContinuationToken", false, "ListObjectsV2")
	res = v2("v2-fetch-owner", "fetch-owner", "true")
	h.present(res, root+".Contents[0].Owner.ID", true, "ListObjectsV2")
	res = v2("v2-delimiter", "delimiter", "/")
	h.eq(res, "keys", strings.Join(keysOf(res, root, "Contents", "Key"), ","), "a,b-,b0,c,z", "ListObjectsV2")
	h.eq(res, "prefixes", strings.Join(keysOf(res, root, "CommonPrefixes", "Prefix"), ","), "b/,d/,s p+&/", "ListObjectsV2")
	h.xmlIs(res, root+".KeyCount", "8", "ListObjectsV2")
	h.xmlIs(res, root+".Delimiter", "/", "ListObjectsV2")
	// Keys and common prefixes share one lexicographic order across pages.
	var walk []string
	token := ""
	for page := 0; page < 10; page++ {
		kv := []string{"delimiter", "/", "max-keys", "2"}
		if token != "" {
			kv = append(kv, "continuation-token", token)
		}
		res = v2(fmt.Sprintf("v2-page-%d", page), kv...)
		h.status(res, 200, "ListObjectsV2")
		// Within a page Contents precede CommonPrefixes; across pages the
		// combined entries follow one lexicographic order.
		entries := append(keysOf(res, root, "Contents", "Key"), keysOf(res, root, "CommonPrefixes", "Prefix")...)
		sort.Strings(entries)
		walk = append(walk, entries...)
		h.xmlIs(res, root+".KeyCount", fmt.Sprint(len(entries)), "ListObjectsV2")
		if token != "" {
			h.xmlIs(res, root+".ContinuationToken", token, "ListObjectsV2")
		}
		if res.xml(root+".IsTruncated") != "true" {
			h.present(res, root+".NextContinuationToken", false, "ListObjectsV2")
			break
		}
		h.present(res, root+".NextContinuationToken", true, "ListObjectsV2")
		token = res.xml(root + ".NextContinuationToken")
	}
	h.eq(res, "walk", strings.Join(walk, ","), "a,b-,b/,b0,c,d/,s p+&/,z", "ListObjectsV2")
	res = v2("v2-prefix-delimiter", "prefix", "b/", "delimiter", "/")
	h.eq(res, "keys", strings.Join(keysOf(res, root, "Contents", "Key"), ","), "b/1,b/2", "ListObjectsV2")
	h.xmlIs(res, root+".KeyCount", "2", "ListObjectsV2")
	h.xmlIs(res, root+".Prefix", "b/", "ListObjectsV2")
	h.present(res, root+".CommonPrefixes", false, "ListObjectsV2")
	res = v2("v2-prefix-nested", "prefix", "d/", "delimiter", "/")
	h.eq(res, "prefixes", strings.Join(keysOf(res, root, "CommonPrefixes", "Prefix"), ","), "d/e/", "ListObjectsV2")
	h.xmlIs(res, root+".KeyCount", "1", "ListObjectsV2")
	res = v2("v2-prefix-partial", "prefix", "b")
	h.eq(res, "keys", strings.Join(keysOf(res, root, "Contents", "Key"), ","), "b-,b/1,b/2,b0", "ListObjectsV2")
	res = v2("v2-start-after", "start-after", "b0")
	h.eq(res, "keys", strings.Join(keysOf(res, root, "Contents", "Key"), ","), "c,d/e/f,s p+&/x,z", "ListObjectsV2")
	h.xmlIs(res, root+".StartAfter", "b0", "ListObjectsV2")
	res = v2("v2-max-keys-zero", "max-keys", "0")
	h.status(res, 200, "ListObjectsV2")
	h.xmlIs(res, root+".KeyCount", "0", "ListObjectsV2")
	h.xmlIs(res, root+".MaxKeys", "0", "ListObjectsV2")
	h.xmlIs(res, root+".IsTruncated", "false", "ListObjectsV2")
	h.code(v2("v2-max-keys-negative", "max-keys", "-1"), 400, "InvalidArgument", "ListObjectsV2")
	h.code(v2("v2-max-keys-text", "max-keys", "abc"), 400, "InvalidArgument", "ListObjectsV2")
	h.code(v2("v2-bad-token", "continuation-token", "not-a-token"), 400, "InvalidArgument", "ListObjectsV2")
	res = v2("v2-encoding-prefix", "encoding-type", "url", "prefix", "s p", "delimiter", "/")
	h.xmlIs(res, root+".EncodingType", "url", "ListObjectsV2")
	// encoding-type=url is form encoding on AWS: a space becomes "+".
	h.xmlIs(res, root+".Prefix", "s+p", "ListObjectsV2")
	h.xmlIs(res, root+".Delimiter", "/", "ListObjectsV2")
	h.eq(res, "prefixes", strings.Join(keysOf(res, root, "CommonPrefixes", "Prefix"), ","), "s+p%2B%26/", "ListObjectsV2")
	res = v2("v2-encoding-key", "encoding-type", "url", "prefix", "s p")
	h.eq(res, "keys", strings.Join(keysOf(res, root, "Contents", "Key"), ","), "s+p%2B%26/x", "ListObjectsV2")
	res = v2("v2-encoding-start-after", "encoding-type", "url", "start-after", "s p")
	h.xmlIs(res, root+".StartAfter", "s+p", "ListObjectsV2")
	h.code(v2("v2-encoding-invalid", "encoding-type", "foo"), 400, "InvalidArgument", "ListObjectsV2")

	res = list("v1-default")
	h.status(res, 200, "ListObjects")
	h.present(res, root+".Marker", true, "ListObjects")
	h.xmlIs(res, root+".Marker", "", "ListObjects")
	h.present(res, root+".KeyCount", false, "ListObjects")
	h.present(res, root+".Contents[0].Owner.ID", true, "ListObjects")
	h.xmlIs(res, root+".IsTruncated", "false", "ListObjects")
	h.eq(res, "keys", strings.Join(keysOf(res, root, "Contents", "Key"), ","), strings.Join(keys, ","), "ListObjects")
	res = list("v1-marker", "marker", "b0")
	h.eq(res, "keys", strings.Join(keysOf(res, root, "Contents", "Key"), ","), "c,d/e/f,s p+&/x,z", "ListObjects")
	h.xmlIs(res, root+".Marker", "b0", "ListObjects")
	walk = nil
	marker := ""
	for page := 0; page < 10; page++ {
		kv := []string{"delimiter", "/", "max-keys", "2"}
		if marker != "" {
			kv = append(kv, "marker", marker)
		}
		res = list(fmt.Sprintf("v1-page-%d", page), kv...)
		h.status(res, 200, "ListObjects")
		entries := append(keysOf(res, root, "Contents", "Key"), keysOf(res, root, "CommonPrefixes", "Prefix")...)
		sort.Strings(entries)
		walk = append(walk, entries...)
		if res.xml(root+".IsTruncated") != "true" {
			h.present(res, root+".NextMarker", false, "ListObjects")
			break
		}
		// NextMarker is documented for delimiter requests.
		h.present(res, root+".NextMarker", true, "ListObjects")
		marker = res.xml(root + ".NextMarker")
	}
	h.eq(res, "walk", strings.Join(walk, ","), "a,b-,b/,b0,c,d/,s p+&/,z", "ListObjects")
	res = list("v1-paged-without-delimiter", "max-keys", "2")
	h.xmlIs(res, root+".IsTruncated", "true", "ListObjects")
	// Without a delimiter the last key is the marker and NextMarker is omitted.
	h.present(res, root+".NextMarker", false, "ListObjects")
	res = list("v1-encoding", "encoding-type", "url", "marker", "s p")
	h.xmlIs(res, root+".Marker", "s+p", "ListObjects")
	h.eq(res, "keys", strings.Join(keysOf(res, root, "Contents", "Key"), ","), "s+p%2B%26/x,z", "ListObjects")
	// An unknown list-type is ignored and answered as a V1 listing.
	res = list("list-type-invalid", "list-type", "3")
	h.status(res, 200, "ListObjects")
	h.present(res, root, true, "ListObjects")
	if !h.tg.posix {
		h.seed("e/", nil, nil)
		h.seed("e/1", []byte("1"), nil)
		res = v2("v2-directory-marker-delimiter", "delimiter", "/", "prefix", "e")
		h.eq(res, "keys", strings.Join(keysOf(res, root, "Contents", "Key"), ","), "", "listing")
		h.eq(res, "prefixes", strings.Join(keysOf(res, root, "CommonPrefixes", "Prefix"), ","), "e/", "listing")
		res = v2("v2-directory-marker-prefix", "prefix", "e/")
		h.eq(res, "keys", strings.Join(keysOf(res, root, "Contents", "Key"), ","), "e/,e/1", "listing")
		res = v2("v2-directory-marker-prefix-delimiter", "prefix", "e/", "delimiter", "/")
		h.eq(res, "keys", strings.Join(keysOf(res, root, "Contents", "Key"), ","), "e/,e/1", "listing")
	}
}
