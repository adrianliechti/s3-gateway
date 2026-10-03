package gateway

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"

	"github.com/adrianliechti/s3-gateway/backend"
)

type aclGrantee struct {
	XMLNS        string `xml:"xmlns:xsi,attr,omitempty"`
	Type         string `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr"`
	ID           string `xml:",omitempty"`
	DisplayName  string `xml:",omitempty"`
	URI          string `xml:",omitempty"`
	EmailAddress string `xml:",omitempty"`
}

// AWS SDKs inspect the literal xsi:type attribute name as well as its namespace.
func (g aclGrantee) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	type wire struct {
		XMLNS       string `xml:"xmlns:xsi,attr"`
		Type        string `xml:"xsi:type,attr"`
		ID          string `xml:",omitempty"`
		DisplayName string `xml:",omitempty"`
		URI         string `xml:",omitempty"`
	}
	return e.EncodeElement(wire{"http://www.w3.org/2001/XMLSchema-instance", g.Type, g.ID, g.DisplayName, g.URI}, start)
}

type aclGrant struct {
	Grantee    aclGrantee
	Permission string
}
type aclPolicy struct {
	XMLName xml.Name `xml:"AccessControlPolicy"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Owner   owner
	Grants  []aclGrant `xml:"AccessControlList>Grant"`
}

func (g *Gateway) privateACL() backend.ACL {
	o := g.owner()
	return backend.ACL{Owner: o, Grants: []backend.Grant{{Type: "CanonicalUser", ID: o.ID, DisplayName: o.DisplayName, Permission: "FULL_CONTROL"}}}
}
func (g *Gateway) normalizeACL(acl backend.ACL) backend.ACL {
	if acl.Owner.ID == "" {
		return g.privateACL()
	}
	return acl
}

func validCannedACL(value string) bool {
	switch value {
	case "", "private", "bucket-owner-full-control", "public-read", "public-read-write", "authenticated-read", "aws-exec-read", "bucket-owner-read", "log-delivery-write":
		return true
	}
	return false
}

func (g *Gateway) requestACL(r *http.Request, body io.Reader) (backend.ACL, error) {
	acl := g.privateACL()
	canned := r.Header.Get("X-Amz-Acl")
	hasGrants := false
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-grant-") {
			hasGrants = true
		}
	}
	if canned != "" && hasGrants {
		return acl, apiError("InvalidRequest", 400, "Canned ACL and explicit grants cannot be combined")
	}
	switch canned {
	case "", "private", "bucket-owner-full-control":
	case "public-read", "public-read-write", "authenticated-read", "aws-exec-read", "bucket-owner-read", "log-delivery-write":
		return acl, apiError("NotImplemented", 501, "Only private ACLs are supported by this single-user gateway")
	default:
		return acl, apiError("InvalidArgument", 400, "Invalid canned ACL")
	}
	if hasGrants {
		acl.Grants = nil
		for name, permission := range map[string]string{"X-Amz-Grant-Full-Control": "FULL_CONTROL", "X-Amz-Grant-Read": "READ", "X-Amz-Grant-Write": "WRITE", "X-Amz-Grant-Read-Acp": "READ_ACP", "X-Amz-Grant-Write-Acp": "WRITE_ACP"} {
			if value := r.Header.Get(name); value != "" {
				for _, item := range strings.Split(value, ",") {
					pair := strings.SplitN(strings.TrimSpace(item), "=", 2)
					if len(pair) != 2 || pair[0] != "id" || strings.Trim(pair[1], `"`) != acl.Owner.ID {
						return acl, apiError("NotImplemented", 501, "Only grants to the configured owner are supported")
					}
					acl.Grants = append(acl.Grants, backend.Grant{Type: "CanonicalUser", ID: acl.Owner.ID, DisplayName: acl.Owner.DisplayName, Permission: permission})
				}
			}
		}
	}
	if body != nil {
		if canned != "" || hasGrants {
			return acl, apiError("InvalidRequest", 400, "ACL XML cannot be combined with ACL headers")
		}
		var policy aclPolicy
		if err := xml.NewDecoder(body).Decode(&policy); err != nil {
			return acl, apiError("MalformedXML", 400, "Invalid ACL document")
		}
		if policy.Owner.ID != acl.Owner.ID {
			return acl, apiError("InvalidArgument", 400, "ACL owner must match the configured owner")
		}
		if len(policy.Grants) > 100 {
			return acl, apiError("InvalidArgument", 400, "An ACL supports at most 100 grants")
		}
		acl.Grants = nil
		for _, grant := range policy.Grants {
			if grant.Grantee.Type != "CanonicalUser" || grant.Grantee.ID != acl.Owner.ID || grant.Grantee.URI != "" || grant.Grantee.EmailAddress != "" {
				return acl, apiError("NotImplemented", 501, "Only private owner grants are supported")
			}
			switch grant.Permission {
			case "FULL_CONTROL", "READ", "WRITE", "READ_ACP", "WRITE_ACP":
			default:
				return acl, apiError("InvalidArgument", 400, "Invalid ACL permission")
			}
			acl.Grants = append(acl.Grants, backend.Grant{Type: "CanonicalUser", ID: acl.Owner.ID, DisplayName: acl.Owner.DisplayName, Permission: grant.Permission})
		}
	}
	return acl, nil
}
func (g *Gateway) acl(w http.ResponseWriter, r *http.Request, b, k string, sig *signature) error {
	var acl backend.ACL
	if k == "" {
		p, err := g.be.GetBucketProperties(r.Context(), b)
		if err != nil {
			return err
		}
		acl = p.ACL
	} else {
		o, err := g.headVersion(w, r, b, k)
		if err != nil {
			return err
		}
		acl = o.ACL
	}
	if r.Method == "GET" {
		p, err := g.be.GetBucketProperties(r.Context(), b)
		if err != nil {
			return err
		}
		if p.Ownership == "BucketOwnerEnforced" {
			acl = g.privateACL()
		}
	}
	switch r.Method {
	case "GET":
		acl = g.normalizeACL(acl)
		out := aclPolicy{XMLNS: xmlns, Owner: acl.Owner}
		for _, grant := range acl.Grants {
			out.Grants = append(out.Grants, aclGrant{Grantee: aclGrantee{Type: grant.Type, ID: grant.ID, DisplayName: grant.DisplayName, URI: grant.URI}, Permission: grant.Permission})
		}
		writeXML(w, 200, out)
	case "PUT":
		p, err := g.body(r, sig, maxXMLSize)
		if err != nil {
			return err
		}
		defer p.close()
		var body io.Reader
		if p.size > 0 {
			body = p.file
		}
		acl, err = g.requestACL(r, body)
		if err != nil {
			return err
		}
		if k == "" {
			err = g.be.UpdateBucketProperties(r.Context(), b, func(p *backend.BucketProperties) error { p.ACL = acl; return nil })
		} else {
			err = g.be.SetACL(r.Context(), b, k, r.URL.Query().Get("versionId"), acl)
		}
		if err != nil {
			return err
		}
		w.WriteHeader(200)
	default:
		return apiError("MethodNotAllowed", 405, "Method not allowed")
	}
	return nil
}
