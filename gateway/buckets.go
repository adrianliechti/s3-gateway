package gateway

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
)

type owner = backend.Owner

func (g *Gateway) owner() owner {
	return owner{ID: sha256Hex(g.opts.AccessKey), DisplayName: g.opts.AccessKey}
}
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
func (g *Gateway) service(w http.ResponseWriter, r *http.Request) error {
	if r.Method != "GET" {
		return apiError("MethodNotAllowed", 405, "Method not allowed")
	}
	b, err := g.be.ListBuckets(r.Context())
	if err != nil {
		return err
	}
	type item struct {
		Name         string
		CreationDate string
	}
	out := struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		XMLNS   string   `xml:"xmlns,attr"`
		Owner   owner
		Buckets []item `xml:"Buckets>Bucket"`
	}{XMLNS: xmlns, Owner: g.owner()}
	for _, b := range b {
		if validBucket(b.Name) {
			out.Buckets = append(out.Buckets, item{b.Name, stamp(b.Created)})
		}
	}
	writeXML(w, 200, out)
	return nil
}
func (g *Gateway) bucket(w http.ResponseWriter, r *http.Request, b string, sig *signature) error {
	q := r.URL.Query()
	if q.Has("tagging") || q.Has("attributes") {
		return apiError("NotImplemented", 501, "This bucket subresource is not implemented")
	}
	if q.Has("acl") {
		return g.acl(w, r, b, "", sig)
	}
	if q.Has("location") && r.Method == "GET" {
		if err := g.be.HeadBucket(r.Context(), b); err != nil {
			return err
		}
		region := g.opts.Region
		if region == "us-east-1" {
			region = ""
		}
		writeXML(w, 200, struct {
			XMLName xml.Name `xml:"LocationConstraint"`
			XMLNS   string   `xml:"xmlns,attr"`
			Value   string   `xml:",chardata"`
		}{XMLNS: xmlns, Value: region})
		return nil
	}
	if q.Has("versioning") {
		return g.versioning(w, r, b, sig)
	}
	if q.Has("versions") && r.Method == "GET" {
		return g.listVersions(w, r, b)
	}
	if q.Has("uploads") && r.Method == "GET" {
		return g.listUploads(w, r, b)
	}
	if q.Has("delete") && r.Method == "POST" {
		return g.deleteObjects(w, r, b, sig)
	}
	switch r.Method {
	case "HEAD":
		if err := g.be.HeadBucket(r.Context(), b); err != nil {
			return err
		}
		w.WriteHeader(200)
	case "PUT":
		acl, err := g.requestACL(r, nil)
		if err != nil {
			return err
		}
		p, err := g.body(r, sig, maxXMLSize)
		if err != nil {
			return err
		}
		defer p.close()
		if p.size > 0 {
			var body struct {
				XMLName            xml.Name `xml:"CreateBucketConfiguration"`
				LocationConstraint string
			}
			if err := xml.NewDecoder(p.file).Decode(&body); err != nil {
				return apiError("MalformedXML", 400, "Invalid bucket configuration")
			}
			if body.LocationConstraint != "" && body.LocationConstraint != g.opts.Region {
				return apiError("InvalidLocationConstraint", 400, "Region does not match gateway")
			}
		}
		if err := g.be.CreateBucket(r.Context(), b); err != nil && !(g.opts.Region == "us-east-1" && errors.Is(err, backend.ErrBucketExists)) {
			return err
		}
		if err := g.be.UpdateBucketProperties(r.Context(), b, func(p *backend.BucketProperties) error { p.ACL = acl; return nil }); err != nil {
			return err
		}
		w.Header().Set("Location", "/"+b)
		w.WriteHeader(200)
	case "DELETE":
		if err := g.be.DeleteBucket(r.Context(), b); err != nil {
			return err
		}
		w.WriteHeader(204)
	case "GET":
		return g.listObjects(w, r, b)
	default:
		return apiError("MethodNotAllowed", 405, "Method not allowed")
	}
	return nil
}
func integer(q string, def, min, max int) (int, error) {
	if q == "" {
		return def, nil
	}
	v, err := strconv.Atoi(q)
	if err != nil || v < min {
		return 0, apiError("InvalidArgument", 400, "Invalid integer parameter")
	}
	if v > max {
		v = max
	}
	return v, nil
}

type listEntry struct {
	Key          string
	LastModified string
	ETag         string
	Size         int64
	StorageClass string
	Owner        *owner `xml:",omitempty"`
}
type commonPrefix struct{ Prefix string }
type cursor struct{ Bucket, Prefix, Delimiter, After string }

func (g *Gateway) listObjects(w http.ResponseWriter, r *http.Request, b string) error {
	q := r.URL.Query()
	v2 := q.Get("list-type") == "2"
	if q.Get("list-type") != "" && !v2 {
		return apiError("InvalidArgument", 400, "Invalid list-type")
	}
	n, err := integer(q.Get("max-keys"), 1000, 0, 1000)
	if err != nil {
		return err
	}
	prefix, delimiter := q.Get("prefix"), q.Get("delimiter")
	after := q.Get("marker")
	if v2 {
		after = q.Get("start-after")
	}
	if token := q.Get("continuation-token"); token != "" {
		raw, e := base64.RawURLEncoding.DecodeString(token)
		var c cursor
		if e != nil || json.Unmarshal(raw, &c) != nil || c.Bucket != b || c.Prefix != prefix || c.Delimiter != delimiter {
			return apiError("InvalidArgument", 400, "Invalid continuation token")
		}
		after = c.After
	}
	encoding := q.Get("encoding-type")
	if encoding != "" && encoding != "url" {
		return apiError("InvalidArgument", 400, "Invalid encoding-type")
	}
	encode := func(s string) string {
		if encoding == "url" {
			return awsEncode(s, true)
		}
		return s
	}
	type result struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		XMLNS                 string   `xml:"xmlns,attr"`
		Name, Prefix          string
		Delimiter             string `xml:",omitempty"`
		MaxKeys               int
		IsTruncated           bool
		Marker                *string `xml:",omitempty"`
		NextMarker            string  `xml:",omitempty"`
		KeyCount              *int    `xml:",omitempty"`
		ContinuationToken     *string `xml:",omitempty"`
		NextContinuationToken string  `xml:",omitempty"`
		StartAfter            string  `xml:",omitempty"`
		EncodingType          string  `xml:",omitempty"`
		Contents              []listEntry
		CommonPrefixes        []commonPrefix
	}
	out := result{XMLNS: xmlns, Name: b, Prefix: encode(prefix), Delimiter: encode(delimiter), MaxKeys: n, EncodingType: encoding}
	if v2 {
		if q.Has("continuation-token") {
			token := q.Get("continuation-token")
			out.ContinuationToken = &token
		}
		out.StartAfter = encode(q.Get("start-after"))
	} else {
		m := encode(after)
		out.Marker = &m
	}
	if err := g.be.HeadBucket(r.Context(), b); err != nil {
		return err
	}
	count := 0
	last := ""
	seen := ""
	scan := after
	if !strings.HasPrefix(prefix, backend.InternalPrefix) && n > 0 {
	outer:
		for {
			objects, next, e := g.be.List(r.Context(), b, prefix, scan, 1000)
			if e != nil {
				return e
			}
			for _, o := range objects {
				entry := o.Key
				isPrefix := false
				if delimiter != "" {
					if at := strings.Index(strings.TrimPrefix(o.Key, prefix), delimiter); at >= 0 {
						entry = prefix + strings.TrimPrefix(o.Key, prefix)[:at+len(delimiter)]
						isPrefix = true
					}
				}
				if entry <= after || entry == seen {
					continue
				}
				seen = entry
				if count == n {
					out.IsTruncated = true
					break outer
				}
				last = entry
				count++
				if isPrefix {
					out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{encode(entry)})
				} else {
					e := listEntry{Key: encode(o.Key), LastModified: stamp(o.Modified), ETag: quote(o.ETag), Size: o.Size, StorageClass: "STANDARD"}
					if !v2 || q.Get("fetch-owner") == "true" {
						own := g.owner()
						e.Owner = &own
					}
					out.Contents = append(out.Contents, e)
				}
			}
			if next == "" {
				break
			}
			scan = next
		}
	}
	if v2 {
		out.KeyCount = &count
		if out.IsTruncated {
			raw, _ := json.Marshal(cursor{b, prefix, delimiter, last})
			out.NextContinuationToken = base64.RawURLEncoding.EncodeToString(raw)
		}
	} else if out.IsTruncated {
		out.NextMarker = encode(last)
	}
	writeXML(w, 200, out)
	return nil
}
func (g *Gateway) deleteObjects(w http.ResponseWriter, r *http.Request, b string, sig *signature) error {
	if err := g.be.HeadBucket(r.Context(), b); err != nil {
		return err
	}
	p, err := g.body(r, sig, maxXMLSize)
	if err != nil {
		return err
	}
	defer p.close()
	var req struct {
		XMLName xml.Name `xml:"Delete"`
		Quiet   bool
		Objects []struct {
			Key              string
			VersionID        string `xml:"VersionId"`
			ETag             string
			Size             *int64
			LastModifiedTime string
		} `xml:"Object"`
	}
	if err = xml.NewDecoder(p.file).Decode(&req); err != nil || len(req.Objects) > 1000 || len(req.Objects) == 0 {
		return apiError("MalformedXML", 400, "Invalid Delete request")
	}
	type item struct {
		Key                   string
		VersionID             string `xml:"VersionId,omitempty"`
		DeleteMarker          bool   `xml:",omitempty"`
		DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
	}
	type failure struct {
		Key, Code, Message string
		VersionID          string `xml:"VersionId,omitempty"`
	}
	out := struct {
		XMLName xml.Name `xml:"DeleteResult"`
		XMLNS   string   `xml:"xmlns,attr"`
		Deleted []item
		Errors  []failure `xml:"Error"`
	}{XMLNS: xmlns}
	for _, obj := range req.Objects {
		var e error
		var deleted backend.Object
		switch {
		case strings.HasPrefix(obj.Key, backend.InternalPrefix):
			e = apiError("AccessDenied", 403, "Reserved namespace")
		case obj.Size != nil || obj.LastModifiedTime != "":
			e = apiError("NotImplemented", 501, "Size and date conditions on batch delete are not implemented")
		default:
			deleted, e = g.be.DeleteVersion(r.Context(), b, obj.Key, obj.VersionID, backend.Conditions{IfMatch: strings.Trim(obj.ETag, `"`)})
		}
		if e != nil {
			code := "InternalError"
			var ae *s3Error
			if errors.As(e, &ae) {
				code = ae.Code
			}
			if errors.Is(e, backend.ErrPrecondition) {
				code = "PreconditionFailed"
			}
			if errors.Is(e, backend.ErrNotFound) {
				code = "NoSuchKey"
			}
			if errors.Is(e, backend.ErrInvalidKey) {
				code = "InvalidArgument"
			}
			out.Errors = append(out.Errors, failure{Key: obj.Key, Code: code, Message: "Object deletion failed", VersionID: obj.VersionID})
		} else if !req.Quiet {
			result := item{Key: obj.Key, VersionID: obj.VersionID, DeleteMarker: deleted.DeleteMarker}
			if deleted.DeleteMarker {
				result.DeleteMarkerVersionID = deleted.VersionID
			}
			out.Deleted = append(out.Deleted, result)
		}
	}
	writeXML(w, 200, out)
	return nil
}
