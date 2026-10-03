// Package conformance runs one scenario set against any S3 endpoint and
// records protocol-level observations: HTTP status, S3 error code, semantic
// headers and flattened XML. Expectations come from the AWS S3 documentation.
// Run the same scenarios against Amazon S3 to validate the oracle and to
// record a baseline; run them against the gateway to find divergences.
//
// Targets (environment):
//
//	default                      in-process gateway on a temporary disk root
//	GATEWAY_TEST_AZURITE_URL     in-process gateway on Azurite
//	GATEWAY_TEST_STORAGE_ENDPOINT in-process gateway on an S3 provider (RustFS)
//	CONFORMANCE_ENDPOINT         any S3 endpoint (+ CONFORMANCE_ACCESS_KEY/SECRET_KEY)
//	CONFORMANCE_TARGET=aws       Amazon S3 using the AWS SDK credential chain
//
// CONFORMANCE_REPORT writes the observations; CONFORMANCE_BASELINE compares
// every recorded step against an earlier report (for example one from AWS).
package conformance_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
	awsbackend "github.com/adrianliechti/s3-gateway/backend/aws"
	"github.com/adrianliechti/s3-gateway/backend/azure"
	"github.com/adrianliechti/s3-gateway/backend/disk"
	"github.com/adrianliechti/s3-gateway/gateway"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
)

const localAccess, localSecret = "conformance-access", "conformance-secret"

type target struct {
	name, endpoint, region string
	creds                  aws.Credentials
	client                 *http.Client
	// aws marks Amazon S3 itself: every documented expectation must hold and
	// the known-divergence list is ignored.
	aws bool
	// posix marks a disk-backed gateway whose keys must be valid file paths.
	posix bool
	// bucket pins every scenario to one existing bucket (CONFORMANCE_BUCKET):
	// scenarios run one at a time, the bucket is emptied but never deleted,
	// and steps that create, delete or reconfigure buckets are skipped.
	bucket string
	// allowVersioning permits changing versioning on the fixed bucket, which
	// cannot be undone (CONFORMANCE_ALLOW_VERSIONING=1).
	allowVersioning bool
	// versioning is the fixed bucket's status at startup ("" when unversioned);
	// disposable buckets always start unversioned.
	versioning string
	close      func()
}

// Scenarios share the fixed bucket sequentially.
var scenarioLock sync.Mutex

func parallel(t *testing.T) {
	if current == nil || current.bucket == "" {
		t.Parallel()
	}
}

var current *target

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func newTarget(ctx context.Context) (*target, error) {
	tg := &target{region: env("CONFORMANCE_REGION", "us-east-1"), bucket: os.Getenv("CONFORMANCE_BUCKET"), allowVersioning: os.Getenv("CONFORMANCE_ALLOW_VERSIONING") == "1", client: &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{DisableCompression: true, Proxy: http.ProxyFromEnvironment}}, close: func() {}}
	switch {
	case os.Getenv("CONFORMANCE_TARGET") == "aws":
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(tg.region))
		if err != nil {
			return nil, err
		}
		tg.creds, err = cfg.Credentials.Retrieve(ctx)
		if err != nil {
			return nil, fmt.Errorf("AWS credentials: %w", err)
		}
		tg.name, tg.aws = "aws", true
		tg.endpoint = env("CONFORMANCE_ENDPOINT", "https://s3."+tg.region+".amazonaws.com")
	case os.Getenv("CONFORMANCE_ENDPOINT") != "":
		tg.endpoint = strings.TrimSuffix(os.Getenv("CONFORMANCE_ENDPOINT"), "/")
		tg.name = env("CONFORMANCE_NAME", "endpoint")
		tg.posix = os.Getenv("CONFORMANCE_BACKEND") == "disk"
		tg.creds = aws.Credentials{AccessKeyID: env("CONFORMANCE_ACCESS_KEY", os.Getenv("GATEWAY_ACCESS_KEY")), SecretAccessKey: env("CONFORMANCE_SECRET_KEY", os.Getenv("GATEWAY_SECRET_KEY")), SessionToken: os.Getenv("CONFORMANCE_SESSION_TOKEN")}
		if tg.creds.AccessKeyID == "" || tg.creds.SecretAccessKey == "" {
			return nil, fmt.Errorf("CONFORMANCE_ACCESS_KEY and CONFORMANCE_SECRET_KEY are required for an external endpoint")
		}
	default:
		var be backend.Backend
		var err error
		cleanup := func() {}
		switch {
		case os.Getenv("GATEWAY_TEST_STORAGE_ENDPOINT") != "":
			tg.name = "s3"
			be, err = awsbackend.New(ctx, awsbackend.Options{Endpoint: os.Getenv("GATEWAY_TEST_STORAGE_ENDPOINT"), Region: "us-east-1", UsePathStyle: true})
		case os.Getenv("GATEWAY_TEST_AZURE") == "1":
			tg.name = "azure"
			be, err = azure.New(azure.Options{Account: os.Getenv("AZURE_STORAGE_ACCOUNT"), AccountKey: os.Getenv("AZURE_STORAGE_KEY"), ServiceURL: os.Getenv("AZURE_STORAGE_SERVICE_URL"), SASToken: os.Getenv("AZURE_STORAGE_SAS_TOKEN")})
		case os.Getenv("GATEWAY_TEST_AZURITE_URL") != "":
			tg.name = "azurite"
			be, err = azure.New(azure.Options{Account: "gateway", AccountKey: "Y29tcGF0aWJpbGl0eS10ZXN0LWtleS1vbmx5", ServiceURL: os.Getenv("GATEWAY_TEST_AZURITE_URL")})
		default:
			tg.name, tg.posix = "disk", true
			root, e := os.MkdirTemp("", "s3gw-conformance-*")
			if e != nil {
				return nil, e
			}
			cleanup = func() { os.RemoveAll(root) }
			be, err = disk.New(disk.Options{Root: root})
		}
		if err != nil {
			cleanup()
			return nil, err
		}
		g, err := gateway.New(be, gateway.Options{AccessKey: localAccess, SecretKey: localSecret, Region: tg.region})
		if err != nil {
			be.Close()
			cleanup()
			return nil, err
		}
		srv := httptest.NewServer(g)
		tg.endpoint = srv.URL
		tg.creds = aws.Credentials{AccessKeyID: localAccess, SecretAccessKey: localSecret}
		tg.close = func() { srv.Close(); be.Close(); cleanup() }
	}
	// External fixtures may still be starting.
	var last error
	for attempt := 0; attempt < 60; attempt++ {
		ready := request{method: "GET"}
		if tg.bucket != "" {
			ready = request{method: "GET", bucket: tg.bucket, query: q("list-type", "2", "max-keys", "0")}
		}
		res, err := tg.send(ctx, ready)
		if err == nil && res.Status == 200 {
			if tg.bucket != "" {
				// Fixed-bucket scenarios use exact keys and listing expectations.
				// Refuse existing data instead of treating it as disposable fixtures.
				for _, probe := range []request{
					{method: "GET", bucket: tg.bucket, query: q("versions", "", "max-keys", "1", "encoding-type", "url")},
					{method: "GET", bucket: tg.bucket, query: q("uploads", "", "max-uploads", "1")},
				} {
					listed, err := tg.send(ctx, probe)
					if err != nil {
						return nil, err
					}
					if listed.Status != 200 || listed.doc == nil {
						return nil, fmt.Errorf("cannot verify that fixed bucket %s is empty: %d %s", tg.bucket, listed.Status, listed.Code)
					}
					if listed.has("ListVersionsResult.Version") || listed.has("ListVersionsResult.DeleteMarker") || listed.has("ListMultipartUploadsResult.Upload") {
						if os.Getenv("CONFORMANCE_PURGE_FIXED_BUCKET") == "1" {
							// Explicit opt-in for buckets dedicated to conformance runs.
							h := &H{tg: tg, bucket: tg.bucket, buckets: []string{tg.bucket}, scenario: "purge"}
							if err := h.purge(ctx); err != nil {
								return nil, err
							}
							break
						}
						return nil, fmt.Errorf("fixed bucket %s must have no objects, versions, delete markers or multipart uploads before conformance testing:\n%s", tg.bucket, tg.inventory(ctx))
					}
				}
				status, err := tg.send(ctx, request{method: "GET", bucket: tg.bucket, query: q("versioning", "")})
				if err != nil {
					return nil, err
				}
				tg.versioning = status.xml("VersioningConfiguration.Status")
			}
			return tg, nil
		}
		if err == nil {
			last = fmt.Errorf("readiness request %s returned %d %s: %s", h0(ready), res.Status, res.Code, res.Message)
		} else {
			last = err
		}
		if tg.aws {
			break
		}
		time.Sleep(time.Second)
	}
	tg.close()
	return nil, fmt.Errorf("target %s at %s is not usable: %v", tg.name, tg.endpoint, last)
}

func h0(r request) string {
	if r.bucket == "" {
		return "ListBuckets"
	}
	return "ListObjectsV2 on " + r.bucket
}

// request describes one raw S3 HTTP request. Fields default to a correctly
// signed SigV4 request with a hashed payload sent by the configured identity.
type request struct {
	method, bucket, key string
	query               url.Values
	header              http.Header
	body                []byte
	// Authentication variations.
	anonymous         bool
	accessKey, secret string
	region            string
	at                time.Time
	presign           time.Duration
	// Payload variations: the declared hash replaces the computed one.
	payloadHash string
	contentMD5  bool
}
type response struct {
	step    string
	Status  int
	Header  http.Header
	Body    []byte
	Code    string
	Message string
	doc     *node
}

func (r *response) xml(path string) string {
	if r.doc == nil {
		return ""
	}
	return r.doc.lookup(path)
}
func (r *response) has(path string) bool      { return r.doc != nil && r.doc.exists(path) }
func (r *response) header(name string) string { return r.Header.Get(name) }

// AWS percent-encodes every byte outside the unreserved set; slashes in
// object keys remain literal path separators.
func awsEncode(s string, keepSlash bool) string {
	const digits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' || (keepSlash && c == '/') {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(digits[c>>4])
			b.WriteByte(digits[c&15])
		}
	}
	return b.String()
}
func encodeQuery(q url.Values) string {
	var keys []string
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, awsEncode(k, false)+"="+awsEncode(v, false))
		}
	}
	return strings.Join(parts, "&")
}
func (tg *target) build(ctx context.Context, r request) (*http.Request, error) {
	u, err := url.Parse(tg.endpoint)
	if err != nil {
		return nil, err
	}
	path := "/"
	if r.bucket != "" {
		path += r.bucket
		if r.key != "" {
			path += "/" + r.key
		}
	}
	u.Path, u.RawPath, u.RawQuery = path, awsEncode(path, true), encodeQuery(r.query)
	req, err := http.NewRequestWithContext(ctx, r.method, u.String(), bytes.NewReader(r.body))
	if err != nil {
		return nil, err
	}
	for k, vs := range r.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if r.contentMD5 {
		sum := md5.Sum(r.body)
		req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	}
	if r.anonymous {
		return req, nil
	}
	creds := tg.creds
	if r.accessKey != "" {
		creds.AccessKeyID = r.accessKey
	}
	if r.secret != "" {
		creds.SecretAccessKey = r.secret
	}
	region := tg.region
	if r.region != "" {
		region = r.region
	}
	at := r.at
	if at.IsZero() {
		at = time.Now()
	}
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if r.presign > 0 {
		q := req.URL.Query()
		q.Set("X-Amz-Expires", strconv.Itoa(int(r.presign/time.Second)))
		req.URL.RawQuery = encodeQuery(q)
		signed, headers, err := signer.PresignHTTP(ctx, creds, req, "UNSIGNED-PAYLOAD", "s3", region, at)
		if err != nil {
			return nil, err
		}
		out, err := http.NewRequestWithContext(ctx, r.method, signed, bytes.NewReader(r.body))
		if err != nil {
			return nil, err
		}
		for k, vs := range headers {
			out.Header[k] = vs
		}
		for k, vs := range r.header {
			out.Header[k] = vs
		}
		return out, nil
	}
	hash := r.payloadHash
	if hash == "" {
		sum := sha256.Sum256(r.body)
		hash = hex.EncodeToString(sum[:])
	}
	req.Header.Set("X-Amz-Content-Sha256", hash)
	if err := signer.SignHTTP(ctx, creds, req, hash, "s3", region, at); err != nil {
		return nil, err
	}
	return req, nil
}

// send performs one request. Bodies live in memory, so a connection that the
// server drops while rejecting a request (S3 does this for some 4xx answers)
// is retried once; a second failure is reported.
func (tg *target) send(ctx context.Context, r request) (*response, error) {
	var res *http.Response
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var req *http.Request
		req, err = tg.build(ctx, r)
		if err != nil {
			return nil, err
		}
		res, err = tg.client.Do(req)
		if err == nil || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	out := &response{Status: res.StatusCode, Header: res.Header, Body: body}
	if len(body) > 0 && bytes.HasPrefix(bytes.TrimSpace(body), []byte("<")) {
		if doc, e := parseXML(body); e == nil {
			out.doc = doc
			if doc.name == "Error" {
				out.Code, out.Message = doc.lookup("Error.Code"), doc.lookup("Error.Message")
			}
		}
	}
	return out, nil
}

// H is one scenario: a disposable bucket, recorded steps and expectations.
type H struct {
	t        *testing.T
	tg       *target
	scenario string
	bucket   string
	buckets  []string
	keys     map[string]map[string]bool
	// fail replaces t.Errorf for cleanup outside a test (bucket purge).
	fail func(what string, res *response, err error)
}

func begin(t *testing.T, scenario string) *H {
	t.Helper()
	if current == nil {
		t.Fatal("no target")
	}
	h := &H{t: t, tg: current, scenario: scenario}
	if current.bucket != "" {
		scenarioLock.Lock()
		t.Cleanup(scenarioLock.Unlock)
		h.bucket = current.bucket
		h.buckets = []string{h.bucket}
		t.Cleanup(func() { h.removeBucket(h.bucket) })
		return h
	}
	h.bucket = h.newBucket()
	return h
}

// unversioned reports whether expectations that need a never-versioned
// bucket apply; a fixed bucket with versioning history logs and skips them.
func (h *H) unversioned(step string) bool {
	if h.tg.versioning == "" {
		return true
	}
	h.t.Logf("skipping %s: bucket %s has versioning %s", step, h.bucket, h.tg.versioning)
	return false
}

// settle waits until the clock has moved past an object's Last-Modified
// second, so a date between modification and now can be formed. S3 ignores
// If-Modified-Since dates in the future (RFC 7232 section 3.3).
func settle(modified time.Time) (between, future string) {
	for time.Now().Before(modified.Add(2 * time.Second)) {
		time.Sleep(200 * time.Millisecond)
	}
	return modified.Add(time.Second).UTC().Format(http.TimeFormat), modified.Add(24 * time.Hour).UTC().Format(http.TimeFormat)
}

// fixed reports whether the scenario runs in an existing bucket; steps that
// would create, delete or reconfigure buckets log and skip.
func (h *H) fixed(step string) bool {
	if h.tg.bucket == "" {
		return false
	}
	h.t.Logf("skipping %s: fixed bucket %s is never created, deleted or reconfigured", step, h.tg.bucket)
	return true
}
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
func (h *H) newBucket() string {
	h.t.Helper()
	name := fmt.Sprintf("s3gw-conf-%d-%s", time.Now().UnixNano()%1e12, randomHex(3))
	res := h.raw(request{method: "PUT", bucket: name})
	if res.Status != 200 {
		h.t.Fatalf("create bucket %s: %d %s %s", name, res.Status, res.Code, res.Message)
	}
	h.buckets = append(h.buckets, name)
	h.t.Cleanup(func() { h.removeBucket(name) })
	return name
}

// removeBucket aborts uploads, deletes every version and the bucket itself,
// so a run against Amazon S3 leaves nothing behind.
func (h *H) removeBucket(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fail := h.fail
	if fail == nil {
		fail = func(what string, res *response, err error) {
			if err != nil {
				h.t.Errorf("cleanup %s %s: %v", name, what, err)
			} else {
				h.t.Errorf("cleanup %s %s: %d %s %s", name, what, res.Status, res.Code, res.Message)
			}
		}
	}
	uploads, err := h.tg.send(ctx, request{method: "GET", bucket: name, query: q("uploads", "")})
	if err != nil || uploads.Status != 200 {
		fail("list uploads", uploads, err)
		return
	}
	for i := 0; uploads.has(fmt.Sprintf("ListMultipartUploadsResult.Upload[%d]", i)); i++ {
		key, id := uploads.xml(fmt.Sprintf("ListMultipartUploadsResult.Upload[%d].Key", i)), uploads.xml(fmt.Sprintf("ListMultipartUploadsResult.Upload[%d].UploadId", i))
		if name == h.tg.bucket && !h.keys[name][key] {
			continue
		}
		if res, err := h.tg.send(ctx, request{method: "DELETE", bucket: name, key: key, query: q("uploadId", id)}); err != nil || res.Status != 204 {
			fail("abort "+key, res, err)
		}
	}
	// Keys are listed URL-encoded and deleted one version at a time: keys
	// with XML-incompatible characters cannot appear in a DeleteObjects body.
	for round := 0; round < 100; round++ {
		versions, err := h.tg.send(ctx, request{method: "GET", bucket: name, query: q("versions", "", "max-keys", "1000", "encoding-type", "url")})
		if (err != nil || versions.Status != 200) && round == 0 && len(h.keys[name]) > 0 {
			// A listing failure (recorded as a divergence by the scenario)
			// must not leave buckets behind: remove the keys written here.
			for key := range h.keys[name] {
				_, _ = h.tg.send(ctx, request{method: "DELETE", bucket: name, key: key})
			}
			continue
		}
		if err != nil || versions.Status != 200 {
			fail("list versions", versions, err)
			return
		}
		count := 0
		for _, kind := range []string{"Version", "DeleteMarker"} {
			for i := 0; versions.has(fmt.Sprintf("ListVersionsResult.%s[%d]", kind, i)); i++ {
				key, err := url.QueryUnescape(versions.xml(fmt.Sprintf("ListVersionsResult.%s[%d].Key", kind, i)))
				if err != nil {
					fail("decode key "+key, nil, err)
					return
				}
				if name == h.tg.bucket && !h.keys[name][key] {
					continue
				}
				count++
				id := versions.xml(fmt.Sprintf("ListVersionsResult.%s[%d].VersionId", kind, i))
				query := url.Values{}
				if id != "" {
					query.Set("versionId", id)
				}
				if res, err := h.tg.send(ctx, request{method: "DELETE", bucket: name, key: key, query: query}); err != nil || res.Status != 204 {
					fail("delete "+key, res, err)
					return
				}
			}
		}
		if count == 0 {
			break
		}
	}
	if name == h.tg.bucket {
		return
	}
	if res, err := h.tg.send(ctx, request{method: "DELETE", bucket: name}); err != nil || res.Status != 204 {
		fail("delete bucket", res, err)
	}
}

// inventory describes what a fixed bucket currently holds (first 1000
// versions and uploads), so an operator can decide what to do with it.
func (tg *target) inventory(ctx context.Context) string {
	var b strings.Builder
	versions, err := tg.send(ctx, request{method: "GET", bucket: tg.bucket, query: q("versions", "", "encoding-type", "url", "max-keys", "1000")})
	if err != nil {
		return err.Error()
	}
	for _, kind := range []string{"Version", "DeleteMarker"} {
		for i := 0; versions.has(fmt.Sprintf("ListVersionsResult.%s[%d]", kind, i)); i++ {
			key, _ := url.QueryUnescape(versions.xml(fmt.Sprintf("ListVersionsResult.%s[%d].Key", kind, i)))
			fmt.Fprintf(&b, "  %s %q %s\n", strings.ToLower(kind), key, versions.xml(fmt.Sprintf("ListVersionsResult.%s[%d].VersionId", kind, i)))
		}
	}
	uploads, err := tg.send(ctx, request{method: "GET", bucket: tg.bucket, query: q("uploads", "")})
	if err != nil {
		return err.Error()
	}
	for i := 0; uploads.has(fmt.Sprintf("ListMultipartUploadsResult.Upload[%d]", i)); i++ {
		fmt.Fprintf(&b, "  upload %q %s\n", uploads.xml(fmt.Sprintf("ListMultipartUploadsResult.Upload[%d].Key", i)), uploads.xml(fmt.Sprintf("ListMultipartUploadsResult.Upload[%d].UploadId", i)))
	}
	b.WriteString("Set CONFORMANCE_PURGE_FIXED_BUCKET=1 to delete everything in a bucket dedicated to conformance runs.")
	return b.String()
}

// purge empties the fixed bucket using the scenario cleanup, reporting the
// first failure as an error instead of a test failure.
func (h *H) purge(ctx context.Context) error {
	var failure error
	h.fail = func(what string, res *response, err error) {
		if failure == nil {
			if err != nil {
				failure = fmt.Errorf("purge %s: %v", what, err)
			} else {
				failure = fmt.Errorf("purge %s: %d %s %s", what, res.Status, res.Code, res.Message)
			}
		}
	}
	h.removeBucket(h.bucket)
	return failure
}

// raw sends without recording; do records the step and compares baselines.
func (h *H) raw(r request) *response {
	h.t.Helper()
	res, err := h.tg.send(h.t.Context(), r)
	if err != nil {
		h.t.Fatalf("%s %s/%s: %v", r.method, r.bucket, r.key, err)
	}
	h.track(r)
	return res
}

// do records the step. A connection failure (for example a handler that
// aborts mid-response) is recorded as status 0 with code TransportError so
// the scenario continues and the expectations report the divergence.
func (h *H) do(step string, r request) *response {
	h.t.Helper()
	res, err := h.tg.send(h.t.Context(), r)
	if err != nil {
		res = &response{Header: http.Header{}, Code: "TransportError", Message: err.Error()}
	}
	h.track(r)
	res.step = step
	h.record(step, r, res)
	return res
}

// track remembers keys written to scenario buckets, so cleanup can still
// remove them when the listing APIs fail.
func (h *H) track(r request) {
	if r.method != "PUT" && r.method != "POST" && r.method != "DELETE" || r.bucket == "" {
		return
	}
	keys := []string{r.key}
	if r.method == "POST" && r.query.Has("delete") {
		var batch struct {
			Keys []string `xml:"Object>Key"`
		}
		if xml.Unmarshal(r.body, &batch) == nil {
			keys = append(keys, batch.Keys...)
		}
	}
	if h.keys == nil {
		h.keys = map[string]map[string]bool{}
	}
	if h.keys[r.bucket] == nil {
		h.keys[r.bucket] = map[string]bool{}
	}
	for _, key := range keys {
		if key != "" {
			h.keys[r.bucket][key] = true
		}
	}
}

// Convenience request builders for the default bucket.
func q(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return v
}
func hdr(kv ...string) http.Header {
	v := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return v
}
func (h *H) put(step, key string, body []byte, header http.Header) *response {
	h.t.Helper()
	return h.do(step, request{method: "PUT", bucket: h.bucket, key: key, body: body, header: header})
}
func (h *H) get(step, key string, header http.Header, query url.Values) *response {
	h.t.Helper()
	return h.do(step, request{method: "GET", bucket: h.bucket, key: key, header: header, query: query})
}
func (h *H) head(step, key string, header http.Header, query url.Values) *response {
	h.t.Helper()
	return h.do(step, request{method: "HEAD", bucket: h.bucket, key: key, header: header, query: query})
}
func (h *H) delete(step, key string, header http.Header, query url.Values) *response {
	h.t.Helper()
	return h.do(step, request{method: "DELETE", bucket: h.bucket, key: key, header: header, query: query})
}

// seed stores fixture objects without recording or asserting anything.
func (h *H) seed(key string, body []byte, header http.Header) *response {
	h.t.Helper()
	res := h.raw(request{method: "PUT", bucket: h.bucket, key: key, body: body, header: header})
	if res.Status != 200 {
		h.t.Fatalf("seed %q: %d %s %s", key, res.Status, res.Code, res.Message)
	}
	return res
}

// require ends the scenario when a prerequisite fails; it is not a divergence.
func (h *H) require(res *response, status int) *response {
	h.t.Helper()
	if res.Status != status {
		h.t.Fatalf("%s: HTTP %d %s %s, need %d", res.step, res.Status, res.Code, res.Message, status)
	}
	return res
}
func md5Hex(b []byte) string { sum := md5.Sum(b); return hex.EncodeToString(sum[:]) }
func quote(s string) string  { return `"` + s + `"` }
