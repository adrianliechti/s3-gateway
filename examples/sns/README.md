# S3 events through SNS

The [program](main.go) creates a topic and HTTP subscription, verifies the SNS signature, confirms the subscription, and configures a temporary bucket for `events/*.json` create/delete events. It uploads and deletes `events/a space+plus.json`, handles the separate `s3:TestEvent` format, decodes object keys, and removes its bucket, topic and subscription afterward.

Start your `../sns-gateway` server on `127.0.0.1:9001`. In this repository's `.env`, add these settings alongside your existing S3 gateway credentials:

```dotenv
GATEWAY_SNS_ENDPOINT=http://127.0.0.1:9001
GATEWAY_SNS_ACCESS_KEY=your-sns-access-key
GATEWAY_SNS_SECRET_KEY=your-sns-secret-key
GATEWAY_REGION=us-east-1
GATEWAY_SNS_REGION=us-east-1
```

Use the credentials configured by your SNS server. Its `task dev` defaults are `gateway-local` / `gateway-local-dev-secret`. Start or restart the S3 gateway so it loads these SNS settings, then run the example in a second terminal:

```sh
task run
# Second terminal, from this repository root:
task example-sns
```

The subscriber listens on `127.0.0.1:9004`. Output includes:

```text
Verified signature and confirmed HTTP subscription
Received s3:TestEvent
Received ObjectCreated:Put for events/a space+plus.json (sequencer ...)
Received ObjectRemoved:Delete for events/a space+plus.json (sequencer ...)
SNS round trip complete; cleaning up bucket, subscription and topic
```

Delivery order can differ. The demo tolerates repeated events; a real consumer should persist its deduplication or processing result before acknowledging delivery. See the [S3 event structure](https://docs.aws.amazon.com/AmazonS3/latest/userguide/notification-content-structure.html).

For an SNS server in Docker Desktop, let it reach the host subscriber:

```sh
task example-sns -- --listen 0.0.0.0:9004 --callback-url http://host.docker.internal:9004/sns
```

This subscriber is for the companion SNS gateway: it pins the certificate fetched from the configured endpoint's `/SimpleNotificationService.pem` and never follows certificate or confirmation URLs from messages. Use a trusted HTTPS endpoint outside local development. Amazon SNS uses a different certificate trust setup; this verifier is not an Amazon SNS subscriber. See [SNS signature verification](https://docs.aws.amazon.com/sns/latest/dg/sns-verify-signature-of-message.html).

For existing buckets, [notification.json](notification.json) shows the AWS CLI configuration shape. Replace its topic ARN with your real same-region topic ARN before running:

```sh
# Export GATEWAY_ACCESS_KEY and GATEWAY_SECRET_KEY in this shell first.
# AWS_* below are client credentials, not storage-provider credentials.
export AWS_ACCESS_KEY_ID="$GATEWAY_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$GATEWAY_SECRET_KEY"
export AWS_DEFAULT_REGION="${GATEWAY_REGION:-us-east-1}"
aws --endpoint-url http://127.0.0.1:9000 s3api put-bucket-notification-configuration \
  --bucket warehouse --notification-configuration file://examples/sns/notification.json
```

This call replaces the bucket's notification configuration. Read its current configuration first if it already has rules.
