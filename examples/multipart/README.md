# Parallel multipart upload

With the gateway running and client credentials in `.env`, run from the repository root:

```sh
task example-multipart
task example-multipart -- --file /path/to/large.parquet --timeout 15m
```

The [program](main.go) uploads four parts concurrently using 8 MiB seekable readers, supplies SHA256 checksums, and completes in part-number order. It prints upload and completion durations, downloads the result to check its bytes, and removes the temporary bucket. Failed uploads are aborted before cleanup.

Without `--file`, it generates an 18 MiB payload without allocating the entire object. File uploads use `io.SectionReader` so retries can seek without loading the full file into memory; keep the source file unchanged during the demo. The last part may be smaller than 5 MiB. This example caps files at 10,000 × 8 MiB; increase the part size for larger inputs.

Without Task: `go run ./examples/multipart` with exported gateway credentials. Azure and S3 backends assemble within the provider; the disk backend uses its local completion path. Reported timings are for your current backend and network, not a throughput guarantee.
