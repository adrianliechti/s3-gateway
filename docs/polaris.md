# Apache Polaris and Iceberg

Polaris is an Iceberg REST catalog; the gateway supplies its S3 storage. Both Polaris's Java FileIO and a query engine's FileIO must be able to reach and authenticate to the gateway.

## Signing and credentials

Iceberg's [REST protocol](https://iceberg.apache.org/docs/latest/rest-protocol/#remote-signing) distinguishes credential vending from remote signing. Remote signing can return signed headers or a presigned URL. Ordinary S3 FileIO also signs requests locally using configured credentials; presigned URLs are not required for every Iceberg deployment.

Polaris **1.7.0 and 1.8.0 do not implement remote signing**: their catalog handlers explicitly reject that delegation mode ([1.7 source](https://github.com/apache/polaris/blob/apache-polaris-1.7.0/runtime/service/src/main/java/org/apache/polaris/service/catalog/iceberg/IcebergCatalogHandler.java), [1.8 source](https://github.com/apache/polaris/blob/apache-polaris-1.8.0/runtime/service/src/main/java/org/apache/polaris/service/catalog/iceberg/IcebergCatalogHandler.java)). This is separate from the gateway accepting valid SigV4 presigned requests made by an SDK or another signer.

Use Polaris's [S3 configuration without STS](https://polaris.apache.org/releases/1.7.0/configuration/configuring-polaris-for-production/configuring-aws-s3-cloud-storage-specific/):

```json
{
  "storageType": "S3",
  "allowedLocations": ["s3://warehouse"],
  "endpoint": "https://s3.example.test",
  "endpointInternal": "https://s3.example.test",
  "region": "us-east-1",
  "pathStyleAccess": true,
  "stsUnavailable": true
}
```

Configure Polaris's AWS credential provider with the gateway's static access key and secret, and configure the same S3 credentials and endpoint on clients. `endpointInternal` is the address reachable by Polaris; `endpoint` is reachable by engines. Omit `X-Iceberg-Access-Delegation` and do not enable `s3.remote-signing-enabled`. For this mode, engine users have direct storage access through the shared gateway principal; Polaris catalog roles do not become S3 object permissions. The gateway does not provide STS, session tokens, or multi-user authorization.

iceberg-go **v0.6.0** sets `X-Iceberg-Access-Delegation: vended-credentials` by default. The test's custom catalog transport removes that header, leaving S3 requests unchanged. Credentials are passed as client configuration, not saved in Polaris table properties. This adaptation is needed for the tested library version and is not a gateway protocol extension.

## Reproducible integration test

```sh
task test-polaris
task compatibility-report
task clean-containers
```

The [Docker fixture](../compose.test.yml) pins Polaris **1.7.0** by image digest and iceberg-go **v0.6.0**. It uses disposable static credentials, creates unique buckets/catalogs, and runs against disk and Azurite. It does not use `.env` or the live Azure account.

The [test](../tests/iceberg/polaris_test.go) exercises catalog/namespace/table creation, Java FileIO metadata writes, Go FileIO Parquet and manifest writes, two snapshot commits, reloads, historical and filtered scans, presigned Parquet footer reads, and two concurrent appends from the same starting snapshot. It asserts a commit conflict, then explicitly reloads and retries the stale writer. The final scan checks all 2,200 row values by count and sum, detecting lost or duplicated writes. Catalog optimistic concurrency is supplied by Polaris; this is not evidence of multiple gateway processes safely writing one versioned bucket.

Results are recorded in `results/polaris-{disk,azure}.json` and included in [COMPATIBILITY.md](../COMPATIBILITY.md). These are selected end-to-end scenarios, not the entire upstream Polaris test suite, and do not certify credential vending, remote signing, Spark, or Trino.

## Relevant upstream tests and requirements

Reviewed Polaris tag `apache-polaris-1.7.0`, revision `4ac2f059d1cce149453d0a5f1ff1dff980ec97cc`:

| Upstream evidence | Requirement and local coverage |
| --- | --- |
| [AccessDelegationModeResolverTest](https://github.com/apache/polaris/blob/apache-polaris-1.7.0/runtime/service/src/test/java/org/apache/polaris/service/catalog/AccessDelegationModeResolverTest.java) | No-STS storage must not pretend to vend credentials; local fixture uses explicit static credentials and omits delegation. |
| [AwsStorageConfigurationInfoTest](https://github.com/apache/polaris/blob/apache-polaris-1.7.0/polaris-core/src/test/java/org/apache/polaris/core/storage/aws/AwsStorageConfigurationInfoTest.java) | Custom endpoint, internal endpoint, region and path-style settings; exercised against both gateway backends. |
| [PolarisS3InteroperabilityTest](https://github.com/apache/polaris/blob/apache-polaris-1.7.0/runtime/service/src/test/java/org/apache/polaris/service/admin/PolarisS3InteroperabilityTest.java) | Catalog location and `s3`/`s3a` validation. Upstream uses in-memory FileIO, so it does not establish S3 wire compatibility; our actual S3 integration covers `s3://`, not all URI aliases. |
| [No-STS Ozone example](https://github.com/apache/polaris/blob/apache-polaris-1.7.0/site/content/guides/ozone/docker-compose.yml) | The same storage configuration pattern works without an AWS STS endpoint. Local fixtures follow the configuration pattern and test actual warehouse reads/writes. |

Storage requirements include reliable Get/Head/Range for metadata and Parquet, Put and multipart writes, object listing/deletion for cleanup, and correct SigV4 handling. Encryption, public ACLs, and Object Lock are optional S3 capabilities, not prerequisites for this tested static-credential workflow. Unsupported encryption and retention requests must fail explicitly rather than return success without the requested protection.
