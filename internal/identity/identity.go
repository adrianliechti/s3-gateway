package identity

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var ErrToken = errors.New("invalid identity or session token")
var ErrRole = errors.New("role assumption is not permitted")
var ErrDuration = errors.New("invalid session duration")

type Principal struct {
	Subject string            `json:"subject"`
	Role    string            `json:"role"`
	Tags    map[string]string `json:"tags"`
}
type Credentials struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}
type session struct {
	Principal
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Expires   int64  `json:"expires"`
}
type keyEntry struct {
	key any
	alg string
}
type Provider struct {
	config             Config
	seal               cipher.AEAD
	client             *http.Client
	mu                 sync.Mutex
	keys               map[string]keyEntry
	fetched, attempted time.Time
}

func New(c Config, accessKey, secret string) (*Provider, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	// Freeze configuration so callers cannot change the trust policy underneath
	// concurrent requests. Changing the root secret revokes all sessions.
	raw, _ := json.Marshal(c)
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	h := sha256.Sum256([]byte("gateway/session/v1\x00" + accessKey + "\x00" + secret))
	block, err := aes.NewCipher(h[:])
	if err != nil {
		return nil, err
	}
	seal, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Provider{config: c, seal: seal, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Claim paths use dots for nested objects (for example realm_access.roles).
func claim(claims map[string]any, path string) any {
	var value any = claims
	for _, part := range strings.Split(path, ".") {
		obj, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = obj[part]
	}
	return value
}
func hasRole(value any, expected string) bool {
	switch v := value.(type) {
	case string:
		return v == expected
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == expected {
				return true
			}
		}
	}
	return false
}

func (p *Provider) Assume(ctx context.Context, token, roleArn string, duration int) (Credentials, Principal, error) {
	var out Credentials
	var who Principal
	if len(token) > 20000 {
		return out, who, ErrToken
	}
	role, ok := p.config.Roles[roleArn]
	if !ok {
		return out, who, ErrRole
	}
	maxDuration := role.MaxSessionDuration
	if maxDuration == 0 {
		maxDuration = 3600
	}
	if duration == 0 {
		duration = min(3600, maxDuration)
	}
	if duration < 900 || duration > maxDuration {
		return out, who, ErrDuration
	}
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" || len(kid) > 256 {
			return nil, ErrToken
		}
		return p.key(ctx, kid, t.Method.Alg())
	}, jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512"}), jwt.WithIssuer(p.config.Issuer), jwt.WithAudience(p.config.Audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || !parsed.Valid {
		return out, who, ErrToken
	}
	sub, err := claims.GetSubject()
	if err != nil || sub == "" || len(sub) > 1024 {
		return out, who, ErrToken
	}
	if !hasRole(claim(map[string]any(claims), p.config.RolesClaim), role.ClaimValue) {
		return out, who, ErrRole
	}
	who = Principal{Subject: sub, Role: roleArn, Tags: map[string]string{}}
	for tag, path := range p.config.PrincipalTags {
		v := claim(map[string]any(claims), path)
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok || len(s) > 256 {
			return out, who, ErrToken
		}
		who.Tags[tag] = s
	}
	expires := time.Now().Add(time.Duration(duration) * time.Second).Truncate(time.Second)
	jwtExpiry, _ := claims.GetExpirationTime()
	if jwtExpiry.Time.Before(expires) {
		expires = jwtExpiry.Time.Truncate(time.Second)
	}
	if !expires.After(time.Now()) {
		return out, who, ErrToken
	}
	var random [42]byte
	if _, err = rand.Read(random[:]); err != nil {
		return out, who, err
	}
	out.AccessKeyID = "ASIA" + strings.ToUpper(hex.EncodeToString(random[:10]))
	out.SecretAccessKey = base64.RawStdEncoding.EncodeToString(random[10:])
	out.Expiration = expires.UTC()
	raw, err := json.Marshal(session{Principal: who, AccessKey: out.AccessKeyID, SecretKey: out.SecretAccessKey, Expires: expires.Unix()})
	if err != nil {
		return out, who, err
	}
	nonce := make([]byte, p.seal.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return out, who, err
	}
	out.SessionToken = base64.RawURLEncoding.EncodeToString(p.seal.Seal(nonce, nonce, raw, []byte("gateway/session/v1")))
	return out, who, nil
}

// Resolve authenticates the session envelope before exposing the signing secret.
// The secret is encrypted, so a presigned URL does not disclose reusable keys.
func (p *Provider) Resolve(accessKey, token string) (string, *Principal, error) {
	if len(token) == 0 || len(token) > 32768 {
		return "", nil, ErrToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	n := p.seal.NonceSize()
	if err != nil || len(raw) < n {
		return "", nil, ErrToken
	}
	raw, err = p.seal.Open(nil, raw[:n], raw[n:], []byte("gateway/session/v1"))
	if err != nil {
		return "", nil, ErrToken
	}
	var s session
	if json.Unmarshal(raw, &s) != nil || subtle.ConstantTimeCompare([]byte(accessKey), []byte(s.AccessKey)) != 1 || time.Now().Unix() >= s.Expires {
		return "", nil, ErrToken
	}
	if _, ok := p.config.Roles[s.Role]; !ok {
		return "", nil, ErrToken
	}
	return s.SecretKey, &s.Principal, nil
}
func (p *Provider) Allows(who *Principal, action, resource string, tags map[string]string) bool {
	role, ok := p.config.Roles[who.Role]
	return ok && role.Policy.Allows(action, resource, who.Tags, tags)
}
func (p *Provider) Issuer() string   { return p.config.Issuer }
func (p *Provider) Audience() string { return p.config.Audience }

func invalidJWKS() error { return fmt.Errorf("invalid JWKS signing key") }
