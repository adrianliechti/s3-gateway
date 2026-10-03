package gateway

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/adrianliechti/s3-gateway/backend"
)

func quote(s string) string { return `"` + s + `"` }
func conditions(r *http.Request) backend.Conditions {
	return backend.Conditions{IfMatch: strings.Trim(r.Header.Get("If-Match"), `"`), IfNoneMatch: strings.Trim(r.Header.Get("If-None-Match"), `"`)}
}
func objectMetadata(r *http.Request) backend.Object {
	o := backend.Object{ContentType: r.Header.Get("Content-Type"), CacheControl: r.Header.Get("Cache-Control"), ContentDisposition: r.Header.Get("Content-Disposition"), ContentEncoding: r.Header.Get("Content-Encoding"), ContentLanguage: r.Header.Get("Content-Language"), Expires: r.Header.Get("Expires"), Metadata: map[string]string{}}
	var enc []string
	for _, v := range strings.Split(o.ContentEncoding, ",") {
		v = strings.TrimSpace(v)
		if v != "" && v != "aws-chunked" {
			enc = append(enc, v)
		}
	}
	o.ContentEncoding = strings.Join(enc, ",")
	for k, vs := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
			o.Metadata[strings.TrimPrefix(strings.ToLower(k), "x-amz-meta-")] = strings.Join(vs, ",")
		}
	}
	return o
}
func headers(w http.ResponseWriter, o backend.Object, checksum bool) {
	h := w.Header()
	h.Set("ETag", quote(o.ETag))
	h.Set("Last-Modified", o.Modified.UTC().Format(http.TimeFormat))
	h.Set("Content-Type", o.ContentType)
	h.Set("Accept-Ranges", "bytes")
	if len(o.Tags) > 0 {
		h.Set("X-Amz-Tagging-Count", strconv.Itoa(len(o.Tags)))
	}
	for k, v := range map[string]string{"Cache-Control": o.CacheControl, "Content-Disposition": o.ContentDisposition, "Content-Encoding": o.ContentEncoding, "Content-Language": o.ContentLanguage, "Expires": o.Expires} {
		if v != "" {
			h.Set(k, v)
		}
	}
	for k, v := range o.Metadata {
		// Preserve lowercase metadata names on the wire. Some S3 clients derive
		// metadata keys from the original header casing rather than normalizing.
		h["x-amz-meta-"+strings.ToLower(k)] = []string{v}
	}
	if checksum {
		if o.ChecksumType != "" {
			h.Set("X-Amz-Checksum-Type", o.ChecksumType)
		}
		for k, v := range o.Checksums {
			h.Set("X-Amz-Checksum-"+k, v)
		}
	}
}
func etagMatches(value, etag string) bool {
	for _, s := range strings.Split(value, ",") {
		s = strings.TrimSpace(s)
		if s == "*" || s == quote(etag) {
			return true
		}
	}
	return false
}
func readConditions(r *http.Request, o backend.Object, prefix string) error {
	get := func(s string) string { return r.Header.Get(prefix + s) }
	if v := get("If-Match"); v != "" {
		if !etagMatches(v, o.ETag) {
			return backend.ErrPrecondition
		}
	} else if t, e := http.ParseTime(get("If-Unmodified-Since")); e == nil && o.Modified.Truncate(1e9).After(t) {
		return backend.ErrPrecondition
	}
	if v := get("If-None-Match"); v != "" {
		if etagMatches(v, o.ETag) {
			if prefix != "" {
				return backend.ErrPrecondition
			}
			return apiError("NotModified", 304, "")
		}
	} else if t, e := http.ParseTime(get("If-Modified-Since")); e == nil && !o.Modified.Truncate(1e9).After(t) {
		if prefix != "" {
			return backend.ErrPrecondition
		}
		return apiError("NotModified", 304, "")
	}
	return nil
}
func byteRange(value string, size int64) (int64, int64, error) {
	if value == "" {
		return 0, size, nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, apiError("InvalidRange", 416, "Invalid byte range")
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
	if len(parts) != 2 || size == 0 {
		return 0, 0, apiError("InvalidRange", 416, "Invalid byte range")
	}
	if parts[0] == "" {
		n, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || n <= 0 {
			return 0, 0, apiError("InvalidRange", 416, "Invalid suffix range")
		}
		if n > size {
			n = size
		}
		return size - n, n, nil
	}
	start, e := strconv.ParseInt(parts[0], 10, 64)
	if e != nil || start < 0 || start >= size {
		return 0, 0, apiError("InvalidRange", 416, "Range is outside object")
	}
	end := size - 1
	if parts[1] != "" {
		end, e = strconv.ParseInt(parts[1], 10, 64)
		if e != nil || end < start {
			return 0, 0, apiError("InvalidRange", 416, "Invalid range end")
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end - start + 1, nil
}
func (g *Gateway) object(w http.ResponseWriter, r *http.Request, b, k string, sig *signature) error {
	q := r.URL.Query()
	if q.Has("tagging") {
		return g.tagging(w, r, b, k, sig)
	}
	if q.Has("attributes") {
		return g.attributes(w, r, b, k)
	}
	if q.Has("acl") {
		return g.acl(w, r, b, k, sig)
	}
	if q.Has("versionId") && (r.Method != "GET" && r.Method != "HEAD" && r.Method != "DELETE" || q.Has("uploadId") || q.Has("uploads")) {
		return apiError("InvalidArgument", 400, "Version ID is not valid for this operation")
	}
	if q.Has("uploadId") {
		return g.multipartRequest(w, r, b, k, sig)
	}
	if q.Has("uploads") && r.Method == "POST" {
		return g.initiate(w, r, b, k, sig)
	}
	if q.Has("partNumber") && r.Method != "GET" && r.Method != "HEAD" {
		return apiError("InvalidArgument", 400, "Part number is not valid for this operation")
	}
	switch r.Method {
	case "PUT":
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			return g.copyObject(w, r, b, k)
		}
		meta := objectMetadata(r)
		acl, err := g.requestACL(r, nil)
		if err != nil {
			return err
		}
		meta.ACL = acl
		meta.Tags, err = requestTags(r)
		if err != nil {
			return err
		}
		size := 0
		for k, v := range meta.Metadata {
			size += len(k) + len(v)
		}
		if size > 2048 {
			return apiError("MetadataTooLarge", 400, "User metadata exceeds 2 KiB")
		}
		p, err := g.body(r, sig, maxObjectSize)
		if err != nil {
			return err
		}
		defer p.close()
		meta.ETag = p.etag
		meta.Checksums = p.checksums
		if len(meta.Checksums) > 0 {
			meta.ChecksumType = "FULL_OBJECT"
		}
		o, err := g.be.Put(r.Context(), b, k, p.file, p.size, backend.PutOptions{Object: meta, Conditions: conditions(r)})
		if err != nil {
			return err
		}
		w.Header().Set("ETag", quote(o.ETag))
		versionWriteHeaders(w, o)
		if o.ChecksumType != "" {
			w.Header().Set("X-Amz-Checksum-Type", o.ChecksumType)
		}
		for k, v := range o.Checksums {
			w.Header().Set("X-Amz-Checksum-"+k, v)
		}
		w.WriteHeader(200)
	case "GET", "HEAD":
		o, err := g.headVersion(w, r, b, k)
		if err != nil {
			return err
		}
		if err = readConditions(r, o, ""); err != nil {
			var ae *s3Error
			if errors.As(err, &ae) && ae.Status == 304 {
				w.Header().Set("ETag", quote(o.ETag))
				w.Header().Set("Last-Modified", o.Modified.UTC().Format(http.TimeFormat))
				w.WriteHeader(304)
				return nil
			}
			return err
		}
		off, n, sums, err := objectRange(r, o)
		if err != nil {
			if ae, ok := err.(*s3Error); ok && ae.Status == 416 {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", o.Size))
			}
			return err
		}
		var body io.ReadCloser
		if r.Method == "GET" {
			_, body, err = g.be.GetVersion(r.Context(), b, k, r.URL.Query().Get("versionId"), backend.ReadOptions{Offset: off, Length: n, Revision: o.Revision})
			if err != nil {
				return err
			}
			defer body.Close()
		}
		responseObject := o
		responseObject.Checksums = sums
		if len(sums) == 0 {
			responseObject.ChecksumType = ""
		}
		headers(w, responseObject, r.Header.Get("X-Amz-Checksum-Mode") == "ENABLED")
		if q.Has("partNumber") && len(o.Parts) > 0 {
			w.Header().Set("X-Amz-Mp-Parts-Count", strconv.Itoa(len(o.Parts)))
		}
		for q, h := range map[string]string{"response-content-type": "Content-Type", "response-content-language": "Content-Language", "response-expires": "Expires", "response-cache-control": "Cache-Control", "response-content-disposition": "Content-Disposition", "response-content-encoding": "Content-Encoding"} {
			if v := r.URL.Query().Get(q); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		status := 200
		if r.Header.Get("Range") != "" || q.Has("partNumber") && n > 0 {
			status = 206
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+n-1, o.Size))
		}
		w.WriteHeader(status)
		if body != nil {
			if _, err := io.CopyN(w, body, n); err != nil {
				panic(http.ErrAbortHandler)
			}
		}
	case "DELETE":
		o, err := g.be.DeleteVersion(r.Context(), b, k, q.Get("versionId"), conditions(r))
		if err != nil {
			return err
		}
		versionHeaders(w, o)
		w.WriteHeader(204)
	default:
		return apiError("MethodNotAllowed", 405, "Method not allowed")
	}
	return nil
}
func (g *Gateway) copySource(r *http.Request) (string, string, string, error) {
	rawSource := r.Header.Get("X-Amz-Copy-Source")
	version := ""
	if path, query, ok := strings.Cut(rawSource, "?"); ok {
		q, err := url.ParseQuery(query)
		if err != nil || len(q) != 1 || len(q["versionId"]) != 1 || q.Get("versionId") == "" {
			return "", "", "", apiError("InvalidArgument", 400, "Invalid copy source version")
		}
		rawSource, version = path, q.Get("versionId")
	}
	src, err := url.PathUnescape(rawSource)
	if err != nil {
		return "", "", "", apiError("InvalidArgument", 400, "Invalid copy source")
	}

	parts := strings.SplitN(strings.TrimPrefix(src, "/"), "/", 2)
	if len(parts) != 2 || !validBucket(parts[0]) || strings.HasPrefix(parts[1], backend.InternalPrefix) {
		return "", "", "", apiError("InvalidArgument", 400, "Invalid copy source")
	}
	return parts[0], parts[1], version, nil
}
func (g *Gateway) copyObject(w http.ResponseWriter, r *http.Request, b, k string) error {
	acl, err := g.requestACL(r, nil)
	if err != nil {
		return err
	}
	sb, sk, version, err := g.copySource(r)
	if err != nil {
		return err
	}
	o, err := g.be.HeadVersion(r.Context(), sb, sk, version)
	if err != nil {
		return err
	}
	if o.Size > maxObjectSize {
		return apiError("EntityTooLarge", 400, "Use multipart copy for objects larger than 5 GiB")
	}
	if err = readConditions(r, o, "X-Amz-Copy-Source-"); err != nil {
		return err
	}
	_, body, err := g.be.GetVersion(r.Context(), sb, sk, version, backend.ReadOptions{Length: -1, Revision: o.Revision})
	if err != nil {
		return err
	}
	defer body.Close()
	meta := o
	switch r.Header.Get("X-Amz-Metadata-Directive") {
	case "", "COPY":
	case "REPLACE":
		meta = objectMetadata(r)
	default:
		return apiError("InvalidArgument", 400, "Invalid metadata directive")
	}
	meta.ETag = ""
	meta.Checksums = nil
	meta.ChecksumType = ""
	meta.Parts = nil
	switch r.Header.Get("X-Amz-Tagging-Directive") {
	case "", "COPY":
		meta.Tags = o.Tags
	case "REPLACE":
		meta.Tags, err = requestTags(r)
		if err != nil {
			return err
		}
	default:
		return apiError("InvalidArgument", 400, "Invalid tagging directive")
	}
	meta.ACL = acl
	result, err := g.be.Put(r.Context(), b, k, body, o.Size, backend.PutOptions{Object: meta, Conditions: conditions(r)})
	if err != nil {
		return err
	}
	versionWriteHeaders(w, result)
	if o.VersionID != "" {
		w.Header().Set("X-Amz-Copy-Source-Version-Id", o.VersionID)
	}
	writeXML(w, 200, struct {
		XMLName            xml.Name `xml:"CopyObjectResult"`
		XMLNS              string   `xml:"xmlns,attr"`
		ETag, LastModified string
	}{XMLNS: xmlns, ETag: quote(result.ETag), LastModified: stamp(result.Modified)})
	return nil
}
