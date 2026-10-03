package conformance_test

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/crc64nvme"
)

func TestConformanceObjects(t *testing.T) {
	parallel(t)
	h := begin(t, "objects")
	body := []byte("hello world")
	res := h.put("put-plain", "plain", body, nil)
	h.status(res, 200, "PutObject")
	h.headerIs(res, "ETag", quote(md5Hex(body)), "PutObject")
	h.note(res, "header:x-amz-server-side-encryption", res.header("x-amz-server-side-encryption"))
	h.note(res, "header:x-amz-checksum-type", res.header("x-amz-checksum-type"))
	h.note(res, "header:x-amz-checksum-crc64nvme?", res.header("x-amz-checksum-crc64nvme") != "")
	res = h.get("get-plain", "plain", nil, nil)
	h.status(res, 200, "GetObject")
	h.bodyIs(res, string(body), "GetObject")
	h.headerIs(res, "ETag", quote(md5Hex(body)), "GetObject")
	h.headerIs(res, "Content-Length", "11", "GetObject")
	h.headerIs(res, "Accept-Ranges", "bytes", "GetObject")
	h.headerIs(res, "Content-Type", "binary/octet-stream", "UsingMetadata")
	_, err := http.ParseTime(res.header("Last-Modified"))
	h.eq(res, "last-modified-format", err == nil, true, "RESTCommonResponseHeaders")
	h.eq(res, "header:x-amz-request-id?", res.header("x-amz-request-id") != "", true, "RESTCommonResponseHeaders")
	h.eq(res, "header:date?", res.header("Date") != "", true, "RESTCommonResponseHeaders")
	res = h.head("head-plain", "plain", nil, nil)
	h.status(res, 200, "HeadObject")
	h.eq(res, "body-length", len(res.Body), 0, "HeadObject")
	h.headerIs(res, "ETag", quote(md5Hex(body)), "HeadObject")
	h.headerIs(res, "Content-Length", "11", "HeadObject")
	h.headerIs(res, "Content-Type", "binary/octet-stream", "HeadObject")
	h.headerIs(res, "Accept-Ranges", "bytes", "HeadObject")

	meta := hdr("Content-Type", "text/plain; charset=utf-8", "Cache-Control", "max-age=60", "Content-Disposition", `attachment; filename="x.txt"`, "Content-Encoding", "gzip", "Content-Language", "de-CH", "Expires", "Wed, 21 Oct 2026 07:28:00 GMT", "X-Amz-Meta-Table", "events", "X-Amz-Meta-UPPER-Case", "Value With Spaces")
	h.status(h.put("put-metadata", "meta", body, meta), 200, "PutObject")
	res = h.get("get-metadata", "meta", nil, nil)
	h.headerIs(res, "Content-Type", "text/plain; charset=utf-8", "UsingMetadata")
	h.headerIs(res, "Cache-Control", "max-age=60", "UsingMetadata")
	h.headerIs(res, "Content-Disposition", `attachment; filename="x.txt"`, "UsingMetadata")
	h.headerIs(res, "Content-Encoding", "gzip", "UsingMetadata")
	h.headerIs(res, "Content-Language", "de-CH", "UsingMetadata")
	h.headerIs(res, "Expires", "Wed, 21 Oct 2026 07:28:00 GMT", "UsingMetadata")
	h.headerIs(res, "x-amz-meta-table", "events", "UsingMetadata")
	h.headerIs(res, "x-amz-meta-upper-case", "Value With Spaces", "UsingMetadata")
	res = h.get("response-overrides", "meta", nil, q("response-content-type", "application/json", "response-cache-control", "no-cache", "response-content-disposition", "inline", "response-content-encoding", "identity", "response-content-language", "en", "response-expires", "Thu, 01 Jan 2026 00:00:00 GMT"))
	h.status(res, 200, "GetObject")
	h.headerIs(res, "Content-Type", "application/json", "GetObject")
	h.headerIs(res, "Cache-Control", "no-cache", "GetObject")
	h.headerIs(res, "Content-Disposition", "inline", "GetObject")
	h.headerIs(res, "Content-Encoding", "identity", "GetObject")
	h.headerIs(res, "Content-Language", "en", "GetObject")
	h.headerIs(res, "Expires", "Thu, 01 Jan 2026 00:00:00 GMT", "GetObject")
	res = h.put("metadata-too-large", "big", body, hdr("X-Amz-Meta-Big", strings.Repeat("v", 2049)))
	h.code(res, 400, "MetadataTooLarge", "UsingMetadata")
	res = h.put("metadata-at-limit", "limit", body, hdr("X-Amz-Meta-Fill", strings.Repeat("v", 2048-len("fill"))))
	h.status(res, 200, "UsingMetadata")

	res = h.put("put-empty", "empty", nil, nil)
	h.status(res, 200, "PutObject")
	h.headerIs(res, "ETag", quote("d41d8cd98f00b204e9800998ecf8427e"), "PutObject")
	res = h.get("get-empty", "empty", nil, nil)
	h.status(res, 200, "GetObject")
	h.headerIs(res, "Content-Length", "0", "GetObject")
	h.eq(res, "body-length", len(res.Body), 0, "GetObject")
	res = h.get("range-empty-object", "empty", hdr("Range", "bytes=0-0"), nil)
	h.code(res, 416, "InvalidRange", "GetObject")

	for _, key := range []string{"dir/sub/a b+c%d.txt", "ünïcödé/雪.parquet", "q?mark#hash", "semi;colon=equals&amp", "tilde~dash-under_dot.", `quote'apos"dq`, "trailing.space ", "paren(brackets)[x]", "back\\slash"} {
		res = h.put("special-key:"+key, key, []byte(key), nil)
		h.status(res, 200, "object-keys")
		res = h.get("special-key-get:"+key, key, nil, nil)
		h.status(res, 200, "object-keys")
		h.bodyIs(res, key, "object-keys")
	}
	// Keys that are not representable as file paths are documented disk
	// limitations; the scenario skips them on disk-backed gateways.
	for _, key := range []string{"dir/", "a//b", "./dot/x", "dot/./inner"} {
		if h.tg.posix {
			t.Logf("skipping key %q: not representable on a filesystem backend", key)
			continue
		}
		res = h.put("path-key:"+key, key, []byte(key), nil)
		h.status(res, 200, "object-keys")
		// A provider may reject a key, but its validation error must not become
		// an internal gateway failure hidden by a known key-support divergence.
		h.eq(res, "server-error", res.Status >= 500, false, "ErrorResponses")
		res = h.get("path-key-get:"+key, key, nil, nil)
		h.bodyIs(res, key, "object-keys")
	}
}

// Keys may contain characters that XML 1.0 cannot carry; listings must be
// requested with encoding-type=url to see them.
func TestConformanceControlCharacters(t *testing.T) {
	parallel(t)
	h := begin(t, "control-characters")
	key := "ctrl\x01char"
	res := h.put("put", key, []byte("c"), nil)
	h.status(res, 200, "object-keys")
	res = h.get("get", key, nil, nil)
	h.status(res, 200, "object-keys")
	h.bodyIs(res, "c", "object-keys")
	res = h.do("list-encoded", request{method: "GET", bucket: h.bucket, query: q("list-type", "2", "encoding-type", "url")})
	h.status(res, 200, "ListObjectsV2")
	h.xmlIs(res, "ListBucketResult.Contents[0].Key", "ctrl%01char", "object-keys")
	res = h.do("list-versions-encoded", request{method: "GET", bucket: h.bucket, query: q("versions", "", "encoding-type", "url")})
	h.status(res, 200, "ListObjectVersions")
	h.xmlIs(res, "ListVersionsResult.Version[0].Key", "ctrl%01char", "object-keys")
	res = h.delete("delete", key, nil, nil)
	h.status(res, 204, "DeleteObject")
}

func TestConformanceRanges(t *testing.T) {
	parallel(t)
	h := begin(t, "ranges")
	data := "0123456789"
	h.seed("r", []byte(data), nil)
	etag := quote(md5Hex([]byte(data)))
	for _, tc := range []struct{ name, rng, body, cr string }{
		{"first-byte", "bytes=0-0", "0", "bytes 0-0/10"},
		{"middle", "bytes=2-5", "2345", "bytes 2-5/10"},
		{"open-end", "bytes=5-", "56789", "bytes 5-9/10"},
		{"suffix", "bytes=-3", "789", "bytes 7-9/10"},
		{"suffix-longer-than-object", "bytes=-20", data, "bytes 0-9/10"},
		{"full-open", "bytes=0-", data, "bytes 0-9/10"},
		{"end-clamped", "bytes=5-100", "56789", "bytes 5-9/10"},
		{"last-byte", "bytes=9-9", "9", "bytes 9-9/10"},
	} {
		res := h.get("range-"+tc.name, "r", hdr("Range", tc.rng), nil)
		h.status(res, 206, "GetObject")
		h.bodyIs(res, tc.body, "GetObject")
		h.headerIs(res, "Content-Range", tc.cr, "GetObject")
		h.headerIs(res, "Content-Length", strconv.Itoa(len(tc.body)), "GetObject")
		h.headerIs(res, "Accept-Ranges", "bytes", "GetObject")
		h.headerIs(res, "ETag", etag, "GetObject")
	}
	for _, tc := range []struct{ name, rng string }{{"start-at-size", "bytes=10-"}, {"beyond-size", "bytes=20-30"}} {
		res := h.get("range-"+tc.name, "r", hdr("Range", tc.rng), nil)
		h.code(res, 416, "InvalidRange", "GetObject")
		h.note(res, "header:content-range", res.header("Content-Range"))
	}
	// S3 ignores a malformed Range header and serves the whole object (RFC
	// 9110 section 14.2 allows either; AWS confirmed 200).
	for _, tc := range []struct{ name, rng string }{{"reversed", "bytes=5-2"}, {"non-numeric", "bytes=abc"}, {"multiple", "bytes=0-1,3-4"}, {"other-unit", "items=0-1"}, {"no-equals", "bytes 0-1"}} {
		res := h.get("range-"+tc.name, "r", hdr("Range", tc.rng), nil)
		h.status(res, 200, "range")
		h.bodyIs(res, data, "range")
	}
	h.get("range-suffix-zero", "r", hdr("Range", "bytes=-0"), nil)
	res := h.head("head-range", "r", hdr("Range", "bytes=2-5"), nil)
	h.status(res, 206, "HeadObject")
	h.headerIs(res, "Content-Length", "4", "HeadObject")
	h.headerIs(res, "Content-Range", "bytes 2-5/10", "HeadObject")
	h.eq(res, "body-length", len(res.Body), 0, "HeadObject")
}

func TestConformanceConditionalReads(t *testing.T) {
	parallel(t)
	h := begin(t, "conditional-reads")
	body := []byte("content")
	h.seed("c", body, nil)
	head := h.raw(request{method: "HEAD", bucket: h.bucket, key: "c"})
	etag := head.header("ETag")
	modified, err := http.ParseTime(head.header("Last-Modified"))
	if err != nil {
		t.Fatalf("Last-Modified %q: %v", head.header("Last-Modified"), err)
	}
	past := modified.Add(-24 * time.Hour).UTC().Format(http.TimeFormat)
	between, future := settle(modified)
	wrong := quote("00000000000000000000000000000000")
	res := h.get("if-match-current", "c", hdr("If-Match", etag), nil)
	h.status(res, 200, "GetObject")
	res = h.get("if-match-wrong", "c", hdr("If-Match", wrong), nil)
	h.code(res, 412, "PreconditionFailed", "GetObject")
	res = h.get("if-none-match-current", "c", hdr("If-None-Match", etag), nil)
	h.status(res, 304, "GetObject")
	h.eq(res, "body-length", len(res.Body), 0, "GetObject")
	h.headerIs(res, "ETag", etag, "GetObject")
	res = h.get("if-none-match-other", "c", hdr("If-None-Match", wrong), nil)
	h.status(res, 200, "GetObject")
	res = h.get("if-modified-since-between", "c", hdr("If-Modified-Since", between), nil)
	h.status(res, 304, "GetObject")
	res = h.get("if-modified-since-past", "c", hdr("If-Modified-Since", past), nil)
	h.status(res, 200, "GetObject")
	// A date later than the server's clock is ignored (RFC 7232 section 3.3).
	res = h.get("if-modified-since-future", "c", hdr("If-Modified-Since", future), nil)
	h.status(res, 200, "rfc7232")
	res = h.get("if-unmodified-since-past", "c", hdr("If-Unmodified-Since", past), nil)
	h.code(res, 412, "PreconditionFailed", "GetObject")
	res = h.get("if-unmodified-since-future", "c", hdr("If-Unmodified-Since", future), nil)
	h.status(res, 200, "GetObject")
	// Documented combinations: If-Match wins over If-Unmodified-Since and
	// If-None-Match wins over If-Modified-Since.
	res = h.get("if-match-true-unmodified-false", "c", hdr("If-Match", etag, "If-Unmodified-Since", past), nil)
	h.status(res, 200, "GetObject")
	res = h.get("if-none-match-false-modified-true", "c", hdr("If-None-Match", etag, "If-Modified-Since", past), nil)
	h.status(res, 304, "GetObject")
	res = h.head("head-if-none-match", "c", hdr("If-None-Match", etag), nil)
	h.status(res, 304, "HeadObject")
	h.eq(res, "body-length", len(res.Body), 0, "HeadObject")
	res = h.head("head-if-match-wrong", "c", hdr("If-Match", wrong), nil)
	h.status(res, 412, "HeadObject")
	// Recorded only: HTTP semantics S3 does not spell out.
	h.get("if-match-star", "c", hdr("If-Match", "*"), nil)
	h.get("if-match-list", "c", hdr("If-Match", wrong+", "+etag), nil)
	h.get("if-none-match-star", "c", hdr("If-None-Match", "*"), nil)
	h.get("if-modified-since-invalid", "c", hdr("If-Modified-Since", "not a date"), nil)
	// Weak comparison: a W/ prefix still matches (AWS confirmed 304).
	res = h.get("if-none-match-weak", "c", hdr("If-None-Match", "W/"+etag), nil)
	h.status(res, 304, "GetObject")
	res = h.get("range-with-if-none-match", "c", hdr("Range", "bytes=0-1", "If-None-Match", etag), nil)
	h.status(res, 304, "GetObject")
}

func TestConformanceConditionalWrites(t *testing.T) {
	parallel(t)
	h := begin(t, "conditional-writes")
	res := h.put("create-if-none-match", "w", []byte("v1"), hdr("If-None-Match", "*"))
	h.status(res, 200, "conditional-writes")
	etag := res.header("ETag")
	res = h.put("overwrite-if-none-match", "w", []byte("v2"), hdr("If-None-Match", "*"))
	h.code(res, 412, "PreconditionFailed", "conditional-writes")
	h.eq(res, "body-after-412", string(h.raw(request{method: "GET", bucket: h.bucket, key: "w"}).Body), "v1", "conditional-writes")
	res = h.put("overwrite-if-match", "w", []byte("v2"), hdr("If-Match", etag))
	h.status(res, 200, "conditional-writes")
	res = h.put("overwrite-if-match-stale", "w", []byte("v3"), hdr("If-Match", etag))
	h.code(res, 412, "PreconditionFailed", "conditional-writes")
	h.eq(res, "body-after-412", string(h.raw(request{method: "GET", bucket: h.bucket, key: "w"}).Body), "v2", "conditional-writes")
	res = h.put("if-match-missing-key", "absent", []byte("x"), hdr("If-Match", quote("d41d8cd98f00b204e9800998ecf8427e")))
	h.code(res, 404, "NoSuchKey", "conditional-writes")
	// PutObject If-Match accepts ETag values only.
	res = h.put("if-match-star-missing-key", "absent", []byte("x"), hdr("If-Match", "*"))
	h.code(res, 501, "NotImplemented", "conditional-writes")
	h.put("if-match-star-existing-key", "w", []byte("v2"), hdr("If-Match", "*"))
	// PutObject If-None-Match accepts only "*" (AWS answers 501).
	res = h.put("if-none-match-etag", "w", []byte("v3"), hdr("If-None-Match", quote("00000000000000000000000000000000")))
	h.code(res, 501, "NotImplemented", "conditional-writes")
	// Recorded only: conditional deletes.
	h.delete("delete-if-match-stale", "w", hdr("If-Match", etag), nil)
	h.delete("delete-if-match-current", "w", hdr("If-Match", h.raw(request{method: "HEAD", bucket: h.bucket, key: "w"}).header("ETag")), nil)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestConformanceIntegrity(t *testing.T) {
	parallel(t)
	h := begin(t, "integrity")
	body := []byte("integrity payload")
	res := h.do("content-md5-valid", request{method: "PUT", bucket: h.bucket, key: "md5", body: body, contentMD5: true})
	h.status(res, 200, "PutObject")
	other, _ := hex.DecodeString(md5Hex([]byte("other")))
	res = h.put("content-md5-wrong", "md5", body, hdr("Content-MD5", b64(other)))
	h.code(res, 400, "BadDigest", "PutObject")
	res = h.put("content-md5-invalid", "md5", body, hdr("Content-MD5", "not-base64!"))
	h.code(res, 400, "InvalidDigest", "PutObject")
	h.eq(res, "body-after-rejections", string(h.raw(request{method: "GET", bucket: h.bucket, key: "md5"}).Body), string(body), "PutObject")
	sum := sha256.Sum256([]byte("other"))
	res = h.do("payload-sha256-mismatch", request{method: "PUT", bucket: h.bucket, key: "sha", body: body, payloadHash: hex.EncodeToString(sum[:])})
	h.code(res, 400, "XAmzContentSHA256Mismatch", "sigv4")
	res = h.do("payload-unsigned", request{method: "PUT", bucket: h.bucket, key: "unsigned", body: body, payloadHash: "UNSIGNED-PAYLOAD"})
	h.status(res, 200, "sigv4")

	crc := make([]byte, 4)
	crc[0], crc[1], crc[2], crc[3] = byte(crc32.ChecksumIEEE(body)>>24), byte(crc32.ChecksumIEEE(body)>>16), byte(crc32.ChecksumIEEE(body)>>8), byte(crc32.ChecksumIEEE(body))
	crc32Value := b64(crc)
	res = h.put("checksum-crc32", "crc", body, hdr("X-Amz-Checksum-Crc32", crc32Value))
	h.status(res, 200, "checksums")
	h.headerIs(res, "x-amz-checksum-crc32", crc32Value, "checksums")
	h.note(res, "header:x-amz-checksum-type", res.header("x-amz-checksum-type"))
	res = h.get("checksum-mode-enabled", "crc", hdr("X-Amz-Checksum-Mode", "ENABLED"), nil)
	h.headerIs(res, "x-amz-checksum-crc32", crc32Value, "checksums")
	h.headerIs(res, "x-amz-checksum-type", "FULL_OBJECT", "checksums")
	res = h.get("checksum-mode-absent", "crc", nil, nil)
	h.headerIs(res, "x-amz-checksum-crc32", "", "checksums")
	res = h.head("checksum-head", "crc", hdr("X-Amz-Checksum-Mode", "ENABLED"), nil)
	h.headerIs(res, "x-amz-checksum-crc32", crc32Value, "checksums")
	res = h.get("checksum-range", "crc", hdr("X-Amz-Checksum-Mode", "ENABLED", "Range", "bytes=0-3"), nil)
	h.note(res, "header:x-amz-checksum-crc32", res.header("x-amz-checksum-crc32"))
	res = h.put("checksum-crc32-wrong", "crc-wrong", body, hdr("X-Amz-Checksum-Crc32", "AAAAAA=="))
	h.code(res, 400, "BadDigest", "checksums")
	res = h.put("checksum-crc32-malformed", "crc-wrong", body, hdr("X-Amz-Checksum-Crc32", "!!"))
	h.code(res, 400, "InvalidRequest", "checksums")
	sha := sha256.Sum256(body)
	res = h.put("checksum-sha256", "sha256", body, hdr("X-Amz-Checksum-Sha256", b64(sha[:])))
	h.status(res, 200, "checksums")
	h.headerIs(res, "x-amz-checksum-sha256", b64(sha[:]), "checksums")
	one := sha1.Sum(body)
	res = h.put("checksum-sha1", "sha1", body, hdr("X-Amz-Checksum-Sha1", b64(one[:])))
	h.status(res, 200, "checksums")
	h.headerIs(res, "x-amz-checksum-sha1", b64(one[:]), "checksums")
	c := crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli))
	res = h.put("checksum-crc32c", "crc32c", body, hdr("X-Amz-Checksum-Crc32c", b64([]byte{byte(c >> 24), byte(c >> 16), byte(c >> 8), byte(c)})))
	h.status(res, 200, "checksums")
	nvme := crc64nvme.Checksum(body)
	raw := make([]byte, 8)
	for i := 0; i < 8; i++ {
		raw[i] = byte(nvme >> (56 - 8*i))
	}
	res = h.put("checksum-crc64nvme", "crc64", body, hdr("X-Amz-Checksum-Crc64nvme", b64(raw)))
	h.status(res, 200, "checksums")
	h.headerIs(res, "x-amz-checksum-crc64nvme", b64(raw), "checksums")
	// Only one checksum header is permitted per request.
	res = h.put("checksum-multiple", "multi", body, hdr("X-Amz-Checksum-Crc32", crc32Value, "X-Amz-Checksum-Sha256", b64(sha[:])))
	h.code(res, 400, "InvalidRequest", "checksums")
	h.put("sdk-algorithm-without-value", "sdk", body, hdr("X-Amz-Sdk-Checksum-Algorithm", "CRC32"))
	// aws-chunked encoding with an unsigned payload and a trailing checksum.
	chunked := fmt.Sprintf("%x\r\n%s\r\n0\r\nx-amz-checksum-sha256:%s\r\n\r\n", len(body), body, b64(sha[:]))
	stream := func(key, trailer string) request {
		payload := fmt.Sprintf("%x\r\n%s\r\n0\r\nx-amz-checksum-sha256:%s\r\n\r\n", len(body), body, trailer)
		return request{method: "PUT", bucket: h.bucket, key: key, body: []byte(payload), payloadHash: "STREAMING-UNSIGNED-PAYLOAD-TRAILER", header: hdr("Content-Encoding", "aws-chunked", "X-Amz-Decoded-Content-Length", strconv.Itoa(len(body)), "X-Amz-Trailer", "x-amz-checksum-sha256")}
	}
	_ = chunked
	res = h.do("aws-chunked-trailer", stream("chunked", b64(sha[:])))
	h.status(res, 200, "checksums")
	h.headerIs(res, "x-amz-checksum-sha256", b64(sha[:]), "checksums")
	res = h.get("aws-chunked-stored", "chunked", hdr("X-Amz-Checksum-Mode", "ENABLED"), nil)
	h.bodyIs(res, string(body), "checksums")
	h.headerIs(res, "Content-Encoding", "", "checksums")
	h.headerIs(res, "Content-Length", strconv.Itoa(len(body)), "checksums")
	h.headerIs(res, "x-amz-checksum-sha256", b64(sha[:]), "checksums")
	res = h.do("aws-chunked-bad-trailer", stream("chunked-bad", b64(make([]byte, 32))))
	h.code(res, 400, "BadDigest", "checksums")
}
