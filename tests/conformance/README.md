# S3 conformance harness

One scenario set, written against the AWS S3 documentation, that runs
unchanged against every target: the gateway on each backend, any S3 endpoint
over HTTP, and Amazon S3 itself. Every request is a raw, hand-signed SigV4
HTTP call, so the suite observes the wire protocol (status, error code,
headers, XML) rather than what an SDK chooses to surface.

## What a run produces

- Go test results. Each scenario is a test; each expectation that fails is a
  *divergence* from the documented AWS behavior.
- `results/conformance-<target>.json` (or `CONFORMANCE_REPORT`): every step's
  normalized observation (status, code, semantic headers, flattened XML,
  notes) plus the divergence list with documentation links.
- A summary on stdout listing known and unknown divergences.

Dynamic values (dates, request ids, version ids, owner ids, continuation
tokens) are reduced to `{present}` and bucket names to `{bucket}`, so two
reports from different targets can be diffed.

## Divergences

`knownDivergences` in `divergences_test.go` lists gateway behavior that
differs from AWS, keyed by `scenario/step#field` with a reason prefixed
`design:` (documented decision), `gap:` (undocumented difference) or
`bug (<backend>):` (found by this suite). Known entries are logged, not
failed, for gateway targets. They are ignored for `CONFORMANCE_TARGET=aws`,
where every expectation must hold: a failure there means the oracle is wrong
and the expectation (not the gateway) needs fixing. Any new divergence fails
the suite.

`knownProviderDivergences` scopes provider limitations by the built-in target
names (`s3`, `azure`, `azurite`). RustFS's unsupported key shapes and Azurite's
control-character listing failure cannot mask failures on other targets.
Azure gzip reads and S3 URL-encoded listings have no failure allowances.

Steps marked "recorded only" in the scenarios carry no expectation because
AWS does not document the behavior; a baseline comparison still covers them.

## Running

```sh
task test-conformance                 # in-process gateway on a temporary disk root
GATEWAY_TEST_AZURITE_URL=http://127.0.0.1:10000/gateway go test ./tests/conformance -v
GATEWAY_TEST_STORAGE_ENDPOINT=http://127.0.0.1:9002 AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… go test ./tests/conformance -v
task test-conformance-containers      # disk, Azurite and RustFS gateway containers over HTTP
```

Any endpoint:

```sh
CONFORMANCE_ENDPOINT=http://127.0.0.1:9000 CONFORMANCE_ACCESS_KEY=… CONFORMANCE_SECRET_KEY=… \
CONFORMANCE_NAME=gateway CONFORMANCE_BACKEND=disk go test ./tests/conformance -v
```

`CONFORMANCE_BACKEND=disk` skips keys that cannot be file paths (`dir/`,
`a//b`, `./x`); the in-process disk target does this automatically.

### Amazon S3

```sh
task test-conformance-aws             # CONFORMANCE_REGION=eu-central-1 task test-conformance-aws
```

This uses the AWS SDK credential chain, creates uniquely named `s3gw-conf-*`
buckets, removes every version, upload and bucket it created, and writes
`results/conformance-aws.json`. Each run touches a few hundred requests and
about 11 MiB of uploads. The oracle was validated against Amazon S3 in
eu-west-1 on 2026-10-04 using an existing, versioning-enabled bucket; the
unversioned-bucket steps, bucket creation, ListBuckets, bucket tagging and
new-bucket defaults were skipped in that mode and still await a run with
disposable buckets. Lessons from that run are folded into the expectations:
AWS encodes a space as `+` in `encoding-type=url` listings, ignores
`If-Modified-Since` dates in the future, accepts `x-amz-acl: private` under
BucketOwnerEnforced, lists `Part` details in GetObjectAttributes only for
composite checksums, answers 501 to `If-Match: *` on PutObject, and returns
DeleteObjects results in no particular order. A second run with the
`dihei` bucket confirmed more: AWS serves the whole object for any malformed
`Range` header, honors weak `If-None-Match` validators, rejects a body on
CopyObject and an ETag in PutObject's `If-None-Match`, answers 400
InvalidArgument for a malformed version id and BadRequest for more than ten
tags, and never carries `x-amz-website-redirect-location` over on CopyObject.
A versioned fixed bucket cannot inform version-related fields, so the
baseline comparison skips them when the two targets' versioning differs.

#### Using an existing bucket

```sh
task test-conformance-aws CONFORMANCE_REGION=eu-west-1 CONFORMANCE_BUCKET=dihei
```

The IAM identity needs full S3 access to that bucket and its objects
(listing, object reads and writes including versions, multipart, tagging,
ACL reads and every bucket configuration read). Without
`CONFORMANCE_ALLOW_VERSIONING` the policy may omit `s3:PutBucketVersioning`.

The bucket must be empty: the run refuses existing objects, versions,
delete markers or multipart uploads and prints what it found. For a bucket
dedicated to conformance runs, `CONFORMANCE_PURGE_FIXED_BUCKET=1` deletes
that content first. In a versioning-enabled bucket the harness's own
"delete a missing key" steps leave delete markers that cleanup removes; an
interrupted run can leave some behind.

With `CONFORMANCE_BUCKET`, scenarios run one at a time in that bucket. The
harness removes keys touched by each scenario and never deletes the bucket. Steps that
would create, delete or reconfigure buckets (bucket creation, ListBuckets,
bucket tagging) are skipped. Enabling versioning cannot be undone, so the
versioned steps are skipped unless `CONFORMANCE_ALLOW_VERSIONING=1` is set;
afterwards the bucket stays in the Suspended state and the
`versioning-default` expectation no longer applies to it.

### Comparing with a baseline

```sh
task test-conformance-compare         # disk gateway vs results/conformance-aws.json
CONFORMANCE_BASELINE=results/conformance-aws.json GATEWAY_TEST_AZURITE_URL=… go test ./tests/conformance -v
```

With a baseline, every recorded step is compared field by field with the
same step in the baseline: status, error code, semantic headers, every XML
element and attribute, and notes. Differences are reported as divergences
with source `baseline`. Informational headers (`Server`, encryption
defaults, connection management) are recorded under `extra_headers` and not
compared.

## Scenarios

| Scenario | Coverage |
| --- | --- |
| errors | NoSuchBucket/NoSuchKey/MethodNotAllowed codes, HEAD without body, idempotent delete, bucket naming rules, 1024-byte key limit, common response headers |
| auth | anonymous, wrong secret, unknown key, clock skew, wrong region scope, presigned expiry limits, mixed authentication |
| buckets | legacy us-east-1 re-create, HeadBucket region, GetBucketLocation, ListBuckets paging, versioning states and malformed configurations, BucketNotEmpty, location constraints |
| bucket-defaults | documented defaults of a new bucket: tagging/CORS/lifecycle absent, Block Public Access, BucketOwnerEnforced, SSE-S3, policy/website/replication/object-lock not-found codes |
| objects | ETag, default content type, content headers, user metadata casing, response overrides, 2 KiB metadata limit, empty objects, special keys, filesystem-hostile keys |
| control-characters | keys with XML-incompatible characters, encoding-type=url listings |
| redirect-metadata | redirect properties on PUT/GET/HEAD, copy directives, multipart completion and tag updates |
| ranges | single, suffix, open and clamped ranges, 416 with Content-Range, HEAD with Range, malformed ranges recorded |
| conditional-reads | If-Match/If-None-Match/If-Modified-Since/If-Unmodified-Since and the documented precedence combinations |
| conditional-writes | If-None-Match `*`, If-Match on PutObject, 404 for a missing key, failed writes leave data intact |
| integrity | Content-MD5, payload SHA-256 mismatch, CRC32/CRC32C/CRC64NVME/SHA1/SHA256 headers and echo, checksum mode, single-checksum rule, aws-chunked trailers |
| copy | directives, tag directives, self-copy rules, source conditions, encoded sources, `versionId=null` |
| listing | V1/V2 shape, KeyCount, fetch-owner, delimiter grouping, cross-page ordering, start-after/marker, max-keys edge cases, encoding-type, NextMarker rules, directory markers |
| multipart-full-checksums | explicit full-object CRC32/CRC32C/CRC64NVME multipart completion, HEAD checksums and GetObjectAttributes without part details |
| multipart | part number limits, NoSuchUpload, ListParts paging, InvalidPart/InvalidPartOrder/EntityTooSmall, composite ETag, part reads, InvalidPartNumber, abort semantics, copy-part ranges, conditional completion, GetObjectAttributes, checksummed uploads |
| versioning | null versions, version ids, delete markers on GET/HEAD/versioned GET, marker removal, suspended writes, version listing order and paging |
| batch-delete | verbose/quiet results, Content-MD5 requirement, size limits, versioned deletes with DeleteMarkerVersionId |
| tagging | object and bucket tag limits and character rules, tag counts, header tagging, status codes |
| acl | default grants, BucketOwnerEnforced rejections, storage classes, SSE and Object Lock headers, website redirect metadata |
