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
		case "S3_ACCESS_KEY", "S3_SECRET_KEY", "AZURE_STORAGE_KEY", "AZURE_STORAGE_SAS_TOKEN":
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
func TestRejectsMissingCredentialsAndInvalidOptions(t *testing.T) {
	var out bytes.Buffer
	_, err := Parse(nil, func(string) string { return "" }, &out)
	if err == nil {
		t.Fatal("missing credentials accepted")
	}
	env := func(k string) string {
		if k == "S3_ACCESS_KEY" || k == "S3_SECRET_KEY" {
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
