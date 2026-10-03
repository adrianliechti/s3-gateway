// Package identity verifies web identities and issues gateway session credentials.
package identity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	Issuer        string            `json:"issuer"`
	JWKSURL       string            `json:"jwks_url"`
	Audience      string            `json:"audience"`
	RolesClaim    string            `json:"roles_claim"`
	PrincipalTags map[string]string `json:"principal_tags"`
	Roles         map[string]Role   `json:"roles"`
	// Only for local development with an HTTP identity provider.
	AllowInsecureHTTP bool `json:"allow_insecure_http,omitempty"`
}

type Role struct {
	ClaimValue         string `json:"claim_value"`
	MaxSessionDuration int    `json:"max_session_duration,omitempty"`
	Policy             Policy `json:"policy"`
}

func Load(path string) (*Config, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("identity configuration exceeds 1 MiB")
	}
	var c Config
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return nil, fmt.Errorf("invalid identity configuration: %w", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("unexpected trailing identity configuration")
	}
	return &c, c.Validate()
}

func (c *Config) Validate() error {
	for _, raw := range []string{c.Issuer, c.JWKSURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && !(c.AllowInsecureHTTP && u.Scheme == "http")) {
			return fmt.Errorf("issuer and JWKS URL must be trusted HTTPS URLs")
		}
	}
	if c.Audience == "" || c.RolesClaim == "" || len(c.Roles) == 0 {
		return fmt.Errorf("audience, roles_claim and roles are required")
	}
	if len(c.PrincipalTags) > 50 {
		return fmt.Errorf("at most 50 principal tag mappings are supported")
	}
	for tag, claim := range c.PrincipalTags {
		if tag == "" || len(tag) > 128 || claim == "" {
			return fmt.Errorf("invalid principal tag mapping")
		}
	}
	for arn, role := range c.Roles {
		parts := strings.SplitN(arn, ":", 6)
		if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != "iam" || parts[3] != "" || len(parts[4]) != 12 || !strings.HasPrefix(parts[5], "role/") || len(parts[5]) <= 5 || role.ClaimValue == "" {
			return fmt.Errorf("roles require an IAM-format role ARN and claim_value")
		}
		for _, ch := range parts[4] {
			if ch < '0' || ch > '9' {
				return fmt.Errorf("role account must contain 12 digits")
			}
		}
		if role.MaxSessionDuration != 0 && (role.MaxSessionDuration < 900 || role.MaxSessionDuration > 43200) {
			return fmt.Errorf("max_session_duration must be 900..43200 seconds")
		}
		if err := role.Policy.Validate(); err != nil {
			return err
		}
	}
	return nil
}
