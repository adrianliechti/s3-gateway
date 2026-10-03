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
)

type Options struct {
	Listen, Region, Domain, AccessKey, SecretKey, CertFile, KeyFile, TempDir string
	ReadOnly                                                                 bool
}
type Gateway struct {
	be        *managed.Store
	opts      Options
	multipart [256]sync.RWMutex
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
	return &Gateway{be: managed.New(be), opts: o}, nil
}
func Run(ctx context.Context, be backend.Backend, o Options) error {
	g, err := New(be, o)
	if err != nil {
		return err
	}
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

func validBucket(b string) bool {
	return bucketName.MatchString(b) && !strings.Contains(b, "..") && !strings.Contains(b, ".-") && !strings.Contains(b, "-.") && net.ParseIP(b) == nil && !strings.HasPrefix(b, "xn--") && !strings.HasSuffix(b, "-s3alias") && !strings.HasSuffix(b, "--ol-s3") && !strings.HasSuffix(b, ".mrap") && !strings.HasSuffix(b, "--x-s3") && !strings.HasSuffix(b, "--table-s3")
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("x-amz-request-id", id())
	w.Header().Set("x-amz-id-2", id())
	w.Header().Set("x-amz-bucket-region", g.opts.Region)
	sig, err := g.authenticate(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	b, k := g.address(r)
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
	// Reject unsupported semantics explicitly; never silently ignore encryption,
	// version IDs, ACLs, object lock or bucket-management subresources.
	if err = g.validateFeatures(r); err != nil {
		writeError(w, r, err)
		return
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
	q := r.URL.Query()
	if q.Has("versionId") && q.Get("versionId") == "" {
		return apiError("InvalidArgument", 400, "Version ID must not be empty")
	}
	for _, k := range []string{"policy", "lifecycle", "cors", "replication", "encryption", "object-lock", "retention", "legal-hold", "website", "notification", "logging", "restore", "select", "torrent", "ownershipControls", "publicAccessBlock", "accelerate", "requestPayment", "inventory", "analytics", "metrics"} {
		if q.Has(k) {
			return apiError("NotImplemented", 501, "This S3 feature is not implemented")
		}
	}
	for _, k := range []string{"annotation", "metadataConfiguration", "metadataTable", "metadataAnnotationTable", "metadataInventoryTable", "metadataJournalTable", "intelligent-tiering", "abac", "policyStatus", "renameObject", "session", "qos", "qos-metrics", "events", "metadata", "allow-unordered", "max-directory-buckets"} {
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
		if strings.HasPrefix(l, "x-amz-server-side-encryption") || strings.HasPrefix(l, "x-amz-object-lock") || l == "x-amz-mfa" {
			return apiError("NotImplemented", 501, "This object feature is not implemented")
		}
	}
	if v := r.Header.Get("X-Amz-Storage-Class"); v != "" && v != "STANDARD" {
		return apiError("InvalidStorageClass", 400, "Only STANDARD storage is supported")
	}
	return nil
}
