package config

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestHelpDoesNotExposeCredentials(t *testing.T) {
	var out bytes.Buffer
	_, err := Parse([]string{"-help"}, func(k string) string {
		switch k {
		case "GATEWAY_ACCESS_KEY", "GATEWAY_SECRET_KEY", "GATEWAY_SNS_ACCESS_KEY", "GATEWAY_SNS_SECRET_KEY", "GATEWAY_SNS_SESSION_TOKEN", "AZURE_STORAGE_KEY", "AZURE_STORAGE_SAS_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN":
			return "super-secret-value"
		}
		return ""
	}, &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "super-secret-value") {
		t.Fatal("help contains a credential")
	}
}

func TestS3BackendConfiguration(t *testing.T) {
	env := map[string]string{
		"GATEWAY_ACCESS_KEY": "gateway", "GATEWAY_SECRET_KEY": "gateway-secret", "GATEWAY_BACKEND": "s3",
		"GATEWAY_REGION": "gateway-region", "AWS_ENDPOINT_URL_S3_MODE": "virtual",
	}
	lookup := func(k string) string { return env[k] }
	var out bytes.Buffer
	c, err := Parse(nil, lookup, &out)
	if err != nil {
		t.Fatal(err)
	}
	if c.Backend != "s3" || c.S3Endpoint != "" || c.S3Region != "" || c.Region != "gateway-region" || c.S3PathStyle {
		t.Fatalf("unexpected configuration: %+v", c)
	}
	c, err = Parse([]string{"-s3-endpoint", "https://storage.example", "-s3-region", "upstream-region", "-s3-path-style"}, lookup, &out)
	if err != nil || c.S3Endpoint != "https://storage.example" || c.S3Region != "upstream-region" || c.Region != "gateway-region" || !c.S3PathStyle {
		t.Fatalf("flag override failed: %v", err)
	}
	for _, mode := range []string{"", "path"} {
		env["AWS_ENDPOINT_URL_S3_MODE"] = mode
		c, err := Parse(nil, lookup, &out)
		if err != nil || !c.S3PathStyle {
			t.Fatalf("mode %q should use path style: %v", mode, err)
		}
	}
	for _, mode := range []string{"true", "false", "invalid"} {
		env["AWS_ENDPOINT_URL_S3_MODE"] = mode
		if _, err := Parse(nil, lookup, &out); err == nil || !strings.Contains(err.Error(), "expected path or virtual") {
			t.Fatalf("invalid mode %q: %v", mode, err)
		}
	}
}
func TestRejectsMissingCredentialsAndInvalidOptions(t *testing.T) {
	var out bytes.Buffer
	_, err := Parse(nil, func(string) string { return "" }, &out)
	if err == nil {
		t.Fatal("missing credentials accepted")
	}
	env := func(k string) string {
		if k == "GATEWAY_ACCESS_KEY" || k == "GATEWAY_SECRET_KEY" {
			return "test"
		}
		return ""
	}
	for _, args := range [][]string{{"-backend", "typo"}, {"-listen", ":0"}, {"-tls-cert", "cert.pem"}, {"extra"}} {
		if _, err := Parse(args, env, &out); err == nil {
			t.Fatalf("invalid options accepted: %v", args)
		}
	}
}
