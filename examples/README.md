# Examples

Run commands from the repository root. These programs use the AWS Go SDK v2 and the root Go module; no extra dependencies are needed.

| Example | What it demonstrates | Run |
| --- | --- | --- |
| [SNS](sns/README.md) | Signed HTTP subscription, bucket filters, create/delete events | `task example-sns` |
| [ABAC and tags](abac/README.md) | JWT → STS credentials, team tags, access denial and revocation | `task example-abac` |
| [Objects](objects/README.md) | Versioning, conditional writes, metadata, presigned GET/PUT | `task example-objects` |
| [Multipart](multipart/README.md) | Four concurrent parts, checksums, completion and abort cleanup | `task example-multipart` |

Each example creates randomly named demo resources and removes them on normal exit, error, or Ctrl+C. They require operator credentials to create and clean up buckets. SIGKILL or unavailable providers can prevent cleanup; any cleanup failure prints the demo resource name.

Set `GATEWAY_ACCESS_KEY` and `GATEWAY_SECRET_KEY` in `.env`, then start the gateway with `task run`. All example tasks load `.env`; exported variables take precedence. Examples connect to `GATEWAY_ENDPOINT` (default `http://127.0.0.1:9000`) and sign with `GATEWAY_REGION` (default `us-east-1`). They use gateway client credentials, independently of the upstream provider's `AWS_*` credentials.

Without Task, export the variables and use `go run ./examples/objects` or another example directory. All clients accept `--endpoint`, `--region`, and `--timeout`. See each README for additional setup and flags.
