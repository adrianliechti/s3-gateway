// Package disk preserves native files and stores helper state in root/.system.
package disk

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"

	"sort"
	"strings"
	"sync"

	"github.com/adrianliechti/s3-gateway/backend"
)

type Options struct{ Root string }
type Store struct {
	root      *os.Root
	lock      *os.File
	closeOnce sync.Once
	closeErr  error
	mu        sync.RWMutex
}

var _ backend.Backend = (*Store)(nil)

func New(opts Options) (*Store, error) {
	if opts.Root == "" {
		return nil, fmt.Errorf("disk root is required")
	}
	if err := os.MkdirAll(opts.Root, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(opts.Root)
	if err != nil {
		return nil, err
	}
	s := &Store{root: root}
	if err := root.MkdirAll(".system", 0700); err != nil {
		root.Close()
		return nil, err
	}
	if i, e := root.Lstat(".system"); e != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
		root.Close()
		return nil, fmt.Errorf(".system must be a real directory")
	}
	s.lock, err = lockRoot(root)
	if err != nil {
		root.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = errors.Join(s.lock.Close(), s.root.Close()) })
	return s.closeErr
}
func validBucket(b string) bool {
	return b != "" && b != "." && b != ".." && b != ".system" && !strings.ContainsAny(b, "/\\\x00")
}
func (s *Store) HeadBucket(ctx context.Context, b string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validBucket(b) {
		return backend.ErrBucketNotFound
	}
	info, err := s.root.Lstat(b)
	if errors.Is(err, fs.ErrNotExist) {
		return backend.ErrBucketNotFound
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return backend.ErrBucketNotFound
	}
	return nil
}
func (s *Store) ListBuckets(ctx context.Context) ([]backend.Bucket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := []backend.Bucket{}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.IsDir() && validBucket(e.Name()) {
			i, err := e.Info()
			if err != nil {
				return nil, err
			}
			out = append(out, backend.Bucket{Name: e.Name(), Created: i.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (s *Store) CreateBucket(ctx context.Context, b string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validBucket(b) {
		return backend.ErrInvalidKey
	}
	err := s.root.Mkdir(b, 0755)
	if errors.Is(err, fs.ErrExist) {
		return backend.ErrBucketExists
	}
	return err
}
func (s *Store) DeleteBucket(ctx context.Context, b string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.HeadBucket(ctx, b); err != nil {
		return err
	}
	// Empty directories are filesystem structure, not S3 objects.
	var dirs []string
	err := fs.WalkDir(s.root.FS(), b, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() {
			return backend.ErrBucketNotEmpty
		}
		dirs = append(dirs, p)
		return ctx.Err()
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := s.root.Remove(dirs[i]); err != nil {
			return err
		}
	}
	// A deleted bucket's unfinished multipart uploads cannot survive recreation.
	if err := s.root.RemoveAll(path.Join(".system/uploads", b)); err != nil {
		return err
	}
	for _, dir := range []string{".system/versions", ".system/buckets"} {
		if err := s.root.RemoveAll(path.Join(dir, b)); err != nil {
			return err
		}
	}
	return s.root.RemoveAll(path.Join(".system/objects", b))
}

func location(b, k string) (string, error) {
	if !validBucket(b) || k == "" || strings.ContainsAny(k, "\\\x00") || strings.HasPrefix(k, "/") || strings.HasSuffix(k, "/") {
		return "", backend.ErrInvalidKey
	}
	for _, part := range strings.Split(k, "/") {
		if part == "" || part == "." || part == ".." {
			return "", backend.ErrInvalidKey
		}
	}
	if strings.HasPrefix(k, backend.InternalPrefix) {
		if strings.HasPrefix(k, backend.InternalPrefix+"versions/") {
			return path.Join(".system/versions", b, strings.TrimPrefix(k, backend.InternalPrefix+"versions/")), nil
		}
		return path.Join(".system/uploads", b, strings.TrimPrefix(k, backend.InternalPrefix)), nil
	}
	return b + "/" + k, nil
}
func fingerprint(i fs.FileInfo) string { return fmt.Sprintf("%x-%x", i.ModTime().UnixNano(), i.Size()) }
func metaPath(b, k, rev string) string {
	h := sha256.Sum256([]byte(k))
	return path.Join(".system/objects", b, hex.EncodeToString(h[:]), rev+".json")
}
func (s *Store) info(b, k string, f *os.File) (backend.Object, error) {
	st, err := f.Stat()
	if err != nil {
		return backend.Object{}, err
	}
	if !st.Mode().IsRegular() {
		return backend.Object{}, backend.ErrNotFound
	}
	rev := fingerprint(st)
	o := backend.Object{Key: k, Size: st.Size(), Modified: st.ModTime().UTC(), Revision: rev, ContentType: "application/octet-stream"}
	raw, err := s.root.ReadFile(metaPath(b, k, rev))
	if err == nil {
		if err := json.Unmarshal(raw, &o); err != nil {
			return o, fmt.Errorf("read object metadata: %w", err)
		}
		o.Key = k
		o.Size = st.Size()
		o.Modified = st.ModTime().UTC()
		o.Revision = rev
	} else if errors.Is(err, fs.ErrNotExist) {
		// Native files need no gateway metadata. Derive a stable content ETag.
		h := md5.New()
		if _, err = io.Copy(h, f); err != nil {
			return o, err
		}
		o.ETag = hex.EncodeToString(h.Sum(nil))
		_, err = f.Seek(0, io.SeekStart)
	} else {
		return o, err
	}
	return o, err
}
func (s *Store) open(ctx context.Context, b, k string) (*os.File, error) {
	if err := s.HeadBucket(ctx, b); err != nil {
		return nil, err
	}
	p, err := location(b, k)
	if err != nil {
		return nil, err
	}
	if err := s.noSymlinks(p); err != nil {
		return nil, err
	}
	f, err := s.root.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, backend.ErrNotFound
	}
	return f, err
}
func (s *Store) head(ctx context.Context, b, k string) (backend.Object, error) {
	f, err := s.open(ctx, b, k)
	if err != nil {
		return backend.Object{}, err
	}
	defer f.Close()
	return s.info(b, k, f)
}
func (s *Store) Head(ctx context.Context, b, k string) (backend.Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.head(ctx, b, k)
}

type sectionReadCloser struct {
	io.Reader
	io.Closer
}

func (s *Store) Get(ctx context.Context, b, k string, r backend.ReadOptions) (backend.Object, io.ReadCloser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, err := s.open(ctx, b, k)
	if err != nil {
		return backend.Object{}, nil, err
	}
	o, err := s.info(b, k, f)
	if err != nil {
		f.Close()
		return o, nil, err
	}
	if r.Revision != "" && r.Revision != o.Revision {
		f.Close()
		return o, nil, backend.ErrPrecondition
	}
	n := r.Length
	if n < 0 {
		n = o.Size - r.Offset
	}
	if r.Offset < 0 || n < 0 {
		f.Close()
		return o, nil, backend.ErrInvalidKey
	}
	return o, sectionReadCloser{io.NewSectionReader(f, r.Offset, n), f}, nil
}
func (s *Store) Put(ctx context.Context, b, k string, body io.Reader, size int64, p backend.PutOptions) (backend.Object, error) {
	target, err := location(b, k)
	if err != nil {
		return backend.Object{}, err
	}
	if err := s.HeadBucket(ctx, b); err != nil {
		return backend.Object{}, err
	}
	// Stage in the private tree on the same filesystem, never in the visible
	// bucket. Failed reads and checksum failures cannot publish partial bytes.
	tmp := ".system/staging/" + randomID()
	if err := s.root.MkdirAll(path.Dir(tmp), 0700); err != nil {
		return backend.Object{}, err
	}
	f, err := s.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return backend.Object{}, err
	}
	defer func() { f.Close(); _ = s.root.Remove(tmp) }()
	h := md5.New()
	n, err := io.Copy(io.MultiWriter(f, h), contextReader{ctx, body})
	if err != nil {
		return backend.Object{}, err
	}
	if size >= 0 && n != size {
		return backend.Object{}, io.ErrUnexpectedEOF
	}
	if err = f.Sync(); err != nil {
		return backend.Object{}, err
	}
	st, err := f.Stat()
	if err != nil {
		return backend.Object{}, err
	}
	o := p.Object
	o.Key = k
	o.Size = n
	o.Modified = st.ModTime().UTC()
	o.Revision = fingerprint(st)
	if o.ETag == "" {
		o.ETag = hex.EncodeToString(h.Sum(nil))
	}
	if o.ContentType == "" {
		o.ContentType = "application/octet-stream"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.HeadBucket(ctx, b); err != nil {
		return o, err
	}
	old, e := s.head(ctx, b, k)
	if e != nil && !errors.Is(e, backend.ErrNotFound) {
		return o, e
	}
	if err = backend.CheckWriteConditions(old, e == nil, p.Conditions); err != nil {
		return o, err
	}
	if err = s.noSymlinks(target); err != nil {
		return o, err
	}
	if err = s.root.MkdirAll(path.Dir(target), 0755); err != nil {
		return o, err
	}
	mp := metaPath(b, k, o.Revision)
	if err = s.root.MkdirAll(path.Dir(mp), 0700); err != nil {
		return o, err
	}
	data, err := json.Marshal(o)
	if err != nil {
		return o, err
	}
	mf, err := s.root.OpenFile(mp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return o, err
	}
	_, err = mf.Write(data)
	if err == nil {
		err = mf.Sync()
	}
	closeErr := mf.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return o, err
	}
	if err = syncDir(s.root, path.Dir(mp)); err != nil {
		return o, err
	}
	// Metadata is keyed by file generation: a crash before rename leaves the old
	// object's metadata intact; a crash after rename finds the new metadata.
	if err = f.Close(); err != nil {
		return o, err
	}
	if err = s.root.Rename(tmp, target); err != nil {
		return o, err
	}
	if err = syncDir(s.root, path.Dir(target)); err != nil {
		return o, err
	}
	return o, nil
}
func syncDir(r *os.Root, p string) error {
	f, err := r.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (s *Store) Delete(ctx context.Context, b, k string, c backend.Conditions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := location(b, k)
	if err != nil {
		return err
	}
	old, e := s.head(ctx, b, k)
	if e != nil && !errors.Is(e, backend.ErrNotFound) {
		return e
	}
	if err = backend.CheckDeleteConditions(old, e == nil, c); err != nil {
		return err
	}
	if e != nil {
		return nil
	}
	if err = s.root.Remove(p); err != nil {
		return err
	}
	// Keep now-empty parents out of object listings; deletion is best effort.
	for d := path.Dir(p); d != b && d != "." && !strings.HasPrefix(d, ".system"); d = path.Dir(d) {
		if err := s.root.Remove(d); err != nil {
			break
		}
	}
	return nil
}
func (s *Store) List(ctx context.Context, b, prefix, after string, limit int) ([]backend.Object, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.HeadBucket(ctx, b); err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		return []backend.Object{}, "", nil
	}
	internal := strings.HasPrefix(prefix, backend.InternalPrefix)
	type tree struct{ root, keyPrefix string }
	trees := []tree{{b, ""}}
	if internal {
		trees = []tree{{path.Join(".system/uploads", b), backend.InternalPrefix}}
		versionPrefix := backend.InternalPrefix + "versions/"
		if strings.HasPrefix(prefix, versionPrefix) {
			trees = nil
		}
		if strings.HasPrefix(prefix, versionPrefix) || strings.HasPrefix(versionPrefix, prefix) {
			trees = append(trees, tree{path.Join(".system/versions", b), versionPrefix})
		}
	}
	keys := []string{}
	for _, tree := range trees {
		err := fs.WalkDir(s.root.FS(), tree.root, func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.IsDir() {
				if !internal && p == b+"/.s3gw" {
					return fs.SkipDir
				}
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			k := tree.keyPrefix + strings.TrimPrefix(p, tree.root+"/")
			if strings.HasPrefix(k, prefix) && k > after {
				keys = append(keys, k)
			}
			return nil
		})
		if err != nil && !(internal && errors.Is(err, fs.ErrNotExist)) {
			return nil, "", err
		}
	}
	sort.Strings(keys)
	next := ""
	if len(keys) > limit {
		keys = keys[:limit]
		next = keys[len(keys)-1]
	}
	out := make([]backend.Object, 0, len(keys))
	for _, k := range keys {
		o, err := s.head(ctx, b, k)
		if err != nil {
			return nil, "", err
		}
		out = append(out, o)
	}
	return out, next, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func randomID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

// Native symlinks are not S3 objects. Refuse them rather than exposing helper
// state or another bucket through a filesystem alias.
func (s *Store) noSymlinks(p string) error {
	parts := strings.Split(p, "/")
	prefix := ""
	for idx, part := range parts {
		prefix = path.Join(prefix, part)
		i, err := s.root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if i.Mode()&os.ModeSymlink != 0 {
			return backend.ErrInvalidKey
		}
		if idx < len(parts)-1 && !i.IsDir() {
			return backend.ErrInvalidKey
		}
	}
	return nil
}
