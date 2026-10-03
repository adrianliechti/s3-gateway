package gateway

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	"github.com/adrianliechti/s3-gateway/backend"
)

// objectRange also supplies the part's checksum, never the whole object's
// checksum for a partial response. Old multipart objects without stored part
// boundaries remain readable normally but cannot be split by part number.
func objectRange(r *http.Request, o backend.Object) (offset, size int64, sums map[string]string, err error) {
	q := r.URL.Query()
	if !q.Has("partNumber") {
		offset, size, err = byteRange(r.Header.Get("Range"), o.Size)
		if err == errMalformedRange {
			// S3 ignores malformed ranges and returns an ordinary full response.
			r.Header.Del("Range")
			offset, size, err = 0, o.Size, nil
		}
		if r.Header.Get("Range") == "" {
			sums = o.Checksums
		}
		return
	}
	if r.Header.Get("Range") != "" {
		err = apiError("InvalidRequest", 400, "Range and partNumber cannot be combined")
		return
	}
	n, e := strconv.Atoi(q.Get("partNumber"))
	if e != nil || n < 1 || n > 10000 {
		err = apiError("InvalidArgument", 400, "Part number must be 1 to 10000")
		return
	}
	if len(o.Parts) == 0 {
		if strings.Contains(o.ETag, "-") {
			err = apiError("NotImplemented", 501, "Part boundaries are unavailable for this existing multipart object")
			return
		}
		if n == 1 {
			return 0, o.Size, o.Checksums, nil
		}
	} else {
		for _, part := range o.Parts {
			if part.Number == n {
				return offset, part.Size, part.Checksums, nil
			}
			offset += part.Size
		}
	}
	err = apiError("InvalidPartNumber", 416, "The requested part does not exist")
	return
}
func (g *Gateway) attributes(w http.ResponseWriter, r *http.Request, b, k string) error {
	if r.Method != "GET" {
		return apiError("MethodNotAllowed", 405, "Method not allowed")
	}
	selected := map[string]bool{}
	if strings.TrimSpace(strings.Join(r.Header.Values("X-Amz-Object-Attributes"), ",")) == "" {
		return apiError("InvalidRequest", 400, "Object attributes are required")
	}
	for _, name := range strings.Split(strings.Join(r.Header.Values("X-Amz-Object-Attributes"), ","), ",") {
		name = strings.TrimSpace(name)
		switch name {
		case "ETag", "Checksum", "ObjectParts", "StorageClass", "ObjectSize":
			selected[name] = true
		default:
			return apiError("InvalidArgument", 400, "Invalid or missing object attributes")
		}
	}
	maxParts, err := integer(r.Header.Get("X-Amz-Max-Parts"), 1000, 0, 1000)
	if err != nil {
		return err
	}
	marker, err := integer(r.Header.Get("X-Amz-Part-Number-Marker"), 0, 0, 10000)
	if err != nil {
		return err
	}
	o, err := g.headVersion(w, r, b, k)
	if err != nil {
		return err
	}
	if err = readConditions(r, o, ""); err != nil {
		if e, ok := err.(*s3Error); ok && e.Status == 304 {
			w.WriteHeader(304)
			return nil
		}
		return err
	}
	type checksum struct {
		checksumValues
		ChecksumType string `xml:",omitempty"`
	}
	type part struct {
		checksumValues
		PartNumber int
		Size       int64
	}
	type parts struct {
		PartsCount           int
		PartNumberMarker     *int   `xml:",omitempty"`
		NextPartNumberMarker int    `xml:",omitempty"`
		MaxParts             *int   `xml:",omitempty"`
		IsTruncated          *bool  `xml:",omitempty"`
		Parts                []part `xml:"Part"`
	}
	out := struct {
		XMLName      xml.Name  `xml:"GetObjectAttributesResponse"`
		XMLNS        string    `xml:"xmlns,attr"`
		ETag         string    `xml:",omitempty"`
		Checksum     *checksum `xml:",omitempty"`
		ObjectParts  *parts    `xml:",omitempty"`
		StorageClass string    `xml:",omitempty"`
		ObjectSize   *int64    `xml:",omitempty"`
	}{XMLNS: xmlns}
	if selected["ETag"] {
		out.ETag = o.ETag
	}
	if selected["StorageClass"] {
		out.StorageClass = "STANDARD"
	}
	if selected["ObjectSize"] {
		out.ObjectSize = &o.Size
	}
	if selected["Checksum"] && len(o.Checksums) > 0 {
		sums := make(map[string]string, len(o.Checksums))
		for algorithm, value := range o.Checksums {
			// Attributes returns the digest alone; GET/HEAD retain the composite
			// checksum's -N suffix. Base64 digests cannot contain a hyphen.
			if o.ChecksumType == "COMPOSITE" {
				value, _, _ = strings.Cut(value, "-")
			}
			sums[algorithm] = value
		}
		out.Checksum = &checksum{xmlChecksums(sums), o.ChecksumType}
	}
	if selected["ObjectParts"] && len(o.Parts) > 0 {
		out.ObjectParts = &parts{PartsCount: len(o.Parts)}
		truncated := false
		// S3 lists part details only for uploads whose parts carry their own
		// composite checksums; default full-object checksums report the count.
		detailed := o.ChecksumType == "COMPOSITE"
		if detailed {
			out.ObjectParts.PartNumberMarker = &marker
			out.ObjectParts.MaxParts = &maxParts
			out.ObjectParts.IsTruncated = &truncated
		}
		for _, p := range o.Parts {
			// Saved boundaries still support ordinary partNumber reads.
			if !detailed {
				break
			}
			if p.Number <= marker {
				continue
			}
			if len(out.ObjectParts.Parts) == maxParts {
				truncated = true
				break
			}
			out.ObjectParts.Parts = append(out.ObjectParts.Parts, part{xmlChecksums(p.Checksums), p.Number, p.Size})
			out.ObjectParts.NextPartNumberMarker = p.Number
		}
	}
	w.Header().Set("Last-Modified", o.Modified.UTC().Format(http.TimeFormat))
	writeXML(w, 200, out)
	return nil
}
