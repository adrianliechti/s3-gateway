package identity

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTagPolicyAndDeny(t *testing.T) {
	p := Policy{Version: "2012-10-17", Statement: []Statement{
		{Effect: "Allow", Action: Strings{"s3:GetObject*"}, Resource: Strings{"arn:aws:s3:::team-*/*"}, Condition: map[string]map[string]Strings{"StringEquals": {"aws:ResourceTag/team": Strings{"${aws:PrincipalTag/team}"}}}},
		{Effect: "Deny", Action: Strings{"*"}, Resource: Strings{"arn:aws:s3:::*/private/*"}},
	}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		key                 string
		principal, resource map[string]string
		allow               bool
	}{
		{"arn:aws:s3:::team-a/nested/file", map[string]string{"team": "a"}, map[string]string{"team": "a"}, true},
		{"arn:aws:s3:::team-a/nested/file", map[string]string{"team": "a"}, map[string]string{"team": "b"}, false},
		{"arn:aws:s3:::team-a/file", nil, nil, false},
		{"arn:aws:s3:::team-a/private/file", map[string]string{"team": "a"}, map[string]string{"team": "a"}, false},
	} {
		if got := p.Allows("s3:GetObject", tt.key, tt.principal, tt.resource); got != tt.allow {
			t.Fatalf("%s allowed=%v", tt.key, got)
		}
	}
	if wildcard("x/*/?.txt", "x/a/b/zz.txt") || !wildcard("x/*/?.txt", "x/a/b/z.txt") {
		t.Fatal("incorrect resource pattern matching")
	}
}

func TestSessionIntegrityExpiryAndRestart(t *testing.T) {
	c := Config{Issuer: "https://issuer.example", JWKSURL: "https://issuer.example/keys", Audience: "gateway", RolesClaim: "roles", Roles: map[string]Role{"arn:aws:iam::000000000000:role/reader": {ClaimValue: "reader", Policy: Policy{Version: "2012-10-17", Statement: []Statement{{Effect: "Allow", Action: Strings{"s3:GetObject"}, Resource: Strings{"*"}}}}}}}
	p, err := New(c, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", strings.Repeat("a", 32769), "invalid"} {
		if _, _, err := p.Resolve("access", token); err == nil {
			t.Fatal("invalid session accepted")
		}
	}
	seal := func(s session) string {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		nonce := make([]byte, p.seal.NonceSize())
		return base64.RawURLEncoding.EncodeToString(p.seal.Seal(nonce, nonce, raw, []byte("gateway/session/v1")))
	}
	s := session{Principal: Principal{Subject: "subject", Role: "arn:aws:iam::000000000000:role/reader", Tags: map[string]string{"team": "blue"}}, AccessKey: "ASIA-test", SecretKey: "temporary-secret", Expires: time.Now().Add(time.Hour).Unix()}
	token := seal(s)
	restarted, err := New(c, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	secret, who, err := restarted.Resolve(s.AccessKey, token)
	if err != nil || secret != s.SecretKey || who.Tags["team"] != "blue" {
		t.Fatal("session not restored")
	}
	if _, _, err = p.Resolve("different-access", token); err == nil {
		t.Fatal("session access key binding missing")
	}
	s.Expires = time.Now().Add(-time.Second).Unix()
	if _, _, err = p.Resolve(s.AccessKey, seal(s)); err == nil {
		t.Fatal("expired credentials accepted")
	}
	rotated, err := New(c, "root", "different-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = rotated.Resolve(s.AccessKey, token); err == nil {
		t.Fatal("root rotation failed to revoke credentials")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(token)
	raw[len(raw)-1] ^= 1
	if _, _, err = p.Resolve(s.AccessKey, base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("tampered session accepted")
	}
	if strings.Contains(string(raw), s.SecretKey) {
		t.Fatal("session token exposes signing secret")
	}
}
