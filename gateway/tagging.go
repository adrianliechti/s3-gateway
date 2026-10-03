package gateway

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/adrianliechti/s3-gateway/backend"
)

type tagSet struct {
	Tags []backend.Tag `xml:"Tag"`
}
type taggingDocument struct {
	XMLName xml.Name `xml:"Tagging"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Set     *tagSet  `xml:"TagSet"`
}

func validateTags(tags []backend.Tag) error {
	if len(tags) > 10 {
		return apiError("BadRequest", 400, "An object supports at most ten tags")
	}
	return validateTagCount(tags, 10)
}
func validateTagCount(tags []backend.Tag, max int) error {
	invalid := func() error {
		return apiError("InvalidTag", 400, "Tags must have unique nonempty keys up to 128 characters and values up to 256 characters; tag count must fit the resource limit")
	}
	if len(tags) > max {
		return invalid()
	}
	seen := map[string]bool{}
	for _, tag := range tags {
		if tag.Key == "" || seen[tag.Key] || strings.HasPrefix(strings.ToLower(tag.Key), "aws:") {
			return invalid()
		}
		seen[tag.Key] = true
		for i, value := range []string{tag.Key, tag.Value} {
			limit := 128
			if i == 1 {
				limit = 256
			}
			if len(utf16.Encode([]rune(value))) > limit {
				return invalid()
			}
			for _, r := range value {
				if !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.Is(unicode.Z, r) && !strings.ContainsRune("+-=._:/@", r) {
					return invalid()
				}
			}
		}
	}
	return nil
}
func requestTags(r *http.Request) ([]backend.Tag, error) {
	raw := r.Header.Get("X-Amz-Tagging")
	if raw == "" {
		return nil, nil
	}
	// Parse each pair to preserve order, and reject duplicate keys.
	var tags []backend.Tag
	seen := map[string]bool{}
	for _, pair := range strings.Split(raw, "&") {
		k, v, _ := strings.Cut(pair, "=")
		key, e1 := url.QueryUnescape(k)
		value, e2 := url.QueryUnescape(v)
		if e1 != nil || e2 != nil {
			return nil, apiError("InvalidTag", 400, "Invalid URL-encoded tag set")
		}
		if seen[key] {
			return nil, apiError("InvalidArgument", 400, "Tagging header contains duplicate keys")
		}
		seen[key] = true
		tags = append(tags, backend.Tag{Key: key, Value: value})
	}
	return tags, validateTags(tags)
}
func (g *Gateway) tagging(w http.ResponseWriter, r *http.Request, b, k string, sig *signature) error {
	switch r.Method {
	case "GET":
		o, err := g.headVersion(w, r, b, k)
		if err != nil {
			return err
		}
		tags := append([]backend.Tag(nil), o.Tags...)
		sort.Slice(tags, func(i, j int) bool { return tags[i].Key < tags[j].Key })
		writeXML(w, 200, taggingDocument{XMLNS: xmlns, Set: &tagSet{Tags: tags}})
	case "PUT", "DELETE":
		var tags []backend.Tag
		if r.Method == "PUT" {
			p, err := g.body(r, sig, maxXMLSize)
			if err != nil {
				return err
			}
			defer p.close()
			var doc taggingDocument
			if err = xml.NewDecoder(p.file).Decode(&doc); err != nil || doc.Set == nil {
				return apiError("MalformedXML", 400, "Invalid tagging document")
			}
			tags = doc.Set.Tags
			if err = validateTags(tags); err != nil {
				return err
			}
		}
		o, err := g.be.SetTags(r.Context(), b, k, r.URL.Query().Get("versionId"), tags)
		if err != nil {
			return err
		}
		versionHeaders(w, o)
		status := 200
		if r.Method == "DELETE" {
			status = 204
		}
		w.WriteHeader(status)
	default:
		return apiError("MethodNotAllowed", 405, "Method not allowed")
	}
	return nil
}
