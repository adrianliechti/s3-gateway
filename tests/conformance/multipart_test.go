package conformance_test

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
	"testing"
)

type completedPart struct {
	number int
	etag   string
	sums   string
}

func completion(parts ...completedPart) []byte {
	var b strings.Builder
	b.WriteString(`<CompleteMultipartUpload xmlns="` + xmlns + `">`)
	for _, p := range parts {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag>%s</Part>", p.number, p.etag, p.sums)
	}
	b.WriteString("</CompleteMultipartUpload>")
	return []byte(b.String())
}

// multipartETag is the documented construction clients rely on: MD5 of the
// concatenated binary part digests, suffixed with the part count.
func multipartETag(etags ...string) string {
	var raw []byte
	for _, e := range etags {
		d, _ := hex.DecodeString(strings.Trim(e, `"`))
		raw = append(raw, d...)
	}
	sum := md5.Sum(raw)
	return quote(hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(len(etags)))
}

func TestConformanceMultipart(t *testing.T) {
	parallel(t)
	h := begin(t, "multipart")
	key := "mp/object"
	initiate := func(step, key string, header map[string][]string) (*response, string) {
		res := h.do(step, request{method: "POST", bucket: h.bucket, key: key, query: q("uploads", ""), header: header})
		return res, res.xml("InitiateMultipartUploadResult.UploadId")
	}
	part := func(step, key, uid, number string, body []byte, header map[string][]string) *response {
		return h.do(step, request{method: "PUT", bucket: h.bucket, key: key, query: q("uploadId", uid, "partNumber", number), body: body, header: header})
	}
	complete := func(step, key, uid string, body []byte, header map[string][]string) *response {
		return h.do(step, request{method: "POST", bucket: h.bucket, key: key, query: q("uploadId", uid), body: body, header: header})
	}
	res, uid := initiate("initiate", key, nil)
	h.status(res, 200, "CreateMultipartUpload")
	h.xmlIs(res, "InitiateMultipartUploadResult.Bucket", h.bucket, "CreateMultipartUpload")
	h.xmlIs(res, "InitiateMultipartUploadResult.Key", key, "CreateMultipartUpload")
	h.xmlIs(res, "InitiateMultipartUploadResult.@xmlns", xmlns, "CreateMultipartUpload")
	if uid == "" {
		t.Fatal("no upload id")
	}
	small := []byte("small part")
	h.code(part("part-number-zero", key, uid, "0", small, nil), 400, "InvalidArgument", "UploadPart")
	h.code(part("part-number-too-large", key, uid, "10001", small, nil), 400, "InvalidArgument", "UploadPart")
	h.code(part("part-number-text", key, uid, "abc", small, nil), 400, "InvalidArgument", "UploadPart")
	h.code(part("part-unknown-upload", key, strings.Repeat("0", 32), "1", small, nil), 404, "NoSuchUpload", "UploadPart")
	h.code(part("part-wrong-key", "mp/other", uid, "1", small, nil), 404, "NoSuchUpload", "UploadPart")
	part1, part2 := bytes.Repeat([]byte("a"), 5<<20), []byte("tail")
	r1 := part("part-1", key, uid, "1", part1, nil)
	h.status(r1, 200, "UploadPart")
	h.headerIs(r1, "ETag", quote(md5Hex(part1)), "UploadPart")
	r2 := part("part-2", key, uid, "2", part2, nil)
	h.status(r2, 200, "UploadPart")
	etag1, etag2 := r1.header("ETag"), r2.header("ETag")
	res = h.do("list-parts", request{method: "GET", bucket: h.bucket, key: key, query: q("uploadId", uid)})
	h.status(res, 200, "ListParts")
	h.xmlIs(res, "ListPartsResult.Bucket", h.bucket, "ListParts")
	h.xmlIs(res, "ListPartsResult.Key", key, "ListParts")
	h.xmlIs(res, "ListPartsResult.UploadId", uid, "ListParts")
	h.xmlIs(res, "ListPartsResult.Part[0].PartNumber", "1", "ListParts")
	h.xmlIs(res, "ListPartsResult.Part[0].ETag", etag1, "ListParts")
	h.xmlIs(res, "ListPartsResult.Part[0].Size", "5242880", "ListParts")
	h.xmlIs(res, "ListPartsResult.Part[1].PartNumber", "2", "ListParts")
	h.xmlIs(res, "ListPartsResult.Part[1].Size", "4", "ListParts")
	h.eq(res, "last-modified-format", isoMillis(res.xml("ListPartsResult.Part[0].LastModified")), true, "ListParts")
	h.xmlIs(res, "ListPartsResult.IsTruncated", "false", "ListParts")
	h.xmlIs(res, "ListPartsResult.StorageClass", "STANDARD", "ListParts")
	h.present(res, "ListPartsResult.Owner.ID", true, "ListParts")
	h.present(res, "ListPartsResult.Initiator.ID", true, "ListParts")
	res = h.do("list-parts-paged", request{method: "GET", bucket: h.bucket, key: key, query: q("uploadId", uid, "max-parts", "1")})
	h.xmlIs(res, "ListPartsResult.IsTruncated", "true", "ListParts")
	h.xmlIs(res, "ListPartsResult.MaxParts", "1", "ListParts")
	h.xmlIs(res, "ListPartsResult.NextPartNumberMarker", "1", "ListParts")
	h.present(res, "ListPartsResult.Part[1]", false, "ListParts")
	res = h.do("list-parts-marker", request{method: "GET", bucket: h.bucket, key: key, query: q("uploadId", uid, "part-number-marker", "1")})
	h.xmlIs(res, "ListPartsResult.PartNumberMarker", "1", "ListParts")
	h.xmlIs(res, "ListPartsResult.Part[0].PartNumber", "2", "ListParts")
	h.present(res, "ListPartsResult.Part[1]", false, "ListParts")
	res = h.do("list-uploads", request{method: "GET", bucket: h.bucket, query: q("uploads", "")})
	h.status(res, 200, "ListMultipartUploads")
	h.xmlIs(res, "ListMultipartUploadsResult.Bucket", h.bucket, "ListMultipartUploads")
	h.xmlIs(res, "ListMultipartUploadsResult.Upload[0].Key", key, "ListMultipartUploads")
	h.xmlIs(res, "ListMultipartUploadsResult.Upload[0].UploadId", uid, "ListMultipartUploads")
	h.xmlIs(res, "ListMultipartUploadsResult.Upload[0].StorageClass", "STANDARD", "ListMultipartUploads")
	h.present(res, "ListMultipartUploadsResult.Upload[0].Initiator.ID", true, "ListMultipartUploads")
	h.eq(res, "initiated-format", isoMillis(res.xml("ListMultipartUploadsResult.Upload[0].Initiated")), true, "ListMultipartUploads")
	h.xmlIs(res, "ListMultipartUploadsResult.IsTruncated", "false", "ListMultipartUploads")
	h.xmlIs(res, "ListMultipartUploadsResult.MaxUploads", "1000", "ListMultipartUploads")
	res = h.do("list-uploads-prefix-miss", request{method: "GET", bucket: h.bucket, query: q("uploads", "", "prefix", "zz")})
	h.present(res, "ListMultipartUploadsResult.Upload", false, "ListMultipartUploads")

	wrong := quote("00000000000000000000000000000000")
	h.code(complete("complete-wrong-etag", key, uid, completion(completedPart{1, wrong, ""}, completedPart{2, etag2, ""}), nil), 400, "InvalidPart", "CompleteMultipartUpload")
	h.code(complete("complete-wrong-order", key, uid, completion(completedPart{2, etag2, ""}, completedPart{1, etag1, ""}), nil), 400, "InvalidPartOrder", "CompleteMultipartUpload")
	h.code(complete("complete-missing-part", key, uid, completion(completedPart{1, etag1, ""}, completedPart{3, wrong, ""}), nil), 400, "InvalidPart", "CompleteMultipartUpload")
	h.code(complete("complete-empty", key, uid, []byte(`<CompleteMultipartUpload xmlns="`+xmlns+`"></CompleteMultipartUpload>`), nil), 400, "MalformedXML", "CompleteMultipartUpload")
	h.code(complete("complete-malformed", key, uid, []byte("<not xml"), nil), 400, "MalformedXML", "CompleteMultipartUpload")
	h.code(complete("complete-unknown-upload", key, strings.Repeat("1", 32), completion(completedPart{1, etag1, ""}), nil), 404, "NoSuchUpload", "CompleteMultipartUpload")
	// Parts before the last must be at least 5 MiB.
	_, tiny := initiate("initiate-tiny", "mp/tiny", nil)
	t1 := part("tiny-part-1", "mp/tiny", tiny, "1", []byte("one"), nil)
	t2 := part("tiny-part-2", "mp/tiny", tiny, "2", []byte("two"), nil)
	h.code(complete("complete-entity-too-small", "mp/tiny", tiny, completion(completedPart{1, t1.header("ETag"), ""}, completedPart{2, t2.header("ETag"), ""}), nil), 400, "EntityTooSmall", "mpu-limits")
	res = complete("complete-tiny-single", "mp/tiny", tiny, completion(completedPart{2, t2.header("ETag"), ""}), nil)
	h.status(res, 200, "CompleteMultipartUpload")
	h.bodyIs(h.get("tiny-get", "mp/tiny", nil, nil), "two", "CompleteMultipartUpload")

	final := multipartETag(etag1, etag2)
	body := completion(completedPart{1, etag1, ""}, completedPart{2, etag2, ""})
	res = complete("complete", key, uid, body, nil)
	h.status(res, 200, "CompleteMultipartUpload")
	h.xmlIs(res, "CompleteMultipartUploadResult.ETag", final, "CompleteMultipartUpload")
	h.xmlIs(res, "CompleteMultipartUploadResult.Bucket", h.bucket, "CompleteMultipartUpload")
	h.xmlIs(res, "CompleteMultipartUploadResult.Key", key, "CompleteMultipartUpload")
	h.present(res, "CompleteMultipartUploadResult.Location", true, "CompleteMultipartUpload")
	h.note(res, "header:etag?", res.header("ETag") != "")
	h.note(res, "xml:CompleteMultipartUploadResult.Location", res.xml("CompleteMultipartUploadResult.Location"))
	total := strconv.Itoa(len(part1) + len(part2))
	res = h.get("get", key, nil, nil)
	h.status(res, 200, "GetObject")
	h.headerIs(res, "ETag", final, "GetObject")
	h.headerIs(res, "Content-Length", total, "GetObject")
	h.eq(res, "body", bytes.Equal(res.Body, append(append([]byte{}, part1...), part2...)), true, "GetObject")
	res = h.get("part-read-1", key, nil, q("partNumber", "1"))
	h.status(res, 206, "GetObject")
	h.headerIs(res, "Content-Length", "5242880", "GetObject")
	h.headerIs(res, "Content-Range", "bytes 0-5242879/"+total, "GetObject")
	h.headerIs(res, "x-amz-mp-parts-count", "2", "GetObject")
	h.headerIs(res, "ETag", final, "GetObject")
	res = h.get("part-read-2", key, nil, q("partNumber", "2"))
	h.status(res, 206, "GetObject")
	h.bodyIs(res, "tail", "GetObject")
	h.headerIs(res, "Content-Range", "bytes 5242880-5242883/"+total, "GetObject")
	res = h.get("part-read-beyond", key, nil, q("partNumber", "3"))
	h.code(res, 416, "InvalidPartNumber", "GetObject")
	res = h.get("part-read-zero", key, nil, q("partNumber", "0"))
	h.code(res, 400, "InvalidArgument", "GetObject")
	res = h.get("part-read-with-range", key, hdr("Range", "bytes=0-1"), q("partNumber", "1"))
	h.code(res, 400, "InvalidRequest", "GetObject")
	res = h.head("head-part-1", key, nil, q("partNumber", "1"))
	h.status(res, 206, "HeadObject")
	h.headerIs(res, "Content-Length", "5242880", "HeadObject")
	h.headerIs(res, "x-amz-mp-parts-count", "2", "HeadObject")
	// Single-part objects: partNumber=1 is the whole object.
	h.seed("mp/single", []byte("single"), nil)
	res = h.get("single-part-read-1", "mp/single", nil, q("partNumber", "1"))
	h.status(res, 206, "GetObject")
	h.bodyIs(res, "single", "GetObject")
	h.note(res, "header:x-amz-mp-parts-count", res.header("x-amz-mp-parts-count"))
	h.note(res, "header:content-range", res.header("Content-Range"))
	res = h.get("single-part-read-2", "mp/single", nil, q("partNumber", "2"))
	h.code(res, 416, "InvalidPartNumber", "GetObject")
	// Recorded only: completing again is a retry on the gateway.
	complete("complete-again", key, uid, body, nil)
	h.code(h.do("list-parts-after-complete", request{method: "GET", bucket: h.bucket, key: key, query: q("uploadId", uid)}), 404, "NoSuchUpload", "ListParts")
	h.code(part("part-after-complete", key, uid, "3", small, nil), 404, "NoSuchUpload", "UploadPart")

	h.code(h.do("abort-unknown", request{method: "DELETE", bucket: h.bucket, key: key, query: q("uploadId", strings.Repeat("2", 32))}), 404, "NoSuchUpload", "AbortMultipartUpload")
	_, aborted := initiate("initiate-abort", "mp/abort", nil)
	part("abort-part", "mp/abort", aborted, "1", small, nil)
	h.status(h.do("abort", request{method: "DELETE", bucket: h.bucket, key: "mp/abort", query: q("uploadId", aborted)}), 204, "AbortMultipartUpload")
	// Recorded only: AWS answered 204 for a repeated abort of a real upload id.
	h.do("abort-again", request{method: "DELETE", bucket: h.bucket, key: "mp/abort", query: q("uploadId", aborted)})
	h.code(part("part-after-abort", "mp/abort", aborted, "2", small, nil), 404, "NoSuchUpload", "UploadPart")
	h.code(h.get("abort-key-missing", "mp/abort", nil, nil), 404, "NoSuchKey", "AbortMultipartUpload")

	h.seed("mp/source", []byte("0123456789"), nil)
	source := awsEncode("/"+h.bucket+"/mp/source", true)
	_, copyID := initiate("initiate-copy", "mp/copy", nil)
	res = part("copy-part-range", "mp/copy", copyID, "1", nil, hdr("X-Amz-Copy-Source", source, "X-Amz-Copy-Source-Range", "bytes=0-4"))
	h.status(res, 200, "UploadPartCopy")
	h.xmlIs(res, "CopyPartResult.ETag", quote(md5Hex([]byte("01234"))), "UploadPartCopy")
	h.eq(res, "last-modified-format", isoMillis(res.xml("CopyPartResult.LastModified")), true, "UploadPartCopy")
	res = part("copy-part-full", "mp/copy", copyID, "2", nil, hdr("X-Amz-Copy-Source", source))
	h.xmlIs(res, "CopyPartResult.ETag", quote(md5Hex([]byte("0123456789"))), "UploadPartCopy")
	h.code(part("copy-part-range-reversed", "mp/copy", copyID, "3", nil, hdr("X-Amz-Copy-Source", source, "X-Amz-Copy-Source-Range", "bytes=5-2")), 400, "InvalidArgument", "UploadPartCopy")
	h.code(part("copy-part-range-beyond", "mp/copy", copyID, "3", nil, hdr("X-Amz-Copy-Source", source, "X-Amz-Copy-Source-Range", "bytes=5-20")), 400, "InvalidArgument", "UploadPartCopy")
	h.code(part("copy-part-range-suffix", "mp/copy", copyID, "3", nil, hdr("X-Amz-Copy-Source", source, "X-Amz-Copy-Source-Range", "bytes=-3")), 400, "InvalidArgument", "UploadPartCopy")
	h.code(part("copy-part-if-match-wrong", "mp/copy", copyID, "3", nil, hdr("X-Amz-Copy-Source", source, "X-Amz-Copy-Source-If-Match", wrong)), 412, "PreconditionFailed", "UploadPartCopy")
	h.code(part("copy-part-missing-source", "mp/copy", copyID, "3", nil, hdr("X-Amz-Copy-Source", awsEncode("/"+h.bucket+"/mp/absent", true))), 404, "NoSuchKey", "UploadPartCopy")

	h.seed("mp/existing", []byte("existing"), nil)
	_, cond := initiate("initiate-conditional", "mp/existing", nil)
	c1 := part("conditional-part", "mp/existing", cond, "1", []byte("replacement"), nil)
	res = complete("complete-if-none-match-existing", "mp/existing", cond, completion(completedPart{1, c1.header("ETag"), ""}), hdr("If-None-Match", "*"))
	h.code(res, 412, "PreconditionFailed", "conditional-writes")
	h.bodyIs(h.get("conditional-unchanged", "mp/existing", nil, nil), "existing", "conditional-writes")

	res = h.do("attributes", request{method: "GET", bucket: h.bucket, key: key, query: q("attributes", ""), header: hdr("X-Amz-Object-Attributes", "ETag,ObjectSize,ObjectParts,StorageClass,Checksum")})
	h.status(res, 200, "GetObjectAttributes")
	h.xmlIs(res, "GetObjectAttributesResponse.ETag", strings.Trim(final, `"`), "GetObjectAttributes")
	h.xmlIs(res, "GetObjectAttributesResponse.ObjectSize", total, "GetObjectAttributes")
	h.xmlIs(res, "GetObjectAttributesResponse.StorageClass", "STANDARD", "GetObjectAttributes")
	h.xmlIs(res, "GetObjectAttributesResponse.ObjectParts.PartsCount", "2", "GetObjectAttributes")
	// Part details are listed only for objects uploaded with a checksum algorithm.
	h.present(res, "GetObjectAttributesResponse.ObjectParts.Part", false, "GetObjectAttributes")
	for _, field := range []string{"MaxParts", "PartNumberMarker", "IsTruncated"} {
		h.present(res, "GetObjectAttributesResponse.ObjectParts."+field, false, "GetObjectAttributes")
	}
	h.eq(res, "header:last-modified?", res.header("Last-Modified") != "", true, "GetObjectAttributes")
	h.code(h.do("attributes-missing-header", request{method: "GET", bucket: h.bucket, key: key, query: q("attributes", "")}), 400, "InvalidRequest", "GetObjectAttributes")
	h.code(h.do("attributes-missing-key", request{method: "GET", bucket: h.bucket, key: "mp/absent", query: q("attributes", ""), header: hdr("X-Amz-Object-Attributes", "ETag")}), 404, "NoSuchKey", "GetObjectAttributes")

	// Checksummed uploads: the algorithm is fixed at initiation and parts
	// must carry it; completion returns a composite value.
	res, sumID := initiate("initiate-crc32", "mp/crc32", hdr("X-Amz-Checksum-Algorithm", "CRC32"))
	h.status(res, 200, "CreateMultipartUpload")
	h.note(res, "header:x-amz-checksum-algorithm", res.header("x-amz-checksum-algorithm"))
	h.note(res, "header:x-amz-checksum-type", res.header("x-amz-checksum-type"))
	part("crc32-part-without-checksum", "mp/crc32", sumID, "1", small, nil)
	crc := crc32.ChecksumIEEE(small)
	value := b64([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})
	res = part("crc32-part", "mp/crc32", sumID, "1", small, hdr("X-Amz-Checksum-Crc32", value))
	h.status(res, 200, "UploadPart")
	h.headerIs(res, "x-amz-checksum-crc32", value, "checksums")
	h.code(part("crc32-part-wrong", "mp/crc32", sumID, "2", small, hdr("X-Amz-Checksum-Crc32", "AAAAAA==")), 400, "BadDigest", "checksums")
	res = complete("crc32-complete", "mp/crc32", sumID, completion(completedPart{1, res.header("ETag"), "<ChecksumCRC32>" + value + "</ChecksumCRC32>"}), nil)
	h.status(res, 200, "CompleteMultipartUpload")
	h.eq(res, "composite-suffix", strings.HasSuffix(res.xml("CompleteMultipartUploadResult.ChecksumCRC32"), "-1"), true, "checksums")
	h.xmlIs(res, "CompleteMultipartUploadResult.ChecksumType", "COMPOSITE", "checksums")
	res = h.head("crc32-head", "mp/crc32", hdr("X-Amz-Checksum-Mode", "ENABLED"), nil)
	h.headerIs(res, "x-amz-checksum-type", "COMPOSITE", "checksums")
	h.eq(res, "composite-suffix", strings.HasSuffix(res.header("x-amz-checksum-crc32"), "-1"), true, "checksums")
	digest, _, _ := strings.Cut(res.header("x-amz-checksum-crc32"), "-")
	res = h.do("attributes-checksummed", request{method: "GET", bucket: h.bucket, key: "mp/crc32", query: q("attributes", ""), header: hdr("X-Amz-Object-Attributes", "ObjectParts,Checksum")})
	h.xmlIs(res, "GetObjectAttributesResponse.Checksum.ChecksumCRC32", digest, "GetObjectAttributes")
	h.xmlIs(res, "GetObjectAttributesResponse.Checksum.ChecksumType", "COMPOSITE", "GetObjectAttributes")
	h.xmlIs(res, "GetObjectAttributesResponse.ObjectParts.MaxParts", "1000", "GetObjectAttributes")
	h.xmlIs(res, "GetObjectAttributesResponse.ObjectParts.PartNumberMarker", "0", "GetObjectAttributes")
	h.xmlIs(res, "GetObjectAttributesResponse.ObjectParts.IsTruncated", "false", "GetObjectAttributes")
	h.xmlIs(res, "GetObjectAttributesResponse.ObjectParts.PartsCount", "1", "GetObjectAttributes")
	h.xmlIs(res, "GetObjectAttributesResponse.ObjectParts.Part[0].PartNumber", "1", "GetObjectAttributes")
	h.xmlIs(res, "GetObjectAttributesResponse.ObjectParts.Part[0].Size", strconv.Itoa(len(small)), "GetObjectAttributes")
	h.xmlIs(res, "GetObjectAttributesResponse.ObjectParts.Part[0].ChecksumCRC32", value, "GetObjectAttributes")
	// Recorded only: AWS accepted an unknown checksum algorithm name.
	h.do("initiate-invalid-algorithm", request{method: "POST", bucket: h.bucket, key: "mp/bad", query: q("uploads", ""), header: hdr("X-Amz-Checksum-Algorithm", "MD5")})
}
