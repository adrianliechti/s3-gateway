# ABAC and tags

The [program](main.go) exchanges a JWT for temporary S3 credentials and compares the JWT's `team` claim with each bucket's `team` tag. It creates `analytics` and `finance` demo buckets, demonstrates allowed and denied requests, updates object tags, then changes a bucket tag through S3 Control and verifies that existing credentials and a presigned URL lose access. Both buckets are removed afterward.

Configure operator credentials (`GATEWAY_ACCESS_KEY`, `GATEWAY_SECRET_KEY`) in `.env`. Run these commands in three terminals, all from the repository root:

```sh
# Terminal 1: loopback-only development issuer and JWKS endpoint.
task example-issuer

# Terminal 2: gateway with the example trust and role policy.
task run -- -identity-config examples/abac/identity.json

# Terminal 3: demonstrate access and revocation.
task example-abac
```

The issuer returns a JWT with `team=analytics` and role `storage`. The policy in [identity.json](identity.json) requires:

```json
"StringEquals": {
  "aws:ResourceTag/team": "${aws:PrincipalTag/team}"
}
```

Expected behavior:

| Request | Result |
| --- | --- |
| Read the `team=analytics` bucket | Allowed |
| Read or tag an object in the `team=finance` bucket | `AccessDenied` |
| Set an object tag to `team=finance` in the analytics bucket | Allowed; bucket tags still control access |
| Change bucket access tags with session credentials | `AccessDenied` |
| Operator retags analytics bucket to `team=finance` | Existing session and presigned URL lose access |

Enable ABAC with `PutBucketAbac`. After that, bucket tags are managed through S3 Control `TagResource`/`UntagResource` at the same gateway endpoint. The example [disables SDK host prefixing](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html) so S3 Control uses the gateway host unchanged. Object tags remain ordinary S3 object metadata; this policy does not authorize by object tags. The STS response's session token must accompany its access and secret keys.

The issuer is intentionally unauthenticated and only binds to `127.0.0.1:9003`. It uses a fresh in-memory key on startup; restart the gateway when restarting the issuer to clear cached JWKS. Stop both servers with Ctrl+C after the demo.

For a real identity provider, adapt [identity-provider.json](identity-provider.json), keep HTTPS issuer/JWKS URLs, restart the gateway with that config, and supply a JWT with the same role/team claims:

```sh
task example-abac -- --token-file /path/to/token.jwt
```

An alternative configured role can be selected with `--role`. JWTs and temporary credentials are never printed by the example.
