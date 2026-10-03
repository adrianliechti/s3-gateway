package gateway

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/adrianliechti/s3-gateway/backend"
)

func validateCORS(c backend.CORSConfiguration) error {
	bad := func() error { return apiError("InvalidRequest", 400, "Invalid CORS configuration") }
	if len(c.Rules) == 0 || len(c.Rules) > 100 {
		return bad()
	}
	for _, r := range c.Rules {
		if len(r.Origins) == 0 || len(r.Methods) == 0 || len(r.ID) > 255 || r.MaxAge != nil && *r.MaxAge < 0 {
			return bad()
		}
		for _, m := range r.Methods {
			switch m {
			case "GET", "PUT", "POST", "DELETE", "HEAD":
			default:
				return bad()
			}
		}
		for _, s := range append(append(append([]string{}, r.Origins...), r.Headers...), r.Expose...) {
			if s == "" || strings.ContainsAny(s, "\r\n") || strings.Count(s, "*") > 1 {
				return bad()
			}
		}
		for _, s := range r.Expose {
			if strings.Contains(s, "*") {
				return bad()
			}
		}
	}
	return nil
}
func corsMatch(pattern, value string) bool {
	if a, b, ok := strings.Cut(pattern, "*"); ok {
		return len(value) >= len(a)+len(b) && strings.HasPrefix(value, a) && strings.HasSuffix(value, b)
	}
	return pattern == value
}
func (g *Gateway) cors(w http.ResponseWriter, r *http.Request, b string, preflight bool) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		if preflight {
			return apiError("BadRequest", 400, "Origin is required")
		}
		return nil
	}
	method := r.Method
	var requested []string
	if preflight {
		method = r.Header.Get("Access-Control-Request-Method")
		if method == "" {
			return apiError("BadRequest", 400, "Access-Control-Request-Method is required")
		}
		if raw := r.Header.Get("Access-Control-Request-Headers"); raw != "" {
			for _, h := range strings.Split(raw, ",") {
				h = strings.TrimSpace(h)
				if h == "" {
					return apiError("BadRequest", 400, "Invalid requested header")
				}
				requested = append(requested, h)
			}
		}
	}
	p, err := g.be.GetBucketProperties(r.Context(), b)
	if err != nil {
		return err
	}
	if p.CORS != nil {
		for _, rule := range p.CORS.Rules {
			matched, allowedOrigin := false, ""
			for _, m := range rule.Methods {
				if m == method {
					matched = true
				}
			}
			if !matched {
				continue
			}
			for _, o := range rule.Origins {
				if corsMatch(o, origin) {
					allowedOrigin = origin
					if o == "*" {
						allowedOrigin = "*"
					}
					break
				}
			}
			if allowedOrigin == "" {
				continue
			}
			allowed := true
			for _, h := range requested {
				found := false
				for _, pattern := range rule.Headers {
					if corsMatch(strings.ToLower(pattern), strings.ToLower(h)) {
						found = true
						break
					}
				}
				if !found {
					allowed = false
					break
				}
			}
			if !allowed {
				continue
			}
			headers := w.Header()
			headers.Set("Access-Control-Allow-Origin", allowedOrigin)
			headers.Set("Access-Control-Allow-Methods", strings.Join(rule.Methods, ", "))
			if allowedOrigin != "*" {
				headers.Set("Access-Control-Allow-Credentials", "true")
			}
			headers.Add("Vary", "Origin, Access-Control-Request-Headers, Access-Control-Request-Method")
			if len(rule.Expose) > 0 {
				headers.Set("Access-Control-Expose-Headers", strings.Join(rule.Expose, ", "))
			}
			if preflight && len(requested) > 0 {
				headers.Set("Access-Control-Allow-Headers", strings.Join(requested, ", "))
			}
			if rule.MaxAge != nil {
				headers.Set("Access-Control-Max-Age", strconv.Itoa(*rule.MaxAge))
			}
			return nil
		}
	}
	if preflight {
		return apiError("AccessDenied", 403, "CORS request is not allowed")
	}
	return nil
}
