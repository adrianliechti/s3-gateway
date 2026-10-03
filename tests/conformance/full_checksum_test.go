package conformance_test

import (
	"encoding/base64"
	"hash"
	"hash/crc32"
	"testing"

	"github.com/minio/crc64nvme"
)

func TestConformanceMultipartFullChecksums(t *testing.T) {
	parallel(t)
	h := begin(t, "multipart-full-checksums")
	for _, alg := range []string{"CRC32", "CRC32C", "CRC64NVME"} {
		key := "full-checksum-" + alg
		var sum hash.Hash = crc32.NewIEEE()
		if alg == "CRC32C" {
			sum = crc32.New(crc32.MakeTable(crc32.Castagnoli))
		}
		if alg == "CRC64NVME" {
			sum = crc64nvme.New()
		}
		data := []byte("full object checksum")
		sum.Write(data)
		value := base64.StdEncoding.EncodeToString(sum.Sum(nil))
		res := h.do("init-"+alg, request{method: "POST", bucket: h.bucket, key: key, query: q("uploads", ""), header: hdr("X-Amz-Checksum-Algorithm", alg, "X-Amz-Checksum-Type", "FULL_OBJECT")})
		h.status(res, 200, "CreateMultipartUpload")
		id := res.xml("InitiateMultipartUploadResult.UploadId")
		if id == "" {
			t.Fatal("no upload ID")
		}
		res = h.do("part-"+alg, request{method: "PUT", bucket: h.bucket, key: key, query: q("uploadId", id, "partNumber", "1"), body: data, header: hdr("X-Amz-Checksum-"+alg, value)})
		h.status(res, 200, "UploadPart")
		res = h.do("complete-"+alg, request{method: "POST", bucket: h.bucket, key: key, query: q("uploadId", id), body: completion(completedPart{1, res.header("ETag"), "<Checksum" + alg + ">" + value + "</Checksum" + alg + ">"}), header: hdr("X-Amz-Checksum-"+alg, value, "X-Amz-Checksum-Type", "FULL_OBJECT")})
		h.status(res, 200, "CompleteMultipartUpload")
		h.xmlIs(res, "CompleteMultipartUploadResult.Checksum"+alg, value, "CompleteMultipartUpload")
		h.xmlIs(res, "CompleteMultipartUploadResult.ChecksumType", "FULL_OBJECT", "CompleteMultipartUpload")
		res = h.do("attributes-"+alg, request{method: "GET", bucket: h.bucket, key: key, query: q("attributes", ""), header: hdr("X-Amz-Object-Attributes", "ObjectParts,Checksum")})
		h.status(res, 200, "GetObjectAttributes")
		h.xmlIs(res, "GetObjectAttributesResponse.Checksum.Checksum"+alg, value, "GetObjectAttributes")
		h.xmlIs(res, "GetObjectAttributesResponse.Checksum.ChecksumType", "FULL_OBJECT", "GetObjectAttributes")
		h.xmlIs(res, "GetObjectAttributesResponse.ObjectParts.PartsCount", "1", "GetObjectAttributes")
		for _, field := range []string{"Part", "MaxParts", "PartNumberMarker", "IsTruncated"} {
			h.present(res, "GetObjectAttributesResponse.ObjectParts."+field, false, "GetObjectAttributes")
		}
		res = h.head("head-"+alg, key, hdr("X-Amz-Checksum-Mode", "ENABLED"), nil)
		h.status(res, 200, "HeadObject")
		h.headerIs(res, "X-Amz-Checksum-"+alg, value, "HeadObject")
		h.headerIs(res, "X-Amz-Checksum-Type", "FULL_OBJECT", "HeadObject")
	}
}
