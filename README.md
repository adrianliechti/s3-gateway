# S3 gateway

A custom Go S3 HTTP implementation with interchangeable disk, Azure Blob and AWS S3-compatible backends. Object bytes and paths stay native: `s3://warehouse/table/data.parquet` is `data/warehouse/table/data.parquet` on disk, or `table/data.parquet` in the Azure container `warehouse`.

Go module: `github.com/adrianliechti/s3-gateway`. The executable is `gateway`, built from `cmd/gateway`.

This is an initial implementation, not a claim of full S3 or Iceberg conformance. It supports optional bucket versioning, private bucket/object ACLs for one configured user, object tagging, and object attributes with completed-part reads. Optional JWT web identity, bucket ABAC and SNS notifications are supported. Tests cover AWS SDKs, Ceph, RustFS, minio-go, Iceberg and live Azure. See the [AWS operations chart and measured results](COMPATIBILITY.md) and [operating limits](docs/compatibility.md) for exact scope and remaining failures.

## Examples

Runnable Go examples each have their own short README and create isolated demo resources:

- [SNS notifications](examples/sns/README.md): signed HTTP subscriber, bucket filters, upload/delete events, and cleanup.
- [ABAC and tags](examples/abac/README.md): local JWT issuer, STS credentials, team-based access, and revocation by retagging.
- [Objects and presigned URLs](examples/objects/README.md): versions, conditional writes, metadata, GET and PUT links.
- [Parallel multipart uploads](examples/multipart/README.md): bounded part readers, checksums, completion, and abort cleanup.

See [example setup](examples/README.md) for shared credentials and commands.

## Run on disk

Requires Go 1.27.1 or newer and [Task](https://taskfile.dev/docs/installation/). Configure credentials in `.env` or the environment. On a fresh checkout, copy `.env.example` to `.env` and replace the placeholder credentials. If `.env` already contains Azure credentials, preserve them and add `GATEWAY_ACCESS_KEY` and `GATEWAY_SECRET_KEY`. `task run` loads `.env`; exported variables take precedence. Flags and help output do not contain credential values.

```sh
export GATEWAY_ACCESS_KEY=gateway-local
export GATEWAY_SECRET_KEY='choose-a-long-random-secret'
task run -- -backend disk -root ./data
```

The endpoint defaults to `http://127.0.0.1:9000`, signing region `us-east-1`. The configured principal can access every bucket exposed by this gateway. Configure TLS using `-tls-cert` and `-tls-key` when serving over an untrusted network.

```sh
export AWS_ACCESS_KEY_ID="$GATEWAY_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$GATEWAY_SECRET_KEY"
export AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://127.0.0.1:9000 s3 mb s3://warehouse
aws --endpoint-url http://127.0.0.1:9000 s3 cp example.parquet s3://warehouse/table/example.parquet
```

Existing directories become buckets, and existing regular files become objects without importing them. Helper state lives beneath the configured root:

```text
data/
  warehouse/
    table/example.parquet             # original object bytes
    .gateway/
      settings                        # bucket configuration
      metadata/<id>                   # metadata indexed by key and file generation
      <upload-id>/                    # durable multipart manifests and parts
      versions/<key-hash>/             # immutable versions, index, recovery journal
  .gateway/
    LOCK                              # one gateway process per disk root
    staging/                          # files awaiting atomic publication
```

Disk files need no embedded gateway field or extended attributes: inode, modification time and size identify their metadata. Metadata is saved before the staged file is atomically renamed into place on the same filesystem. Native files without helpers remain readable without creating metadata on read. Use atomic replacement for native edits; in-place edits that deliberately preserve both size and modification time cannot be detected reliably. Copying files to new inodes changes their identity, so copying the data directory alone does not preserve gateway metadata associations.

Use a case-sensitive filesystem on Linux or macOS. Native filesystem paths cannot represent every S3 key: trailing slashes, repeated slashes, `.`/`..` components, symlinks, and a file alongside a directory of the same name are unsupported. These restrictions matter for arbitrary S3 applications; conventional Iceberg warehouse paths fit this mapping.

## Run with Azure

```sh
export GATEWAY_ACCESS_KEY=gateway-local
export GATEWAY_SECRET_KEY='choose-a-long-random-secret'
# AZURE_STORAGE_ACCOUNT and AZURE_STORAGE_KEY are loaded from .env.
task run -- -backend azure
```

Alternatively set `AZURE_STORAGE_SAS_TOKEN`, or omit both key and SAS to use the Azure SDK's `DefaultAzureCredential`. `AZURE_STORAGE_SERVICE_URL` selects a custom endpoint, including Azurite. The SAS token is configured separately from that URL.

Buckets map directly to containers; keys map directly to blob names. Content type, cache control, disposition, encoding, language, and Content-MD5 use native Azure properties. Compatible user metadata is stored as native metadata. The reserved `gateway` metadata field contains only a format version and a reference to `.gateway/metadata/<id>`. That helper holds the full gateway metadata record: S3 ETags, checksums, completed-part boundaries, tags, version identity, private ACLs, expiry, and metadata names/values Azure cannot represent directly. The helper is written before publishing the reference; unreferenced helpers are collected after a 24-hour grace period, while current objects and retained versions keep their helpers. Bucket settings use a JSON object at `.gateway/settings`, the same layout as S3 and disk. Native readers see the original object bytes. Multipart helpers and immutable version history occupy the reserved `.gateway/` blob prefix, hidden from S3 clients.

`task run` and `task test-azure` load the standard `.env` file without sourcing it as shell code. The compiled binary reads environment variables, so launch it through Task or export the configuration yourself. The opt-in live test uses the Azure credentials from `.env`:

```sh
task test-azure
```

That test creates one uniquely named `s3gw-test-*` Azure container and removes its own objects and container afterward. It does not use existing containers as test fixtures. Keep `.env` private (`chmod 600 .env`); it is excluded from Git and the Docker build context. `.env.example` contains only placeholders.

## Run with AWS S3 or RustFS

The adapter is `backend/aws`; select it with `-backend s3`. Gateway credentials and region are independent of the upstream provider. It uses the AWS Go SDK v2 credential chain (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, optional `AWS_SESSION_TOKEN`, profiles or workload roles).

```sh
# A RustFS provider on port 9002, separate from the gateway and local SNS.
export AWS_ENDPOINT_URL_S3=http://127.0.0.1:9002
export AWS_REGION=us-east-1
export AWS_ACCESS_KEY_ID=rustfs-access
export AWS_SECRET_ACCESS_KEY=rustfs-secret
task run -- -backend s3
```

`AWS_ENDPOINT_URL_S3` and `AWS_REGION` are read by the AWS SDK; `-s3-endpoint` and `-s3-region` override them. `AWS_ENDPOINT_URL_S3_MODE` is a gateway-specific setting accepting `path` (`host/bucket/key`, the default) or `virtual` (`bucket.host/key`). The boolean flag `-s3-path-style` overrides it; the AWS SDK does not read this variable itself. For AWS, leave endpoint overrides unset and usually set `AWS_ENDPOINT_URL_S3_MODE=virtual`. Buckets and object keys map directly to S3. Native content properties, metadata, version retention, multipart transfer and conditional operations are used. The gateway keeps its own logical S3 semantics across providers: version IDs, private ACLs, lifecycle execution and multipart retry receipts do not simply pass through to AWS. As on Azure, the native `gateway` metadata field contains only a format version and a reference to the full gateway metadata record at `.gateway/metadata/<id>`. Ordinary unversioned writes create a metadata helper without duplicating object bytes. S3 streaming uploads larger than 8 MiB use a write-once helper: checksums are known only after the stream ends, while S3 fixes multipart metadata at initiation. The reserved `.gateway/` prefix also contains indexes, recovery records and pending upload parts. Bucket configuration lives in `.gateway/settings` on all three backends.

Presigned URLs are supported at the gateway endpoint, including GET, HEAD, PUT and UploadPart. For example, `aws --endpoint-url http://127.0.0.1:9000 s3 presign s3://warehouse/table/example.parquet --expires-in 3600`. They authorize access to the gateway and remain independent of the upstream provider's credentials.

## Versioning and private ACLs

Buckets start unversioned. Enable versioning through the standard S3 API:

```sh
aws --endpoint-url http://127.0.0.1:9000 s3api put-bucket-versioning \
  --bucket warehouse --versioning-configuration Status=Enabled
aws --endpoint-url http://127.0.0.1:9000 s3api list-object-versions --bucket warehouse
```

Enabled writes receive unique version IDs. Suspended writes replace the `null` version. Deletes create delete markers; specifying a version ID permanently deletes that version. Removing the latest version restores the preceding version at the native file/blob path. Reads, copies, batch deletes and multipart completion support versions. All versions and delete markers must be removed before deleting a bucket. MFA Delete is unsupported.

Get/PutBucketAcl and Get/PutObjectAcl support `private`, `bucket-owner-full-control`, and explicit grants to the configured owner, including object-version ACLs. Public/group/cross-user grants are rejected; anonymous access remains disabled. ACL changes preserve object content, ETag, modification time and version identity.

Object tags support put/get/delete, upload headers, copy directives, and explicit version IDs. Tag updates preserve object bytes, ETag, modification time, and version identity. `GetObjectAttributes` supports attribute selection and paginated completed-part metadata. `GetObject`/`HeadObject` accept `partNumber`; boundaries are retained for new multipart completions, including historical versions. Existing multipart objects written before this metadata was recorded still support whole-object and byte-range reads, but part-number reads return `NotImplemented`.

Use one gateway process for the buckets visible to a set of provider credentials; background maintenance scans that whole namespace. Versioned objects require all mutations to go through that gateway. Native reads remain supported, but native writes bypass version history. AWS/RustFS retain bytes in native S3 versions; Azure uses native blob versions when account versioning is enabled. Logical version IDs, delete markers and recovery still use small gateway indexes and journals. Disk and Azure accounts without native versioning use immutable helper copies. The gateway does not enable Azure account-level versioning itself. Native S3 versioning stays enabled even when logical gateway versioning is suspended, so retained null versions remain immutable. [Recovery and scale limits](docs/compatibility.md) apply.

Multipart completion journals the final bytes, version identity and completion receipt together. Interrupted publication recovers before the next S3 access or during background maintenance; matching retries return the original result even after a later overwrite or deletion. Cleanup failures do not invalidate a committed completion. See [recovery details](docs/compatibility.md#scale-and-recovery).

## Bucket configuration and lifecycle

Bucket tagging, CORS, public-access-block configuration and ownership controls are available through the standard S3 APIs. CORS supports browser preflight and signed cross-origin requests. Access requires the static operator credentials or an authorized web-identity session. Explicit `BucketOwnerEnforced` ownership disables ACL writes; buckets otherwise keep the existing private ACL behavior.

Lifecycle rules execute in a background worker: current-object expiration, noncurrent version expiration/retention, delete-marker cleanup and abandoned multipart cleanup. Rules support prefix, tag, size and And filters. Production uses UTC-day boundaries and a one-minute scan interval. Storage-class transitions are unsupported. Read-only mode disables lifecycle execution. See [lifecycle semantics and limits](docs/compatibility.md#bucket-settings-and-lifecycle) before applying expiration to warehouse data.

## JWT roles and bucket ABAC

Set `GATEWAY_IDENTITY_CONFIG` to a JSON file such as [examples/abac/identity-provider.json](examples/abac/identity-provider.json), with your trusted issuer, JWKS URL, audience, roles claim and principal tag mappings. Nested claims use dotted paths. Role policies are an explicit subset: Allow/Deny statements, S3 action and resource patterns (`*`, `?`), and `StringEquals` on `aws:ResourceTag/<key>` or `aws:PrincipalTag/<key>`. Whole-value `${aws:PrincipalTag/<key>}` substitutions are supported. Unknown policy fields/operators fail startup. This is gateway authorization, not AWS IAM policy provisioning.

An administrator tags a bucket and enables ABAC:

```sh
aws --endpoint-url http://127.0.0.1:9000 s3control tag-resource \
  --account-id 000000000000 --resource-arn arn:aws:s3:::warehouse \
  --tags Key=team,Value=blue
aws --endpoint-url http://127.0.0.1:9000 s3api put-bucket-abac \
  --bucket warehouse --abac-status Status=Enabled
```

Bucket ABAC is disabled by default. Once enabled, use S3 Control `TagResource` / `UntagResource` to change bucket tags; `GetBucketTagging` and `ListTagsForResource` remain available. The static operator credentials control bucket administration, including tags, ABAC, ACL writes and notification destinations. JWT roles grant object access and bucket configuration reads; even a wildcard session policy cannot administer buckets or change ACLs. This prevents a session from retagging a bucket to grant itself access.

Exchange an issuer-signed JWT through STS `AssumeRoleWithWebIdentity` at the same endpoint. The response contains `AccessKeyId`, `SecretAccessKey`, `SessionToken` and `Expiration`; pass all three credentials to an ordinary AWS S3 client. AWS SDK web-identity credential providers work when their STS client points at the gateway. The supplied RoleArn must match a configured role and its required JWT role claim. Session policies, role chaining and other STS actions are rejected.

JWT verification accepts RSA and ECDSA signatures with fixed supported algorithms, requires issuer, audience, subject and expiry, and checks nbf/iat. JWKS are fetched only from the configured HTTPS URL, cached for five minutes and refreshed for new key IDs with a 30-second request limit. Local test issuers can explicitly set `allow_insecure_http`. Sessions last no longer than the JWT, with a default maximum of one hour and configurable role duration of 900–43200 seconds. Expiring sessions also expire their presigned URLs. Keep JWTs and temporary signing secrets out of logs; a JWT is never used as the S3 signing secret.

Sessions survive restart with the same gateway root credentials. Root-secret rotation revokes them; issuer-key rotation affects subsequent exchanges. Role policy changes take effect when the configuration is reloaded by restarting. Bucket tag changes take effect on subsequent requests, including existing presigned URLs. Copy sources, batch deletion items and every multipart operation are authorized; ListBuckets filters out buckets without ListBucket permission. Public and cross-user ACL grants remain unsupported.

## SNS notifications

For a complete local walkthrough, run [the SNS example](examples/sns/README.md) with `task example-sns`.

Enable `-sns` for AWS SNS, or set `GATEWAY_SNS_ENDPOINT=http://127.0.0.1:9001` for the local SNS gateway. Set `GATEWAY_SNS_ACCESS_KEY` and `GATEWAY_SNS_SECRET_KEY` to that server's credentials; without explicit credentials, the SNS client uses the AWS SDK chain. `GATEWAY_SNS_SESSION_TOKEN` is optional. These credentials are independent of gateway client credentials and upstream storage credentials.

Configure topics with S3 `PutBucketNotificationConfiguration`; `GetBucketNotificationConfiguration` reads them and an empty configuration disables new notifications. Topic ARNs must be standard SNS topics in the gateway region. Configuration normally publishes an `s3:TestEvent` before committing; the standard skip-destination-validation header is supported. Prefix and suffix filters are accepted, while overlapping rules for the same event are rejected.

Supported events are `s3:ObjectCreated:Put`, `Copy`, `CompleteMultipartUpload`, `s3:ObjectRemoved:Delete`, `DeleteMarkerCreated`, and the two category wildcards. Keys use S3's URL form encoding. SNS supplies its normal notification envelope around the S3 Records JSON. SQS, Lambda, EventBridge, lifecycle events, object tagging events and FIFO topics are rejected rather than silently accepted.

Successful mutations persist matching events under `.gateway/notifications/` before the HTTP response is acknowledged. The gateway attempts immediate publication and retries failed deliveries during background scans, including after restart. Delivered receipts are kept for 24 hours; consumers must tolerate duplicates and out-of-order delivery. Bucket deletion waits for pending notifications. Object publication and queue insertion are separate commits: a crash between them may leave a changed object without an event for that unacknowledged request. Delivery is not a distributed transaction with the storage provider or SNS. Embedders using `gateway.New` should schedule `RunNotificationsOnce` alongside lifecycle and maintenance.

`GATEWAY_TEST_SNS_ENDPOINT=http://127.0.0.1:9001 task test-sns` creates a temporary SNS topic, confirms a local HTTP subscription, verifies test/create/delete events and removes the topic. A live local SNS run has passed this complete path.

## Configuration

| Flag | Environment | Default |
| --- | --- | --- |
| `-backend` | `GATEWAY_BACKEND` | `disk` |
| `-listen` | `GATEWAY_LISTEN` | `127.0.0.1:9000` |
| `-region` | `GATEWAY_REGION` | `us-east-1` |
| `-root` | `GATEWAY_DISK_ROOT` | `./data` |
| `-domain` | `GATEWAY_DOMAIN` | path-style addressing |
| `-temp-dir` | `GATEWAY_TEMP_DIR` | system temporary directory |
| `-s3-endpoint` | `AWS_ENDPOINT_URL_S3` (via AWS SDK) | AWS endpoint from the SDK |
| `-s3-region` | `AWS_REGION` (via AWS SDK) | AWS SDK region, then `us-east-1` |
| `-s3-path-style` | `AWS_ENDPOINT_URL_S3_MODE` (`path` or `virtual`, gateway-specific) | `path` (flag: `true`) |
| `-identity-config` | `GATEWAY_IDENTITY_CONFIG` | web identity disabled |
| `-sns` | `GATEWAY_SNS_ENABLED` | `false` |
| `-sns-endpoint` | `GATEWAY_SNS_ENDPOINT` | AWS SNS; setting an endpoint enables SNS |
| `-sns-region` | `GATEWAY_SNS_REGION` | gateway region |
| `-azure-account` | `AZURE_STORAGE_ACCOUNT` | none |
| `-azure-url` | `AZURE_STORAGE_SERVICE_URL` | derived from account |
| `-tls-cert`, `-tls-key` | `GATEWAY_TLS_CERT`, `GATEWAY_TLS_KEY` | TLS disabled |
| `-read-only` | none | false |
| `-lifecycle-interval` | `GATEWAY_LIFECYCLE_INTERVAL` | `1m` |
| `-test-lifecycle-day` | `GATEWAY_TEST_LIFECYCLE_DAY` | `0s` (production UTC days; nonzero is test-only) |

Credentials are environment-only: `GATEWAY_ACCESS_KEY`, `GATEWAY_SECRET_KEY`, `AZURE_STORAGE_KEY`, and `AZURE_STORAGE_SAS_TOKEN`. Virtual-host addressing additionally requires DNS and, for HTTPS, a matching certificate. Proxies must preserve the signed host and object path.

Azure and S3 PUTs and uploaded parts stream to native blocks/parts with bounded memory and up to four transfers in flight. Each transfer uses an 8 MiB retry buffer; S3 also probes the first block to select PutObject or multipart. Bytes reach the provider before the request ends, while signature, length and checksum validation still gates publication. Cloud multipart completion uses S3 UploadPartCopy/CompleteMultipartUpload or Azure StageBlockFromURL/CommitBlockList; it never downloads or concatenates the object through the gateway. Full-object CRCs are combined from part checksums. A durable journal records the selected sources so completion can recover after a restart. Parts remain isolated until accepted, and native version history retains completed bytes where enabled.

Small XML requests, disk-backend uploads/completion, and checksummed copy requests still use temporary files. Set `-temp-dir` to an existing directory sized for those operations. Azurite does not implement StageBlockFromURL; only its explicit APINotImplemented response activates a bounded streaming copy fallback. Production Azure uses native server-side copies. Transfer concurrency is per request, so memory scales with simultaneous uploads; no global admission limit is implemented.

## Verification

```sh
task --list               # available tasks
task test                 # AWS SDK HTTP tests, disk tests, race detector
task vet
task build                # bin/gateway
task test-containers      # Ceph profiles + SDKs + Iceberg + Polaris
task test-minio           # client compatibility in Docker; no runtime SDK dependency
task test-protocol        # protocol, bucket settings and lifecycle tests
task test-s3              # AWS backend contract, protocol and minio-go on RustFS
task test-rustfs          # RustFS curated Ceph selection; unsupported features fail
task test-azure-protocol  # live Azure protocol, native-history and recovery suite
task test-sns             # running SNS server; set GATEWAY_TEST_SNS_ENDPOINT
task test-polaris         # real REST catalog and concurrent Iceberg commits
task ceph-versioning      # 28 selected upstream versioning cases on both backends
task ceph-acl             # four selected private ACL cases on both backends
task ceph-full            # excludes browser POST and explicit SigV2 tests
task ceph-unfiltered      # includes those tests; separate audit reports
task compatibility-report # refresh the checked-in results from local artifacts
task clean-containers     # remove only this project's disposable fixtures
```

The Go tests use real AWS SDK request signing and response parsing through an in-process HTTP transport. They cover native files, Unicode and escaped keys, ranges, conditional writes, copy, pagination, multipart restart/abort/retry, integrity rejection, and presigned requests. Published AWS signed-chunk and trailer vectors are also checked. Azure metadata mapping has local tests; the live Azure test checks object round trips, metadata, conditional writes, presigned requests, versioned multipart uploads, and reads after handler restart. The Docker suites exercise disk, Azurite and RustFS. Opt-in live Azure tests also cover native version retention, multipart crash recovery, metadata helper collection, ABAC and notification persistence.

[Ceph s3-tests](https://github.com/ceph/s3-tests) is checked out at commit `5522d1c` inside its test image. `--core` selects nine explicit upstream listing tests; `--versioning` selects 28 version/copy/delete scenarios; `--acl` selects four private ACL scenarios. `--full` selects `s3tests/functional/test_s3.py`, excluding browser form POST uploads and explicit SigV2 scenarios by default: **793 selected, 45 deselected** out of 838 tests at this revision. Browser POST is excluded by project scope; SigV4 browser POST remains an S3 feature. ListObjectsV2, SigV4 presigned URLs, and ordinary CORS tests remain selected. One mixed logging-authentication test is excluded because it requires SigV2. Assertions are unchanged. `task ceph-unfiltered` (or `task ceph-full -- --include-legacy`) restores all 838 tests and writes separate `*-unfiltered` artifacts. Deselected cases, with names and reasons, are recorded separately from passes, failures, and skips. Disposable gateway fixtures use a one-second lifecycle day and 250 ms scan interval, with upstream `lc_debug_interval=1`; production defaults are unchanged. A documented [fixture adapter](tests/ceph/gateway_fixtures.py) replaces setup/cleanup with main-user cleanup that deletes versions and delete markers, because upstream setup assumes additional identities. Alternate credentials remain distinct, so unsupported identity tests fail. JUnit results, collection counts, summaries, the resolved source revision, fixture description, and installed Python package versions go to `results/`. Passing selected profiles is not full conformance. Run `task ceph-full -- --maxfail=0 --timeout=30` to process the configured selection without a failure-count cutoff, with a 30-second timeout per test; both backends run even if disk fails.

The [conformance harness](tests/conformance/README.md) sends raw SigV4 requests and checks protocol details (status codes, S3 error codes, headers, XML shape) against expectations taken from the AWS S3 documentation, each with its documentation reference. The same scenarios run in-process on disk (`task test-conformance`, part of `go test ./...`), over HTTP against the disk, Azurite and RustFS gateway containers (`task test-conformance-containers`), and against Amazon S3 (`task test-conformance-aws`), which both validates the expectations and records a baseline that later gateway runs are diffed against step by step (`task test-conformance-compare`). Divergences from AWS are listed per run; the ones already classified as design decisions, gaps or backend bugs live in [divergences_test.go](tests/conformance/divergences_test.go), and any new one fails the suite.

The [minio-go client harness](tests/minio/compatibility_test.go) pins v7.3.0 in a separate test module and runs only in Docker. It covers object operations, known/unknown-size checksummed multipart uploads, copied parts, composition, incomplete uploads, and presigning. It does not add minio-go to the gateway module or run the entire upstream MinIO suite. JSON test events are written to `results/`.

The separate [Iceberg test module](tests/iceberg/iceberg_test.go) uses [apache/iceberg-go v0.6.0](https://github.com/apache/iceberg-go/tree/v0.6.0). It exercises S3 FileIO writes, seek/range reads, deletion, and SQL catalog table creation/reload with metadata stored through this gateway. A separate scenario writes real Parquet files, commits two snapshots, reloads the table, and verifies complete, historical, and filtered scans. The separate Polaris scenario covers concurrent REST catalog commits, stale-snapshot conflict retry, and a final scan with both appends retained. The gateway supplies object storage; an Iceberg catalog is still required.

See [Polaris integration](docs/polaris.md) for a tested REST catalog setup using a custom S3 endpoint and static credentials without STS. `task test-polaris` runs Polaris 1.7.0 and iceberg-go in Docker against both backends. Polaris 1.7/1.8 do not implement remote signing; valid SDK-generated presigned S3 URLs are supported independently.

RustFS is pinned to image 1.0.1 and its source revision. `task test-rustfs` uses that revision’s `implemented_tests.txt` selection against the pinned Ceph harness. This exercises the same curated S3 behavior RustFS tests, not the RustFS in-process server tests, which launch their own server. Selection files, unavailable names, upstream revisions and failures are retained in reports; unsupported IAM/encryption/object-lock features are expected to fail.

Container fixtures use disposable storage and explicit local test credentials, including Azurite. They do not mount `.env` or pass its Azure credentials to containers. Iceberg and minio-go each have their own dependencies, outside the gateway module. Run `task compatibility-report` after the suites to refresh [COMPATIBILITY.md](COMPATIBILITY.md) and the compact [result snapshot](tests/compatibility-results.json); raw reports remain under ignored `results/`.

## Add a backend

Implement [backend.Backend](backend/backend.go) and register its constructor in [cmd/gateway](cmd/gateway/main.go). The shared [managed backend layer](backend/managed/store.go) adds persistent version history and publication journals. Implement `backend.Properties` to store bucket settings natively and update private object ACLs and tags while preserving content identity. HTTP routing, SigV4, XML, multipart handling, checksums, and S3 semantics live in [gateway](gateway/); the AWS backend uses the AWS Go SDK v2; the disk and Azure adapters use their own native facilities.

The contract requires atomic publication, atomic conditional mutations, reads pinned to a backend revision, and sorted pagination. S3 ETags and backend concurrency revisions are separate. Multipart state persists through the same interface, with `.gateway/` reserved for internal operations. A backend may redirect this prefix to private storage, as the disk backend does.
