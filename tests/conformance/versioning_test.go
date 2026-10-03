package conformance_test

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
)

func (h *H) setVersioning(status string) {
	h.t.Helper()
	if h.tg.bucket != "" && !h.tg.allowVersioning && !(status == "Enabled" && h.tg.versioning == "Enabled") {
		h.t.Skipf("changing versioning of %s to %s cannot be undone; set CONFORMANCE_ALLOW_VERSIONING=1 to run these steps", h.tg.bucket, status)
	}
	if h.tg.bucket != "" && status != h.tg.versioning && h.tg.versioning != "" {
		restore := h.tg.versioning
		h.t.Cleanup(func() {
			body := fmt.Sprintf(`<VersioningConfiguration xmlns="%s"><Status>%s</Status></VersioningConfiguration>`, xmlns, restore)
			_ = h.raw(request{method: "PUT", bucket: h.bucket, query: q("versioning", ""), body: []byte(body), contentMD5: true})
		})
	}
	body := fmt.Sprintf(`<VersioningConfiguration xmlns="%s"><Status>%s</Status></VersioningConfiguration>`, xmlns, status)
	res := h.raw(request{method: "PUT", bucket: h.bucket, query: q("versioning", ""), body: []byte(body), contentMD5: true})
	if res.Status != 200 {
		h.t.Fatalf("versioning %s: %d %s", status, res.Status, res.Code)
	}
}

func TestConformanceVersioning(t *testing.T) {
	parallel(t)
	h := begin(t, "versioning")
	const root = "ListVersionsResult"
	versions := func(step string, kv ...string) *response {
		return h.do(step, request{method: "GET", bucket: h.bucket, query: q(append([]string{"versions", ""}, kv...)...)})
	}
	var res *response
	// A never-versioned bucket holds a null version that survives enabling.
	nullVersion := h.unversioned("unversioned steps")
	if nullVersion {
		res = h.do("versioning-default", request{method: "GET", bucket: h.bucket, query: q("versioning", "")})
		h.status(res, 200, "GetBucketVersioning")
		h.present(res, "VersioningConfiguration", true, "GetBucketVersioning")
		h.present(res, "VersioningConfiguration.Status", false, "GetBucketVersioning")
		res = h.put("unversioned-put", "k", []byte("plain"), nil)
		h.status(res, 200, "PutObject")
		h.headerIs(res, "x-amz-version-id", "", "versioning")
		res = versions("unversioned-list")
		h.status(res, 200, "ListObjectVersions")
		h.xmlIs(res, root+".Version[0].Key", "k", "ListObjectVersions")
		h.xmlIs(res, root+".Version[0].VersionId", "null", "ListObjectVersions")
		h.xmlIs(res, root+".Version[0].IsLatest", "true", "ListObjectVersions")
		h.xmlIs(res, root+".Version[0].ETag", quote(md5Hex([]byte("plain"))), "ListObjectVersions")
		h.xmlIs(res, root+".Version[0].Size", "5", "ListObjectVersions")
		h.xmlIs(res, root+".Version[0].StorageClass", "STANDARD", "ListObjectVersions")
		h.present(res, root+".Version[0].Owner.ID", true, "ListObjectVersions")
		h.xmlIs(res, root+".MaxKeys", "1000", "ListObjectVersions")
		h.xmlIs(res, root+".IsTruncated", "false", "ListObjectVersions")
		h.present(res, root+".KeyMarker", true, "ListObjectVersions")
		h.present(res, root+".VersionIdMarker", true, "ListObjectVersions")
		res = h.get("unversioned-get-null", "k", nil, q("versionId", "null"))
		h.status(res, 200, "GetObject")
		h.headerIs(res, "x-amz-version-id", "null", "GetObject")
		// Recorded only: AWS validates version id syntax before lookup.
		h.get("get-bogus-version", "k", nil, q("versionId", "bogus"))
	}
	history := func(ids ...string) string {
		if nullVersion {
			ids = append(ids, "null")
		}
		return strings.Join(ids, ",")
	}
	latest := func(flags ...string) string {
		if nullVersion {
			flags = append(flags, "false")
		}
		return strings.Join(flags, ",")
	}

	h.setVersioning("Enabled")
	configure := func(status string) []byte {
		return []byte(fmt.Sprintf(`<VersioningConfiguration xmlns="%s"><Status>%s</Status></VersioningConfiguration>`, xmlns, status))
	}
	res = h.do("versioning-enabled", request{method: "GET", bucket: h.bucket, query: q("versioning", "")})
	h.xmlIs(res, "VersioningConfiguration.Status", "Enabled", "GetBucketVersioning")
	res = h.do("versioning-invalid-status", request{method: "PUT", bucket: h.bucket, query: q("versioning", ""), body: configure("Paused"), contentMD5: true})
	h.code(res, 400, "MalformedXML", "PutBucketVersioning")
	res = h.do("versioning-malformed", request{method: "PUT", bucket: h.bucket, query: q("versioning", ""), body: []byte("<Versioning"), contentMD5: true})
	h.code(res, 400, "MalformedXML", "PutBucketVersioning")
	v1 := h.put("put-v1", "k", []byte("version one"), nil)
	h.status(v1, 200, "PutObject")
	id1 := v1.header("x-amz-version-id")
	h.eq(v1, "version-id-assigned", id1 != "" && id1 != "null", true, "versioning")
	v2 := h.put("put-v2", "k", []byte("version two"), nil)
	id2 := v2.header("x-amz-version-id")
	h.eq(v2, "version-id-unique", id2 != "" && id2 != id1, true, "versioning")
	res = h.get("get-latest", "k", nil, nil)
	h.bodyIs(res, "version two", "versioning")
	h.headerIs(res, "x-amz-version-id", id2, "GetObject")
	res = h.get("get-v1", "k", nil, q("versionId", id1))
	h.bodyIs(res, "version one", "GetObject")
	h.headerIs(res, "x-amz-version-id", id1, "GetObject")
	res = h.head("head-v1", "k", nil, q("versionId", id1))
	h.status(res, 200, "HeadObject")
	h.headerIs(res, "x-amz-version-id", id1, "HeadObject")
	if nullVersion {
		res = h.get("get-null-after-enable", "k", nil, q("versionId", "null"))
		h.bodyIs(res, "plain", "versioning")
	}
	res = versions("list-enabled", "prefix", "k")
	h.eq(res, "version-order", strings.Join(keysOf(res, root, "Version", "VersionId"), ","), history(id2, id1), "ListObjectVersions")
	h.eq(res, "is-latest", strings.Join(keysOf(res, root, "Version", "IsLatest"), ","), latest("true", "false"), "ListObjectVersions")
	// A syntactically invalid version id is rejected before lookup.
	res = h.get("get-missing-version", "k", nil, q("versionId", "null"+id1))
	h.code(res, 400, "InvalidArgument", "GetObject")

	del := h.delete("delete-marker", "k", nil, nil)
	h.status(del, 204, "DeleteObject")
	h.headerIs(del, "x-amz-delete-marker", "true", "delete-markers")
	marker := del.header("x-amz-version-id")
	h.eq(del, "marker-id-assigned", marker != "" && marker != "null", true, "delete-markers")
	res = h.get("get-after-marker", "k", nil, nil)
	h.code(res, 404, "NoSuchKey", "delete-markers")
	h.headerIs(res, "x-amz-delete-marker", "true", "delete-markers")
	res = h.head("head-after-marker", "k", nil, nil)
	h.status(res, 404, "delete-markers")
	h.headerIs(res, "x-amz-delete-marker", "true", "delete-markers")
	res = h.get("get-marker-version", "k", nil, q("versionId", marker))
	h.code(res, 405, "MethodNotAllowed", "delete-markers")
	h.headerIs(res, "x-amz-delete-marker", "true", "delete-markers")
	res = h.do("list-after-marker", request{method: "GET", bucket: h.bucket, query: q("list-type", "2")})
	h.present(res, "ListBucketResult.Contents", false, "delete-markers")
	res = versions("list-with-marker", "prefix", "k")
	h.xmlIs(res, root+".DeleteMarker[0].Key", "k", "ListObjectVersions")
	h.xmlIs(res, root+".DeleteMarker[0].VersionId", marker, "ListObjectVersions")
	h.xmlIs(res, root+".DeleteMarker[0].IsLatest", "true", "ListObjectVersions")
	h.present(res, root+".DeleteMarker[0].Owner.ID", true, "ListObjectVersions")
	h.present(res, root+".DeleteMarker[0].Size", false, "ListObjectVersions")
	h.present(res, root+".DeleteMarker[0].ETag", false, "ListObjectVersions")
	h.eq(res, "is-latest", strings.Join(keysOf(res, root, "Version", "IsLatest"), ","), latest("false", "false"), "ListObjectVersions")
	res = h.get("get-v1-behind-marker", "k", nil, q("versionId", id1))
	h.status(res, 200, "delete-markers")
	res = h.delete("remove-marker", "k", nil, q("versionId", marker))
	h.status(res, 204, "DeleteObject")
	h.headerIs(res, "x-amz-delete-marker", "true", "DeleteObject")
	h.headerIs(res, "x-amz-version-id", marker, "DeleteObject")
	res = h.get("get-after-marker-removed", "k", nil, nil)
	h.bodyIs(res, "version two", "delete-markers")
	h.headerIs(res, "x-amz-version-id", id2, "delete-markers")
	res = h.delete("delete-v1", "k", nil, q("versionId", id1))
	h.status(res, 204, "DeleteObject")
	h.headerIs(res, "x-amz-version-id", id1, "DeleteObject")
	h.headerIs(res, "x-amz-delete-marker", "", "DeleteObject")
	res = h.get("get-deleted-v1", "k", nil, q("versionId", id1))
	h.code(res, 404, "NoSuchVersion", "GetObject")
	res = h.delete("delete-absent-key-versioned", "absent", nil, nil)
	h.status(res, 204, "DeleteObject")
	h.headerIs(res, "x-amz-delete-marker", "true", "delete-markers")
	res = versions("list-absent-marker", "prefix", "absent")
	h.xmlIs(res, root+".DeleteMarker[0].Key", "absent", "delete-markers")
	res = h.delete("delete-v1-again", "k", nil, q("versionId", id1))
	h.note(res, "status", res.Status)
	h.note(res, "code", res.Code)

	source := awsEncode("/"+h.bucket+"/k", true)
	res = h.put("copy-version", "copied", nil, hdr("X-Amz-Copy-Source", source+"?versionId="+id2))
	h.status(res, 200, "CopyObject")
	h.headerIs(res, "x-amz-copy-source-version-id", id2, "CopyObject")
	h.eq(res, "version-id-assigned", res.header("x-amz-version-id") != "" && res.header("x-amz-version-id") != "null", true, "CopyObject")
	h.bodyIs(h.get("copy-version-get", "copied", nil, nil), "version two", "CopyObject")
	res = h.put("copy-missing-version", "copied", nil, hdr("X-Amz-Copy-Source", source+"?versionId="+id1))
	h.code(res, 404, "NoSuchVersion", "CopyObject")
	res = h.put("tag-version", "copied", nil, nil)
	_ = res

	h.setVersioning("Suspended")
	res = h.do("versioning-suspended", request{method: "GET", bucket: h.bucket, query: q("versioning", "")})
	h.xmlIs(res, "VersioningConfiguration.Status", "Suspended", "GetBucketVersioning")
	res = h.put("suspended-put", "k", []byte("suspended"), nil)
	h.status(res, 200, "suspended")
	h.note(res, "header:x-amz-version-id", res.header("x-amz-version-id"))
	res = versions("suspended-list", "prefix", "k")
	h.eq(res, "version-order", strings.Join(keysOf(res, root, "Version", "VersionId"), ","), "null,"+id2, "suspended")
	h.xmlIs(res, root+".Version[0].IsLatest", "true", "suspended")
	res = h.put("suspended-put-again", "k", []byte("suspended again"), nil)
	h.status(res, 200, "suspended")
	res = versions("suspended-list-again", "prefix", "k")
	h.eq(res, "version-count", len(keysOf(res, root, "Version", "VersionId")), 2, "suspended")
	h.bodyIs(h.get("suspended-get-null", "k", nil, q("versionId", "null")), "suspended again", "suspended")
	res = h.delete("suspended-delete", "k", nil, nil)
	h.status(res, 204, "suspended")
	h.headerIs(res, "x-amz-delete-marker", "true", "suspended")
	h.headerIs(res, "x-amz-version-id", "null", "suspended")
	res = versions("suspended-list-marker", "prefix", "k")
	h.xmlIs(res, root+".DeleteMarker[0].VersionId", "null", "suspended")
	h.eq(res, "version-order", strings.Join(keysOf(res, root, "Version", "VersionId"), ","), id2, "suspended")

	h.setVersioning("Enabled")
	for i := 0; i < 3; i++ {
		h.seed("paged", []byte(fmt.Sprint(i)), nil)
	}
	var walk []string
	keyMarker, versionMarker := "", ""
	for page := 0; page < 10; page++ {
		kv := []string{"prefix", "paged", "max-keys", "1"}
		if keyMarker != "" {
			kv = append(kv, "key-marker", keyMarker, "version-id-marker", versionMarker)
		}
		res = versions(fmt.Sprintf("paged-%d", page), kv...)
		h.status(res, 200, "ListObjectVersions")
		walk = append(walk, keysOf(res, root, "Version", "VersionId")...)
		if res.xml(root+".IsTruncated") != "true" {
			h.present(res, root+".NextKeyMarker", false, "ListObjectVersions")
			break
		}
		h.xmlIs(res, root+".NextKeyMarker", "paged", "ListObjectVersions")
		h.present(res, root+".NextVersionIdMarker", true, "ListObjectVersions")
		keyMarker, versionMarker = res.xml(root+".NextKeyMarker"), res.xml(root+".NextVersionIdMarker")
	}
	h.eq(res, "walk-count", len(walk), 3, "ListObjectVersions")
	full := keysOf(versions("paged-all", "prefix", "paged"), root, "Version", "VersionId")
	h.eq(res, "walk-order", strings.Join(walk, ","), strings.Join(full, ","), "ListObjectVersions")
	h.code(versions("version-marker-without-key", "version-id-marker", "x"), 400, "InvalidArgument", "ListObjectVersions")
	res = versions("list-delimiter", "delimiter", "/")
	h.status(res, 200, "ListObjectVersions")
	h.xmlIs(res, root+".Delimiter", "/", "ListObjectVersions")
	res = versions("list-encoding", "encoding-type", "url", "prefix", "pa")
	h.xmlIs(res, root+".EncodingType", "url", "ListObjectVersions")
	h.xmlIs(res, root+".Prefix", "pa", "ListObjectVersions")
}

func TestConformanceBatchDelete(t *testing.T) {
	parallel(t)
	h := begin(t, "batch-delete")
	body := func(quiet bool, objects ...[2]string) []byte {
		var b strings.Builder
		fmt.Fprintf(&b, `<Delete xmlns="%s"><Quiet>%v</Quiet>`, xmlns, quiet)
		for _, o := range objects {
			b.WriteString("<Object><Key>" + o[0] + "</Key>")
			if o[1] != "" {
				b.WriteString("<VersionId>" + o[1] + "</VersionId>")
			}
			b.WriteString("</Object>")
		}
		b.WriteString("</Delete>")
		return []byte(b.String())
	}
	del := func(step string, payload []byte, md5 bool) *response {
		return h.do(step, request{method: "POST", bucket: h.bucket, query: q("delete", ""), body: payload, contentMD5: md5})
	}
	for _, k := range []string{"a", "b", "c"} {
		h.seed(k, []byte(k), nil)
	}
	res := del("delete-verbose", body(false, [2]string{"a", ""}, [2]string{"absent", ""}), true)
	h.status(res, 200, "DeleteObjects")
	h.xmlIs(res, "DeleteResult.@xmlns", xmlns, "DeleteObjects")
	// Result order is not specified.
	deleted := keysOf(res, "DeleteResult", "Deleted", "Key")
	sort.Strings(deleted)
	h.eq(res, "deleted", strings.Join(deleted, ","), "a,absent", "DeleteObjects")
	h.present(res, "DeleteResult.Error", false, "DeleteObjects")
	// Only versioned buckets create delete markers.
	h.present(res, "DeleteResult.Deleted[0].DeleteMarker", h.tg.versioning != "", "DeleteObjects")
	res = del("delete-quiet", body(true, [2]string{"b", ""}), true)
	h.status(res, 200, "DeleteObjects")
	h.present(res, "DeleteResult.Deleted", false, "DeleteObjects")
	h.present(res, "DeleteResult.Error", false, "DeleteObjects")
	h.code(h.get("quiet-deleted", "b", nil, nil), 404, "NoSuchKey", "DeleteObjects")
	// Content-MD5 (or a checksum) is required for DeleteObjects.
	res = del("delete-without-md5", body(true, [2]string{"c", ""}), false)
	h.code(res, 400, "InvalidRequest", "DeleteObjects")
	h.code(del("delete-empty", body(false), true), 400, "MalformedXML", "DeleteObjects")
	h.code(del("delete-malformed", []byte("<Delete"), true), 400, "MalformedXML", "DeleteObjects")
	var many [][2]string
	for i := 0; i < 1001; i++ {
		many = append(many, [2]string{fmt.Sprintf("many-%04d", i), ""})
	}
	h.code(del("delete-too-many", body(true, many...), true), 400, "MalformedXML", "DeleteObjects")
	res = del("delete-reserved-key", body(false, [2]string{".gateway/internal", ""}), true)
	h.note(res, "status", res.Status)
	h.note(res, "error-code", res.xml("DeleteResult.Error[0].Code"))

	h.setVersioning("Enabled")
	v := h.seed("v", []byte("v"), nil)
	id := v.header("x-amz-version-id")
	res = del("delete-versioned", body(false, [2]string{"v", ""}, [2]string{"c", id}), true)
	h.status(res, 200, "DeleteObjects")
	// Entries are not returned in request order; locate the one for "v".
	entry := fmt.Sprintf("DeleteResult.Deleted[%d]", slices.Index(keysOf(res, "DeleteResult", "Deleted", "Key"), "v"))
	h.xmlIs(res, entry+".Key", "v", "DeleteObjects")
	h.xmlIs(res, entry+".DeleteMarker", "true", "DeleteObjects")
	h.present(res, entry+".DeleteMarkerVersionId", true, "DeleteObjects")
	// Recorded only: the outcome for a well-formed but unknown version id.
	h.note(res, "unknown-version-error?", res.has("DeleteResult.Error[0]"))
	h.note(res, "unknown-version-error-code", res.xml("DeleteResult.Error[0].Code"))
	h.note(res, "unknown-version-deleted?", res.has("DeleteResult.Deleted[1]"))
	res = del("delete-specific-version", body(false, [2]string{"v", id}), true)
	h.xmlIs(res, "DeleteResult.Deleted[0].Key", "v", "DeleteObjects")
	h.xmlIs(res, "DeleteResult.Deleted[0].VersionId", id, "DeleteObjects")
	h.present(res, "DeleteResult.Deleted[0].DeleteMarker", false, "DeleteObjects")
}
