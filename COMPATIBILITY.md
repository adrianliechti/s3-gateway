# S3 compatibility

Reviewed on **2026-10-04** against the [official AWS S3 operation inventory](https://docs.aws.amazon.com/AmazonS3/latest/API/API_Operations_Amazon_Simple_Storage_Service.html): **116 operations**, including legacy aliases. This chart covers the S3 API; S3 Control, S3 Tables, IAM and STS are separate services and are outside this inventory. A method in a client SDK does not establish server support.

The gateway has a static operator, optional JWT web-identity sessions with bucket ABAC, one region and optional per-bucket versioning. **Implemented** means the ordinary operation works within those constraints; **Partial** identifies additional known gaps; **Unsupported** means there is no implementation. These are implementation states, not certification. Evidence below distinguishes actual tests from a code review.

All three backends share the protocol implementation. Disk preserves native files and cannot represent every S3 key; Azure preserves blob paths but inherits container naming/metadata constraints. See [operating limits](docs/compatibility.md) for concurrency, durability, scale and native-writer restrictions. Those limits apply to every row.

## AWS S3 operations chart

“Docker client” refers to the isolated minio-go test container. The minio-go SDK is not a gateway dependency. “Ceph” indicates relevant scenarios in the measured selections below, not that every test for an operation passes. Detailed assertions live in [AWS SDK tests](gateway/gateway_test.go), [versioning/private ACL tests](gateway/versioning_test.go), [protocol regressions](gateway/compatibility_test.go), [bucket configuration/lifecycle tests](gateway/configuration_test.go), [multipart recovery/cleanup tests](gateway/recovery_test.go) and [Docker client tests](tests/minio/compatibility_test.go).

| AWS S3 operation | Disk | Azure Blob | AWS / RustFS | Behavior, limits and evidence |
| --- | --- | --- | --- | --- |
| [AbortMultipartUpload](https://docs.aws.amazon.com/AmazonS3/latest/API/API_AbortMultipartUpload.html) | Implemented | Implemented | Implemented | Removes uploaded parts; committed objects remain intact. AWS SDK; Docker client. |
| [CompleteMultipartUpload](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CompleteMultipartUpload.html) | Partial | Partial | Partial | Ordered parts, minimum part size, conditions and checksums. Durable publication intent and completion receipt recover the original version/result across interrupted writes, including conditional retries. One gateway writer; no distributed coordination. AWS SDK fault tests; Ceph; Docker client. |
| [CopyObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CopyObject.html) | Partial | Partial | Partial | Streaming copy, metadata directives and source version selection. New destination version when enabled. Tagging COPY/REPLACE directives, self-copy validation and supported checksum-algorithm selection/recalculation. Checksummed copies use a staging file. AWS SDK; Ceph; Docker client. |
| [CreateBucket](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucket.html) | Partial | Partial | Partial | One owner and region; new buckets default to BucketOwnerEnforced with all four PublicAccessBlock flags enabled; explicit ObjectOwnership settings are supported. us-east-1 recreation succeeds and resets private ACL while preserving versioning/history; other regions return BucketAlreadyOwnedByYou. Native naming restrictions apply. AWS SDK; Ceph; Docker client. |
| [CreateBucketMetadataConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucketMetadataConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [CreateBucketMetadataTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucketMetadataTableConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [CreateMultipartUpload](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateMultipartUpload.html) | Implemented | Implemented | Implemented | Persistent upload state, content metadata and supported checksum algorithms. AWS SDK; Docker client. |
| [CreateSession](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateSession.html) | Unsupported | Unsupported | Unsupported | S3 Express / directory buckets are not implemented. |
| [DeleteBucket](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucket.html) | Partial | Partial | Partial | Empty buckets only. Azure emptiness check is not atomic with external writers. AWS SDK; Ceph; Docker client. |
| [DeleteBucketAnalyticsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketAnalyticsConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [DeleteBucketCors](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketCors.html) | Implemented | Implemented | Implemented | Persisted CORS rules, origin/method/header matching, unsigned preflight and authenticated actual requests. CORS never grants object access. AWS SDK; Ceph configuration case. |
| [DeleteBucketEncryption](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketEncryption.html) | Unsupported | Unsupported | Unsupported | S3 SSE configuration/rewrapping is not implemented; native Azure encryption is not this API. |
| [DeleteBucketIntelligentTieringConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketIntelligentTieringConfiguration.html) | Unsupported | Unsupported | Unsupported | Storage-class tiering and archive restoration are not implemented. |
| [DeleteBucketInventoryConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketInventoryConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [DeleteBucketLifecycle](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketLifecycle.html) | Implemented | Implemented | Implemented | Persistent validated rules; current/noncurrent expiration, retained-version counts, expired delete markers and abandoned multipart cleanup. Prefix/tag/size/And filters; UTC dates and expiration headers. Storage-class transitions rejected. AWS SDK; Ceph; recovery tests. |
| [DeleteBucketMetadataConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketMetadataConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [DeleteBucketMetadataTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketMetadataTableConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [DeleteBucketMetricsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketMetricsConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [DeleteBucketOwnershipControls](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketOwnershipControls.html) | Partial | Partial | Partial | Persists all three ownership modes for one principal. BucketOwnerEnforced disables ACL writes and accepts uploads without ACLs or with private/bucket-owner-full-control. No cross-account ownership. AWS SDK. |
| [DeleteBucketPolicy](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketPolicy.html) | Unsupported | Unsupported | Unsupported | Bucket policies, ABAC and cross-user authorization are not implemented. |
| [DeleteBucketReplication](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketReplication.html) | Unsupported | Unsupported | Unsupported | S3 replication configuration is not implemented. |
| [DeleteBucketTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketTagging.html) | Implemented | Implemented | Implemented | Up to 50 validated bucket tags, separate from object tags; persisted across restart. AWS SDK; Ceph. |
| [DeleteBucketWebsite](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketWebsite.html) | Unsupported | Unsupported | Unsupported | Static website hosting is not implemented. |
| [DeleteObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObject.html) | Partial | Partial | Partial | Delete markers when enabled/suspended, permanent version deletion, ETag conditions and latest-version restoration. Size/date delete conditions unsupported. AWS SDK; Ceph. |
| [DeleteObjectAnnotation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjectAnnotation.html) | Unsupported | Unsupported | Unsupported | Object annotation resources are not implemented. |
| [DeleteObjectTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjectTagging.html) | Implemented | Implemented | Implemented | Object/version tags; up to ten validated unique tags, preserving bytes, ETag and mtime. Private metadata helpers on disk, Azure and S3. AWS SDK; Ceph. |
| [DeleteObjects](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjects.html) | Partial | Partial | Partial | Up to 1,000 entries; validated Content-MD5 or flexible checksum required; version IDs, marker responses, quiet mode and per-entry ETag conditions/errors. Size/date conditions unsupported. AWS SDK; Ceph. |
| [DeletePublicAccessBlock](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeletePublicAccessBlock.html) | Partial | Partial | Partial | Persists all four flags; BlockPublicAcls rejects public canned ACLs. Gateway remains private regardless of flag values; bucket policies and public grants stay unsupported. AWS SDK; Ceph configuration cases. |
| [GetBucketAbac](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketAbac.html) | Partial | Partial | Partial | Bucket ABAC status; gateway JWT role/tag policies; no full IAM or bucket policy language. SDK integration tests. |
| [GetBucketAccelerateConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketAccelerateConfiguration.html) | Unsupported | Unsupported | Unsupported | Transfer Acceleration is not implemented. |
| [GetBucketAcl](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketAcl.html) | Partial | Partial | Partial | Returns persistent private owner ACL; default FULL_CONTROL for the single configured principal. AWS SDK; Ceph private ACL selection. |
| [GetBucketAnalyticsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketAnalyticsConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [GetBucketCors](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketCors.html) | Implemented | Implemented | Implemented | Persisted CORS rules, origin/method/header matching, unsigned preflight and authenticated actual requests. CORS never grants object access. AWS SDK; Ceph configuration case. |
| [GetBucketEncryption](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketEncryption.html) | Unsupported | Unsupported | Unsupported | S3 SSE configuration/rewrapping is not implemented; native Azure encryption is not this API. |
| [GetBucketIntelligentTieringConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketIntelligentTieringConfiguration.html) | Unsupported | Unsupported | Unsupported | Storage-class tiering and archive restoration are not implemented. |
| [GetBucketInventoryConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketInventoryConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [GetBucketLifecycle](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLifecycle.html) | Implemented | Implemented | Implemented | Persistent validated rules; current/noncurrent expiration, retained-version counts, expired delete markers and abandoned multipart cleanup. Prefix/tag/size/And filters; UTC dates and expiration headers. Storage-class transitions rejected. AWS SDK; Ceph; recovery tests. |
| [GetBucketLifecycleConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLifecycleConfiguration.html) | Implemented | Implemented | Implemented | Persistent validated rules; current/noncurrent expiration, retained-version counts, expired delete markers and abandoned multipart cleanup. Prefix/tag/size/And filters; UTC dates and expiration headers. Storage-class transitions rejected. AWS SDK; Ceph; recovery tests. |
| [GetBucketLocation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLocation.html) | Implemented | Implemented | Implemented | Configured region; empty value for us-east-1. Docker client region discovery. |
| [GetBucketLogging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLogging.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [GetBucketMetadataConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketMetadataConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [GetBucketMetadataTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketMetadataTableConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [GetBucketMetricsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketMetricsConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [GetBucketNotification](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketNotification.html) | Partial | Partial | Partial | SNS topic create/remove notifications, filters, destination test and persistent retry queue. Other destinations/events unsupported. SDK and live local SNS tests. |
| [GetBucketNotificationConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketNotificationConfiguration.html) | Partial | Partial | Partial | SNS topic create/remove notifications, filters, destination test and persistent retry queue. Other destinations/events unsupported. SDK and live local SNS tests. |
| [GetBucketOwnershipControls](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketOwnershipControls.html) | Partial | Partial | Partial | Persists all three ownership modes for one principal. BucketOwnerEnforced disables ACL writes and accepts uploads without ACLs or with private/bucket-owner-full-control. No cross-account ownership. AWS SDK. |
| [GetBucketPolicy](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketPolicy.html) | Unsupported | Unsupported | Unsupported | Bucket policies, ABAC and cross-user authorization are not implemented. |
| [GetBucketPolicyStatus](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketPolicyStatus.html) | Unsupported | Unsupported | Unsupported | Bucket policies, ABAC and cross-user authorization are not implemented. |
| [GetBucketReplication](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketReplication.html) | Unsupported | Unsupported | Unsupported | S3 replication configuration is not implemented. |
| [GetBucketRequestPayment](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketRequestPayment.html) | Unsupported | Unsupported | Unsupported | Requester Pays is not implemented. |
| [GetBucketTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketTagging.html) | Implemented | Implemented | Implemented | Up to 50 validated bucket tags, separate from object tags; persisted across restart. AWS SDK; Ceph. |
| [GetBucketVersioning](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketVersioning.html) | Implemented | Implemented | Implemented | Returns unset, Enabled or Suspended status from durable bucket settings. AWS SDK; Ceph. |
| [GetBucketWebsite](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketWebsite.html) | Unsupported | Unsupported | Unsupported | Static website hosting is not implemented. |
| [GetObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html) | Partial | Partial | Partial | Latest or selected version, delete-marker headers, byte ranges and persisted part-number reads, conditions, response overrides and checksums scoped to full/part responses. Legacy multipart objects without saved boundaries cannot use partNumber. No SSE request semantics. AWS SDK; Ceph; Docker client. |
| [GetObjectAcl](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectAcl.html) | Partial | Partial | Partial | Returns current or selected version ACL. Private owner grants only. AWS SDK; Ceph private ACL selection. |
| [GetObjectAnnotation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectAnnotation.html) | Unsupported | Unsupported | Unsupported | Object annotation resources are not implemented. |
| [GetObjectAttributes](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectAttributes.html) | Partial | Partial | Partial | Selected attributes, size/ETag/storage class, checksums, version selection and paginated part details for composite checksums; composite attribute digests omit the part-count suffix. Legacy objects lack saved part boundaries; SSE unsupported. AWS SDK; Ceph. |
| [GetObjectLegalHold](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectLegalHold.html) | Unsupported | Unsupported | Unsupported | Object lock, retention and legal holds are not implemented. |
| [GetObjectLockConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectLockConfiguration.html) | Unsupported | Unsupported | Unsupported | Object lock, retention and legal holds are not implemented. |
| [GetObjectRetention](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectRetention.html) | Unsupported | Unsupported | Unsupported | Object lock, retention and legal holds are not implemented. |
| [GetObjectTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectTagging.html) | Implemented | Implemented | Implemented | Object/version tags; up to ten validated unique tags, preserving bytes, ETag and mtime. Private metadata helpers on disk, Azure and S3. AWS SDK; Ceph. |
| [GetObjectTorrent](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectTorrent.html) | Unsupported | Unsupported | Unsupported | Torrent retrieval is not implemented. |
| [GetPublicAccessBlock](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetPublicAccessBlock.html) | Partial | Partial | Partial | Persists all four flags; BlockPublicAcls rejects public canned ACLs. Gateway remains private regardless of flag values; bucket policies and public grants stay unsupported. AWS SDK; Ceph configuration cases. |
| [HeadBucket](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadBucket.html) | Implemented | Implemented | Implemented | Existence check. AWS SDK; Docker client. |
| [HeadObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html) | Partial | Partial | Partial | Current/version-specific metadata, delete-marker headers, conditions, ranges, stored checksums and persisted part-number reads. Legacy multipart part boundaries are unavailable. AWS SDK; Ceph; Docker client. |
| [ListBucketAnalyticsConfigurations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBucketAnalyticsConfigurations.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [ListBucketIntelligentTieringConfigurations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBucketIntelligentTieringConfigurations.html) | Unsupported | Unsupported | Unsupported | Storage-class tiering and archive restoration are not implemented. |
| [ListBucketInventoryConfigurations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBucketInventoryConfigurations.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [ListBucketMetricsConfigurations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBucketMetricsConfigurations.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [ListBuckets](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBuckets.html) | Partial | Partial | Partial | Lists visible buckets for the single principal; max-buckets, continuation tokens, prefix and endpoint-region filtering. Native disk creation dates use directory mtime. AWS SDK; Ceph; Docker client. |
| [ListDirectoryBuckets](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListDirectoryBuckets.html) | Unsupported | Unsupported | Unsupported | S3 Express / directory buckets are not implemented. |
| [ListMultipartUploads](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListMultipartUploads.html) | Partial | Partial | Partial | Prefix/delimiter grouping, key/upload markers, URL encoding, initiation ordering. Listing rescans durable upload state; pagination when the marker upload has been removed uses an approximate ID ordering. AWS SDK; Docker client. |
| [ListObjectAnnotations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectAnnotations.html) | Unsupported | Unsupported | Unsupported | Object annotation resources are not implemented. |
| [ListObjectVersions](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectVersions.html) | Partial | Partial | Partial | Versions/delete markers, latest flags, key/version markers, prefix, delimiter, max-keys and URL encoding. Full history scan; removed version markers skip remaining entries for that key. AWS SDK; Ceph. |
| [ListObjects](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjects.html) | Partial | Partial | Partial | V1 prefix/delimiter/marker pagination and URL encoding. Ceph has a remaining newline-prefix encoding assertion failure. AWS SDK; Ceph; Docker client. |
| [ListObjectsV2](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectsV2.html) | Partial | Partial | Partial | V2 continuation/start-after pagination and empty-token echo. Optional newer result fields are incomplete. AWS SDK; Ceph; Docker client. |
| [ListParts](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListParts.html) | Implemented | Implemented | Implemented | Pagination, ETags, sizes, checksum algorithm/type and per-part checksums. AWS SDK; Docker client. |
| [PutBucketAbac](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketAbac.html) | Partial | Partial | Partial | Bucket ABAC status; gateway JWT role/tag policies; no full IAM or bucket policy language. SDK integration tests. |
| [PutBucketAccelerateConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketAccelerateConfiguration.html) | Unsupported | Unsupported | Unsupported | Transfer Acceleration is not implemented. |
| [PutBucketAcl](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketAcl.html) | Partial | Partial | Partial | Private canned ACL or XML/header grants to the configured owner. Public groups and other identities rejected. Azure container metadata / disk bucket settings. AWS SDK; Ceph. |
| [PutBucketAnalyticsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketAnalyticsConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [PutBucketCors](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketCors.html) | Implemented | Implemented | Implemented | Persisted CORS rules, origin/method/header matching, unsigned preflight and authenticated actual requests. CORS never grants object access. AWS SDK; Ceph configuration case. |
| [PutBucketEncryption](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketEncryption.html) | Unsupported | Unsupported | Unsupported | S3 SSE configuration/rewrapping is not implemented; native Azure encryption is not this API. |
| [PutBucketIntelligentTieringConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketIntelligentTieringConfiguration.html) | Unsupported | Unsupported | Unsupported | Storage-class tiering and archive restoration are not implemented. |
| [PutBucketInventoryConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketInventoryConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [PutBucketLifecycle](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketLifecycle.html) | Partial | Partial | Partial | Persistent validated rules; current/noncurrent expiration, retained-version counts, expired delete markers and abandoned multipart cleanup. Prefix/tag/size/And filters; UTC dates and expiration headers. Storage-class transitions rejected. AWS SDK; Ceph; recovery tests. |
| [PutBucketLifecycleConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketLifecycleConfiguration.html) | Partial | Partial | Partial | Persistent validated rules; current/noncurrent expiration, retained-version counts, expired delete markers and abandoned multipart cleanup. Prefix/tag/size/And filters; UTC dates and expiration headers. Storage-class transitions rejected. AWS SDK; Ceph; recovery tests. |
| [PutBucketLogging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketLogging.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [PutBucketMetricsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketMetricsConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [PutBucketNotification](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketNotification.html) | Partial | Partial | Partial | SNS topic create/remove notifications, filters, destination test and persistent retry queue. Other destinations/events unsupported. SDK and live local SNS tests. |
| [PutBucketNotificationConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketNotificationConfiguration.html) | Partial | Partial | Partial | SNS topic create/remove notifications, filters, destination test and persistent retry queue. Other destinations/events unsupported. SDK and live local SNS tests. |
| [PutBucketOwnershipControls](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketOwnershipControls.html) | Partial | Partial | Partial | Persists all three ownership modes for one principal. BucketOwnerEnforced disables ACL writes and accepts uploads without ACLs or with private/bucket-owner-full-control. No cross-account ownership. AWS SDK. |
| [PutBucketPolicy](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketPolicy.html) | Unsupported | Unsupported | Unsupported | Bucket policies, ABAC and cross-user authorization are not implemented. |
| [PutBucketReplication](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketReplication.html) | Unsupported | Unsupported | Unsupported | S3 replication configuration is not implemented. |
| [PutBucketRequestPayment](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketRequestPayment.html) | Unsupported | Unsupported | Unsupported | Requester Pays is not implemented. |
| [PutBucketTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketTagging.html) | Implemented | Implemented | Implemented | Up to 50 validated bucket tags, separate from object tags; persisted across restart. AWS SDK; Ceph. |
| [PutBucketVersioning](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketVersioning.html) | Partial | Partial | Partial | Enable/suspend per bucket; native objects start as null versions. MFA Delete unsupported. AWS SDK; Ceph. |
| [PutBucketWebsite](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketWebsite.html) | Unsupported | Unsupported | Unsupported | Static website hosting is not implemented. |
| [PutObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html) | Partial | Partial | Partial | Atomic puts, optional new version, private ACL, metadata, conditions and five checksum algorithms. Suspended writes replace null version and omit new-version response header. Upload tags supported; no append or SSE. AWS SDK; Ceph; Docker client. |
| [PutObjectAcl](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectAcl.html) | Partial | Partial | Partial | Private canned ACL or owner-only XML/header grants; supports versionId and preserves bytes, ETag and modification time. AWS SDK; Ceph. |
| [PutObjectAnnotation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectAnnotation.html) | Unsupported | Unsupported | Unsupported | Object annotation resources are not implemented. |
| [PutObjectLegalHold](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectLegalHold.html) | Unsupported | Unsupported | Unsupported | Object lock, retention and legal holds are not implemented. |
| [PutObjectLockConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectLockConfiguration.html) | Unsupported | Unsupported | Unsupported | Object lock, retention and legal holds are not implemented. |
| [PutObjectRetention](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectRetention.html) | Unsupported | Unsupported | Unsupported | Object lock, retention and legal holds are not implemented. |
| [PutObjectTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectTagging.html) | Implemented | Implemented | Implemented | Object/version tags; up to ten validated unique tags, preserving bytes, ETag and mtime. Private metadata helpers on disk, Azure and S3. AWS SDK; Ceph. |
| [PutPublicAccessBlock](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutPublicAccessBlock.html) | Partial | Partial | Partial | Persists all four flags; BlockPublicAcls rejects public canned ACLs. Gateway remains private regardless of flag values; bucket policies and public grants stay unsupported. AWS SDK; Ceph configuration cases. |
| [RenameObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_RenameObject.html) | Unsupported | Unsupported | Unsupported | S3 Express / directory buckets are not implemented. |
| [RestoreObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_RestoreObject.html) | Unsupported | Unsupported | Unsupported | Storage-class tiering and archive restoration are not implemented. |
| [SelectObjectContent](https://docs.aws.amazon.com/AmazonS3/latest/API/API_SelectObjectContent.html) | Unsupported | Unsupported | Unsupported | S3 Select is not implemented. |
| [UpdateBucketMetadataAnnotationTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateBucketMetadataAnnotationTableConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [UpdateBucketMetadataInventoryTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateBucketMetadataInventoryTableConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [UpdateBucketMetadataJournalTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateBucketMetadataJournalTableConfiguration.html) | Unsupported | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [UpdateObjectEncryption](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateObjectEncryption.html) | Unsupported | Unsupported | Unsupported | S3 SSE configuration/rewrapping is not implemented; native Azure encryption is not this API. |
| [UploadPart](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UploadPart.html) | Implemented | Implemented | Implemented | Receive and verify bodies before locking; concurrent atomic part commits, overlapping retries, abort/completion state recheck. Up to 5 GiB each. AWS SDK; Ceph; Docker client. |
| [UploadPartCopy](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UploadPartCopy.html) | Implemented | Implemented | Implemented | Source versions, strict copy-range validation, conditions, staged concurrent commits and checksum XML responses. AWS SDK; Ceph; Docker client. |
| [WriteGetObjectResponse](https://docs.aws.amazon.com/AmazonS3/latest/API/API_WriteGetObjectResponse.html) | Unsupported | Unsupported | Unsupported | S3 Object Lambda is not implemented. |

## Request and client features

| Feature | State |
| --- | --- |
| SigV4 headers and presigned GET/PUT/HEAD/UploadPart | Implemented; escaped paths, version IDs, conditions, ranges, query metadata/checksums, expiry and tampering checks; AWS SDK and Docker client tests |
| Signed aws-chunked bodies; signed/unsigned checksum trailers | Implemented; published AWS vectors and Docker client uploads |
| Path-style / virtual-host addressing | Implemented; presigned tests cover both; broader tests primarily use path-style |
| Content-MD5 and payload SHA256 | Validated before publishing an object |
| Flexible checksums | CRC32, CRC32C, CRC64NVME, SHA1, SHA256; multipart composite/full-object rules depend on algorithm |
| Additional checksum algorithms | SHA512, flexible MD5, XXHASH64/3/128 are not implemented; Content-MD5 is a separate supported header |
| Multipart checksum responses | Copy-part, list-parts and completion XML; GET/HEAD headers; persisted on both backends |
| File upload/download and seek | SDK conveniences built on Put/Get/Head/Range; no distinct AWS operation |
| Unknown-size upload / ComposeObject | SDK conveniences built on multipart upload/copy; Docker client coverage |
| Browser form POST / presigned POST policy | Unsupported; can return authentication errors before operation routing |
| Temporary session credentials | STS AssumeRoleWithWebIdentity with configured JWT issuer/JWKS/audience/roles; encrypted expiring sessions, SigV4 and presigned URLs; AWS SDK integration tests |
| Bucket ABAC and resource tags | Get/PutBucketAbac plus S3 Control TagResource/UntagResource/ListTagsForResource; restricted policy subset; operator-only bucket administration |
| Anonymous access, SigV2, SigV4a | Unsupported |
| Expected bucket owner, Requester Pays, append offset | Explicitly rejected; these semantics are not silently applied as ordinary writes |
| MinIO extensions (QoS, listening notifications, listing metadata, Snowball extraction) | Unsupported; recognized extension requests are rejected |
| Access points, Outposts, S3 Express, Object Lambda | Unsupported |

## Versioning and private ACL scope

Versioning uses the shared [managed backend layer](backend/managed/store.go). Every version keeps immutable content and metadata; the latest non-deleted version is also published at the original native path. Current files/blobs remain directly readable. Private history, indexes and publication journals occupy each bucket's `.gateway/versions/` namespace on disk, S3 and Azure. Bucket settings use `.gateway/settings`; object metadata uses `.gateway/metadata/<id>`, referenced by a small native `gateway` field on cloud objects or by file identity on disk. Azure account versioning and public access policies are not changed.

Enabled writes create unique versions; suspended writes replace `null`. Deletes create markers, and deleting a specific latest version restores its predecessor at the native path. Bucket deletion rejects remaining versions/markers. Private bucket/object ACL APIs accept only the configured owner's grants, including version-specific ACL updates. The bucket-owner-full-control canned ACL maps to that same single owner. Anonymous/public/cross-user access remains disabled.

Versioned buckets require one gateway writer and S3-mediated mutations. Direct native changes do not participate in history. AWS/RustFS and Azure with account versioning retain historical bytes in native versions. Disk and Azure without native versioning use immutable helper copies. Per-key version indexes and journals are replayable after interrupted publication; fault-injection tests cover write/delete recovery. They do not provide distributed transactions or native-reader atomicity across history and current-path publication. See [operating limits](docs/compatibility.md).

## Measured compatibility results

The raw HTTP [conformance suite](tests/conformance/README.md) was validated against Amazon S3 in `eu-west-1` on 2026-10-04: **19 scenarios, 396 steps and 801 expectations**, with no divergences. The run used an existing versioned bucket, so bucket creation, new-bucket defaults, bucket tagging, ListBuckets and versioning transitions remain outside that AWS measurement. Disk, RustFS and Azurite pass the expanded scenarios with documented feature/provider differences; the disk comparison against the AWS recording has no unclassified differences. This is measured coverage, not full S3 parity.

<!-- compatibility-results:start -->
Snapshot generated **2026-10-04**. Dates of individual runs and failure details are retained in the [result snapshot](tests/compatibility-results.json).

| Suite / profile | Backend | Selected | Executed | Passed | Failed | Errors | Skipped | Deselected | Result |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| Ceph core | Disk | 9 | 9 | 9 | 0 | 0 | 0 | 0 | Passed selection |
| Ceph core | Azure / Azurite | 9 | 9 | 9 | 0 | 0 | 0 | 0 | Passed selection |
| Ceph versioning | Disk | 28 | 28 | 28 | 0 | 0 | 0 | 0 | Passed selection |
| Ceph versioning | Azure / Azurite | 28 | 28 | 28 | 0 | 0 | 0 | 0 | Passed selection |
| Ceph acl | Disk | 4 | 4 | 4 | 0 | 0 | 0 | 0 | Passed selection |
| Ceph acl | Azure / Azurite | 4 | 4 | 4 | 0 | 0 | 0 | 0 | Passed selection |
| Ceph full | Disk | 793 | 793 | 330 | 369 | 0 | 94 | 45 | Failed selection |
| Ceph full | Azure / Azurite | 793 | 793 | 334 | 365 | 0 | 94 | 45 | Failed selection |
| Ceph rustfs | AWS adapter / RustFS | 538 | 538 | 289 | 249 | 0 | 0 | 300 | Failed selection |
| minio-go Docker | Disk | 5 | 5 | 5 | 0 | 0 | 0 | 0 | Passed selection |
| minio-go Docker | Azure / Azurite | 5 | 5 | 5 | 0 | 0 | 0 | 0 | Passed selection |
| minio-go Docker | AWS adapter / RustFS | 5 | 5 | 5 | 0 | 0 | 0 | 0 | Passed selection |
| Iceberg Docker | Disk | 3 | 3 | 3 | 0 | 0 | 0 | 0 | Passed selection |
| Iceberg Docker | Azure / Azurite | 3 | 3 | 3 | 0 | 0 | 0 | 0 | Passed selection |
| Protocol SDK | Disk | 25 | 25 | 25 | 0 | 0 | 0 | 0 | Passed selection |
| Protocol SDK | Azure / Azurite | 30 | 30 | 29 | 0 | 0 | 1 | 0 | Passed selection |
| Protocol SDK | AWS adapter / RustFS | 30 | 30 | 30 | 0 | 0 | 0 | 0 | Passed selection |
| Provider contract | AWS adapter / RustFS | 7 | 7 | 7 | 0 | 0 | 0 | 0 | Passed selection |
| Polaris + Iceberg Docker | Disk | 1 | 1 | 1 | 0 | 0 | 0 | 0 | Passed selection |
| Polaris + Iceberg Docker | Azure / Azurite | 1 | 1 | 1 | 0 | 0 | 0 | 0 | Passed selection |

**Both broader Ceph selections completed.** Every selected test has a recorded outcome; failures and skips are retained. Deselected cases are excluded from pass counts, with names and reasons in the result snapshot. This covers the configured S3 functional test file and profile filters, not every suite in the upstream repository.
<!-- compatibility-results:end -->

Ceph tests are pinned to [`5522d1c351f75bc00ae0f64f742f3f095f5939d9`](https://github.com/ceph/s3-tests/tree/5522d1c351f75bc00ae0f64f742f3f095f5939d9). The core profile selects nine listing tests; versioning selects 28 version/copy/delete scenarios; private ACL selects four default/private/mtime scenarios. The broader profile collects `s3tests/functional/test_s3.py` and deselects 40 browser form POST cases plus five explicit SigV2 scenarios (including one mixed logging-authentication test). The default is 793 selected and 45 deselected. ListObjectsV2, SigV4 presigning, and ordinary CORS tests remain enabled. Browser POST is outside the chosen scope, although SigV4 browser POST is still an S3 feature. `task ceph-unfiltered` restores all 838 cases and writes separate `*-unfiltered` artifacts. Both recorded audits completed all 793 selected cases with `--maxfail=0 --timeout=30`: no failure-count cutoff, with a 30-second per-test timeout. This is the full configured functional selection, not every test file in the upstream repository. No tests are converted to expected failures. Disposable Docker gateways use a one-second test lifecycle day and 250 ms scan interval, with upstream `lc_debug_interval=1`; production uses real UTC days and a one-minute scan by default. Test assertions are unchanged.

A [fixture adapter](tests/ceph/gateway_fixtures.py) adapts setup/cleanup and applies the documented collection filter: main-user cleanup deletes all versions and delete markers through ListObjectVersions/DeleteObjects; upstream alternate-account setup/cleanup is bypassed. Assertions are unchanged, and alternate identities remain distinct. This is an adapted conformance run, not an untouched upstream deployment. Ceph also contains RGW-specific assertions, which do not automatically represent AWS S3 requirements.

The broader audit encounters unsupported policies, public/cross-user ACLs, encryption, object lock, and logging, alongside protocol discrepancies and RGW-specific extensions. Disk additionally fails five delimiter cases involving keys that cannot be represented as native files; Azure additionally rejects a bucket name containing a period because of native container naming restrictions. Failures are retained in the results.

The scope change excludes **38 formerly failing and seven formerly passing cases per backend**, recorded as 45 deselections. These exclusions are not implementation gains. The current audit introduces one additional failure per backend: `test_object_raw_put_authenticated_expired` supplies `ExpiresIn=-1000` and expects 403, while the gateway rejects an invalid expiry with 400 `AuthorizationQueryParametersError`. Valid but expired URLs still return 403. No retained cases became new Ceph passes. Both runs retain 94 upstream skips, separate from passes. New recovery behavior is covered by 40 fault-injection scenarios in each protocol SDK run, plus failed-precondition and helper-collection checks. The result snapshot records durations and every excluded test name/reason; fixture runtimes are not throughput benchmarks.

The [snapshot](tests/compatibility-results.json) retains every failing test name and assertion/error message. Raw tracebacks are in `results/ceph-*-full.xml`.

The minio-go client is pinned to **v7.3.0** in its own [test module](tests/minio/go.mod) and runs **only in Docker**, against disk and Azurite. The five scenarios cover ordinary object operations, checksummed known/unknown-size multipart uploads, incomplete uploads and composition, presigning, and copied-part checksum responses. They are our interoperability checks using the SDK, not the entire upstream MinIO test suite.

Azurite results verify the Azure adapter against an emulator. A separate expanded live Azure test passed (see coverage below), but the Ceph and Docker client results here do not certify real Azure account behavior. The three Apache iceberg-go v0.6.0 container scenarios cover FileIO write/seek/range/delete, SQL catalog metadata creation/reload, and real Parquet append/scan across two committed snapshots. The separate [Polaris fixture](docs/polaris.md) exercises a real REST catalog, concurrent commits with explicit conflict retry, Java metadata FileIO, and Go Parquet scans.

## Reproduce and refresh

```sh
task test                 # local AWS SDK tests, backend tests, race detector
task vet
task ceph-core            # selected upstream listing tests, disk then Azurite
task ceph-versioning      # 28 selected versioning/copy/delete cases
task ceph-acl             # four private ACL cases
task test-protocol        # AWS SDK protocol, bucket configuration and lifecycle scenarios
task test-minio           # isolated client SDK tests, disk then Azurite
task iceberg              # FileIO, SQL catalog and real Parquet tests
task test-polaris         # REST catalog, concurrent commits and scans
task ceph-full -- --maxfail=0 --timeout=30 # filtered scope; no failure cutoff
task ceph-unfiltered -- --maxfail=0 --timeout=30 # include browser POST and SigV2
task compatibility-report # refresh this results section from local reports
task clean-containers
```

`task ceph-full` runs without the audit failure limit by default. Extra pytest options can be passed after `--`. Fixtures use disposable buckets and local test credentials. They do not mount `.env` or use the real Azure account. Raw JUnit XML, Python package versions, source revision, collection counts, and client JSON logs remain under ignored `results/`; the compact [result snapshot](tests/compatibility-results.json) is suitable for version control.

## Implemented from the full audit

- Range responses omit full-object checksums; part reads return the saved part checksum with the object's checksum type. Bodyless conditional responses omit payload checksum headers.
- Incoming multipart bodies are staged and verified before upload locking. Parts can commit concurrently; completion and abort exclude commits and recheck upload state. Protocol tests cover an overlapping same-part retry and an abort while a request is still sending.
- Conditional writes, including multipart completion, return `NoSuchKey` for an absent current object or a current delete marker with `If-Match`. Existing-object ETag mismatches remain `PreconditionFailed`.
- GetObjectAttributes supports attribute selection, checksums, object size/ETag/storage class, version IDs and completed-part pagination. GET/HEAD support `partNumber` for saved parts. Part metadata survives restart, completion cleanup, and subsequent versions.
- Object tagging supports validated put/get/delete, upload tags, copy directives and explicit versions. Tags preserve object identity. All three backends use `.gateway/metadata/<id>` helpers; cloud objects keep a small reference in native metadata, while disk derives the helper location from file identity.
- Iceberg coverage now includes real Parquet append, two SQL catalog snapshot commits, catalog reload, historical snapshot reads and filtered scans.

The remaining `test_multipart_resend_first_finishes_last` failure is duplicate part-number validation during completion, rather than a timeout: the upstream case submits two entries for part 1. The gateway continues to reject duplicate completion entries. The protocol regression completes an overlapping retry with a valid unique-part list.

Conditional-delete tests that expect success on an absent object or current delete marker differ from the implemented general-purpose S3 existence checks. [AWS documents a failed existence precondition for a current delete marker and Not Found for absent objects](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-deletes.html). Date/size conditional deletes remain unsupported directory-bucket features. These Ceph outcomes remain failures, with assertions unchanged.

The protocol SDK profile now contains **25 scenarios per backend**, including bucket configuration persistence/overflow, ownership enforcement, signed CORS, ListBuckets pagination, copy checksum persistence, lifecycle current/noncurrent expiration, suspended null versions, multipart cleanup, read-only behavior and delete-marker age. Local fault-injection and version-age tests cover lifecycle recovery and rechecking concurrent changes.

## Bucket configuration and lifecycle implementation

- ListBuckets supports bounded pages, continuation tokens, prefix and region filters. Content-Encoding keeps the client formatting after removing aws-chunked. CopyObject validates no-op self-copies and recalculates selected/saved supported checksums.
- Unicode metadata is decoded on input and returned using [AWS-documented RFC 2047 encoding](https://docs.aws.amazon.com/AmazonS3/latest/userguide/UsingMetadata.html). Ceph's raw non-ASCII metadata expectation still fails; the SDK regression verifies decoded content. The V1 newline-prefix expectation also remains different from the [URL-encoded response described by AWS SDK documentation](https://docs.aws.amazon.com/AWSJavaSDK/latest/javadoc/com/amazonaws/services/s3/model/ListObjectsV2Result.html).
- Bucket tags, CORS, public-access-block flags and ownership controls persist on all three backends. New gateway buckets default to BucketOwnerEnforced and enable all four PublicAccessBlock flags. BucketOwnerEnforced disables explicit ACL writes; existing native buckets without saved ownership controls retain the private ACL model. PublicAccessBlock configuration cannot enable public access; policy-related flags do not add bucket-policy support.
- Lifecycle execution handles current-object expiration, versioned delete markers, suspended null versions, noncurrent expiration/retention and abandoned multipart uploads. Filtering supports prefix, tags, exclusive size bounds and And. Rule validation rejects unknown actions and unsupported storage-class transitions. Version ages survive intermediate-version deletion and restart; expiration uses publication journals.
- Remaining Ceph lifecycle cases include filters combining multiple top-level selectors instead of And. [AWS permits one top-level selector](https://docs.aws.amazon.com/AmazonS3/latest/API/API_LifecycleRuleFilter.html); those requests stay rejected. The delete-marker expiration test also expects an explicit false header after removal, whereas the gateway omits that header when no delete marker exists. These failures remain in the report.

## Presigned requests and explicit rejection

[Presigning regressions](gateway/presign_test.go) use the AWS SDK signer and an actual HTTP server in addition to the in-process transport. They cover special characters and encoded slashes, signed header/query integrity (including unsigned If-Range rejection when Range is signed), metadata, checksum validation before publication, range/response overrides, GET/PUT/HEAD/UploadPart, explicit versions, conditional writes, virtual-host addressing, expiry limits and requests after handler restart. These are SigV4 URLs with the configured static principal, not STS credentials or browser POST policies. Signed options hoisted into the query are promoted only after signature verification; conflicting header/query values are rejected. Canonicalization preserves the escaped wire path and does not mutate metadata values.

CreateBucket Object Lock flags, governance-retention bypass, and copy-source encryption headers now fail explicitly. Query-based requests also reach these validators. The gateway does not report successful encryption or retention protection that it has not implemented. See [AWS's presigned query authentication rules](https://docs.aws.amazon.com/AmazonS3/latest/developerguide/sigv4-query-string-auth.html).

The live Azure test was expanded to check presigned checksum/metadata/range behavior, historical versions, conditional PUT, multipart uploads and reads through a fresh handler. It passed on 2026-10-04 using one unique container, including cleanup. This is selected live-account coverage; the broad Ceph results still use Azurite.

## Residual Ceph protocol audit

The residual review compares assertions with public AWS documentation and upstream test code; it is not a differential run against an AWS account. No additional Ceph cases were excluded or turned into expected failures.

| Cases | Assessment / action |
| --- | --- |
| Encryption, policy/public-ACL/other-user, logging, Object Lock families | Mostly unsupported feature scope. Keep failures visible and reject requested protection explicitly. |
| Six lifecycle mixed-filter tests | Multiple top-level selectors are invalid under [AWS LifecycleRuleFilter](https://docs.aws.amazon.com/AmazonS3/latest/API/API_LifecycleRuleFilter.html); callers must use And. Keep strict validation. |
| `test_bucket_get_location` | AWS returns null for [us-east-1](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLocation.html); Ceph expects the region string. |
| Unicode metadata and newline-prefix cases | Encoding expectations differ; see the metadata/listing notes above. Retain failures rather than alter encoding solely for these assertions. |
| `test_object_raw_put_authenticated_expired` | Uses a negative expiry, outside AWS's documented 1–604800 seconds. The gateway returns 400 for invalid authorization query parameters; Ceph expects 403. Ordinary expired URLs return 403. |
| Fourteen conditional-delete cases | Twelve require unsupported directory-bucket date/size conditions; two differ on missing current objects/delete markers. See the conditional-delete notes above. |
| `test_multipart_resend_first_finishes_last` | Sends duplicate completion entries for part 1. Unique-part overlapping retries pass our protocol test; duplicate entries remain rejected. |
| `test_delete_marker_nonversioned`, `test_delete_marker_expiration` | Ceph requires an explicit false delete-marker header for absent objects. Gateway omits it unless a marker exists. Further live AWS comparison would resolve this discrepancy. |
| `test_object_read_unreadable` | Ceph expects URI parse failure for this Unicode key; gateway returns missing-key status. Keep visible pending live AWS comparison. |
| `test_get_object_torrent`, RGW usage/statistics/unordered cases | Unsupported operation or RGW-specific behavior. |
| Five disk delimiter cases; one Azure dotted-bucket case | Native filesystem key representation and Azure container-name restrictions. Documented transparency tradeoffs. |

## Next priorities

1. Backend listing pagination and bounded memory/temp-space admission under concurrent large uploads.
2. Broader live Azure recovery/failure testing and repeatable real-AWS comparisons for the remaining ambiguous protocol cases.
3. Additional engine integration (Spark/Trino) through Polaris. STS or scoped identities would be a separate expansion beyond the chosen single-user scope.
