# Objects, versions and presigned URLs

With the gateway running and client credentials in `.env`, run from the repository root:

```sh
task example-objects
```

The [program](main.go) creates a temporary bucket, enables versioning, uploads JSON with metadata and tags, and replaces it using `If-Match`. It reads the first version, fetches the latest version through a presigned GET, and uploads another object using a presigned PUT and an ordinary HTTP client. Both presigned requests expire after five minutes. All objects, versions and the bucket are removed afterward.

Without Task: `go run ./examples/objects` with exported `GATEWAY_ACCESS_KEY` and `GATEWAY_SECRET_KEY`. Use `--endpoint` and `--region` to select a different gateway. Presigned clients must send the returned signed headers; URLs authorize the gateway independently of storage-provider credentials.
