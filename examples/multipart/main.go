// Upload a file with four concurrent, seekable 8 MiB parts using AWS SDK v2.
package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/adrianliechti/s3-gateway/examples/internal/example"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const partSize int64 = 8 << 20

func main() {
	o := example.Flags()
	file := flag.String("file", "", "file to upload (default: generated 18 MiB object)")
	example.Main(o, func(ctx context.Context) error { return run(ctx, *o, *file) })
}

// Generated data behaves like a file without allocating the complete payload.
type generated struct{}

func (generated) ReadAt(p []byte, off int64) (int, error) {
	for i := range p {
		p[i] = byte((off + int64(i)) % 251)
	}
	return len(p), nil
}

func run(ctx context.Context, o example.Options, path string) error {
	var source io.ReaderAt = generated{}
	size := int64(18 << 20)
	if path != "" {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("file must be a regular file")
		}
		source, size = file, info.Size()
	}
	count := int(max(1, (size+partSize-1)/partSize))
	if count > 10000 {
		return fmt.Errorf("example supports at most 10000 parts; increase partSize for larger files")
	}
	c, err := o.Client()
	if err != nil {
		return err
	}
	b, cleanup, err := example.NewBucket(ctx, c, "example-multipart")
	if err != nil {
		return err
	}
	defer cleanup()
	key := "large-object.bin"
	init, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &b, Key: &key, ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		if !completed {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := c.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{Bucket: &b, Key: &key, UploadId: init.UploadId}); err != nil {
				log.Printf("Abort upload: %v", err)
			}
		}
	}()
	parts := make([]types.CompletedPart, count)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var workers sync.WaitGroup
	var once sync.Once
	var failure error
	started := time.Now()
	for range min(count, 4) {
		workers.Go(func() {
			for i := range jobs {
				offset := int64(i) * partSize
				number := int32(i + 1)
				part, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: &b, Key: &key, UploadId: init.UploadId, PartNumber: &number, Body: io.NewSectionReader(source, offset, min(partSize, size-offset)), ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
				if err != nil {
					once.Do(func() { failure = err; cancel() })
					return
				}
				// Keep completion sorted even when requests finish out of order.
				parts[i] = types.CompletedPart{PartNumber: &number, ETag: part.ETag, ChecksumSHA256: part.ChecksumSHA256}
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
	workers.Wait()
	if failure != nil {
		return failure
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fmt.Printf("Uploaded %d bytes in %d parts in %s\n", size, count, time.Since(started).Round(time.Millisecond))
	started = time.Now()
	out, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &b, Key: &key, UploadId: init.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	if err != nil {
		return err
	}
	completed = true
	fmt.Printf("Completed assembly in %s; ETag %s\n", time.Since(started).Round(time.Millisecond), aws.ToString(out.ETag))
	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &b, Key: &key})
	if err != nil {
		return err
	}
	defer got.Body.Close()
	expected, actual := sha256.New(), sha256.New()
	if _, err = io.Copy(expected, io.NewSectionReader(source, 0, size)); err != nil {
		return err
	}
	n, err := io.Copy(actual, got.Body)
	if err != nil {
		return err
	}
	if n != size || string(expected.Sum(nil)) != string(actual.Sum(nil)) {
		return fmt.Errorf("downloaded object differs from source")
	}
	fmt.Println("Downloaded bytes match the source; cleaning up demo bucket")
	return nil
}
