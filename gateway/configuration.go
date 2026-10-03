package gateway

import (
	"encoding/xml"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/adrianliechti/s3-gateway/backend"
)

func (g *Gateway) configurationBody(r *http.Request, sig *signature, limit int64, out any) error {
	p, err := g.body(r, sig, limit)
	if err != nil {
		return err
	}
	defer p.close()
	d := xml.NewDecoder(p.file)
	if err = d.Decode(out); err != nil {
		return apiError("MalformedXML", 400, "Invalid configuration document")
	}
	for {
		token, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return apiError("MalformedXML", 400, "Invalid configuration document")
		}
		if text, ok := token.(xml.CharData); !ok || strings.TrimSpace(string(text)) != "" {
			return apiError("MalformedXML", 400, "Unexpected content after configuration")
		}
	}
	return nil
}

func (g *Gateway) bucketConfiguration(w http.ResponseWriter, r *http.Request, b, kind string, sig *signature) error {
	p, err := g.be.GetBucketProperties(r.Context(), b)
	if err != nil {
		return err
	}
	missing := func(code string) error { return apiError(code, 404, "The bucket configuration does not exist") }
	switch r.Method {
	case "GET":
		switch kind {
		case "tagging":
			if len(p.Tags) == 0 {
				return missing("NoSuchTagSet")
			}
			sort.Slice(p.Tags, func(i, j int) bool { return p.Tags[i].Key < p.Tags[j].Key })
			writeXML(w, 200, taggingDocument{XMLNS: xmlns, Set: &tagSet{Tags: p.Tags}})
		case "cors":
			if p.CORS == nil {
				return missing("NoSuchCORSConfiguration")
			}
			p.CORS.XMLNS = xmlns
			writeXML(w, 200, p.CORS)
		case "publicAccessBlock":
			if p.PublicAccessBlock == nil {
				return missing("NoSuchPublicAccessBlockConfiguration")
			}
			p.PublicAccessBlock.XMLNS = xmlns
			writeXML(w, 200, p.PublicAccessBlock)
		case "ownershipControls":
			if p.Ownership == "" {
				return missing("OwnershipControlsNotFoundError")
			}
			writeXML(w, 200, ownershipDocument{XMLNS: xmlns, Rules: []ownershipRule{{p.Ownership}}})
		case "lifecycle":
			if p.Lifecycle == nil {
				return missing("NoSuchLifecycleConfiguration")
			}
			p.Lifecycle.XMLNS = xmlns
			writeXML(w, 200, p.Lifecycle)
		}
		return nil
	case "DELETE":
		err = g.be.UpdateBucketProperties(r.Context(), b, func(p *backend.BucketProperties) error {
			switch kind {
			case "tagging":
				if p.ABAC == "Enabled" {
					return apiError("InvalidRequest", 400, "Use UntagResource while ABAC is enabled")
				}
				p.Tags = nil
			case "cors":
				p.CORS = nil
			case "publicAccessBlock":
				p.PublicAccessBlock = nil
			case "ownershipControls":
				p.Ownership = ""
			case "lifecycle":
				p.Lifecycle = nil
			}
			return nil
		})
		if err != nil {
			return err
		}
		w.WriteHeader(204)
		return nil
	case "PUT":
		var update func(*backend.BucketProperties) error
		switch kind {
		case "tagging":
			var doc taggingDocument
			if err = g.configurationBody(r, sig, maxXMLSize, &doc); err != nil {
				return err
			}
			if doc.Set == nil {
				return apiError("MalformedXML", 400, "TagSet is required")
			}
			if err = validateTagCount(doc.Set.Tags, 50); err != nil {
				return err
			}
			update = func(p *backend.BucketProperties) error {
				if p.ABAC == "Enabled" {
					return apiError("InvalidRequest", 400, "Use TagResource while ABAC is enabled")
				}
				p.Tags = doc.Set.Tags
				return nil
			}
		case "cors":
			var doc backend.CORSConfiguration
			if err = g.configurationBody(r, sig, 64<<10, &doc); err != nil {
				return err
			}
			if err = validateCORS(doc); err != nil {
				return err
			}
			update = func(p *backend.BucketProperties) error { p.CORS = &doc; return nil }
		case "publicAccessBlock":
			var doc backend.PublicAccessBlock
			if err = g.configurationBody(r, sig, maxXMLSize, &doc); err != nil {
				return err
			}
			update = func(p *backend.BucketProperties) error { p.PublicAccessBlock = &doc; return nil }
		case "ownershipControls":
			var doc ownershipDocument
			if err = g.configurationBody(r, sig, maxXMLSize, &doc); err != nil {
				return err
			}
			if len(doc.Rules) != 1 || !validOwnership(doc.Rules[0].Value) {
				return apiError("InvalidArgument", 400, "Exactly one valid ownership rule is required")
			}
			update = func(p *backend.BucketProperties) error { p.Ownership = doc.Rules[0].Value; return nil }
		case "lifecycle":
			var doc backend.LifecycleConfiguration
			if err = g.configurationBody(r, sig, maxXMLSize, &doc); err != nil {
				return err
			}
			if err = validateLifecycle(&doc); err != nil {
				return err
			}
			update = func(p *backend.BucketProperties) error { p.Lifecycle = &doc; return nil }
		}
		if err = g.be.UpdateBucketProperties(r.Context(), b, update); err != nil {
			return err
		}
		status := 200
		if kind == "tagging" {
			status = 204
		}
		w.WriteHeader(status)
		return nil
	default:
		return apiError("MethodNotAllowed", 405, "Method not allowed")
	}
}

type ownershipRule struct {
	Value string `xml:"ObjectOwnership"`
}
type ownershipDocument struct {
	XMLName xml.Name        `xml:"OwnershipControls"`
	XMLNS   string          `xml:"xmlns,attr,omitempty"`
	Rules   []ownershipRule `xml:"Rule"`
}

func validOwnership(s string) bool {
	return s == "BucketOwnerEnforced" || s == "BucketOwnerPreferred" || s == "ObjectWriter"
}

// Bucket settings only restrict the configured principal's requests. They never
// grant anonymous or additional principals access to the gateway.
func (g *Gateway) enforceBucketACL(r *http.Request, b, k string) error {
	if b == "" || r.Method != "PUT" && r.Method != "POST" && !r.URL.Query().Has("acl") {
		return nil
	}
	if k == "" && !r.URL.Query().Has("acl") {
		return nil
	} // CreateBucket is checked separately.
	p, err := g.be.GetBucketProperties(r.Context(), b)
	if err != nil {
		return err
	}
	if p.Ownership == "BucketOwnerEnforced" {
		if r.URL.Query().Has("acl") {
			if r.Method == "PUT" {
				return apiError("AccessControlListNotSupported", 400, "The bucket does not allow ACLs")
			}
		} else {
			for key := range r.Header {
				if strings.HasPrefix(strings.ToLower(key), "x-amz-grant-") {
					return apiError("AccessControlListNotSupported", 400, "The bucket does not allow ACLs")
				}
			}
			if acl := r.Header.Get("X-Amz-Acl"); acl != "" && acl != "private" && acl != "bucket-owner-full-control" {
				return apiError("AccessControlListNotSupported", 400, "The bucket does not allow ACLs")
			}
		}
	}
	if p.PublicAccessBlock != nil && p.PublicAccessBlock.BlockPublicAcls {
		acl := r.Header.Get("X-Amz-Acl")
		if acl == "public-read" || acl == "public-read-write" || acl == "authenticated-read" {
			return apiError("AccessDenied", 403, "Public ACLs are blocked")
		}
	}
	return nil
}
