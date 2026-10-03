# Compatibility and operating limits

The [AWS operations chart and measured test results](../COMPATIBILITY.md) track protocol support and conformance findings. This document describes storage and operating limits, not an S3 certification.

| Area | Current behavior |
| --- | --- |
| Authentication | SigV4 headers and presigned URLs; one static principal; no anonymous, IAM, STS, SigV2, or SigV4a |
| Addressing | Path-style; optional virtual-host style |
| Buckets | Create, head, list, delete empty bucket, location, enabled/suspended versioning |
| Objects | Put, get, head, delete, batch delete, streaming copy, GetObjectAttributes |
| Reads | Single, suffix, and open-ended byte ranges; completed-part GET/HEAD; ETag/date conditions; response header overrides; checksums scoped to response bytes |
| Writes | `If-Match` / `If-None-Match`; complete bytes published atomically |
| Listing | V1/V2, prefix, delimiter, pagination, URL encoding |
| Versioning | Unique versions, null versions, delete markers, version reads/copies/deletes, version listing, native latest-path restoration |
| ACLs | Private bucket/object ACLs and version-specific owner grants; one configured principal |
| Multipart | Initiate, upload, copy part, list parts/uploads, complete, abort; persistent state and completion retry |
| Integrity | Content-MD5, SHA256 payload validation, CRC32, CRC32C, CRC64NVME, SHA1, SHA256; signed chunks and checksum trailers |
| Metadata | Content headers, user metadata, object/version tags, completed-part descriptors; Azure native metadata with overflow manifests, disk private helper files |
| Unsupported | Public/cross-user ACLs, IAM/bucket policies, MFA Delete, bucket tagging, SSE request semantics, object lock, lifecycle, notifications, CORS, browser POST, select, restore |

Unsupported recognized features return an S3 error rather than silently claiming support. Private ACLs and explicit owner-only grants are persisted, while the configured principal retains owner access. Public/group/other-user grants are rejected. These S3 ACLs do not modify Azure IAM or public container policies. `STANDARD` is the only accepted S3 storage class. Native Azure encryption remains an Azure account concern; accepting S3 encryption headers would require additional implementation.

Some finer compatibility details remain incomplete: part-number reads for legacy multipart objects without saved boundaries, copy checksum selection, Unicode metadata response encoding, and newline-prefix listing behavior in Ceph. Multipart delimiter listing and checksum fields in copy-part/list-parts/completion XML have Docker client coverage. Core tests exercise common paths; arbitrary combinations of S3 features are not yet certified.

## Transparency boundaries

**Disk.** Buckets are directories and keys are relative file paths. The root `.system` directory is reserved; an ordinary object named `.system/example` inside a bucket is allowed. The `.s3gw/` object prefix is reserved across both backends. Existing regular files are readable and derive ETags from their contents. Filesystem case folding, component/path length limits, and file/directory collisions constrain the key space. Do not mount a case-insensitive disk when case-distinct object names are required.

Metadata records are selected by file size and nanosecond modification time. A native overwrite that changes either invalidates saved S3 metadata. A native writer that preserves both can leave stale metadata; modify objects through the gateway when preserving S3 metadata and conditional semantics matters. Symlinks are rejected, and the tree must not be modified by an untrusted local process. Use atomic replacement for native writers; in-place mutation can affect an ongoing read.

Disk uses a process lock and atomic file rename. Only one gateway process can open a root. Files and their metadata are synced before publication, but this is not a fully audited crash-consistent filesystem database. Old metadata generations and crash-abandoned staging files are retained; automated garbage collection is not implemented. Directory timestamps currently supply disk bucket creation dates, including for native buckets.

**Azure.** Container/blob names are preserved, so Azure naming and metadata limits apply. For example, S3 bucket names containing dots cannot become native Azure containers. Existing blobs do not require migration. Blobs without gateway metadata use native Content-MD5 where present, otherwise an opaque Azure ETag. ETags must be treated as opaque by clients.

Gateway metadata is bound to native Content-MD5 and size. An external overwrite that changes these invalidates saved S3 checksums and multipart ETags. External tools that preserve stale Content-MD5 can also preserve stale S3 metadata; native writers should update content properties correctly. Native user metadata edits remain visible through S3.

Private ACL and object tag updates normally use Azure SetMetadata. If a native blob has no Content-MD5, the gateway republishes its unchanged bytes with a computed digest and metadata in one block commit, preserving the S3 ETag and modification time. This allows subsequent metadata edits to remain tied to the same content; the first such metadata update costs a full read/write.

Large S3 metadata envelopes (for example, many completed parts or Unicode tag sets) exceed Azure's 8 KiB metadata capacity. The gateway writes an immutable SHA256-addressed helper under `.s3gw/metadata/` before atomically publishing a native metadata pointer. Reads verify the helper digest and fail if the helper is missing or corrupt. Native bytes and user metadata remain accessible. Helpers may be shared by versions and remain until bucket deletion; automatic garbage collection is not implemented. Tags live in gateway metadata and are not mirrored into Azure Blob Index Tags.

Azure puts stage blocks and commit bytes, properties, and metadata together. Conditions use Azure ETags for compare-and-swap. Bucket deletion is protected from concurrent mutations within the same gateway process. Azure does not provide atomic "delete only if empty" container deletion; concurrent writers outside that process can race it. Use a single gateway instance and avoid native writers during bucket deletion. Azure snapshots, leases, soft deletion, and account retention settings can affect deletion and are not mapped to S3 versioning.

## Scale and recovery

Versioning stores immutable history plus a second copy of the latest object at its native path. Existing native objects are preserved as null versions on their first S3 mutation after enabling versioning. All writes/deletes to versioned buckets must go through one gateway process: native writers and multiple Azure gateway instances cannot participate in version history or recovery. Native reads continue to work; a delete marker removes the native current path, and deleting the latest version republishes its predecessor.

Version indexes and publication journals live in `.system/versions/<bucket>/` on disk or `.s3gw/versions/` in Azure. Bucket status/private ACLs live in `.system/buckets/` or Azure container metadata. The shared layer serializes operations per bucket and replays interrupted publication before subsequent S3 reads/listing/mutations. Fault-injection tests exercise write/delete recovery. Publication across the history record and native path is not an atomic transaction for native readers, and a failed or disconnected request may finish during journal replay. Corrupt/missing helper state fails rather than inventing history.

Version listing scans indexes and native objects; each key's index contains its entire version history. Listing pagination after the marker version has been permanently removed skips the rest of that key. Metadata generations and abandoned unpublished history copies can accumulate; comprehensive garbage collection is still pending. This is not yet a design for very large histories or distributed writers.

Single puts and individual multipart parts are limited to 5 GiB; non-final multipart parts must be at least 5 MiB. The initial Azure uploader uses 8 MiB blocks, so Azure's block-count limit can constrain very large completed objects before S3's own size ceiling. Requests are spooled and copies pass through the gateway; this implementation is not yet optimized for throughput or very large warehouses.

Disk listing walks and sorts the tree. Azure listing starts at the prefix and skips through the continuation key. Both need indexing/cursor work for large namespaces. Native disk objects without saved metadata require content hashing when listed. There is no upload concurrency admission limit or automatic temporary-disk quota management yet.

Multipart coordination uses locks in one gateway process. Incoming part bodies are staged outside the upload lock; part commits can run concurrently, while completion and abort exclude commits and recheck upload state. Multiple Azure gateway instances must not operate on the same multipart upload. Completed manifests are retained to serve retry requests and currently have no expiry. A crash between final publication and saving the completion record remains a recovery window; an unconditional retry may publish again, while a conditional retry may fail its condition. Treat the current implementation as a development gateway until recovery and distributed coordination are strengthened.

## Next compatibility milestones

1. Continue triaging remaining Ceph discrepancies against AWS semantics; some RGW tests deliberately differ, including missing-object conditional deletes.
2. Extend the Iceberg Parquet append/scan coverage with concurrent catalog commit scenarios.
3. Verify native Azure reads/writes, conditional races, multipart recovery, and account-specific behavior against real Blob Storage.
4. Improve multipart recovery/garbage collection, copy checksum behavior, pagination scale, and request resource limits.
5. Add identities and other S3 APIs only with explicit storage semantics and conformance tests; expand versioning boundary and concurrency coverage.
