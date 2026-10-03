package gateway

import (
	"encoding/xml"
	"net/http"
	"sort"
	"strings"

	"github.com/adrianliechti/s3-gateway/backend"
)

type abacDocument struct {
	XMLName xml.Name `xml:"AbacStatus"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Status  string
	Unknown []backend.UnknownElement `xml:",any"`
}

func (g *Gateway) abac(w http.ResponseWriter, r *http.Request, b string, sig *signature) error {
	switch r.Method {
	case "GET":
		p, err := g.be.GetBucketProperties(r.Context(), b)
		if err != nil {
			return err
		}
		status := p.ABAC
		if status == "" {
			status = "Disabled"
		}
		writeXML(w, 200, abacDocument{XMLNS: xmlns, Status: status})
	case "PUT":
		var doc abacDocument
		if err := g.configurationBody(r, sig, maxXMLSize, &doc); err != nil {
			return err
		}
		if len(doc.Unknown) > 0 || doc.Status != "Enabled" && doc.Status != "Disabled" {
			return apiError("InvalidArgument", 400, "ABAC status must be Enabled or Disabled")
		}
		if err := g.be.UpdateBucketProperties(r.Context(), b, func(p *backend.BucketProperties) error { p.ABAC = doc.Status; return nil }); err != nil {
			return err
		}
		w.WriteHeader(200)
	default:
		return apiError("MethodNotAllowed", 405, "Method not allowed")
	}
	return nil
}

// S3 Control's tagging path is served by the same gateway endpoint. Only
// general purpose bucket ARNs are supported; account IDs are syntactically
// validated, with the operator credentials defining this gateway's ownership.
func (g *Gateway) resourceTags(w http.ResponseWriter, r *http.Request, b string, sig *signature) error {
	if r.URL.Path != "/v20180820/tags/arn:aws:s3:::"+b || !validBucket(b) {
		return apiError("InvalidArgument", 400, "Expected a general purpose bucket ARN")
	}
	account := r.Header.Get("X-Amz-Account-Id")
	if len(account) != 12 || strings.Trim(account, "0123456789") != "" {
		return apiError("InvalidArgument", 400, "Account ID must contain 12 digits")
	}
	if principal(r) != nil {
		if r.Method != "GET" {
			return accessDenied()
		}
		if err := g.authorize(r, "s3:ListTagsForResource", b, ""); err != nil {
			return err
		}
	}
	switch r.Method {
	case "GET":
		p, err := g.be.GetBucketProperties(r.Context(), b)
		if err != nil {
			return err
		}
		sort.Slice(p.Tags, func(i, j int) bool { return p.Tags[i].Key < p.Tags[j].Key })
		writeXML(w, 200, struct {
			XMLName xml.Name      `xml:"ListTagsForResourceResult"`
			XMLNS   string        `xml:"xmlns,attr"`
			Tags    []backend.Tag `xml:"Tags>Tag"`
		}{XMLNS: "http://awss3control.amazonaws.com/doc/2018-08-20/", Tags: p.Tags})
	case "POST":
		var doc struct {
			XMLName xml.Name `xml:"TagResourceRequest"`
			Tags    *tagSet  `xml:"Tags"`
		}
		if err := g.configurationBody(r, sig, maxXMLSize, &doc); err != nil {
			return err
		}
		if doc.Tags == nil {
			return apiError("MalformedXML", 400, "Tags is required")
		}
		if err := validateTagCount(doc.Tags.Tags, 50); err != nil {
			return err
		}
		if err := g.be.UpdateBucketProperties(r.Context(), b, func(p *backend.BucketProperties) error {
			m := map[string]string{}
			for _, t := range p.Tags {
				m[t.Key] = t.Value
			}
			for _, t := range doc.Tags.Tags {
				m[t.Key] = t.Value
			}
			var tags []backend.Tag
			for k, v := range m {
				tags = append(tags, backend.Tag{Key: k, Value: v})
			}
			if err := validateTagCount(tags, 50); err != nil {
				return err
			}
			p.Tags = tags
			return nil
		}); err != nil {
			return err
		}
		w.WriteHeader(204)
	case "DELETE":
		keys, ok := r.URL.Query()["tagKeys"]
		if !ok {
			return apiError("InvalidArgument", 400, "tagKeys is required")
		}
		var tags []backend.Tag
		for _, k := range keys {
			tags = append(tags, backend.Tag{Key: k})
		}
		if err := validateTagCount(tags, 50); err != nil {
			return err
		}
		if err := g.be.UpdateBucketProperties(r.Context(), b, func(p *backend.BucketProperties) error {
			remove := map[string]bool{}
			for _, k := range keys {
				remove[k] = true
			}
			var keep []backend.Tag
			for _, t := range p.Tags {
				if !remove[t.Key] {
					keep = append(keep, t)
				}
			}
			p.Tags = keep
			return nil
		}); err != nil {
			return err
		}
		w.WriteHeader(204)
	default:
		return apiError("MethodNotAllowed", 405, "Method not allowed")
	}
	return nil
}
