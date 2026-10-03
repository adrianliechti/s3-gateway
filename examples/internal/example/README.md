# Shared example helpers

Client configuration, command-line options, cancellation, and cleanup for the runnable examples. Credentials come from `GATEWAY_ACCESS_KEY` / `GATEWAY_SECRET_KEY`, independently of upstream `AWS_*` storage credentials.

`NewBucket` creates a unique demo bucket and supplies cleanup for its objects, versions and delete markers. Multipart and SNS examples additionally clean up their own upload sessions and notification resources.
