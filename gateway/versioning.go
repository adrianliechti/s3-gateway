package gateway

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/adrianliechti/s3-gateway/backend"
)

func versionHeaders(w http.ResponseWriter, o backend.Object) {
	if o.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", o.VersionID)
	}
	if o.DeleteMarker {
		w.Header().Set("X-Amz-Delete-Marker", "true")
		w.Header().Set("Last-Modified", o.Modified.UTC().Format(http.TimeFormat))
	}
}

// Successful writes advertise a newly assigned version only when versioning
// is enabled. Suspended writes still replace the addressable null version.
func versionWriteHeaders(w http.ResponseWriter, o backend.Object) {
	if o.VersionID != "null" {
		versionHeaders(w, o)
	}
}
func (g *Gateway) headVersion(w http.ResponseWriter, r *http.Request, b, k string) (backend.Object, error) {
	o, err := g.be.HeadVersion(r.Context(), b, k, r.URL.Query().Get("versionId"))
	versionHeaders(w, o)
	return o, err
}
func (g *Gateway) versioning(w http.ResponseWriter, r *http.Request, b string, sig *signature) error {
	type configuration struct {
		XMLName   xml.Name `xml:"VersioningConfiguration"`
		XMLNS     string   `xml:"xmlns,attr,omitempty"`
		Status    string   `xml:",omitempty"`
		MFADelete string   `xml:"MfaDelete,omitempty"`
	}
	switch r.Method {
	case "GET":
		p, err := g.be.GetBucketProperties(r.Context(), b)
		if err != nil {
			return err
		}
		writeXML(w, 200, configuration{XMLNS: xmlns, Status: p.Versioning})
	case "PUT":
		p, err := g.body(r, sig, maxXMLSize)
		if err != nil {
			return err
		}
		defer p.close()
		var config configuration
		if err = xml.NewDecoder(p.file).Decode(&config); err != nil {
			return apiError("MalformedXML", 400, "Invalid versioning configuration")
		}
		if config.MFADelete != "" && config.MFADelete != "Disabled" {
			return apiError("NotImplemented", 501, "MFA Delete is not supported")
		}
		if config.Status != "Enabled" && config.Status != "Suspended" {
			return apiError("MalformedXML", 400, "Versioning status must be Enabled or Suspended")
		}
		if err = g.be.UpdateBucketProperties(r.Context(), b, func(p *backend.BucketProperties) error { p.Versioning = config.Status; return nil }); err != nil {
			return err
		}
		w.WriteHeader(200)
	default:
		return apiError("MethodNotAllowed", 405, "Method not allowed")
	}
	return nil
}
func (g *Gateway) listVersions(w http.ResponseWriter, r *http.Request, b string) error {
	q := r.URL.Query()
	prefix, delimiter := q.Get("prefix"), q.Get("delimiter")
	keyMarker, versionMarker := q.Get("key-marker"), q.Get("version-id-marker")
	if versionMarker != "" && keyMarker == "" {
		return apiError("InvalidArgument", 400, "Version marker requires key marker")
	}
	limit, err := integer(q.Get("max-keys"), 1000, 0, 1000)
	if err != nil {
		return err
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
	type entry struct {
		XMLName      xml.Name
		Key          string
		VersionID    string `xml:"VersionId"`
		IsLatest     bool
		LastModified string
		ETag         string `xml:",omitempty"`
		Size         *int64 `xml:",omitempty"`
		StorageClass string `xml:",omitempty"`
		Owner        owner
	}
	out := struct {
		XMLName             xml.Name `xml:"ListVersionsResult"`
		XMLNS               string   `xml:"xmlns,attr"`
		Name, Prefix        string
		Delimiter           string `xml:",omitempty"`
		KeyMarker           string
		VersionIDMarker     string `xml:"VersionIdMarker"`
		NextKeyMarker       string `xml:",omitempty"`
		NextVersionIDMarker string `xml:"NextVersionIdMarker,omitempty"`
		MaxKeys             int
		IsTruncated         bool
		EncodingType        string  `xml:",omitempty"`
		Entries             []entry `xml:",any"`
		CommonPrefixes      []commonPrefix
	}{XMLNS: xmlns, Name: b, Prefix: encode(prefix), Delimiter: encode(delimiter), KeyMarker: encode(keyMarker), VersionIDMarker: versionMarker, MaxKeys: limit, EncodingType: encoding}
	versions, err := g.be.ListVersions(r.Context(), b, prefix)
	if err != nil {
		return err
	}
	markerIndex := -1
	if versionMarker != "" {
		for i, v := range versions {
			if v.Object.Key == keyMarker && v.Object.VersionID == versionMarker {
				markerIndex = i
				break
			}
		}
	}
	count, lastPrefix := 0, ""
	for i, v := range versions {
		o := v.Object
		if o.Key < keyMarker || o.Key == keyMarker && (versionMarker == "" || markerIndex < 0 || i <= markerIndex) {
			continue
		}
		group := ""
		if delimiter != "" {
			if at := strings.Index(strings.TrimPrefix(o.Key, prefix), delimiter); at >= 0 {
				group = prefix + strings.TrimPrefix(o.Key, prefix)[:at+len(delimiter)]
			}
		}
		if group != "" && (group <= keyMarker || group == lastPrefix) {
			continue
		}
		if count == limit {
			out.IsTruncated = limit > 0
			break
		}
		count++
		if group != "" {
			out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{encode(group)})
			lastPrefix = group
			out.NextKeyMarker, out.NextVersionIDMarker = encode(group), ""
			continue
		}
		e := entry{XMLName: xml.Name{Local: "Version"}, Key: encode(o.Key), VersionID: o.VersionID, IsLatest: v.IsLatest, LastModified: stamp(o.Modified), Owner: g.normalizeACL(o.ACL).Owner}
		if o.DeleteMarker {
			e.XMLName.Local = "DeleteMarker"
		} else {
			size := o.Size
			e.Size = &size
			e.ETag = quote(o.ETag)
			e.StorageClass = "STANDARD"
		}
		out.Entries = append(out.Entries, e)
		out.NextKeyMarker, out.NextVersionIDMarker = encode(o.Key), o.VersionID
	}
	if !out.IsTruncated {
		out.NextKeyMarker, out.NextVersionIDMarker = "", ""
	}
	writeXML(w, 200, out)
	return nil
}
