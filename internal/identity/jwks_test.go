package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestJWTVerificationAndKeyRotation(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var keyID atomic.Value
	keyID.Store("first")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kid": keyID.Load().(string), "kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "key_ops": []string{"verify"}, "x": base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))), "y": base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}})
	}))
	defer server.Close()
	role := "arn:aws:iam::000000000000:role/user"
	c := Config{Issuer: server.URL, JWKSURL: server.URL, AllowInsecureHTTP: true, Audience: "gateway", RolesClaim: "roles", Roles: map[string]Role{role: {ClaimValue: "user", Policy: Policy{Version: "2012-10-17", Statement: []Statement{{Effect: "Allow", Action: Strings{"s3:GetObject"}, Resource: Strings{"*"}}}}}}}
	p, err := New(c, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.MapClaims{"iss": server.URL, "aud": "gateway", "sub": "user", "exp": time.Now().Add(time.Hour).Unix(), "roles": []string{"user"}}
	sign := func(kid string, method jwt.SigningMethod, private any) string {
		token := jwt.NewWithClaims(method, claims)
		token.Header["kid"] = kid
		s, err := token.SignedString(private)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := sign("first", jwt.SigningMethodES256, key)
	if _, _, err = p.Assume(t.Context(), first, role, 900); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err = p.Assume(t.Context(), sign("unknown", jwt.SigningMethodES256, key), role, 900); err == nil {
			t.Fatal("unknown kid accepted")
		}
	}
	if requests.Load() != 1 {
		t.Fatal("unknown kid bypassed fetch rate limit")
	}
	// Rotation replaces the complete key set; retired signing keys cannot mint
	// new credentials after the refresh, while issued sessions remain valid.
	keyID.Store("second")
	p.fetched = time.Now().Add(-6 * time.Minute)
	p.attempted = time.Time{}
	if _, _, err = p.Assume(t.Context(), sign("second", jwt.SigningMethodES256, key), role, 900); err != nil {
		t.Fatal(err)
	}
	if _, _, err = p.Assume(t.Context(), first, role, 900); err == nil {
		t.Fatal("retired signing key accepted")
	}
	if _, _, err = p.Assume(t.Context(), sign("second", jwt.SigningMethodHS256, []byte("public-key-as-secret")), role, 900); err == nil {
		t.Fatal("symmetric algorithm accepted")
	}
	wrong, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = p.Assume(t.Context(), sign("second", jwt.SigningMethodES256, wrong), role, 900); err == nil {
		t.Fatal("invalid signature accepted")
	}
	if _, _, err = p.Assume(t.Context(), sign("second", jwt.SigningMethodES256, key), role, 43200); err != ErrDuration {
		t.Fatal("role duration limit ignored")
	}
	delete(claims, "sub")
	if _, _, err = p.Assume(t.Context(), sign("second", jwt.SigningMethodES256, key), role, 900); err == nil {
		t.Fatal("missing subject accepted")
	}
}
