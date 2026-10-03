package gateway

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/minio/crc64nvme"
)

// Durable completion records include both requested and committed part metadata.
const maxUploadState int64 = 16 << 20

type upload struct {
	Key                     string
	Metadata                backend.Object
	Initiated               time.Time
	Algorithm, ChecksumType string
	Completed               *backend.Object
	Parts                   []completedPart
}
type completedPart struct {
	PartNumber                                                                     int
	ETag                                                                           string
	ChecksumCRC32, ChecksumCRC32C, ChecksumCRC64NVME, ChecksumSHA1, ChecksumSHA256 string
}

func uploadBase(id string) string     { return backend.InternalPrefix + id + "/" }
func partKey(id string, n int) string { return uploadBase(id) + fmt.Sprintf("parts/%05d", n) }
func (g *Gateway) lockUpload(id string) func() {
	h := sha256.Sum256([]byte(id))
	m := &g.multipart[h[0]]
	m.Lock()
	return m.Unlock
}
func (g *Gateway) saveUpload(ctx context.Context, b, id string, u upload) error {
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	_, err = g.be.Put(ctx, b, uploadBase(id)+"manifest", bytes.NewReader(data), int64(len(data)), backend.PutOptions{})
	return err
}
func (g *Gateway) loadUpload(ctx context.Context, b, k, id string) (upload, error) {
	var u upload
	if len(id) != 32 {
		return u, apiError("NoSuchUpload", 404, "Upload does not exist")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return u, apiError("NoSuchUpload", 404, "Upload does not exist")
	}
	_, body, err := g.be.Get(ctx, b, uploadBase(id)+"manifest", backend.ReadOptions{Length: -1})
	if errors.Is(err, backend.ErrNotFound) {
		return u, apiError("NoSuchUpload", 404, "Upload does not exist")
	}
	if err != nil {
		return u, err
	}
	defer body.Close()
	if err = json.NewDecoder(io.LimitReader(body, maxUploadState)).Decode(&u); err != nil {
		return u, err
	}
	if u.Key != k {
		return u, apiError("NoSuchUpload", 404, "Upload does not exist")
	}
	return u, nil
}
func checksumHash(alg string) hash.Hash {
	switch alg {
	case "CRC32":
		return crc32.NewIEEE()
	case "CRC32C":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case "CRC64NVME":
		return crc64nvme.New()
	case "SHA1":
		return sha1.New()
	case "SHA256":
		return sha256.New()
	}
	return nil
}
func (g *Gateway) initiate(w http.ResponseWriter, r *http.Request, b, k string, sig *signature) error {
	if err := g.be.HeadBucket(r.Context(), b); err != nil {
		return err
	}
	p, err := g.body(r, sig, maxXMLSize)
	if err != nil {
		return err
	}
	p.close()
	alg := r.Header.Get("X-Amz-Checksum-Algorithm")
	typ := r.Header.Get("X-Amz-Checksum-Type")
	if alg != "" && checksumHash(alg) == nil {
		return apiError("InvalidRequest", 400, "Unsupported checksum algorithm")
	}
	if typ == "" {
		typ = "COMPOSITE"
		if alg == "CRC64NVME" {
			typ = "FULL_OBJECT"
		}
	}
	if typ != "COMPOSITE" && typ != "FULL_OBJECT" || alg == "CRC64NVME" && typ != "FULL_OBJECT" || (alg == "SHA1" || alg == "SHA256") && typ == "FULL_OBJECT" {
		return apiError("InvalidRequest", 400, "Invalid multipart checksum type")
	}
	uid := id()
	u := upload{Key: k, Metadata: objectMetadata(r), Initiated: time.Now().UTC(), Algorithm: alg, ChecksumType: typ}
	u.Metadata.Tags, err = requestTags(r)
	if err != nil {
		return err
	}
	u.Metadata.ACL, err = g.requestACL(r, nil)
	if err != nil {
		return err
	}
	if err := g.saveUpload(r.Context(), b, uid, u); err != nil {
		return err
	}
	if alg != "" {
		w.Header().Set("X-Amz-Checksum-Algorithm", alg)
		w.Header().Set("X-Amz-Checksum-Type", typ)
	}
	writeXML(w, 200, struct {
		XMLName     xml.Name `xml:"InitiateMultipartUploadResult"`
		XMLNS       string   `xml:"xmlns,attr"`
		Bucket, Key string
		UploadID    string `xml:"UploadId"`
	}{XMLNS: xmlns, Bucket: b, Key: k, UploadID: uid})
	return nil
}
func (g *Gateway) multipartRequest(w http.ResponseWriter, r *http.Request, b, k string, sig *signature) error {
	uid := r.URL.Query().Get("uploadId")
	if r.Method == "PUT" {
		return g.uploadPart(w, r, b, k, uid, sig)
	}
	var completion *payload
	if r.Method == "POST" {
		bodyRequest := r.Clone(r.Context())
		for name := range bodyRequest.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-amz-checksum-") || strings.EqualFold(name, "x-amz-sdk-checksum-algorithm") {
				bodyRequest.Header.Del(name)
			}
		}
		p, err := g.body(bodyRequest, sig, maxXMLSize)
		if err != nil {
			return err
		}
		defer p.close()
		completion = p
	}
	unlock := g.lockUpload(uid)
	defer unlock()
	u, err := g.loadUpload(r.Context(), b, k, uid)
	if err != nil {
		return err
	}
	if u.Completed != nil && r.Method != "POST" {
		return apiError("NoSuchUpload", 404, "Upload is complete")
	}
	switch r.Method {
	case "GET":
		return g.listParts(w, r, b, k, uid, u)
	case "DELETE":
		if err := g.cleanupUpload(r.Context(), b, uid, true); err != nil {
			return err
		}
		w.WriteHeader(204)
	case "POST":
		return g.complete(w, r, b, k, uid, u, completion)
	default:
		return apiError("MethodNotAllowed", 405, "Method not allowed")
	}
	return nil
}
func (g *Gateway) complete(w http.ResponseWriter, r *http.Request, b, k, uid string, u upload, p *payload) error {
	var err error
	var req struct {
		XMLName xml.Name        `xml:"CompleteMultipartUpload"`
		Parts   []completedPart `xml:"Part"`
	}
	if err = xml.NewDecoder(p.file).Decode(&req); err != nil || len(req.Parts) == 0 || len(req.Parts) > 10000 {
		return apiError("MalformedXML", 400, "Invalid completion request")
	}
	for i, part := range req.Parts {
		if part.PartNumber < 1 || part.PartNumber > 10000 || (i > 0 && part.PartNumber <= req.Parts[i-1].PartNumber) {
			return apiError("InvalidPartOrder", 400, "Parts must be in ascending order")
		}
	}
	if u.Completed != nil {
		previous, _ := json.Marshal(u.Parts)
		current, _ := json.Marshal(req.Parts)
		if !bytes.Equal(previous, current) {
			return apiError("NoSuchUpload", 404, "Upload was completed with different parts")
		}
		writeCompletion(w, b, k, *u.Completed)
		return nil
	}
	f, err := os.CreateTemp(g.opts.TempDir, "s3gateway-complete-*")
	if err != nil {
		return err
	}
	defer func() { f.Close(); _ = os.Remove(f.Name()) }()
	md := md5.New()
	var total int64
	var objectParts []backend.ObjectPart
	var digest hash.Hash
	if u.Algorithm != "" {
		digest = checksumHash(u.Algorithm)
	}
	for i, part := range req.Parts {
		o, body, e := g.be.Get(r.Context(), b, partKey(uid, part.PartNumber), backend.ReadOptions{Length: -1})
		if errors.Is(e, backend.ErrNotFound) {
			return apiError("InvalidPart", 400, "Part does not exist")
		}
		if e != nil {
			return e
		}
		if strings.Trim(part.ETag, `"`) != o.ETag {
			body.Close()
			return apiError("InvalidPart", 400, "Part ETag mismatch")
		}
		if i < len(req.Parts)-1 && o.Size < 5<<20 {
			body.Close()
			return apiError("EntityTooSmall", 400, "Non-final parts must be at least 5 MiB")
		}
		raw, e := hex.DecodeString(o.ETag)
		if e != nil {
			body.Close()
			return e
		}
		md.Write(raw)
		var dst io.Writer = f
		if digest != nil && u.ChecksumType == "FULL_OBJECT" {
			dst = io.MultiWriter(f, digest)
		}
		n, e := io.Copy(dst, body)
		body.Close()
		if e != nil {
			return e
		}
		if n != o.Size {
			return io.ErrUnexpectedEOF
		}
		total += n
		objectParts = append(objectParts, backend.ObjectPart{Number: part.PartNumber, Size: n, Checksums: o.Checksums})
		if digest != nil && u.ChecksumType == "COMPOSITE" {
			v, e := base64.StdEncoding.DecodeString(o.Checksums[u.Algorithm])
			if e != nil || len(v) == 0 {
				return apiError("InvalidPart", 400, "Part checksum missing")
			}
			digest.Write(v)
		}
		supplied := map[string]string{"CRC32": part.ChecksumCRC32, "CRC32C": part.ChecksumCRC32C, "CRC64NVME": part.ChecksumCRC64NVME, "SHA1": part.ChecksumSHA1, "SHA256": part.ChecksumSHA256}
		for alg, v := range supplied {
			if v != "" && v != o.Checksums[alg] {
				return apiError("InvalidPart", 400, "Part checksum mismatch")
			}
		}
	}
	meta := u.Metadata
	meta.Parts = objectParts
	meta.ETag = hex.EncodeToString(md.Sum(nil)) + "-" + strconv.Itoa(len(req.Parts))
	meta.Checksums = map[string]string{}
	meta.ChecksumType = u.ChecksumType
	if digest != nil {
		encoded := base64.StdEncoding.EncodeToString(digest.Sum(nil))
		value := encoded
		if u.ChecksumType == "COMPOSITE" {
			value += "-" + strconv.Itoa(len(req.Parts))
		}
		// Completion requests may carry the raw composite digest; response
		// values also include the part count. Validate the digest in either form.
		if supplied := r.Header.Get("X-Amz-Checksum-" + u.Algorithm); supplied != "" && supplied != value && supplied != encoded {
			return apiError("BadDigest", 400, "Completion checksum mismatch")
		}
		meta.Checksums[u.Algorithm] = value
	}
	if typ := r.Header.Get("X-Amz-Checksum-Type"); typ != "" && typ != u.ChecksumType {
		return apiError("BadDigest", 400, "Completion checksum type differs from initiation")
	}
	if size := r.Header.Get("X-Amz-Mp-Object-Size"); size != "" {
		expected, e := strconv.ParseInt(size, 10, 64)
		if e != nil || expected != total {
			return apiError("InvalidRequest", 400, "Multipart object size mismatch")
		}
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	result, err := g.be.Put(r.Context(), b, k, f, total, backend.PutOptions{Object: meta, Conditions: conditions(r)})
	if err != nil {
		return err
	}
	u.Completed = &result
	u.Parts = req.Parts
	if err = g.saveUpload(r.Context(), b, uid, u); err != nil {
		return err
	}
	// Keep the completion record for idempotent retries; parts can now be removed.
	if err = g.cleanupUpload(r.Context(), b, uid, false); err != nil {
		return err
	}
	writeCompletion(w, b, k, result)
	return nil
}
func writeCompletion(w http.ResponseWriter, b, k string, o backend.Object) {
	versionWriteHeaders(w, o)
	w.Header().Set("ETag", quote(o.ETag))
	for alg, v := range o.Checksums {
		w.Header().Set("X-Amz-Checksum-"+alg, v)
	}
	writeXML(w, 200, struct {
		checksumValues
		ChecksumType                string   `xml:",omitempty"`
		XMLName                     xml.Name `xml:"CompleteMultipartUploadResult"`
		XMLNS                       string   `xml:"xmlns,attr"`
		Location, Bucket, Key, ETag string
	}{checksumValues: xmlChecksums(o.Checksums), ChecksumType: o.ChecksumType, XMLNS: xmlns, Location: "/" + b + "/" + awsEncode(k, true), Bucket: b, Key: k, ETag: quote(o.ETag)})
}
func (g *Gateway) cleanupUpload(ctx context.Context, b, uid string, manifest bool) error {
	prefix := uploadBase(uid) + "parts/"
	if manifest {
		prefix = uploadBase(uid)
	}
	for {
		objects, _, err := g.be.List(ctx, b, prefix, "", 1000)
		if err != nil {
			return err
		}
		if len(objects) == 0 {
			return nil
		}
		for _, o := range objects {
			if err = g.be.Delete(ctx, b, o.Key, backend.Conditions{}); err != nil {
				return err
			}
		}
	}
}
func (g *Gateway) listParts(w http.ResponseWriter, r *http.Request, b, k, uid string, u upload) error {
	marker, err := integer(r.URL.Query().Get("part-number-marker"), 0, 0, 10000)
	if err != nil {
		return err
	}
	n, err := integer(r.URL.Query().Get("max-parts"), 1000, 0, 1000)
	if err != nil {
		return err
	}
	type part struct {
		checksumValues
		PartNumber         int
		LastModified, ETag string
		Size               int64
	}
	out := struct {
		ChecksumAlgorithm                                string   `xml:",omitempty"`
		ChecksumType                                     string   `xml:",omitempty"`
		XMLName                                          xml.Name `xml:"ListPartsResult"`
		XMLNS                                            string   `xml:"xmlns,attr"`
		Bucket, Key                                      string
		UploadID                                         string `xml:"UploadId"`
		Initiator, Owner                                 owner
		StorageClass                                     string
		PartNumberMarker, NextPartNumberMarker, MaxParts int
		IsTruncated                                      bool
		Parts                                            []part `xml:"Part"`
	}{ChecksumAlgorithm: u.Algorithm, ChecksumType: u.ChecksumType, XMLNS: xmlns, Bucket: b, Key: k, UploadID: uid, Initiator: g.owner(), Owner: g.owner(), StorageClass: "STANDARD", PartNumberMarker: marker, MaxParts: n}
	objects, next, err := g.be.List(r.Context(), b, uploadBase(uid)+"parts/", partKey(uid, marker), n)
	if err != nil {
		return err
	}
	out.IsTruncated = next != ""
	for _, o := range objects {
		number, _ := strconv.Atoi(strings.TrimPrefix(o.Key, uploadBase(uid)+"parts/"))
		out.Parts = append(out.Parts, part{xmlChecksums(o.Checksums), number, stamp(o.Modified), quote(o.ETag), o.Size})
		out.NextPartNumberMarker = number
	}
	writeXML(w, 200, out)
	return nil
}
func (g *Gateway) listUploads(w http.ResponseWriter, r *http.Request, b string) error {
	q := r.URL.Query()
	delimiter := q.Get("delimiter")
	encoding := q.Get("encoding-type")
	if encoding != "" && encoding != "url" {
		return apiError("InvalidArgument", 400, "Invalid encoding-type")
	}
	encode := func(v string) string {
		if encoding == "url" {
			return awsEncode(v, true)
		}
		return v
	}
	n, err := integer(q.Get("max-uploads"), 1000, 0, 1000)
	if err != nil {
		return err
	}
	type item struct {
		started                 time.Time
		Key                     string
		UploadID                string `xml:"UploadId"`
		Initiator, Owner        owner
		StorageClass, Initiated string
	}
	out := struct {
		Delimiter          string `xml:",omitempty"`
		EncodingType       string `xml:",omitempty"`
		CommonPrefixes     []commonPrefix
		XMLName            xml.Name `xml:"ListMultipartUploadsResult"`
		XMLNS              string   `xml:"xmlns,attr"`
		Bucket, KeyMarker  string
		UploadIDMarker     string `xml:"UploadIdMarker"`
		NextKeyMarker      string
		NextUploadIDMarker string `xml:"NextUploadIdMarker"`
		Prefix             string
		MaxUploads         int
		IsTruncated        bool
		Uploads            []item `xml:"Upload"`
	}{Delimiter: delimiter, EncodingType: encoding, XMLNS: xmlns, Bucket: b, KeyMarker: q.Get("key-marker"), UploadIDMarker: q.Get("upload-id-marker"), Prefix: q.Get("prefix"), MaxUploads: n}
	var all []item
	after := ""
	for {
		objects, next, e := g.be.List(r.Context(), b, backend.InternalPrefix, after, 1000)
		if e != nil {
			return e
		}
		for _, o := range objects {
			if !strings.HasSuffix(o.Key, "/manifest") {
				continue
			}
			_, body, e := g.be.Get(r.Context(), b, o.Key, backend.ReadOptions{Length: -1})
			if e != nil {
				return e
			}
			var u upload
			e = json.NewDecoder(io.LimitReader(body, maxUploadState)).Decode(&u)
			body.Close()
			if e != nil {
				return e
			}
			uid := strings.Split(strings.TrimPrefix(o.Key, backend.InternalPrefix), "/")[0]
			if u.Completed == nil && strings.HasPrefix(u.Key, out.Prefix) {
				all = append(all, item{u.Initiated, u.Key, uid, g.owner(), g.owner(), "STANDARD", stamp(u.Initiated)})
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Key == all[j].Key {
			if !all[i].started.Equal(all[j].started) {
				return all[i].started.Before(all[j].started)
			}
			return all[i].UploadID < all[j].UploadID
		}
		return all[i].Key < all[j].Key
	})
	markerIndex := -1
	for i, v := range all {
		if v.Key == out.KeyMarker && v.UploadID == out.UploadIDMarker {
			markerIndex = i
			break
		}
	}
	count, previousPrefix := 0, ""
	for i, v := range all {
		if v.Key < out.KeyMarker {
			continue
		}
		if v.Key == out.KeyMarker && (out.UploadIDMarker == "" || markerIndex >= 0 && i <= markerIndex || markerIndex < 0 && v.UploadID <= out.UploadIDMarker) {
			continue
		}
		group := ""
		if delimiter != "" {
			if at := strings.Index(strings.TrimPrefix(v.Key, out.Prefix), delimiter); at >= 0 {
				group = out.Prefix + strings.TrimPrefix(v.Key, out.Prefix)[:at+len(delimiter)]
			}
		}
		if group != "" && (group <= out.KeyMarker || group == previousPrefix) {
			continue
		}
		if count == n {
			out.IsTruncated = true
			break
		}
		count++
		if group != "" {
			out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{encode(group)})
			previousPrefix = group
			out.NextKeyMarker, out.NextUploadIDMarker = group, ""
		} else {
			out.NextKeyMarker, out.NextUploadIDMarker = v.Key, v.UploadID
			v.Key = encode(v.Key)
			out.Uploads = append(out.Uploads, v)
		}
	}
	out.Prefix, out.Delimiter = encode(out.Prefix), encode(out.Delimiter)
	out.KeyMarker, out.NextKeyMarker = encode(out.KeyMarker), encode(out.NextKeyMarker)
	writeXML(w, 200, out)
	return nil
}

// uploadPart stages and verifies bytes without holding an upload lock, allowing
// overlapping retries even when the first client has not finished sending.
func (g *Gateway) uploadPart(w http.ResponseWriter, r *http.Request, b, k, uid string, sig *signature) error {
	u, err := g.loadUpload(r.Context(), b, k, uid)
	if err != nil {
		return err
	}
	if u.Completed != nil {
		return apiError("NoSuchUpload", 404, "Upload is complete")
	}
	n, err := integer(r.URL.Query().Get("partNumber"), 0, 1, 10000)
	if err != nil || n < 1 || r.URL.Query().Get("partNumber") != strconv.Itoa(n) {
		return apiError("InvalidArgument", 400, "Part number must be 1 to 10000")
	}
	var staged *os.File
	var length int64
	meta := backend.Object{}
	if r.Header.Get("X-Amz-Copy-Source") != "" {
		sb, sk, version, e := g.copySource(r)
		if e != nil {
			return e
		}
		src, e := g.be.HeadVersion(r.Context(), sb, sk, version)
		if e != nil {
			return e
		}
		if e = readConditions(r, src, "X-Amz-Copy-Source-"); e != nil {
			return e
		}
		off, count, e := copyRange(r.Header.Get("X-Amz-Copy-Source-Range"), src.Size)
		length = count
		if e != nil {
			return e
		}
		if length > maxObjectSize {
			return apiError("EntityTooLarge", 400, "Part exceeds 5 GiB")
		}
		_, body, e := g.be.GetVersion(r.Context(), sb, sk, version, backend.ReadOptions{Offset: off, Length: length, Revision: src.Revision})
		if e != nil {
			return e
		}
		defer body.Close()
		if src.VersionID != "" {
			w.Header().Set("X-Amz-Copy-Source-Version-Id", src.VersionID)
		}
		f, e := os.CreateTemp(g.opts.TempDir, "s3gateway-copy-*")
		if e != nil {
			return e
		}
		defer func() { f.Close(); _ = os.Remove(f.Name()) }()
		var dst io.Writer = f
		var digest hash.Hash
		if u.Algorithm != "" {
			digest = checksumHash(u.Algorithm)
			dst = io.MultiWriter(f, digest)
		}
		copied, e := io.Copy(dst, body)
		if e != nil {
			return e
		}
		if copied != length {
			return io.ErrUnexpectedEOF
		}
		if _, e = f.Seek(0, io.SeekStart); e != nil {
			return e
		}
		if digest != nil {
			meta.Checksums = map[string]string{u.Algorithm: base64.StdEncoding.EncodeToString(digest.Sum(nil))}
		}
		staged = f
	} else {
		p, e := g.body(r, sig, maxObjectSize)
		if e != nil {
			return e
		}
		defer p.close()
		if u.Algorithm != "" && p.checksums[u.Algorithm] == "" {
			return apiError("InvalidRequest", 400, "Part checksum is required")
		}
		staged, length = p.file, p.size
		meta = backend.Object{ETag: p.etag, Checksums: p.checksums}
	}
	// Multiple parts may commit concurrently; completion/abort takes the
	// exclusive lock and rechecks the manifest after all current commits.
	h := sha256.Sum256([]byte(uid))
	g.multipart[h[0]].RLock()
	defer g.multipart[h[0]].RUnlock()
	u, err = g.loadUpload(r.Context(), b, k, uid)
	if err != nil {
		return err
	}
	if u.Completed != nil {
		return apiError("NoSuchUpload", 404, "Upload is complete")
	}
	o, err := g.be.Put(r.Context(), b, partKey(uid, n), staged, length, backend.PutOptions{Object: meta})
	if err != nil {
		return err
	}
	w.Header().Set("ETag", quote(o.ETag))
	for alg, value := range o.Checksums {
		w.Header().Set("X-Amz-Checksum-"+alg, value)
	}
	if r.Header.Get("X-Amz-Copy-Source") != "" {
		writeXML(w, 200, struct {
			checksumValues
			XMLName            xml.Name `xml:"CopyPartResult"`
			ETag, LastModified string
		}{checksumValues: xmlChecksums(o.Checksums), ETag: quote(o.ETag), LastModified: stamp(o.Modified)})
	} else {
		w.WriteHeader(200)
	}

	return nil
}

func copyRange(value string, size int64) (int64, int64, error) {
	if value == "" {
		return 0, size, nil
	}
	invalid := apiError("InvalidArgument", 400, "Copy range must be bytes=first-last")
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, invalid
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return 0, 0, invalid
	}
	start, e1 := strconv.ParseInt(parts[0], 10, 64)
	end, e2 := strconv.ParseInt(parts[1], 10, 64)
	if e1 != nil || e2 != nil || start < 0 || end < start {
		return 0, 0, invalid
	}
	if start >= size || end >= size {
		return 0, 0, apiError("InvalidRange", 416, "Copy range is outside object")
	}
	return start, end - start + 1, nil
}
