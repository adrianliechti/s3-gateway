# S3 gateway

A custom Go S3 HTTP implementation with interchangeable disk and Azure Blob backends. Object bytes and paths stay native: `s3://warehouse/table/data.parquet` is `data/warehouse/table/data.parquet` on disk, or `table/data.parquet` in the Azure container `warehouse`.

Go module: `github.com/adrianliechti/s3-gateway`. The executable is `gateway`, built from `cmd/gateway`.

This is an initial implementation, not a claim of full S3 or Iceberg conformance. It supports optional bucket versioning, private bucket/object ACLs for one configured user, object tagging, and object attributes with completed-part reads. Tests cover the AWS SDK, Ceph, minio-go and Iceberg on disk and Azurite, plus a basic live Azure smoke test. See the [AWS operations chart and measured results](COMPATIBILITY.md) and [operating limits](docs/compatibility.md) for exact scope and remaining failures.

## Run on disk

Requires Go 1.27.1 or newer and [Task](https://taskfile.dev/docs/installation/). Configure credentials in `.env` or the environment. On a fresh checkout, copy `.env.example` to `.env` and replace the placeholder credentials. If `.env` already contains Azure credentials, preserve them and add `S3_ACCESS_KEY` and `S3_SECRET_KEY`. `task run` loads `.env`; exported variables take precedence. Flags and help output do not contain credential values.

```sh
export S3_ACCESS_KEY=gateway-local
export S3_SECRET_KEY='choose-a-long-random-secret'
task run -- -backend disk -root ./data
```

The endpoint defaults to `http://127.0.0.1:9000`, signing region `us-east-1`. The configured principal can access every bucket exposed by this gateway. Configure TLS using `-tls-cert` and `-tls-key` when serving over an untrusted network.

```sh
export AWS_ACCESS_KEY_ID="$S3_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$S3_SECRET_KEY"
export AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://127.0.0.1:9000 s3 mb s3://warehouse
aws --endpoint-url http://127.0.0.1:9000 s3 cp example.parquet s3://warehouse/table/example.parquet
```

Existing directories become buckets, and existing regular files become objects without importing them. Helper state lives beneath the configured root:

```text
data/
  warehouse/
    table/example.parquet             # original object bytes
  .system/
    LOCK                              # one gateway process per disk root
    objects/<bucket>/<key-hash>/       # metadata indexed by file generation
    uploads/<bucket>/<upload-id>/      # durable multipart manifests and parts
    buckets/<bucket>/settings.json    # versioning status and private bucket ACL
    versions/<bucket>/<key-hash>/     # immutable versions, index, recovery journal
    staging/                          # files awaiting atomic publication
```

Use a case-sensitive filesystem on Linux or macOS. Native filesystem paths cannot represent every S3 key: trailing slashes, repeated slashes, `.`/`..` components, symlinks, and a file alongside a directory of the same name are unsupported. These restrictions matter for arbitrary S3 applications; conventional Iceberg warehouse paths fit this mapping.

## Run with Azure

```sh
export S3_ACCESS_KEY=gateway-local
export S3_SECRET_KEY='choose-a-long-random-secret'
# AZURE_STORAGE_ACCOUNT and AZURE_STORAGE_KEY are loaded from .env.
task run -- -backend azure
```

Alternatively set `AZURE_STORAGE_SAS_TOKEN`, or omit both key and SAS to use the Azure SDK's `DefaultAzureCredential`. `AZURE_STORAGE_SERVICE_URL` selects a custom endpoint, including Azurite. The SAS token is configured separately from that URL.

Buckets map directly to containers; keys map directly to blob names. Content type, cache control, disposition, encoding, language, and Content-MD5 use native Azure properties. Compatible user metadata is stored as native metadata. A reserved `s3gw` metadata field stores S3 ETags, checksums, completed-part boundaries, tags, version identity, private ACLs, expiry, and metadata names/values Azure cannot represent directly. Large envelopes use a content-addressed helper blob under `.s3gw/metadata/`, referenced by native metadata; these helpers are retained until bucket deletion. Container metadata stores bucket versioning and ACL settings. Native readers see the original object bytes. Multipart helpers and immutable version history occupy the reserved `.s3gw/` blob prefix, hidden from S3 clients.

`task run` and `task test-azure` load the standard `.env` file without sourcing it as shell code. The compiled binary reads environment variables, so launch it through Task or export the configuration yourself. The opt-in live test uses the Azure credentials from `.env`:

```sh
task test-azure
```

That test creates one uniquely named `s3gw-test-*` Azure container and removes its own objects and container afterward. It does not use existing containers as test fixtures. Keep `.env` private (`chmod 600 .env`); it is excluded from Git and the Docker build context. `.env.example` contains only placeholders.

## Versioning and private ACLs

Buckets start unversioned. Enable versioning through the standard S3 API:

```sh
aws --endpoint-url http://127.0.0.1:9000 s3api put-bucket-versioning \
  --bucket warehouse --versioning-configuration Status=Enabled
aws --endpoint-url http://127.0.0.1:9000 s3api list-object-versions --bucket warehouse
```

Enabled writes receive unique version IDs. Suspended writes replace the `null` version. Deletes create delete markers; specifying a version ID permanently deletes that version. Removing the latest version restores the preceding version at the native file/blob path. Reads, copies, batch deletes and multipart completion support versions. All versions and delete markers must be removed before deleting a bucket. MFA Delete is unsupported.

Get/PutBucketAcl and Get/PutObjectAcl support `private` and explicit grants to the configured owner, including object-version ACLs. Public/group/cross-user grants are rejected; anonymous access remains disabled. ACL changes preserve object content, ETag, modification time and version identity.

Object tags support put/get/delete, upload headers, copy directives, and explicit version IDs. Tag updates preserve object bytes, ETag, modification time, and version identity. `GetObjectAttributes` supports attribute selection and paginated completed-part metadata. `GetObject`/`HeadObject` accept `partNumber`; boundaries are retained for new multipart completions, including historical versions. Existing multipart objects written before this metadata was recorded still support whole-object and byte-range reads, but part-number reads return `NotImplemented`.

Versioned buckets require all mutations to go through one gateway process. Native reads remain supported, but native writes bypass version history. History is stored as immutable copies, with an additional copy at the current native path; budget storage accordingly. Azure account-level versioning does not need to be enabled. [Recovery and scale limits](docs/compatibility.md) apply.

## Configuration

| Flag | Environment | Default |
| --- | --- | --- |
| `-backend` | `S3_BACKEND` | `disk` |
| `-listen` | `S3_LISTEN` | `127.0.0.1:9000` |
| `-region` | `S3_REGION` | `us-east-1` |
| `-root` | `S3_DISK_ROOT` | `./data` |
| `-domain` | `S3_DOMAIN` | path-style addressing |
| `-temp-dir` | `S3_TEMP_DIR` | system temporary directory |
| `-azure-account` | `AZURE_STORAGE_ACCOUNT` | none |
| `-azure-url` | `AZURE_STORAGE_SERVICE_URL` | derived from account |
| `-tls-cert`, `-tls-key` | `S3_TLS_CERT`, `S3_TLS_KEY` | TLS disabled |
| `-read-only` | none | false |

Credentials are environment-only: `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `AZURE_STORAGE_KEY`, and `AZURE_STORAGE_SAS_TOKEN`. Virtual-host addressing additionally requires DNS and, for HTTPS, a matching certificate. Proxies must preserve the signed host and object path.

Request bodies are spooled to temporary files for integrity checks before publication. Multipart completion assembles another temporary file. Set `-temp-dir` to an existing directory with enough free space for concurrent uploads and complete objects.

## Verification

```sh
task --list               # available tasks
task test                 # AWS SDK HTTP tests, disk tests, race detector
task vet
task build                # bin/gateway
task test-containers      # Ceph profiles + protocol SDK + minio-go + Iceberg
task test-minio           # client compatibility in Docker; no runtime SDK dependency
task test-protocol        # versioning, ACL, tags, checksums, parts and retry tests
task ceph-versioning      # 28 selected upstream versioning cases on both backends
task ceph-acl             # four selected private ACL cases on both backends
task ceph-full            # wider compatibility report; failures are expected
task compatibility-report # refresh the checked-in results from local artifacts
task clean-containers     # remove only this project's disposable fixtures
```

The Go tests use real AWS SDK request signing and response parsing through an in-process HTTP transport. They cover native files, Unicode and escaped keys, ranges, conditional writes, copy, pagination, multipart restart/abort/retry, integrity rejection, and presigned requests. Published AWS signed-chunk and trailer vectors are also checked. Azure metadata mapping has local tests; the basic live Azure test checks object round trips, metadata, conditional writes, and range reads. The Docker suites exercise both disk and the Azure adapter through Azurite; broader live Azure compatibility remains unverified.

[Ceph s3-tests](https://github.com/ceph/s3-tests) is checked out at commit `5522d1c` inside its test image. `--core` selects nine explicit upstream listing tests; `--versioning` selects 28 version/copy/delete scenarios; `--acl` selects four private ACL scenarios. `--full` selects `s3tests/functional/test_s3.py` (838 tests at this revision). Assertions are unchanged. A documented [fixture adapter](tests/ceph/gateway_fixtures.py) replaces setup/cleanup with main-user cleanup that deletes versions and delete markers, because upstream setup assumes additional identities. Alternate credentials remain distinct, so unsupported identity tests fail. JUnit results, collection counts, summaries, the resolved source revision, fixture description, and installed Python package versions go to `results/`. Passing selected profiles is not full conformance. Run `task ceph-full -- --maxfail=0 --timeout=30` to process the full selection without a failure-count cutoff, with a 30-second timeout per test; both backends run even if disk fails.

The [minio-go client harness](tests/minio/compatibility_test.go) pins v7.3.0 in a separate test module and runs only in Docker. It covers object operations, known/unknown-size checksummed multipart uploads, copied parts, composition, incomplete uploads, and presigning. It does not add minio-go to the gateway module or run the entire upstream MinIO suite. JSON test events are written to `results/`.

The separate [Iceberg test module](tests/iceberg/iceberg_test.go) uses [apache/iceberg-go v0.6.0](https://github.com/apache/iceberg-go/tree/v0.6.0). It exercises S3 FileIO writes, seek/range reads, deletion, and SQL catalog table creation/reload with metadata stored through this gateway. A separate scenario writes real Parquet files, commits two snapshots, reloads the table, and verifies complete, historical, and filtered scans. Concurrent Iceberg catalog commits are not covered. The gateway supplies object storage; an Iceberg catalog is still required.

Container fixtures use disposable storage and explicit local test credentials, including Azurite. They do not mount `.env` or pass its Azure credentials to containers. Iceberg and minio-go each have their own dependencies, outside the gateway module. Run `task compatibility-report` after the suites to refresh [COMPATIBILITY.md](COMPATIBILITY.md) and the compact [result snapshot](tests/compatibility-results.json); raw reports remain under ignored `results/`.

## Add a backend

Implement [backend.Backend](backend/backend.go) and register its constructor in [cmd/gateway](cmd/gateway/main.go). The shared [managed backend layer](backend/managed/store.go) adds persistent version history and publication journals. Implement `backend.Properties` to store bucket settings natively and update private object ACLs and tags while preserving content identity. HTTP routing, SigV4, XML, multipart handling, checksums, and S3 semantics live in [gateway](gateway/); storage implementations do not depend on the AWS SDK.

The contract requires atomic publication, atomic conditional mutations, reads pinned to a backend revision, and sorted pagination. S3 ETags and backend concurrency revisions are separate. Multipart state persists through the same interface, with `.s3gw/` reserved for internal operations. A backend may redirect this prefix to private storage, as the disk backend does.
