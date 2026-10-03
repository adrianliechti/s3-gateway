package config

import (
	"flag"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	SNSEnabled                                                          bool
	SNSEndpoint, SNSRegion, SNSAccessKey, SNSSecretKey, SNSSessionToken string
	IdentityConfig                                                      string
	TempDir                                                             string
	Backend, Listen, Region, Domain                                     string
	AccessKey, SecretKey                                                string
	Root                                                                string
	S3Endpoint, S3Region                                                string
	S3PathStyle                                                         bool
	AzureAccount, AzureKey, AzureURL, AzureSAS                          string
	CertFile, KeyFile                                                   string
	ReadOnly                                                            bool
	LifecycleInterval, LifecycleTestDay                                 time.Duration
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
	snsEnabled, err := strconv.ParseBool(env("GATEWAY_SNS_ENABLED", "false"))
	if err != nil {
		return c, fmt.Errorf("invalid GATEWAY_SNS_ENABLED")
	}
	f.BoolVar(&c.SNSEnabled, "sns", snsEnabled, "enable SNS bucket notifications (uses the AWS SDK credential chain by default)")
	f.StringVar(&c.SNSEndpoint, "sns-endpoint", getenv("GATEWAY_SNS_ENDPOINT"), "SNS endpoint override (also enables notifications)")
	f.StringVar(&c.SNSRegion, "sns-region", getenv("GATEWAY_SNS_REGION"), "SNS signing region (default: gateway region)")
	c.SNSAccessKey, c.SNSSecretKey, c.SNSSessionToken = getenv("GATEWAY_SNS_ACCESS_KEY"), getenv("GATEWAY_SNS_SECRET_KEY"), getenv("GATEWAY_SNS_SESSION_TOKEN")
	f.StringVar(&c.IdentityConfig, "identity-config", getenv("GATEWAY_IDENTITY_CONFIG"), "JSON web identity trust and role policy configuration (optional)")
	f.StringVar(&c.Backend, "backend", env("GATEWAY_BACKEND", "disk"), "storage backend: disk, azure or s3")
	f.StringVar(&c.Listen, "listen", env("GATEWAY_LISTEN", "127.0.0.1:9000"), "S3 listen host:port")
	f.StringVar(&c.Region, "region", env("GATEWAY_REGION", "us-east-1"), "S3 signing region")
	f.StringVar(&c.Domain, "domain", getenv("GATEWAY_DOMAIN"), "optional base domain for virtual-host bucket addressing")
	f.StringVar(&c.Root, "root", env("GATEWAY_DISK_ROOT", "./data"), "disk bucket root")
	f.StringVar(&c.TempDir, "temp-dir", getenv("GATEWAY_TEMP_DIR"), "request and multipart staging directory (default: system temp)")
	f.StringVar(&c.AzureAccount, "azure-account", getenv("AZURE_STORAGE_ACCOUNT"), "Azure storage account")
	f.StringVar(&c.AzureURL, "azure-url", getenv("AZURE_STORAGE_SERVICE_URL"), "Azure Blob service URL (optional with account)")
	f.StringVar(&c.S3Endpoint, "s3-endpoint", "", "upstream S3 endpoint (default: AWS SDK configuration, including AWS_ENDPOINT_URL_S3)")
	f.StringVar(&c.S3Region, "s3-region", "", "upstream S3 region (default: AWS SDK configuration, including AWS_REGION, or us-east-1)")
	endpointMode := env("AWS_ENDPOINT_URL_S3_MODE", "path")
	if endpointMode != "path" && endpointMode != "virtual" {
		return c, fmt.Errorf("invalid AWS_ENDPOINT_URL_S3_MODE: expected path or virtual")
	}
	f.BoolVar(&c.S3PathStyle, "s3-path-style", endpointMode == "path", "use path-style requests to upstream S3 (AWS_ENDPOINT_URL_S3_MODE: path or virtual)")
	f.StringVar(&c.CertFile, "tls-cert", getenv("GATEWAY_TLS_CERT"), "TLS certificate PEM file")
	f.StringVar(&c.KeyFile, "tls-key", getenv("GATEWAY_TLS_KEY"), "TLS private key PEM file")
	interval, err := time.ParseDuration(env("GATEWAY_LIFECYCLE_INTERVAL", "1m"))
	if err != nil {
		return c, fmt.Errorf("invalid GATEWAY_LIFECYCLE_INTERVAL")
	}
	testDay, err := time.ParseDuration(env("GATEWAY_TEST_LIFECYCLE_DAY", "0s"))
	if err != nil {
		return c, fmt.Errorf("invalid GATEWAY_TEST_LIFECYCLE_DAY")
	}
	f.DurationVar(&c.LifecycleInterval, "lifecycle-interval", interval, "background lifecycle scan interval")
	f.DurationVar(&c.LifecycleTestDay, "test-lifecycle-day", testDay, "TEST ONLY: accelerated lifecycle day (0 uses production UTC days)")
	f.BoolVar(&c.ReadOnly, "read-only", false, "reject S3 mutations")
	f.Usage = func() {
		fmt.Fprintln(out, "Usage: gateway [options]\n\nCredentials: GATEWAY_ACCESS_KEY and GATEWAY_SECRET_KEY (required).")
		fmt.Fprintln(out, "Azure: AZURE_STORAGE_KEY or AZURE_STORAGE_SAS_TOKEN; otherwise DefaultAzureCredential.")
		fmt.Fprintln(out, "S3 backend: standard AWS credential chain (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, optional AWS_SESSION_TOKEN, profiles or roles).")
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		return c, err
	}
	if f.NArg() != 0 {
		return c, fmt.Errorf("unexpected positional arguments")
	}
	c.AccessKey, c.SecretKey = getenv("GATEWAY_ACCESS_KEY"), getenv("GATEWAY_SECRET_KEY")
	c.AzureKey, c.AzureSAS = getenv("AZURE_STORAGE_KEY"), getenv("AZURE_STORAGE_SAS_TOKEN")
	if c.AccessKey == "" || c.SecretKey == "" {
		return c, fmt.Errorf("GATEWAY_ACCESS_KEY and GATEWAY_SECRET_KEY are required")
	}
	if c.Backend != "disk" && c.Backend != "azure" && c.Backend != "s3" {
		return c, fmt.Errorf("backend must be disk, azure or s3")
	}
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return c, fmt.Errorf("listen must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return c, fmt.Errorf("listen port must be between 1 and 65535")
	}
	if c.LifecycleInterval <= 0 || c.LifecycleTestDay < 0 || c.LifecycleTestDay > 24*time.Hour {
		return c, fmt.Errorf("invalid lifecycle durations")
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
	for _, p := range []*string{&c.Root, &c.CertFile, &c.KeyFile, &c.TempDir, &c.IdentityConfig} {
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
