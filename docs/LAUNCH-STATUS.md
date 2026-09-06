# Cloud launch status — unreleased 0.2.0

Implementation is available for review and an invited pilot deployment. It is
not a live production service or a published release. The workspace was
fast-forwarded to upstream main `504b4e7` before these changes.

## Implemented

1. **Sharing contract:** immutable archives, private defaults, explicit
   public/unlisted access, expiration, individual account grants, team roles,
   revocation, deletion, and independent forks. See [CLOUD.md](CLOUD.md).
2. **Backend:** Go HTTP service, Postgres migrations, encrypted private
   S3-compatible storage, GitHub OAuth and CLI device login, hashed sessions,
   transactional quotas, idempotent publication, audit events, health checks,
   bounded requests, CSRF protection, security headers, and private metrics.
3. **Complete sharing flow:** local review/redaction, browser and CLI publishing,
   authenticated viewer/download, invitations, team management, private forks,
   integrity checking, and native continuation with fresh session identity.
4. **Installation and first run:** five platform package targets, checksums,
   dependency inventories, release provenance workflow, generated Homebrew and
   winget manifests, readonly doctor, and zero-write import/continue dry runs.
5. **Landing and operations:** responsive embedded landing, working waitlist,
   dashboard, viewer, capability-derived format claims, Docker deployment,
   HTTPS proxy example, backup/restore and incident runbooks.

The requested `../moirai-landing` directory was absent. The landing is served
from this repository with the backend; the separate `../moirai-app` prototype
was preserved. No deployment, release tag, npm publication, or installer registry
submission has been made.

## Verification

- Final source passed `go test -race ./...`, `go vet`, Staticcheck, workflow
  Actionlint, JavaScript syntax checks, and `git diff --check`.
- Final browser and compiled-CLI journeys passed separately under the race
  detector with isolated SQLite storage. Chromium covered desktop/mobile layout,
  waitlist, review, private publication, viewer, download, and revocation. CLI
  coverage included account isolation, grants, pull, dry run, native Claude store
  import without launching a model, fork, and revocation.
- Browser canonicalization and all ten TypeScript SDK tests passed. npm audits
  reported no vulnerabilities. Govulncheck found no reachable vulnerabilities;
  it reported one advisory in a required module outside imported packages.
- Postgres permission/quota tests, real MinIO encrypted storage, and a
  `pg_dump`/`pg_restore` plus encrypted-blob recovery drill passed during
  implementation. A final combined rerun could not reconnect because Docker
  Desktop was stopping. Rerun that gate before deployment using the commands
  below. A race found in test-server startup ordering was fixed and the affected
  browser/CLI tests subsequently passed under the race detector.
- The cloud Docker image and five cross-platform CLI packages built locally.
  Cross-compilation is not a substitute for native installation/resume tests on
  each supported operating system.

To repeat the full integration gate (all credentials here are synthetic):

```sh
docker compose -f deploy/compose.test.yaml up -d --wait
npm ci
npx playwright install chromium
MOIRAI_BROWSER_TESTS=1 MOIRAI_CLI_E2E=1 MOIRAI_RECOVERY_TEST=1 \
  MOIRAI_TEST_POSTGRES='postgres://moirai_test:synthetic-test-password@127.0.0.1:55432/moirai_test?sslmode=disable' \
  MOIRAI_TEST_S3=127.0.0.1:59000 go test -race ./...
docker compose -f deploy/compose.test.yaml down
```

## Gates before inviting real users

- Select the hosting project and provision Postgres, private object storage,
  archive encryption key with separate escrow, GitHub OAuth, TLS, and DNS.
  Set secrets through the host's secret manager, not chat or source control.
- Configure monitoring, backups, ingress rate limits/trusted proxies, support
  ownership, and an operator-approved privacy/retention policy. The current
  privacy page describes technical behavior; it is not a completed legal policy.
- Exercise real GitHub login and CLI approval on the deployed origin with two
  independent accounts. Verify private/team access, expiry, revoke/delete,
  recovery, and actual model continuation on supported harness versions.
- Review changes and CI, publish the versioned binaries, configure npm trusted
  publishing, and submit the generated installer manifests. Local manifests
  reference future release URLs and are not yet installable registry entries.
- Start with an invited pilot and review failures before a general launch.
  Redaction is best-effort, not a proof that an archive contains no secrets.

Billing, public discovery, email invitation delivery, self-service account/team
deletion and ownership transfer, and automated artifact uploads remain outside
this pilot implementation. See [OPERATIONS.md](OPERATIONS.md) for deployment and
recovery, and [INSTALL.md](INSTALL.md) for release/distribution details.
