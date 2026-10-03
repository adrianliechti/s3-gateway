// Package backend defines the storage contract independently of HTTP and AWS SDKs.
package backend

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrBucketNotFound   = errors.New("NoSuchBucket")
	ErrBucketExists     = errors.New("BucketAlreadyOwnedByYou")
	ErrBucketNotEmpty   = errors.New("BucketNotEmpty")
	ErrNotFound         = errors.New("NoSuchKey")
	ErrPrecondition     = errors.New("PreconditionFailed")
	ErrInvalidKey       = errors.New("InvalidObjectName")
	ErrInvalidRequest   = errors.New("InvalidRequest")
	ErrConflict         = errors.New("OperationAborted")
	ErrMetadataTooLarge = errors.New("MetadataTooLarge")
	ErrVersionNotFound  = errors.New("NoSuchVersion")
	ErrDeleteMarker     = errors.New("DeleteMarker")
)

// InternalPrefix is reserved for gateway state. HTTP clients cannot address it.
const InternalPrefix = ".gateway/"

type Bucket struct {
	Name    string
	Created time.Time
}
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
	ETag     string // unquoted S3 entity tag; opaque to callers
	// Revision is the backend's concurrency token, independent of the S3 ETag.
	Revision                string `json:"-"`
	ContentType             string
	CacheControl            string
	ContentDisposition      string
	ContentEncoding         string
	ContentLanguage         string
	WebsiteRedirectLocation string
	Expires                 string
	Metadata                map[string]string
	Checksums               map[string]string
	ChecksumType            string
	VersionID               string
	DeleteMarker            bool
	ACL                     ACL
	Tags                    []Tag        `json:",omitempty"`
	Parts                   []ObjectPart `json:",omitempty"`
}

// Parts describe the committed object, independently of temporary upload state.
type ObjectPart struct {
	Number    int
	Size      int64
	Checksums map[string]string `json:",omitempty"`
}
type Tag struct{ Key, Value string }
type Owner struct{ ID, DisplayName string }
type Grant struct{ Type, ID, DisplayName, URI, Permission string }
type ACL struct {
	Owner  Owner
	Grants []Grant
}
type BucketProperties struct {
	Notifications     *NotificationConfiguration `json:",omitempty"`
	ABAC              string                     `json:",omitempty"`
	Versioning        string
	ACL               ACL
	Tags              []Tag                   `json:",omitempty"`
	CORS              *CORSConfiguration      `json:",omitempty"`
	PublicAccessBlock *PublicAccessBlock      `json:",omitempty"`
	Ownership         string                  `json:",omitempty"`
	Lifecycle         *LifecycleConfiguration `json:",omitempty"`
}

// Properties persists S3 settings in the backend's private helper state.
// Object metadata updates must preserve bytes, ETag, modification time and
// version identity. Revision, when nonempty, guards against replacing new data.
type Properties interface {
	GetBucketProperties(context.Context, string) (BucketProperties, error)
	SetBucketProperties(context.Context, string, BucketProperties) error
	SetObjectMetadata(context.Context, string, string, Object) error
}
type Conditions struct{ IfMatch, IfNoneMatch string }
type PutOptions struct {
	Object     Object
	Conditions Conditions
}
type ReadOptions struct {
	Offset, Length int64
	Revision       string
	// DataOnly skips optional gateway metadata lookups when the caller has
	// already resolved it. Native revision checks still protect the bytes.
	DataOnly bool
}

// Backend must publish complete objects atomically. Put/Delete conditions must
// be evaluated atomically with the mutation. Get with Revision must return the
// requested generation or ErrPrecondition. Length -1 reads to EOF.
// List returns keys after After in UTF-8 byte order; Next is the last returned
// key when another page exists. InternalPrefix is listed only if requested.
type Backend interface {
	ListBuckets(context.Context) ([]Bucket, error)
	CreateBucket(context.Context, string) error
	HeadBucket(context.Context, string) error
	DeleteBucket(context.Context, string) error
	Head(context.Context, string, string) (Object, error)
	Get(context.Context, string, string, ReadOptions) (Object, io.ReadCloser, error)
	Put(context.Context, string, string, io.Reader, int64, PutOptions) (Object, error)
	Delete(context.Context, string, string, Conditions) error
	List(context.Context, string, string, string, int) ([]Object, string, error)
	Close() error
}

func CheckConditions(o Object, exists bool, c Conditions) error {
	if c.IfMatch != "" && (!exists || (c.IfMatch != "*" && c.IfMatch != o.ETag)) {
		return ErrPrecondition
	}
	if c.IfNoneMatch != "" && exists && (c.IfNoneMatch == "*" || c.IfNoneMatch == o.ETag) {
		return ErrPrecondition
	}
	return nil
}

// Conditional writes distinguish an absent current object from an ETag mismatch.
func CheckWriteConditions(o Object, exists bool, c Conditions) error {
	if c.IfMatch != "" && !exists {
		return ErrNotFound
	}
	return CheckConditions(o, exists, c)
}

func CheckDeleteConditions(o Object, exists bool, c Conditions) error {
	if c.IfMatch != "" && !exists && !o.DeleteMarker {
		return ErrNotFound
	}
	return CheckConditions(o, exists, c)
}
