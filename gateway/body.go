package gateway

import (
	"bufio"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"

	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/minio/crc64nvme"
)

const maxObjectSize int64 = 5 << 30
const maxXMLSize int64 = 2 << 20

type payload struct {
	file      *os.File
	size      int64
	etag      string
	checksums map[string]string
}

func (p *payload) close() { p.file.Close(); _ = os.Remove(p.file.Name()) }
func (g *Gateway) body(r *http.Request, sig *signature, limit int64) (*payload, error) {
	f, err := os.CreateTemp(g.opts.TempDir, "s3gateway-body-*")
	if err != nil {
		return nil, err
	}
	p := &payload{file: f, checksums: map[string]string{}}
	ok := false
	defer func() {
		if !ok {
			p.close()
		}
	}()
	hs := map[string]hash.Hash{"CRC32": crc32.NewIEEE(), "CRC32C": crc32.New(crc32.MakeTable(crc32.Castagnoli)), "SHA1": sha1.New(), "SHA256": sha256.New(), "CRC64NVME": crc64nvme.New()}
	md := md5.New()
	writers := []io.Writer{f, md}
	for _, h := range hs {
		writers = append(writers, h)
	}
	dst := io.MultiWriter(writers...)
	trailers := http.Header{}
	expected := r.ContentLength
	streaming := strings.HasPrefix(sig.payload, "STREAMING-")
	if streaming {
		switch sig.payload {
		case "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER":
		default:
			return nil, apiError("NotImplemented", 501, "Unsupported streaming payload format")
		}
		expected, err = strconv.ParseInt(r.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64)
		if err != nil || expected < 0 {
			return nil, apiError("InvalidRequest", 400, "Decoded content length is required")
		}
		p.size, trailers, err = decodeChunks(dst, r.Body, sig, limit)
	} else {
		p.size, err = io.Copy(dst, io.LimitReader(r.Body, limit+1))
	}
	if err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, apiError("IncompleteBody", 400, "Incomplete request body")
		}
		return nil, err
	}
	if p.size > limit {
		return nil, apiError("EntityTooLarge", 400, "Request body is too large")
	}
	if expected >= 0 && expected != p.size {
		return nil, apiError("IncompleteBody", 400, "Body length does not match content length")
	}
	p.etag = hex.EncodeToString(md.Sum(nil))
	if h := r.Header.Get("Content-MD5"); h != "" {
		decoded, e := base64.StdEncoding.DecodeString(h)
		if e != nil || len(decoded) != 16 {
			return nil, apiError("InvalidDigest", 400, "Invalid Content-MD5")
		}
		if subtle.ConstantTimeCompare(decoded, md.Sum(nil)) != 1 {
			return nil, apiError("BadDigest", 400, "Content-MD5 mismatch")
		}
	}
	if !streaming && sig.payload != "UNSIGNED-PAYLOAD" {
		expected, e := hex.DecodeString(sig.payload)
		if e != nil || len(expected) != 32 {
			return nil, apiError("InvalidRequest", 400, "Invalid payload SHA256")
		}
		if subtle.ConstantTimeCompare(expected, hs["SHA256"].Sum(nil)) != 1 {
			return nil, apiError("XAmzContentSHA256Mismatch", 400, "Payload SHA256 mismatch")
		}
	}
	for _, name := range strings.Split(r.Header.Get("X-Amz-Trailer"), ",") {
		name = strings.TrimSpace(name)
		if name != "" && trailers.Get(name) == "" {
			return nil, apiError("InvalidRequest", 400, "A declared trailer is missing")
		}
	}
	for alg, h := range hs {
		name := "X-Amz-Checksum-" + alg
		v := r.Header.Get(name)
		if v == "" {
			v = r.URL.Query().Get(strings.ToLower(name))
		}
		if trailer := trailers.Get(name); trailer != "" {
			if v != "" && v != trailer {
				return nil, apiError("BadDigest", 400, "Conflicting checksums")
			}
			v = trailer
		}
		if v != "" {
			actual := base64.StdEncoding.EncodeToString(h.Sum(nil))
			if subtle.ConstantTimeCompare([]byte(actual), []byte(v)) != 1 {
				return nil, apiError("BadDigest", 400, "Checksum mismatch")
			}
			p.checksums[alg] = actual
		}
	}
	if alg := r.Header.Get("X-Amz-Sdk-Checksum-Algorithm"); alg != "" {
		if _, exists := hs[alg]; !exists {
			return nil, apiError("InvalidRequest", 400, "Unsupported checksum algorithm")
		}
		if p.checksums[alg] == "" {
			return nil, apiError("InvalidRequest", 400, "Requested checksum is missing")
		}
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	ok = true
	return p, nil
}
func decodeChunks(dst io.Writer, src io.Reader, sig *signature, limit int64) (int64, http.Header, error) {
	br := bufio.NewReaderSize(src, 64<<10)
	trailers := http.Header{}
	var total int64
	line := func() (string, error) {
		raw, e := br.ReadSlice('\n')
		v := string(raw)
		if len(v) > 8192 {
			return "", apiError("InvalidRequest", 400, "Chunk header too long")
		}
		return strings.TrimSuffix(strings.TrimSuffix(v, "\n"), "\r"), e
	}
	signedTrailer := sig.payload == "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	for {
		l, err := line()
		if err != nil {
			return total, trailers, apiError("IncompleteBody", 400, "Missing chunk header")
		}
		pieces := strings.Split(l, ";")
		n, err := strconv.ParseInt(pieces[0], 16, 64)
		if err != nil || n < 0 || n > 16<<20 || total+n > limit {
			return total, trailers, apiError("InvalidRequest", 400, "Invalid chunk size")
		}
		data := make([]byte, int(n))
		if _, err = io.ReadFull(br, data); err != nil {
			return total, trailers, apiError("IncompleteBody", 400, "Incomplete chunk")
		}
		signed := strings.HasPrefix(sig.payload, "STREAMING-AWS4-HMAC-SHA256")
		if signed {
			chunkSig := ""
			for _, v := range pieces[1:] {
				if strings.HasPrefix(v, "chunk-signature=") {
					chunkSig = strings.TrimPrefix(v, "chunk-signature=")
				}
			}
			if err = sig.checkChunk(data, chunkSig); err != nil {
				return total, trailers, err
			}
		}
		if n == 0 {
			break
		}
		ending := make([]byte, 2)
		if _, err = io.ReadFull(br, ending); err != nil || string(ending) != "\r\n" {
			return total, trailers, apiError("InvalidRequest", 400, "Invalid chunk terminator")
		}
		if _, err = dst.Write(data); err != nil {
			return total, trailers, err
		}
		total += n
	}
	trailerSeparator := false
	for {
		l, err := line()
		if err != nil && err != io.EOF {
			return total, trailers, err
		}
		if l == "" {
			// Some S3 clients separate checksum lines from the trailer signature
			// with an extra CRLF. Permit one separator, but still require and
			// verify the signature before accepting the payload.
			if signedTrailer && !trailerSeparator && len(trailers) > 0 && trailers.Get("x-amz-trailer-signature") == "" {
				trailerSeparator = true
				continue
			}
			break
		}
		kv := strings.SplitN(l, ":", 2)
		if len(kv) != 2 || len(trailers) >= 8 {
			return total, trailers, apiError("InvalidRequest", 400, "Invalid trailer")
		}
		name := strings.TrimSpace(kv[0])
		if trailers.Get(name) != "" {
			return total, trailers, apiError("InvalidRequest", 400, "Duplicate trailer")
		}
		trailers.Set(name, strings.TrimSpace(kv[1]))
		if err == io.EOF {
			break
		}
	}
	if strings.HasSuffix(sig.payload, "-TRAILER") && strings.HasPrefix(sig.payload, "STREAMING-AWS4-") {
		var keys []string
		for k := range trailers {
			if !strings.EqualFold(k, "x-amz-trailer-signature") {
				keys = append(keys, strings.ToLower(k))
			}
		}
		sort.Strings(keys)
		var text strings.Builder
		for _, k := range keys {
			text.WriteString(k + ":" + trailers.Get(k) + "\n")
		}
		expected := hex.EncodeToString(mac(sig.key, "AWS4-HMAC-SHA256-TRAILER\n"+sig.date+"\n"+sig.scope+"\n"+sig.previous+"\n"+sha256Hex(text.String())))
		if subtle.ConstantTimeCompare([]byte(expected), []byte(trailers.Get("x-amz-trailer-signature"))) != 1 {
			return total, trailers, apiError("SignatureDoesNotMatch", 403, "Trailer signature mismatch")
		}
	}
	if _, err := br.ReadByte(); err != io.EOF {
		return total, trailers, apiError("InvalidRequest", 400, "Unexpected data after final chunk")
	}
	return total, trailers, nil
}
