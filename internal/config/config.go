package config

import (
	"flag"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	TempDir                                    string
	Backend, Listen, Region, Domain            string
	AccessKey, SecretKey                       string
	Root                                       string
	AzureAccount, AzureKey, AzureURL, AzureSAS string
	CertFile, KeyFile                          string
	ReadOnly                                   bool
}

// Parse accepts public settings on flags and credentials only through the
// supplied environment lookup. In particular, -help never prints secrets.
func Parse(args []string, getenv func(string) string, out io.Writer) (Config, error) {
	var c Config
	env := func(k, fallback string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return fallback
	}
	f := flag.NewFlagSet("gateway", flag.ContinueOnError)
	f.SetOutput(out)
	f.StringVar(&c.Backend, "backend", env("S3_BACKEND", "disk"), "storage backend: disk or azure")
	f.StringVar(&c.Listen, "listen", env("S3_LISTEN", "127.0.0.1:9000"), "S3 listen host:port")
	f.StringVar(&c.Region, "region", env("S3_REGION", "us-east-1"), "S3 signing region")
	f.StringVar(&c.Domain, "domain", getenv("S3_DOMAIN"), "optional base domain for virtual-host bucket addressing")
	f.StringVar(&c.Root, "root", env("S3_DISK_ROOT", "./data"), "disk bucket root")
	f.StringVar(&c.TempDir, "temp-dir", getenv("S3_TEMP_DIR"), "request and multipart staging directory (default: system temp)")
	f.StringVar(&c.AzureAccount, "azure-account", getenv("AZURE_STORAGE_ACCOUNT"), "Azure storage account")
	f.StringVar(&c.AzureURL, "azure-url", getenv("AZURE_STORAGE_SERVICE_URL"), "Azure Blob service URL (optional with account)")
	f.StringVar(&c.CertFile, "tls-cert", getenv("S3_TLS_CERT"), "TLS certificate PEM file")
	f.StringVar(&c.KeyFile, "tls-key", getenv("S3_TLS_KEY"), "TLS private key PEM file")
	f.BoolVar(&c.ReadOnly, "read-only", false, "reject S3 mutations")
	f.Usage = func() {
		fmt.Fprintln(out, "Usage: gateway [options]\n\nCredentials: S3_ACCESS_KEY and S3_SECRET_KEY (required).")
		fmt.Fprintln(out, "Azure: AZURE_STORAGE_KEY or AZURE_STORAGE_SAS_TOKEN; otherwise DefaultAzureCredential.")
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		return c, err
	}
	if f.NArg() != 0 {
		return c, fmt.Errorf("unexpected positional arguments")
	}
	c.AccessKey, c.SecretKey = getenv("S3_ACCESS_KEY"), getenv("S3_SECRET_KEY")
	c.AzureKey, c.AzureSAS = getenv("AZURE_STORAGE_KEY"), getenv("AZURE_STORAGE_SAS_TOKEN")
	if c.AccessKey == "" || c.SecretKey == "" {
		return c, fmt.Errorf("S3_ACCESS_KEY and S3_SECRET_KEY are required")
	}
	if c.Backend != "disk" && c.Backend != "azure" {
		return c, fmt.Errorf("backend must be disk or azure")
	}
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return c, fmt.Errorf("listen must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return c, fmt.Errorf("listen port must be between 1 and 65535")
	}
	if c.Region == "" {
		return c, fmt.Errorf("region must not be empty")
	}
	if strings.ContainsAny(c.Domain, "/: ") {
		return c, fmt.Errorf("domain must be a hostname without scheme or port")
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return c, fmt.Errorf("tls-cert and tls-key must be provided together")
	}
	// Resolve file paths once at startup.
	for _, p := range []*string{&c.Root, &c.CertFile, &c.KeyFile, &c.TempDir} {
		if *p == "" {
			continue
		}
		*p, err = filepath.Abs(*p)
		if err != nil {
			return c, err
		}
	}
	return c, nil
}
