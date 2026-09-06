# Running Moirai Cloud

## Local development

Generate a key with `openssl rand -hex 32`. Run:

```sh
MOIRAI_DEVELOPMENT=true MOIRAI_ENCRYPTION_KEY=YOUR_KEY go run ./cmd/moirai-cloud
```

This creates `.cloud/cloud.db` and encrypted `.cloud/blobs`. Use a GitHub OAuth
application with callback `http://127.0.0.1:8080/auth/callback` and supply
`GITHUB_CLIENT_ID` and `GITHUB_CLIENT_SECRET` to exercise login. There is no
development authentication bypass. Automated tests seed synthetic accounts only
inside isolated databases.

The landing, waitlist API, account dashboard, and shared viewer are served by
the same binary and origin. Their source is `internal/cloud/assets` and
`internal/cloud/web.go`; no separate frontend build or Node runtime is needed.
The earlier standalone `../moirai-app` design remains a separate uncommitted
prototype. `../moirai-landing` was absent during implementation.

## Production deployment

1. Create a private S3-compatible bucket. Disable anonymous access and public
   ACLs. Configure lifecycle retention for old object versions, and allow the
   service identity only get/put/delete/list on this bucket. Do not expose
   presigned download URLs; all reads pass through the authorization API.
2. Provision Postgres with automated backups. Use TLS to managed Postgres;
   compose's `sslmode=disable` is only for its isolated local container network.
3. Copy `.env.example` to `.env`, fill every secret, and publish an operator
   support address. Generate the 32-byte archive key once and escrow it separately.
4. Register a GitHub OAuth application with callback
   `https://moirai.to/auth/callback`. Supply its client ID and secret.
5. Run `docker compose up --build -d`. The API binds host loopback. Install the
   supplied Caddy configuration for HTTPS and point apex/www DNS to that host.
6. Verify `/readyz`, browser OAuth, CLI `login --server https://moirai.to`, and
   the two-account share smoke test below. Start with an invited pilot.

Production configuration refuses HTTP origins, filesystem/SQLite storage,
missing OAuth credentials, invalid encryption keys, and unencrypted S3 endpoints.
The bucket must exist before deployment. Migrations are additive and use a
Postgres advisory transaction lock; `moirai-cloud migrate` runs them separately.

Put limits at the ingress as well as in the application: 120 requests/minute per
client, tighter limits on login and waitlist, 33 MiB bodies, bounded concurrent
uploads, and request timeouts. The built-in limiter is per process. Set
`MOIRAI_TRUSTED_PROXY_CIDRS` to the exact proxy peer address range to use its
X-Forwarded-For chain; otherwise forwarded headers are ignored and requests behind
a proxy share its rate budget. Never trust arbitrary client-supplied headers.
Deploy an edge limiter before scaling replicas.

## Monitoring and response

Probe `/healthz` for process liveness and `/readyz` for database readiness every
30 seconds. Alert after three failed probes. Logs emit request IDs, methods,
duration and aggregate request/failure counters; do not add bodies, authorization
headers, cookies, session IDs, or share URLs to logs. Start with an availability
target of 99.5% and alert on a 5xx ratio above 1% over 5 minutes. Run a synthetic
upload/download/delete against a dedicated test account hourly to cover storage.

Set `MOIRAI_METRICS_TOKEN` to enable the private `/metrics` Prometheus endpoint.
Scrapes require `Authorization: Bearer TOKEN`. It exposes aggregate request,
failure, duration, publication, and download counters without user/session labels.
Logs are JSON. Restrict scrape credentials to monitoring operators.

On a disclosure incident, revoke affected publications first, remove grants,
invalidate affected account tokens, then investigate audit events. Database
operators can review `audit_events` using a read-only support role. This initial
version has no privileged HTTP admin endpoint. Support operations require
explicit operator database access.

## Backups and recovery

Before inviting production users, adopt and publish backup retention (suggested
pilot policy: 30 days), and test recovery. Back up Postgres and the private bucket
at consistent checkpoints; keep the encryption key separately. A database-only
backup cannot restore archives. An archive-only backup cannot restore permissions.

Restore into an isolated staging database and bucket, run migrations, then
verify an owner's download, denial to another account, a revoked link, an expired
link, and archive digest verification. Measure time to recovery; suggested pilot
targets are a one-hour RPO and four-hour RTO. Never point a restored staging API
at the live production bucket.

Deletion revokes access before removing the blob. A storage failure returns
`delete_pending_retry`; retry deletion. If a transaction commit outcome is
unknown, the upload is retained to avoid removing committed data. Reconcile
orphan objects against publication IDs after a conservative 24-hour grace period.
Expired publications remain in the owner's list and count toward quota until
deleted. Token/device/OAuth state expiry records are pruned every minute.

## Smoke test and rollback

Use synthetic session data. On account A:

```sh
moirai login --server https://STAGING_ORIGIN
moirai publish testdata/session-v1.json --from simple --preview-out reviewed.moirai
moirai publish reviewed.moirai --visibility private --yes
moirai invite PUBLICATION_ID --login ACCOUNT_B
```

On a separate machine/profile, sign in as B, pull the link, inspect the archive,
run `continue --dry-run`, then continue into an isolated installed harness. Verify
the original session did not change. Publish a fork with `--parent`, remove B's
grant through the API, and verify denial. Repeat with unlisted, expired, revoked,
and deleted publications. Do not feed synthetic test commands to a real agent
without reviewing them.

Build an image from a reviewed commit and retain the prior image digest. Deploy
to staging before production. Roll back the binary/image first; additive schema
version 1 requires no destructive down migration. Snapshot the database before
future incompatible migrations. Run the same smoke test after rollback.

Public launch remains dependent on provisioned infrastructure, a restore drill,
real harness smoke tests, operator-specific privacy/terms and support procedures.
