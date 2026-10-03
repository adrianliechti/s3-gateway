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
)

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

type signature struct {
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
	if fields[0] != g.opts.AccessKey {
		return signature{}, apiError("InvalidAccessKeyId", 403, "The access key does not exist")
	}
	if fields[2] != g.opts.Region {
		return signature{}, apiError("AuthorizationHeaderMalformed", 400, "Incorrect region")
	}
	if r.Header.Get("X-Amz-Security-Token") != "" || q.Get("X-Amz-Security-Token") != "" {
		return signature{}, apiError("InvalidToken", 403, "Session credentials are not configured")
	}
	t, err := time.Parse("20060102T150405Z", date)
	if err != nil || fields[1] != t.Format("20060102") {
		return signature{}, apiError("AccessDenied", 403, "Invalid signing date")
	}
	now := time.Now()
	if presigned {
		sec, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
		if err != nil || sec < 1 || sec > 604800 {
			return signature{}, apiError("AccessDenied", 403, "Invalid expiry")
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
		for i := range vs {
			vs[i] = strings.Join(strings.Fields(vs[i]), " ")
		}
		headers.WriteString(n + ":" + strings.Join(vs, ",") + "\n")
		seen[n] = true
	}
	if !seen["host"] {
		return signature{}, apiError("AuthorizationHeaderMalformed", 400, "Host must be signed")
	}
	for n := range r.Header {
		l := strings.ToLower(n)
		if strings.HasPrefix(l, "x-amz-") && l != "x-amz-content-sha256" && l != "x-amz-user-agent" && !seen[l] {
			return signature{}, apiError("AccessDenied", 403, "An x-amz header is not signed")
		}
	}
	canonical := r.Method + "\n" + awsEncode(r.URL.Path, true) + "\n" + canonicalQuery(q, presigned) + "\n" + headers.String() + "\n" + signed + "\n" + payload
	scope := strings.Join(fields[1:], "/")
	key := mac([]byte("AWS4"+g.opts.SecretKey), fields[1])
	key = mac(key, fields[2])
	key = mac(key, "s3")
	key = mac(key, "aws4_request")
	expected := hex.EncodeToString(mac(key, "AWS4-HMAC-SHA256\n"+date+"\n"+scope+"\n"+sha256Hex(canonical)))
	if len(sig) != 64 || subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) != 1 {
		return signature{}, apiError("SignatureDoesNotMatch", 403, "The request signature does not match")
	}
	return signature{key: key, date: date, scope: scope, previous: sig, payload: payload}, nil
}
func (s *signature) checkChunk(data []byte, sig string) error {
	expected := hex.EncodeToString(mac(s.key, "AWS4-HMAC-SHA256-PAYLOAD\n"+s.date+"\n"+s.scope+"\n"+s.previous+"\n"+emptySHA256+"\n"+sha256Hex(string(data))))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) != 1 {
		return apiError("SignatureDoesNotMatch", 403, "Chunk signature mismatch")
	}
	s.previous = sig
	return nil
}
