// Package gateway implements an S3 HTTP endpoint over the backend contract.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/adrianliechti/s3-gateway/backend/managed"
	"github.com/adrianliechti/s3-gateway/internal/identity"
	"github.com/adrianliechti/s3-gateway/notification"
)

type Options struct {
	Notifications                                                            notification.Publisher
	Identity                                                                 *identity.Config
	Listen, Region, Domain, AccessKey, SecretKey, CertFile, KeyFile, TempDir string
	ReadOnly                                                                 bool
	LifecycleInterval                                                        time.Duration
	// LifecycleTestDay accelerates object age only in disposable test fixtures.
	LifecycleTestDay time.Duration
}

// Public aliases let applications embedding the gateway configure identity
// without importing its internal implementation package.
type IdentityConfig = identity.Config
type IdentityRole = identity.Role
type IdentityPolicy = identity.Policy
type IdentityStatement = identity.Statement
type PolicyStrings = identity.Strings
type Gateway struct {
	notificationMu sync.Mutex
	identity       *identity.Provider
	be             *managed.Store
	opts           Options
	multipart      [256]sync.RWMutex
}

func New(be backend.Backend, o Options) (*Gateway, error) {
	if be == nil || o.AccessKey == "" || o.SecretKey == "" {
		return nil, fmt.Errorf("backend and S3 credentials are required")
	}
	if o.Region == "" {
		o.Region = "us-east-1"
	}
	if o.Listen == "" {
		o.Listen = "127.0.0.1:9000"
	}
	if (o.CertFile == "") != (o.KeyFile == "") {
		return nil, fmt.Errorf("TLS certificate and key must be provided together")
	}
	if o.LifecycleInterval < 0 || o.LifecycleTestDay < 0 || o.LifecycleTestDay > 24*time.Hour {
		return nil, fmt.Errorf("invalid lifecycle clock configuration")
	}
	if o.LifecycleInterval == 0 {
		o.LifecycleInterval = time.Minute
	}
	g := &Gateway{be: managed.New(be), opts: o}
	if o.Identity != nil {
		var err error
		g.identity, err = identity.New(*o.Identity, o.AccessKey, o.SecretKey)
		if err != nil {
			return nil, err
		}
	}
	return g, nil
}
func Run(ctx context.Context, be backend.Backend, o Options) error {
	g, err := New(be, o)
	if err != nil {
		return err
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); g.runLifecycle(workerCtx) }()
	defer func() { stopWorker(); <-workerDone }()
	srv := &http.Server{Addr: g.opts.Listen, Handler: g, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 20}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := srv.Shutdown(c); err != nil {
				_ = srv.Close()
			}
		case <-done:
		}
	}()
	if o.CertFile != "" {
		err = srv.ListenAndServeTLS(o.CertFile, o.KeyFile)
	} else {
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

const xmlns = "http://s3.amazonaws.com/doc/2006-03-01/"

type s3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string
	Message   string
	Resource  string `xml:",omitempty"`
	RequestID string `xml:"RequestId"`
	Status    int    `xml:"-"`
}

func (e *s3Error) Error() string { return e.Code + ": " + e.Message }
func apiError(code string, status int, message string) error {
	return &s3Error{Code: code, Status: status, Message: message}
}
func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var e *s3Error
	if !errors.As(err, &e) {
		e = &s3Error{Code: "InternalError", Status: 500, Message: "An internal error occurred"}
		switch {
		case errors.Is(err, backend.ErrNotFound):
			e.Code = "NoSuchKey"
			e.Status = 404
			e.Message = "The specified key does not exist"
		case errors.Is(err, backend.ErrVersionNotFound):
			e.Code, e.Status, e.Message = "NoSuchVersion", 404, "The specified version does not exist"
		case errors.Is(err, backend.ErrDeleteMarker):
			e.Code, e.Status, e.Message = "MethodNotAllowed", 405, "The specified version is a delete marker"
		case errors.Is(err, backend.ErrBucketNotFound):
			e.Code = "NoSuchBucket"
			e.Status = 404
			e.Message = "The specified bucket does not exist"
		case errors.Is(err, backend.ErrBucketExists):
			e.Code = "BucketAlreadyOwnedByYou"
			e.Status = 409
			e.Message = "The bucket already exists"
		case errors.Is(err, backend.ErrBucketNotEmpty):
			e.Code = "BucketNotEmpty"
			e.Status = 409
			e.Message = "The bucket is not empty"
		case errors.Is(err, backend.ErrPrecondition):
			e.Code = "PreconditionFailed"
			e.Status = 412
			e.Message = "A request precondition failed"
		case errors.Is(err, backend.ErrInvalidKey):
			e.Code = "InvalidArgument"
			e.Status = 400
			e.Message = "The key cannot be represented by this backend"
		case errors.Is(err, backend.ErrInvalidRequest):
			e.Code, e.Status, e.Message = "InvalidRequest", 400, "The storage provider rejected the request"
		case errors.Is(err, backend.ErrConflict):
			e.Code = "OperationAborted"
			e.Status = 409
			e.Message = "A conflicting operation is in progress"
		case errors.Is(err, backend.ErrMetadataTooLarge):
			e.Code = "MetadataTooLarge"
			e.Status = 400
			e.Message = "Metadata exceeds the backend capacity"
		}
		if e.Status == 500 {
			slog.Error("S3 request failed", "request_id", w.Header().Get("x-amz-request-id"), "error", err)
		}
	}
	for name := range w.Header() {
		if name == "Content-Length" || name == "Content-Encoding" || name == "ETag" || strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") || strings.HasPrefix(strings.ToLower(name), "x-amz-checksum-") {
			delete(w.Header(), name)
		}
	}
	copy := *e
	copy.Resource = r.URL.Path
	copy.RequestID = w.Header().Get("x-amz-request-id")
	if r.Method == http.MethodHead {
		w.WriteHeader(copy.Status)
		return
	}
	writeXML(w, copy.Status, copy)
}
func id() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
var versionID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// validVersionID accepts the gateway's own identifiers and the null version;
// anything else is rejected before lookup, as S3 does for malformed ids.
func validVersionID(v string) bool { return v == "null" || versionID.MatchString(v) }

func validBucket(b string) bool {
	return bucketName.MatchString(b) && !strings.Contains(b, "..") && !strings.Contains(b, ".-") && !strings.Contains(b, "-.") && net.ParseIP(b) == nil && !strings.HasPrefix(b, "xn--") && !strings.HasSuffix(b, "-s3alias") && !strings.HasSuffix(b, "--ol-s3") && !strings.HasSuffix(b, ".mrap") && !strings.HasSuffix(b, "--x-s3") && !strings.HasSuffix(b, "--table-s3")
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		r = r.WithContext(g.be.ReadContext(r.Context()))
	}
	w.Header().Set("x-amz-request-id", id())
	w.Header().Set("x-amz-id-2", id())
	w.Header().Set("x-amz-bucket-region", g.opts.Region)
	b, k := g.address(r)
	if b == "" && k == "" && (r.Method == "POST" || r.URL.Query().Has("Action")) {
		g.sts(w, r)
		return
	}
	control := strings.HasPrefix(r.URL.Path, "/v20180820/tags/")
	if control {
		b = strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v20180820/tags/"), "arn:aws:s3:::")
		k = ""
	}
	if r.Method == "OPTIONS" {
		if b == "" || !validBucket(b) {
			writeError(w, r, apiError("InvalidBucketName", 400, "Invalid bucket name"))
			return
		}
		if err := g.cors(w, r, b, true); err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(200)
		return
	}
	sig, err := g.authenticate(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, sig.principal))
	if b != "" && !validBucket(b) {
		writeError(w, r, apiError("InvalidBucketName", 400, "Invalid bucket name"))
		return
	}
	if strings.HasPrefix(k, backend.InternalPrefix) {
		writeError(w, r, apiError("AccessDenied", 403, "Reserved internal namespace"))
		return
	}
	if len([]byte(k)) > 1024 {
		writeError(w, r, apiError("KeyTooLongError", 400, "Object key is too long"))
		return
	}
	if g.opts.ReadOnly && r.Method != "GET" && r.Method != "HEAD" {
		writeError(w, r, apiError("AccessDenied", 403, "Gateway is read-only"))
		return
	}
	if control {
		if err := g.resourceTags(w, r, b, &sig); err != nil {
			writeError(w, r, err)
		}
		return
	}
	// Reject unsupported semantics explicitly; never silently ignore encryption,
	// version IDs, ACLs, object lock or bucket-management subresources.
	if err = g.validateFeatures(r); err != nil {
		writeError(w, r, err)
		return
	}
	if err = g.authorizeRequest(r, b, k); err != nil {
		writeError(w, r, err)
		return
	}
	if err = g.enforceBucketACL(r, b, k); err != nil {
		writeError(w, r, err)
		return
	}
	if b != "" && r.Header.Get("Origin") != "" {
		if err = g.cors(w, r, b, false); err != nil {
			writeError(w, r, err)
			return
		}
	}
	if b == "" {
		err = g.service(w, r)
	} else if k == "" {
		err = g.bucket(w, r, b, &sig)
	} else {
		err = g.object(w, r, b, k, &sig)
	}
	if err != nil {
		writeError(w, r, err)
	}
}
func (g *Gateway) address(r *http.Request) (string, string) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if g.opts.Domain != "" && strings.HasSuffix(host, "."+g.opts.Domain) {
		return strings.TrimSuffix(host, "."+g.opts.Domain), strings.TrimPrefix(r.URL.Path, "/")
	}
	p := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(p) == 1 {
		return p[0], ""
	}
	return p[0], p[1]
}
func (g *Gateway) validateFeatures(r *http.Request) error {
	if !validCannedACL(r.Header.Get("X-Amz-Acl")) {
		return apiError("InvalidArgument", 400, "Invalid canned ACL")
	}
	q := r.URL.Query()
	if principal(r) != nil {
		count := 0
		for _, name := range []string{"tagging", "abac", "notification", "cors", "publicAccessBlock", "ownershipControls", "lifecycle", "acl", "location", "versioning", "versions", "uploads", "delete", "attributes", "uploadId"} {
			if q.Has(name) {
				count++
				if len(q[name]) != 1 {
					return apiError("InvalidRequest", 400, "Duplicate subresource")
				}
			}
		}
		if count > 1 {
			return apiError("InvalidRequest", 400, "Conflicting subresources")
		}
	}
	_, key := g.address(r)
	if key != "" {
		for _, name := range []string{"cors", "publicAccessBlock", "ownershipControls", "lifecycle", "abac", "notification"} {
			if q.Has(name) {
				return apiError("InvalidRequest", 400, "This configuration applies to a bucket")
			}
		}
	}
	if q.Has("versionId") && !validVersionID(q.Get("versionId")) {
		return apiError("InvalidArgument", 400, "Invalid version id specified")
	}
	for _, k := range []string{"policy", "replication", "encryption", "object-lock", "retention", "legal-hold", "website", "logging", "restore", "select", "torrent", "accelerate", "requestPayment", "inventory", "analytics", "metrics"} {
		if q.Has(k) {
			return apiError("NotImplemented", 501, "This S3 feature is not implemented")
		}
	}
	for _, k := range []string{"annotation", "metadataConfiguration", "metadataTable", "metadataAnnotationTable", "metadataInventoryTable", "metadataJournalTable", "intelligent-tiering", "policyStatus", "renameObject", "session", "qos", "qos-metrics", "events", "metadata", "allow-unordered", "max-directory-buckets"} {
		if q.Has(k) {
			return apiError("NotImplemented", 501, "This S3 operation or extension is not implemented")
		}
	}
	if q.Get("x-id") == "ListDirectoryBuckets" {
		return apiError("NotImplemented", 501, "Directory buckets are not implemented")
	}
	for k := range r.Header {
		l := strings.ToLower(k)
		if l == "x-amz-if-match-last-modified-time" || l == "x-amz-if-match-size" {
			return apiError("NotImplemented", 501, "This conditional delete header is not implemented")
		}
		if l == "x-amz-write-offset-bytes" || l == "x-amz-rename-source" || l == "x-amz-expected-bucket-owner" || l == "x-amz-source-expected-bucket-owner" || l == "x-amz-request-payer" || strings.HasPrefix(l, "x-minio-extract") {
			return apiError("NotImplemented", 501, "This request feature is not implemented")
		}
		if strings.HasPrefix(l, "x-amz-server-side-encryption") || strings.HasPrefix(l, "x-amz-copy-source-server-side-encryption") || strings.HasPrefix(l, "x-amz-object-lock") || l == "x-amz-bucket-object-lock-enabled" || l == "x-amz-bypass-governance-retention" || l == "x-amz-mfa" {
			return apiError("NotImplemented", 501, "This object feature is not implemented")
		}
	}
	if v := r.Header.Get("X-Amz-Storage-Class"); v != "" && v != "STANDARD" {
		return apiError("InvalidStorageClass", 400, "Only STANDARD storage is supported")
	}
	return nil
}
