package backend

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"sync"
)

// UploadMetadata is consulted only after a successful EOF. Streaming readers
// can supply digests and trailers without buffering the entire request first.
type UploadMetadata interface {
	UploadObject(Object) Object
}

func UploadedObject(r io.Reader, o Object) Object {
	if source, ok := r.(UploadMetadata); ok {
		return source.UploadObject(o)
	}
	return o
}

// ComposeSource identifies immutable bytes, guarded by their native revision.
type ComposeSource struct {
	Key, Revision string
	Size          int64
}

// Composer assembles objects within the provider, without downloading their
// contents. Empty sources publish an empty object. Destination conditions are
// evaluated atomically at publication. It also advertises streaming Put support.
type Composer interface {
	Compose(context.Context, string, string, []ComposeSource, PutOptions) (Object, error)
}

const TransferBlockSize = 8 << 20
const TransferConcurrency = 4

// Transfer keeps at most four replayable blocks in memory. Each SDK operation
// can retry its own block; backpressure bounds memory independently of size.
func Transfer(ctx context.Context, r io.Reader, blockSize int, send func(context.Context, int, []byte) error) (int64, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if closer, ok := r.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	fail := func(err error) { once.Do(func() { first = err; cancel() }) }
	free := make(chan []byte, TransferConcurrency)
	for range TransferConcurrency {
		free <- nil
	}
	h := md5.New()
	var total int64
	for number := 0; ; number++ {
		var buf []byte
		select {
		case <-ctx.Done():
			fail(ctx.Err())
			goto done
		case buf = <-free:
		}
		if buf == nil {
			buf = make([]byte, blockSize)
		}
		n, err := io.ReadFull(r, buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			fail(err)
			break
		}
		if n > 0 {
			h.Write(buf[:n])
			total += int64(n)
			wg.Add(1)
			go func(number int, buf []byte, n int) {
				defer wg.Done()
				if err := send(ctx, number, buf[:n]); err != nil {
					fail(err)
				}
				free <- buf
			}(number, buf, n)
		}
		if err != nil {
			break
		}
	}
done:
	wg.Wait()
	return total, hex.EncodeToString(h.Sum(nil)), first
}

// Parallel runs provider-side copy operations with bounded concurrency.
func Parallel(ctx context.Context, count int, fn func(context.Context, int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	jobs := make(chan int)
	for range min(count, TransferConcurrency) {
		wg.Go(func() {
			for i := range jobs {
				if err := fn(ctx, i); err != nil {
					once.Do(func() { first = err; cancel() })
					return
				}
			}
		})
	}
send:
	for i := range count {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break send
		}
	}
	close(jobs)
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}
