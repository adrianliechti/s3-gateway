package gateway

import (
	"net/http"
	"strings"

	"github.com/adrianliechti/s3-gateway/internal/identity"
)

type principalContextKey struct{}

func principal(r *http.Request) *identity.Principal {
	p, _ := r.Context().Value(principalContextKey{}).(*identity.Principal)
	return p
}
func accessDenied() error { return apiError("AccessDenied", 403, "Access denied") }

func (g *Gateway) authorize(r *http.Request, action, b, k string) error {
	p := principal(r)
	if p == nil {
		return nil
	}
	if action == "" || g.identity == nil {
		return accessDenied()
	}
	resource := "*"
	var tags map[string]string
	if b != "" {
		resource = "arn:aws:s3:::" + b
		if k != "" {
			resource += "/" + k
		}
		properties, err := g.be.GetBucketProperties(r.Context(), b)
		if err != nil {
			return accessDenied()
		}
		if properties.ABAC == "Enabled" {
			tags = map[string]string{}
			for _, tag := range properties.Tags {
				tags[tag.Key] = tag.Value
			}
		}
	}
	if !g.identity.Allows(p, action, resource, tags) {
		return accessDenied()
	}
	return nil
}

func versionAction(base, version string) string {
	if version != "" {
		return strings.Replace(base, "Object", "ObjectVersion", 1)
	}
	return base
}

// Bucket administration remains exclusive to the static operator credentials.
// Session policies grant data access and configuration reads, never tag/ABAC
// administration that could grant access to another tenant's bucket.
func (g *Gateway) authorizeRequest(r *http.Request, b, k string) error {
	if principal(r) == nil {
		return nil
	}
	q := r.URL.Query()
	action := ""
	method := r.Method
	if b == "" {
		if method == "GET" {
			action = "s3:ListAllMyBuckets"
		}
	} else if k == "" {
		if q.Has("notification") {
			if method != "GET" {
				return accessDenied()
			}
			return g.authorize(r, "s3:GetBucketNotification", b, "")
		}
		for name, permission := range map[string]string{
			"tagging": "s3:GetBucketTagging", "abac": "s3:GetBucketAbac", "cors": "s3:GetBucketCORS", "publicAccessBlock": "s3:GetBucketPublicAccessBlock", "ownershipControls": "s3:GetBucketOwnershipControls", "lifecycle": "s3:GetLifecycleConfiguration", "acl": "s3:GetBucketAcl", "location": "s3:GetBucketLocation", "versioning": "s3:GetBucketVersioning",
		} {
			if q.Has(name) {
				if method != "GET" {
					return accessDenied()
				}
				return g.authorize(r, permission, b, "")
			}
		}
		switch {
		case q.Has("versions") && method == "GET":
			action = "s3:ListBucketVersions"
		case q.Has("uploads") && method == "GET":
			action = "s3:ListBucketMultipartUploads"
		case q.Has("delete") && method == "POST":
			return nil // Each item is checked before deletion.
		case method == "GET" || method == "HEAD":
			action = "s3:ListBucket"
		}
	} else {
		switch {
		case q.Has("tagging"):
			switch method {
			case "GET":
				action = "s3:GetObjectTagging"
			case "PUT":
				action = "s3:PutObjectTagging"
			case "DELETE":
				action = "s3:DeleteObjectTagging"
			}
			action = versionAction(action, q.Get("versionId"))
		case q.Has("attributes"):
			if method == "GET" {
				action = versionAction("s3:GetObjectAttributes", q.Get("versionId"))
			}
		case q.Has("acl"):
			if method == "GET" {
				action = versionAction("s3:GetObjectAcl", q.Get("versionId"))
			}
		case q.Has("uploadId"):
			switch method {
			case "GET":
				action = "s3:ListMultipartUploadParts"
			case "DELETE":
				action = "s3:AbortMultipartUpload"
			case "PUT", "POST":
				action = "s3:PutObject"
			}
		case q.Has("uploads"):
			if method == "POST" {
				action = "s3:PutObject"
			}
		default:
			switch method {
			case "GET", "HEAD":
				action = versionAction("s3:GetObject", q.Get("versionId"))
			case "PUT":
				action = "s3:PutObject"
			case "DELETE":
				action = versionAction("s3:DeleteObject", q.Get("versionId"))
			}
		}
	}
	if err := g.authorize(r, action, b, k); err != nil {
		return err
	}
	if action == "s3:PutObject" && r.Header.Get("X-Amz-Tagging") != "" {
		if err := g.authorize(r, "s3:PutObjectTagging", b, k); err != nil {
			return err
		}
	}
	if method == "PUT" || method == "POST" {
		if v := r.Header.Get("X-Amz-Acl"); v != "" && v != "private" && v != "bucket-owner-full-control" {
			return accessDenied()
		}
		for name := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-amz-grant-") {
				return accessDenied()
			}
		}
	}
	return nil
}
