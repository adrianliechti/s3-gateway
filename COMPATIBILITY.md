# S3 compatibility

Reviewed on **2026-10-03** against the [official AWS S3 operation inventory](https://docs.aws.amazon.com/AmazonS3/latest/API/API_Operations_Amazon_Simple_Storage_Service.html): **116 operations**, including legacy aliases. This chart covers the S3 API; S3 Control, S3 Tables, IAM and STS are separate services and are outside this inventory. A method in a client SDK does not establish server support.

The gateway has one static principal, one region and optional per-bucket versioning. **Implemented** means the ordinary operation works within those constraints; **Partial** identifies additional known gaps; **Unsupported** means there is no implementation. These are implementation states, not certification. Evidence below distinguishes actual tests from a code review.

Both backends share the protocol implementation. Disk preserves native files and cannot represent every S3 key; Azure preserves blob paths but inherits container naming/metadata constraints. See [operating limits](docs/compatibility.md) for concurrency, durability, scale and native-writer restrictions. Those limits apply to every row.

## AWS S3 operations chart

“Docker client” refers to the isolated minio-go test container. The SDK is not a gateway dependency. “Ceph” indicates relevant scenarios in the measured selections below, not that every test for an operation passes. Detailed assertions live in [AWS SDK tests](gateway/gateway_test.go), [versioning/private ACL tests](gateway/versioning_test.go) and [Docker client tests](tests/minio/compatibility_test.go).

| AWS S3 operation | Disk | Azure Blob | Behavior, limits and evidence |
| --- | --- | --- | --- |
| [AbortMultipartUpload](https://docs.aws.amazon.com/AmazonS3/latest/API/API_AbortMultipartUpload.html) | Implemented | Implemented | Removes uploaded parts; committed objects remain intact. AWS SDK; Docker client. |
| [CompleteMultipartUpload](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CompleteMultipartUpload.html) | Partial | Partial | Ordered parts, minimum part size, conditions, checksums and retry record; one destination version on normal completion/retry. Crash between version publication and saving completion record can duplicate a version. AWS SDK; Ceph; Docker client. | 
| [CopyObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CopyObject.html) | Partial | Partial | Streaming copy, metadata directives and source version selection. New destination version when enabled. Tagging COPY/REPLACE directives; no checksum-algorithm selection. AWS SDK; Ceph; Docker client. | 
| [CreateBucket](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucket.html) | Partial | Partial | One owner and region. us-east-1 recreation succeeds and resets private ACL while preserving versioning/history; other regions return BucketAlreadyOwnedByYou. Native naming restrictions apply. AWS SDK; Ceph; Docker client. |
| [CreateBucketMetadataConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucketMetadataConfiguration.html) | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [CreateBucketMetadataTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucketMetadataTableConfiguration.html) | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [CreateMultipartUpload](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateMultipartUpload.html) | Implemented | Implemented | Persistent upload state, content metadata and supported checksum algorithms. AWS SDK; Docker client. |
| [CreateSession](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateSession.html) | Unsupported | Unsupported | S3 Express / directory buckets are not implemented. |
| [DeleteBucket](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucket.html) | Partial | Partial | Empty buckets only. Azure emptiness check is not atomic with external writers. AWS SDK; Ceph; Docker client. |
| [DeleteBucketAnalyticsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketAnalyticsConfiguration.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [DeleteBucketCors](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketCors.html) | Unsupported | Unsupported | CORS configuration is not implemented. |
| [DeleteBucketEncryption](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketEncryption.html) | Unsupported | Unsupported | S3 SSE configuration/rewrapping is not implemented; native Azure encryption is not this API. |
| [DeleteBucketIntelligentTieringConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketIntelligentTieringConfiguration.html) | Unsupported | Unsupported | Lifecycle, tiering and archive restoration are not implemented. |
| [DeleteBucketInventoryConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketInventoryConfiguration.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [DeleteBucketLifecycle](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketLifecycle.html) | Unsupported | Unsupported | Lifecycle, tiering and archive restoration are not implemented. |
| [DeleteBucketMetadataConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketMetadataConfiguration.html) | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [DeleteBucketMetadataTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketMetadataTableConfiguration.html) | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [DeleteBucketMetricsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketMetricsConfiguration.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [DeleteBucketOwnershipControls](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketOwnershipControls.html) | Unsupported | Unsupported | Multi-user authorization, ACLs and account-level ownership controls are not implemented. |
| [DeleteBucketPolicy](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketPolicy.html) | Unsupported | Unsupported | Multi-user authorization, ACLs and account-level ownership controls are not implemented. |
| [DeleteBucketReplication](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketReplication.html) | Unsupported | Unsupported | S3 replication configuration is not implemented. |
| [DeleteBucketTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketTagging.html) | Unsupported | Unsupported | Bucket tags are not implemented. |
| [DeleteBucketWebsite](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketWebsite.html) | Unsupported | Unsupported | Static website hosting is not implemented. |
| [DeleteObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObject.html) | Partial | Partial | Delete markers when enabled/suspended, permanent version deletion, ETag conditions and latest-version restoration. Size/date delete conditions unsupported. AWS SDK; Ceph. | 
| [DeleteObjectAnnotation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjectAnnotation.html) | Unsupported | Unsupported | Object annotation resources are not implemented. |
| [DeleteObjectTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjectTagging.html) | Implemented | Implemented | Object/version tags; up to ten validated unique tags, preserving bytes, ETag and mtime. Disk private metadata; Azure metadata with overflow manifests. AWS SDK; Ceph. |
| [DeleteObjects](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjects.html) | Partial | Partial | Up to 1,000 entries; version IDs, marker responses, quiet mode and per-entry ETag conditions/errors. Size/date conditions unsupported. AWS SDK; Ceph. | 
| [DeletePublicAccessBlock](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeletePublicAccessBlock.html) | Unsupported | Unsupported | Multi-user authorization, ACLs and account-level ownership controls are not implemented. |
| [GetBucketAbac](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketAbac.html) | Unsupported | Unsupported | Multi-user authorization, ACLs and account-level ownership controls are not implemented. |
| [GetBucketAccelerateConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketAccelerateConfiguration.html) | Unsupported | Unsupported | Transfer Acceleration is not implemented. |
| [GetBucketAcl](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketAcl.html) | Partial | Partial | Returns persistent private owner ACL; default FULL_CONTROL for the single configured principal. AWS SDK; Ceph private ACL selection. | 
| [GetBucketAnalyticsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketAnalyticsConfiguration.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [GetBucketCors](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketCors.html) | Unsupported | Unsupported | CORS configuration is not implemented. |
| [GetBucketEncryption](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketEncryption.html) | Unsupported | Unsupported | S3 SSE configuration/rewrapping is not implemented; native Azure encryption is not this API. |
| [GetBucketIntelligentTieringConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketIntelligentTieringConfiguration.html) | Unsupported | Unsupported | Lifecycle, tiering and archive restoration are not implemented. |
| [GetBucketInventoryConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketInventoryConfiguration.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [GetBucketLifecycle](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLifecycle.html) | Unsupported | Unsupported | Lifecycle, tiering and archive restoration are not implemented. |
| [GetBucketLifecycleConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLifecycleConfiguration.html) | Unsupported | Unsupported | Lifecycle, tiering and archive restoration are not implemented. |
| [GetBucketLocation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLocation.html) | Implemented | Implemented | Configured region; empty value for us-east-1. Docker client region discovery. |
| [GetBucketLogging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLogging.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [GetBucketMetadataConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketMetadataConfiguration.html) | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [GetBucketMetadataTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketMetadataTableConfiguration.html) | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [GetBucketMetricsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketMetricsConfiguration.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [GetBucketNotification](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketNotification.html) | Unsupported | Unsupported | Bucket event notifications are not implemented. |
| [GetBucketNotificationConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketNotificationConfiguration.html) | Unsupported | Unsupported | Bucket event notifications are not implemented. |
| [GetBucketOwnershipControls](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketOwnershipControls.html) | Unsupported | Unsupported | Multi-user authorization, ACLs and account-level ownership controls are not implemented. |
| [GetBucketPolicy](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketPolicy.html) | Unsupported | Unsupported | Multi-user authorization, ACLs and account-level ownership controls are not implemented. |
| [GetBucketPolicyStatus](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketPolicyStatus.html) | Unsupported | Unsupported | Multi-user authorization, ACLs and account-level ownership controls are not implemented. |
| [GetBucketReplication](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketReplication.html) | Unsupported | Unsupported | S3 replication configuration is not implemented. |
| [GetBucketRequestPayment](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketRequestPayment.html) | Unsupported | Unsupported | Requester Pays is not implemented. |
| [GetBucketTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketTagging.html) | Unsupported | Unsupported | Bucket tags are not implemented. |
| [GetBucketVersioning](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketVersioning.html) | Implemented | Implemented | Returns unset, Enabled or Suspended status from durable bucket settings. AWS SDK; Ceph. | 
| [GetBucketWebsite](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketWebsite.html) | Unsupported | Unsupported | Static website hosting is not implemented. |
| [GetObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html) | Partial | Partial | Latest or selected version, delete-marker headers, byte ranges and persisted part-number reads, conditions, response overrides and checksums scoped to full/part responses. Legacy multipart objects without saved boundaries cannot use partNumber. No SSE request semantics. AWS SDK; Ceph; Docker client. | 
| [GetObjectAcl](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectAcl.html) | Partial | Partial | Returns current or selected version ACL. Private owner grants only. AWS SDK; Ceph private ACL selection. | 
| [GetObjectAnnotation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectAnnotation.html) | Unsupported | Unsupported | Object annotation resources are not implemented. |
| [GetObjectAttributes](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectAttributes.html) | Partial | Partial | Selected attributes, size/ETag/storage class, checksums, version selection and paginated completed-part metadata. Legacy objects lack saved part boundaries; SSE unsupported. AWS SDK; Ceph. |
| [GetObjectLegalHold](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectLegalHold.html) | Unsupported | Unsupported | Object lock, retention and legal holds are not implemented. |
| [GetObjectLockConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectLockConfiguration.html) | Unsupported | Unsupported | Object lock, retention and legal holds are not implemented. |
| [GetObjectRetention](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectRetention.html) | Unsupported | Unsupported | Object lock, retention and legal holds are not implemented. |
| [GetObjectTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectTagging.html) | Implemented | Implemented | Object/version tags; up to ten validated unique tags, preserving bytes, ETag and mtime. Disk private metadata; Azure metadata with overflow manifests. AWS SDK; Ceph. |
| [GetObjectTorrent](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectTorrent.html) | Unsupported | Unsupported | Torrent retrieval is not implemented. |
| [GetPublicAccessBlock](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetPublicAccessBlock.html) | Unsupported | Unsupported | Multi-user authorization, ACLs and account-level ownership controls are not implemented. |
| [HeadBucket](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadBucket.html) | Implemented | Implemented | Existence check. AWS SDK; Docker client. |
| [HeadObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html) | Partial | Partial | Current/version-specific metadata, delete-marker headers, conditions, ranges, stored checksums and persisted part-number reads. Legacy multipart part boundaries are unavailable. AWS SDK; Ceph; Docker client. | 
| [ListBucketAnalyticsConfigurations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBucketAnalyticsConfigurations.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [ListBucketIntelligentTieringConfigurations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBucketIntelligentTieringConfigurations.html) | Unsupported | Unsupported | Lifecycle, tiering and archive restoration are not implemented. |
| [ListBucketInventoryConfigurations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBucketInventoryConfigurations.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [ListBucketMetricsConfigurations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBucketMetricsConfigurations.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [ListBuckets](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBuckets.html) | Partial | Partial | Lists visible buckets for the single principal; no AWS pagination/filter parameters. Native disk creation dates use directory mtime. AWS SDK; Ceph; Docker client. |
| [ListDirectoryBuckets](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListDirectoryBuckets.html) | Unsupported | Unsupported | S3 Express / directory buckets are not implemented. |
| [ListMultipartUploads](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListMultipartUploads.html) | Partial | Partial | Prefix/delimiter grouping, key/upload markers, URL encoding, initiation ordering. Listing rescans durable upload state; pagination when the marker upload has been removed uses an approximate ID ordering. AWS SDK; Docker client. |
| [ListObjectAnnotations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectAnnotations.html) | Unsupported | Unsupported | Object annotation resources are not implemented. |
| [ListObjectVersions](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectVersions.html) | Partial | Partial | Versions/delete markers, latest flags, key/version markers, prefix, delimiter, max-keys and URL encoding. Full history scan; removed version markers skip remaining entries for that key. AWS SDK; Ceph. | 
| [ListObjects](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjects.html) | Partial | Partial | V1 prefix/delimiter/marker pagination and URL encoding. Ceph has a remaining newline-prefix encoding assertion failure. AWS SDK; Ceph; Docker client. |
| [ListObjectsV2](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectsV2.html) | Partial | Partial | V2 continuation/start-after pagination and empty-token echo. Optional newer result fields are incomplete. AWS SDK; Ceph; Docker client. |
| [ListParts](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListParts.html) | Implemented | Implemented | Pagination, ETags, sizes, checksum algorithm/type and per-part checksums. AWS SDK; Docker client. |
| [PutBucketAbac](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketAbac.html) | Unsupported | Unsupported | Multi-user authorization, ACLs and account-level ownership controls are not implemented. |
| [PutBucketAccelerateConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketAccelerateConfiguration.html) | Unsupported | Unsupported | Transfer Acceleration is not implemented. |
| [PutBucketAcl](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketAcl.html) | Partial | Partial | Private canned ACL or XML/header grants to the configured owner. Public groups and other identities rejected. Azure container metadata / disk bucket settings. AWS SDK; Ceph. | 
| [PutBucketAnalyticsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketAnalyticsConfiguration.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [PutBucketCors](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketCors.html) | Unsupported | Unsupported | CORS configuration is not implemented. |
| [PutBucketEncryption](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketEncryption.html) | Unsupported | Unsupported | S3 SSE configuration/rewrapping is not implemented; native Azure encryption is not this API. |
| [PutBucketIntelligentTieringConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketIntelligentTieringConfiguration.html) | Unsupported | Unsupported | Lifecycle, tiering and archive restoration are not implemented. |
| [PutBucketInventoryConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketInventoryConfiguration.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [PutBucketLifecycle](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketLifecycle.html) | Unsupported | Unsupported | Lifecycle, tiering and archive restoration are not implemented. |
| [PutBucketLifecycleConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketLifecycleConfiguration.html) | Unsupported | Unsupported | Lifecycle, tiering and archive restoration are not implemented. |
| [PutBucketLogging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketLogging.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [PutBucketMetricsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketMetricsConfiguration.html) | Unsupported | Unsupported | AWS reporting/monitoring configuration is not implemented. |
| [PutBucketNotification](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketNotification.html) | Unsupported | Unsupported | Bucket event notifications are not implemented. |
| [PutBucketNotificationConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketNotificationConfiguration.html) | Unsupported | Unsupported | Bucket event notifications are not implemented. |
| [PutBucketOwnershipControls](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketOwnershipControls.html) | Unsupported | Unsupported | Multi-user authorization, ACLs and account-level ownership controls are not implemented. |
| [PutBucketPolicy](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketPolicy.html) | Unsupported | Unsupported | Multi-user authorization, ACLs and account-level ownership controls are not implemented. |
| [PutBucketReplication](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketReplication.html) | Unsupported | Unsupported | S3 replication configuration is not implemented. |
| [PutBucketRequestPayment](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketRequestPayment.html) | Unsupported | Unsupported | Requester Pays is not implemented. |
| [PutBucketTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketTagging.html) | Unsupported | Unsupported | Bucket tags are not implemented. |
| [PutBucketVersioning](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketVersioning.html) | Partial | Partial | Enable/suspend per bucket; native objects start as null versions. MFA Delete unsupported. AWS SDK; Ceph. | 
| [PutBucketWebsite](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketWebsite.html) | Unsupported | Unsupported | Static website hosting is not implemented. |
| [PutObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html) | Partial | Partial | Atomic puts, optional new version, private ACL, metadata, conditions and five checksum algorithms. Suspended writes replace null version and omit new-version response header. Upload tags supported; no append or SSE. AWS SDK; Ceph; Docker client. | 
| [PutObjectAcl](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectAcl.html) | Partial | Partial | Private canned ACL or owner-only XML/header grants; supports versionId and preserves bytes, ETag and modification time. AWS SDK; Ceph. | 
| [PutObjectAnnotation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectAnnotation.html) | Unsupported | Unsupported | Object annotation resources are not implemented. |
| [PutObjectLegalHold](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectLegalHold.html) | Unsupported | Unsupported | Object lock, retention and legal holds are not implemented. |
| [PutObjectLockConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectLockConfiguration.html) | Unsupported | Unsupported | Object lock, retention and legal holds are not implemented. |
| [PutObjectRetention](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectRetention.html) | Unsupported | Unsupported | Object lock, retention and legal holds are not implemented. |
| [PutObjectTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectTagging.html) | Implemented | Implemented | Object/version tags; up to ten validated unique tags, preserving bytes, ETag and mtime. Disk private metadata; Azure metadata with overflow manifests. AWS SDK; Ceph. |
| [PutPublicAccessBlock](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutPublicAccessBlock.html) | Unsupported | Unsupported | Multi-user authorization, ACLs and account-level ownership controls are not implemented. |
| [RenameObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_RenameObject.html) | Unsupported | Unsupported | S3 Express / directory buckets are not implemented. |
| [RestoreObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_RestoreObject.html) | Unsupported | Unsupported | Lifecycle, tiering and archive restoration are not implemented. |
| [SelectObjectContent](https://docs.aws.amazon.com/AmazonS3/latest/API/API_SelectObjectContent.html) | Unsupported | Unsupported | S3 Select is not implemented. |
| [UpdateBucketMetadataAnnotationTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateBucketMetadataAnnotationTableConfiguration.html) | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [UpdateBucketMetadataInventoryTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateBucketMetadataInventoryTableConfiguration.html) | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [UpdateBucketMetadataJournalTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateBucketMetadataJournalTableConfiguration.html) | Unsupported | Unsupported | AWS S3 Metadata table management is not implemented; distinct from x-amz-meta object headers. |
| [UpdateObjectEncryption](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateObjectEncryption.html) | Unsupported | Unsupported | S3 SSE configuration/rewrapping is not implemented; native Azure encryption is not this API. |
| [UploadPart](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UploadPart.html) | Implemented | Implemented | Receive and verify bodies before locking; concurrent atomic part commits, overlapping retries, abort/completion state recheck. Up to 5 GiB each. AWS SDK; Ceph; Docker client. |
| [UploadPartCopy](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UploadPartCopy.html) | Implemented | Implemented | Source versions, strict copy-range validation, conditions, staged concurrent commits and checksum XML responses. AWS SDK; Ceph; Docker client. |
| [WriteGetObjectResponse](https://docs.aws.amazon.com/AmazonS3/latest/API/API_WriteGetObjectResponse.html) | Unsupported | Unsupported | S3 Object Lambda is not implemented. |

## Request and client features

| Feature | State |
| --- | --- |
| SigV4 headers and presigned GET/PUT/HEAD | Implemented; AWS SDK and Docker client tests |
| Signed aws-chunked bodies; signed/unsigned checksum trailers | Implemented; published AWS vectors and Docker client uploads |
| Path-style / virtual-host addressing | Implemented; tests primarily cover path-style |
| Content-MD5 and payload SHA256 | Validated before publishing an object |
| Flexible checksums | CRC32, CRC32C, CRC64NVME, SHA1, SHA256; multipart composite/full-object rules depend on algorithm |
| Additional checksum algorithms | SHA512, flexible MD5, XXHASH64/3/128 are not implemented; Content-MD5 is a separate supported header |
| Multipart checksum responses | Copy-part, list-parts and completion XML; GET/HEAD headers; persisted on both backends |
| File upload/download and seek | SDK conveniences built on Put/Get/Head/Range; no distinct AWS operation |
| Unknown-size upload / ComposeObject | SDK conveniences built on multipart upload/copy; Docker client coverage |
| Browser form POST / presigned POST policy | Unsupported; can return authentication errors before operation routing |
| Anonymous access, SigV2, SigV4a, temporary session credentials | Unsupported |
| Expected bucket owner, Requester Pays, append offset | Explicitly rejected; these semantics are not silently applied as ordinary writes |
| MinIO extensions (QoS, listening notifications, listing metadata, Snowball extraction) | Unsupported; recognized extension requests are rejected |
| Access points, Outposts, S3 Express, Object Lambda | Unsupported |

## Versioning and private ACL scope

Versioning uses the shared [managed backend layer](backend/managed/store.go). Every version keeps immutable content and metadata; the latest non-deleted version is also published at the original native path. Current files/blobs remain directly readable. Private history, indexes and publication journals occupy `.system/versions/<bucket>/` on disk and `.s3gw/versions/` on Azure. Bucket status and ACLs use `.system/buckets/` or native Azure container metadata. Azure account versioning and public access policies are not changed.

Enabled writes create unique versions; suspended writes replace `null`. Deletes create markers, and deleting a specific latest version restores its predecessor at the native path. Bucket deletion rejects remaining versions/markers. Private bucket/object ACL APIs accept only the configured owner's grants, including version-specific ACL updates. Anonymous/public/cross-user access remains disabled.

Versioned buckets require one gateway writer and S3-mediated mutations. Direct native changes do not participate in history. A versioned object consumes space for its history plus another copy of the latest bytes. Per-key version indexes and journals are replayable after interrupted publication; fault-injection tests cover write/delete recovery. They do not provide distributed transactions or native-reader atomicity across history and current-path publication. See [operating limits](docs/compatibility.md).

## Measured compatibility results

<!-- compatibility-results:start -->
Snapshot generated **2026-10-03**. Dates of individual runs and failure details are retained in the [result snapshot](tests/compatibility-results.json).

| Suite / profile | Backend | Selected | Executed | Passed | Failed | Errors | Skipped | Result |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| Ceph core | Disk | 9 | 9 | 9 | 0 | 0 | 0 | Passed selection |
| Ceph core | Azure / Azurite | 9 | 9 | 9 | 0 | 0 | 0 | Passed selection |
| Ceph versioning | Disk | 28 | 28 | 28 | 0 | 0 | 0 | Passed selection |
| Ceph versioning | Azure / Azurite | 28 | 28 | 28 | 0 | 0 | 0 | Passed selection |
| Ceph acl | Disk | 4 | 4 | 4 | 0 | 0 | 0 | Passed selection |
| Ceph acl | Azure / Azurite | 4 | 4 | 4 | 0 | 0 | 0 | Passed selection |
| Ceph full | Disk | 838 | 838 | 254 | 490 | 0 | 94 | Failed selection |
| Ceph full | Azure / Azurite | 838 | 838 | 258 | 486 | 0 | 94 | Failed selection |
| minio-go Docker | Disk | 5 | 5 | 5 | 0 | 0 | 0 | Passed selection |
| minio-go Docker | Azure / Azurite | 5 | 5 | 5 | 0 | 0 | 0 | Passed selection |
| Iceberg Docker | Disk | 2 | 2 | 2 | 0 | 0 | 0 | Passed selection |
| Iceberg Docker | Azure / Azurite | 2 | 2 | 2 | 0 | 0 | 0 | Passed selection |
| Versioning/private ACL SDK | Disk | 4 | 4 | 4 | 0 | 0 | 0 | Passed selection |
| Versioning/private ACL SDK | Azure / Azurite | 4 | 4 | 4 | 0 | 0 | 0 | Passed selection |

**Both broader Ceph selections completed.** Every selected test has a recorded outcome; failures and skips are retained. This covers the configured S3 functional test file, not every suite in the upstream repository.
<!-- compatibility-results:end -->

Ceph tests are pinned to [`5522d1c351f75bc00ae0f64f742f3f095f5939d9`](https://github.com/ceph/s3-tests/tree/5522d1c351f75bc00ae0f64f742f3f095f5939d9). The core profile selects nine listing tests; versioning selects 28 version/copy/delete scenarios; private ACL selects four default/private/mtime scenarios. The broader profile selects the whole `s3tests/functional/test_s3.py` file. Both recorded audits completed all 838 selected cases with `--maxfail=0 --timeout=30`: no failure-count cutoff, with a 30-second per-test timeout. This is the full configured functional selection, not every test file in the upstream repository. No tests are converted to expected failures.

A [fixture adapter](tests/ceph/gateway_fixtures.py) changes only setup/cleanup: main-user cleanup deletes all versions and delete markers through ListObjectVersions/DeleteObjects; upstream alternate-account setup/cleanup is bypassed. Assertions are unchanged, and alternate identities remain distinct. This is an adapted conformance run, not an untouched upstream deployment. Ceph also contains RGW-specific assertions, which do not automatically represent AWS S3 requirements.

The broader audit encounters unsupported policies, public/cross-user ACLs, encryption, lifecycle, tagging, object lock, logging, GetObjectAttributes, and browser POST uploads, alongside protocol discrepancies and RGW-specific extensions. Disk additionally fails five delimiter cases involving keys that cannot be represented as native files; Azure additionally rejects a bucket name containing a period because of native container naming restrictions. Failures are retained in the results.

Each backend records one timeout failure in `test_multipart_resend_first_finishes_last`; those failures are included in the totals above and have not been rerun with a longer timeout. Both runs have the same 94 upstream skips: 74 for a Ceph logging extension unsupported by the Python client, 18 for storage-class/cloud configuration or an empty parameter set, and two for IAM account prerequisites. Skips are not passes. Disk took about 3 minutes 5 seconds; Azure/Azurite took about 3 minutes 50 seconds.

The [snapshot](tests/compatibility-results.json) retains every failing test name and assertion/error message. Raw tracebacks are in `results/ceph-*-full.xml`.

The minio-go client is pinned to **v7.3.0** in its own [test module](tests/minio/go.mod) and runs **only in Docker**, against disk and Azurite. The five scenarios cover ordinary object operations, checksummed known/unknown-size multipart uploads, incomplete uploads and composition, presigning, and copied-part checksum responses. They are our interoperability checks using the SDK, not the entire upstream MinIO test suite.

Azurite results verify the Azure adapter against an emulator. A separate live Azure smoke test previously passed, but the Ceph and Docker client results here do not certify real Azure account behavior. The three Apache iceberg-go v0.6.0 container scenarios cover FileIO write/seek/range/delete, SQL catalog metadata creation/reload, and real Parquet append/scan across two committed snapshots. Concurrent catalog commits remain untested.

## Reproduce and refresh

```sh
task test                 # local AWS SDK tests, backend tests, race detector
task vet
task ceph-core            # selected upstream listing tests, disk then Azurite
task ceph-versioning      # 28 selected versioning/copy/delete cases
task ceph-acl             # four private ACL cases
task test-protocol        # AWS SDK versioning, ACL, checksums, parts, tags and retries
task test-minio           # isolated client SDK tests, disk then Azurite
task iceberg              # FileIO and catalog metadata tests, disk then Azurite
task ceph-full -- --maxfail=0 --timeout=30 # no failure cutoff; both backends run
task compatibility-report # refresh this results section from local reports
task clean-containers
```

`task ceph-full` runs without the audit failure limit by default. Extra pytest options can be passed after `--`. Fixtures use disposable buckets and local test credentials. They do not mount `.env` or use the real Azure account. Raw JUnit XML, Python package versions, source revision, collection counts, and client JSON logs remain under ignored `results/`; the compact [result snapshot](tests/compatibility-results.json) is suitable for version control.

## Implemented from the full audit

- Range responses omit full-object checksums; part reads return the saved part checksum with the object's checksum type. Bodyless conditional responses omit payload checksum headers.
- Incoming multipart bodies are staged and verified before upload locking. Parts can commit concurrently; completion and abort exclude commits and recheck upload state. Protocol tests cover an overlapping same-part retry and an abort while a request is still sending.
- Conditional writes, including multipart completion, return `NoSuchKey` for an absent current object or a current delete marker with `If-Match`. Existing-object ETag mismatches remain `PreconditionFailed`.
- GetObjectAttributes supports attribute selection, checksums, object size/ETag/storage class, version IDs and completed-part pagination. GET/HEAD support `partNumber` for saved parts. Part metadata survives restart, completion cleanup, and subsequent versions.
- Object tagging supports validated put/get/delete, upload tags, copy directives and explicit versions. Tags preserve object identity. Azure uses native metadata and content-addressed overflow manifests; disk uses its private metadata tree.
- Iceberg coverage now includes real Parquet append, two SQL catalog snapshot commits, catalog reload, historical snapshot reads and filtered scans.

The remaining `test_multipart_resend_first_finishes_last` failure is duplicate part-number validation during completion, rather than a timeout: the upstream case submits two entries for part 1. The gateway continues to reject duplicate completion entries. The protocol regression completes an overlapping retry with a valid unique-part list.

Conditional-delete tests that expect success on an absent object or current delete marker differ from the implemented general-purpose S3 existence checks. [AWS documents a failed existence precondition for a current delete marker and Not Found for absent objects](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-deletes.html). Date/size conditional deletes remain unsupported directory-bucket features. These Ceph outcomes remain failures, with assertions unchanged.

## Next priorities

1. Copy checksum selection, Unicode metadata, newline-prefix encoding, ListBuckets pagination and self-copy validation.
2. Multipart completion crash recovery, helper/old-generation garbage collection, backend pagination and resource admission limits.
3. Concurrent Iceberg catalog commits and broader live Azure validation, including account-specific behavior.
4. Verify ambiguous completion edge cases against AWS before changing validation. Keep public/cross-user ACLs outside the configured private-only scope.
