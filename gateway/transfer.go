package gateway

import (
	"context"
	"encoding/base64"
	"hash"
	"io"
	"net/http"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
)

type uploadReader struct {
	*io.PipeReader
	payload *payload
}

func (r *uploadReader) UploadObject(o backend.Object) backend.Object {
	o.ETag, o.Checksums = r.payload.etag, r.payload.checksums
	if len(o.Checksums) > 0 {
		o.ChecksumType = "FULL_OBJECT"
	}
	return o
}

// A successful EOF is delivered only after every signature, length and checksum
// has been verified. Providers may stage blocks beforehand, but cannot publish.
func (g *Gateway) putBody(r *http.Request, sig *signature, b, k string, meta backend.Object, conditions backend.Conditions, requiredAlgorithm string) (backend.Object, error) {
	if _, ok := g.be.Backend.(backend.Composer); !ok {
		p, err := g.body(r, sig, maxObjectSize)
		if err != nil {
			return meta, err
		}
		defer p.close()
		if requiredAlgorithm != "" && p.checksums[requiredAlgorithm] == "" {
			return meta, apiError("InvalidRequest", 400, "Part checksum is required")
		}
		meta = (&uploadReader{payload: p}).UploadObject(meta)
		return g.be.Put(r.Context(), b, k, p.file, p.size, backend.PutOptions{Object: meta, Conditions: conditions})
	}
	reader, writer := io.Pipe()
	source := &uploadReader{PipeReader: reader}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p, err := g.bodyTo(r, sig, maxObjectSize, writer)
		if err == nil && requiredAlgorithm != "" && p.checksums[requiredAlgorithm] == "" {
			err = apiError("InvalidRequest", 400, "Part checksum is required")
		}
		source.payload = p
		writer.CloseWithError(err)
	}()
	stop := context.AfterFunc(r.Context(), func() { reader.CloseWithError(r.Context().Err()); r.Body.Close() })
	defer stop()
	o, err := g.be.Put(r.Context(), b, k, source, -1, backend.PutOptions{Object: meta, Conditions: conditions})
	reader.Close()
	if err != nil {
		r.Body.Close()
	}
	<-done
	return o, err
}

func (g *Gateway) removeIncoming(ctx context.Context, b, k string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = g.be.Delete(cleanup, b, k, backend.Conditions{})
}

// A copied part can hash its source while streaming it into isolated native
// staging; unlike a request it has no checksum assertion to verify at EOF.
type copyReader struct {
	io.Reader
	algorithm string
	digest    hash.Hash
}

func (r *copyReader) UploadObject(o backend.Object) backend.Object {
	if r.digest != nil {
		o.Checksums = map[string]string{r.algorithm: base64.StdEncoding.EncodeToString(r.digest.Sum(nil))}
	}
	return o
}
