# Local JWT issuer

Run `task example-issuer` from the repository root, or `go run ./examples/abac/issuer` with Go.

Serves JWKS at `http://127.0.0.1:9003/.well-known/jwks.json` and a development JWT at `/token`. Tokens have audience `s3-gateway`, role `storage`, and team `analytics`; they expire after 30 minutes. There is no user authentication. The signing key exists only in memory.

Use it with the parent [ABAC example](../README.md). Ctrl+C shuts it down.
