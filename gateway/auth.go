package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"

	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/adrianliechti/s3-gateway/internal/identity"
)

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

type signature struct {
	principal                      *identity.Principal
	key                            []byte
	date, scope, previous, payload string
}

func mac(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}
func sha256Hex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func awsEncode(s string, slash bool) string {
	const digits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.~", rune(c)) || (slash && c == '/') {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(digits[c>>4])
			b.WriteByte(digits[c&15])
		}
	}
	return b.String()
}
func canonicalQuery(q url.Values, presigned bool) string {
	var parts []string
	for k, vs := range q {
		if presigned && k == "X-Amz-Signature" {
			continue
		}
		for _, v := range vs {
			parts = append(parts, awsEncode(k, false)+"="+awsEncode(v, false))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}
func (g *Gateway) authenticate(r *http.Request) (signature, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return signature{}, apiError("InvalidArgument", 400, "Malformed query")
	}
	for k, v := range q {
		if strings.HasPrefix(k, "X-Amz-") && len(v) != 1 {
			return signature{}, apiError("InvalidArgument", 400, "Duplicate authentication parameter")
		}
	}
	a := r.Header.Get("Authorization")
	presigned := q.Get("X-Amz-Algorithm") != ""
	// Presigners may move x-amz headers into the signed query. Keep them out
	// of canonical headers until authentication finishes, then expose their
	// authenticated values to the same validation and operation handlers.
	hoisted := http.Header{}
	if presigned {
		for name, values := range q {
			if !strings.HasPrefix(strings.ToLower(name), "x-amz-") || authenticationParameter(name) {
				continue
			}
			if len(values) != 1 || hoisted.Values(name) != nil {
				return signature{}, apiError("InvalidArgument", 400, "Duplicate request parameter")
			}
			if existing := r.Header.Values(name); len(existing) > 0 && (len(existing) != 1 || existing[0] != values[0]) {
				return signature{}, apiError("InvalidRequest", 400, "Conflicting header and query parameter")
			}
			hoisted.Set(name, values[0])
		}
	}
	if a != "" && presigned {
		return signature{}, apiError("InvalidArgument", 400, "Multiple authentication methods")
	}
	cred, signed, sig, date := "", "", "", r.Header.Get("X-Amz-Date")
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if presigned {
		if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
			return signature{}, apiError("InvalidRequest", 400, "Unsupported signing algorithm")
		}
		cred, signed, sig, date = q.Get("X-Amz-Credential"), q.Get("X-Amz-SignedHeaders"), q.Get("X-Amz-Signature"), q.Get("X-Amz-Date")
		if payload == "" {
			payload = "UNSIGNED-PAYLOAD"
		}
	} else {
		if !strings.HasPrefix(a, "AWS4-HMAC-SHA256 ") {
			return signature{}, apiError("AccessDenied", 403, "AWS Signature Version 4 is required")
		}
		fields := map[string]string{}
		for _, part := range strings.Split(strings.TrimPrefix(a, "AWS4-HMAC-SHA256 "), ",") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) != 2 || fields[kv[0]] != "" {
				return signature{}, apiError("AuthorizationHeaderMalformed", 400, "Malformed Authorization header")
			}
			fields[kv[0]] = kv[1]
		}
		cred, signed, sig = fields["Credential"], fields["SignedHeaders"], fields["Signature"]
		if payload == "" {
			payload = emptySHA256
		}
	}
	fields := strings.Split(cred, "/")
	if len(fields) != 5 || fields[3] != "s3" || fields[4] != "aws4_request" {
		return signature{}, apiError("AuthorizationHeaderMalformed", 400, "Invalid credential scope")
	}
	if fields[2] != g.opts.Region {
		return signature{}, apiError("AuthorizationHeaderMalformed", 400, "Incorrect region")
	}
	token := r.Header.Get("X-Amz-Security-Token")
	if len(r.Header.Values("X-Amz-Security-Token")) > 1 || token != "" && q.Has("X-Amz-Security-Token") || !presigned && q.Has("X-Amz-Security-Token") {
		return signature{}, apiError("InvalidToken", 403, "Ambiguous session token")
	}
	if presigned {
		token = q.Get("X-Amz-Security-Token")
	}
	secret := g.opts.SecretKey
	var principal *identity.Principal
	if fields[0] == g.opts.AccessKey {
		if token != "" {
			return signature{}, apiError("InvalidToken", 403, "Unexpected session token")
		}
	} else {
		if g.identity == nil {
			return signature{}, apiError("InvalidAccessKeyId", 403, "The access key does not exist")
		}
		secret, principal, err = g.identity.Resolve(fields[0], token)
		if err != nil {
			return signature{}, apiError("InvalidToken", 403, "Invalid or expired session credentials")
		}
	}
	t, err := time.Parse("20060102T150405Z", date)
	if err != nil || fields[1] != t.Format("20060102") {
		return signature{}, apiError("AccessDenied", 403, "Invalid signing date")
	}
	now := time.Now()
	if presigned {
		sec, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
		if err != nil || sec < 1 || sec > 604800 {
			return signature{}, apiError("AuthorizationQueryParametersError", 400, "Expiry must be between 1 and 604800 seconds")
		}
		if now.Before(t.Add(-15*time.Minute)) || now.After(t.Add(time.Duration(sec)*time.Second)) {
			return signature{}, apiError("AccessDenied", 403, "Request has expired")
		}
	} else if now.Sub(t) > 15*time.Minute || t.Sub(now) > 15*time.Minute {
		return signature{}, apiError("RequestTimeTooSkewed", 403, "Request time is too skewed")
	}
	names := strings.Split(signed, ";")
	seen := map[string]bool{}
	var headers strings.Builder
	for i, n := range names {
		if n == "" || n != strings.ToLower(n) || (i > 0 && n <= names[i-1]) {
			return signature{}, apiError("AuthorizationHeaderMalformed", 400, "Invalid signed headers")
		}
		var vs []string
		if n == "host" {
			vs = []string{r.Host}
		} else {
			vs = r.Header.Values(n)
			if n == "content-length" && len(vs) == 0 && r.ContentLength >= 0 {
				vs = []string{strconv.FormatInt(r.ContentLength, 10)}
			}
		}
		if len(vs) == 0 {
			return signature{}, apiError("SignatureDoesNotMatch", 403, "A signed header is missing")
		}
		// Canonicalization must not rewrite the actual metadata/header value.
		vs = append([]string(nil), vs...)
		for i := range vs {
			vs[i] = strings.Join(strings.Fields(vs[i]), " ")
		}
		headers.WriteString(n + ":" + strings.Join(vs, ",") + "\n")
		seen[n] = true
	}
	if !seen["host"] {
		return signature{}, apiError("AuthorizationHeaderMalformed", 400, "Host must be signed")
	}
	if presigned && seen["range"] && r.Header.Get("If-Range") != "" && !seen["if-range"] {
		return signature{}, apiError("AccessDenied", 403, "If-Range must be signed when Range is signed")
	}
	for n := range r.Header {
		l := strings.ToLower(n)
		if strings.HasPrefix(l, "x-amz-") && l != "x-amz-content-sha256" && l != "x-amz-user-agent" && !seen[l] && hoisted.Values(n) == nil {
			return signature{}, apiError("AccessDenied", 403, "An x-amz header is not signed")
		}
	}
	// S3 signs the escaped wire path without normalizing it. In particular,
	// an encoded slash must not become a literal slash during verification.
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := r.Method + "\n" + path + "\n" + canonicalQuery(q, presigned) + "\n" + headers.String() + "\n" + signed + "\n" + payload
	scope := strings.Join(fields[1:], "/")
	key := mac([]byte("AWS4"+secret), fields[1])
	key = mac(key, fields[2])
	key = mac(key, "s3")
	key = mac(key, "aws4_request")
	expected := hex.EncodeToString(mac(key, "AWS4-HMAC-SHA256\n"+date+"\n"+scope+"\n"+sha256Hex(canonical)))
	if len(sig) != 64 || subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) != 1 {
		return signature{}, apiError("SignatureDoesNotMatch", 403, "The request signature does not match")
	}
	for name, values := range hoisted {
		r.Header[name] = values
	}
	return signature{principal: principal, key: key, date: date, scope: scope, previous: sig, payload: payload}, nil
}

func authenticationParameter(name string) bool {
	switch name {
	case "X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires", "X-Amz-SignedHeaders", "X-Amz-Signature", "X-Amz-Security-Token":
		return true
	}
	return false
}
func (s *signature) checkChunk(data []byte, sig string) error {
	expected := hex.EncodeToString(mac(s.key, "AWS4-HMAC-SHA256-PAYLOAD\n"+s.date+"\n"+s.scope+"\n"+s.previous+"\n"+emptySHA256+"\n"+sha256Hex(string(data))))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) != 1 {
		return apiError("SignatureDoesNotMatch", 403, "Chunk signature mismatch")
	}
	s.previous = sig
	return nil
}
