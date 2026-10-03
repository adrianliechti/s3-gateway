package conformance_test

import "testing"

func TestConformanceRedirectMetadata(t *testing.T) {
	parallel(t)
	h := begin(t, "redirect-metadata")
	const header = "X-Amz-Website-Redirect-Location"
	res := h.put("put", "redirect", []byte("body"), hdr(header, "/destination"))
	h.status(res, 200, "PutObject")
	res = h.get("get", "redirect", nil, nil)
	h.status(res, 200, "GetObject")
	h.bodyIs(res, "body", "GetObject")
	h.headerIs(res, header, "/destination", "GetObject")
	h.headerIs(h.head("head", "redirect", nil, nil), header, "/destination", "HeadObject")
	for _, directive := range []string{"COPY", "REPLACE"} {
		for _, location := range []string{"", "https://example.com/new"} {
			step := directive
			if location != "" {
				step += "-explicit"
			}
			// An empty header value is rejected by AWS; omit it instead.
			headers := hdr("X-Amz-Copy-Source", "/"+h.bucket+"/redirect", "X-Amz-Metadata-Directive", directive)
			if location != "" {
				headers.Set(header, location)
			}
			res = h.put("copy-"+step, "copy-"+step, nil, headers)
			h.status(res, 200, "CopyObject")
			head := h.head("head-"+step, "copy-"+step, nil, nil)
			// Without the header the copy has no redirect, whatever the
			// directive (AWS confirmed for COPY and REPLACE).
			h.headerIs(head, header, location, "CopyObject")
		}
	}
	res = h.do("multipart-init", request{method: "POST", bucket: h.bucket, key: "multipart", query: q("uploads", ""), header: hdr(header, "/multipart-destination")})
	h.status(res, 200, "CreateMultipartUpload")
	id := res.xml("InitiateMultipartUploadResult.UploadId")
	if id == "" {
		t.Fatal("no upload ID")
	}
	res = h.do("multipart-part", request{method: "PUT", bucket: h.bucket, key: "multipart", query: q("uploadId", id, "partNumber", "1"), body: []byte("part")})
	h.status(res, 200, "UploadPart")
	res = h.do("multipart-complete", request{method: "POST", bucket: h.bucket, key: "multipart", query: q("uploadId", id), body: completion(completedPart{number: 1, etag: res.header("ETag")})})
	h.status(res, 200, "CompleteMultipartUpload")
	h.headerIs(h.head("multipart-head", "multipart", nil, nil), header, "/multipart-destination", "HeadObject")
	res = h.do("update-tags", request{method: "PUT", bucket: h.bucket, key: "multipart", query: q("tagging", ""), body: tagging([2]string{"kind", "redirect"}), contentMD5: true})
	h.status(res, 200, "PutObjectTagging")
	h.headerIs(h.head("head-after-tags", "multipart", nil, nil), header, "/multipart-destination", "HeadObject")
}
