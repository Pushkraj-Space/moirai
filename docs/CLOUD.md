# Moirai Cloud

## Sharing contract

Local discovery never uploads data. A publication is an immutable, validated
schema 1.0 archive. Publishing requires an authenticated account and explicit
confirmation after reviewing the prepared archive. The client removes workspace
paths and persisted thinking by default. Pattern-based secret detection is an
aid, not proof that an archive is safe; users must inspect tool output and files.

Visibility is `private` (default), `unlisted` (anyone with the random URL), or
`public` (anyone). Private publications allow the owner and explicitly invited
GitHub accounts to read. Invitations grant reading and downloading, not editing,
deleting, or inviting others. Public does not automatically mean discovery or
search indexing. All session pages initially use noindex and generic metadata.

URLs contain 192 bits of randomness. IDs are locators, not authorization for
private publications. Every metadata, archive, and viewer request checks access,
expiry, and revocation. Blobs are never served directly from a public bucket.
Revocation is immediate for new service requests. Deletion removes the live
archive and access grants; backup copies expire under the operator's published
backup retention. Neither operation recalls downloaded copies or independent
forks. A fork is a new publication with an access-checked parent reference;
private parent metadata is never disclosed to readers lacking parent access.
The one-click fork creates a private copy with the parent's expiry; a subsequent
explicit publication can choose a different expiry.

The owner selects an expiry, can revoke access, and can delete a publication.
Publishing retries use an idempotency key scoped to the authenticated owner;
reusing a key for different content fails. A fork cannot inherit permissions or
silently update its parent. Original sessions are never overwritten on import.

## Architecture and API

`cmd/moirai-cloud` serves the API and embedded web interface. The local CLI
continues to work without a server. Production uses Postgres for accounts,
sessions, grants, idempotency, and audit events; S3-compatible private storage
holds AES-GCM encrypted archives. Development can use SQLite and a private
filesystem blob directory. Encryption keys and database/object-store credentials
must be supplied by the operator, separately from backups.

Authentication uses GitHub OAuth. Only the stable GitHub account ID establishes
identity; handles are display names. The CLI uses a browser approval flow and
stores its opaque token in a private configuration file. Tokens are stored only
as SHA-256 hashes on the server and expire. Browser writes require same-origin
requests. OAuth uses one-time state and PKCE. GitHub access tokens are used only
to identify the account and are not stored.

| Route | Access | Purpose |
| --- | --- | --- |
| `GET /healthz`, `GET /readyz` | anonymous | process/database health |
| `GET /v1/formats` | anonymous | actual codec capabilities |
| `POST /v1/auth/device` | anonymous, limited | begin CLI approval |
| `GET /v1/auth/device/{id}` | device secret | poll approval |
| `GET /auth/start`, `GET /auth/callback` | browser | GitHub login |
| `GET /v1/me` | account | identity |
| `POST /v1/logout` | account | revoke current token |
| `GET /v1/publications` | account | owner's publications |
| `POST /v1/publications` | account | publish immutable archive |
| `GET /v1/publications/{id}` | reader | metadata |
| `GET /v1/publications/{id}/archive` | reader | verified archive |
| `POST /v1/publications/{id}/revoke` | owner | revoke all reading |
| `DELETE /v1/publications/{id}` | owner | delete live data |
| `POST /v1/publications/{id}/grants` | owner | grant registered account access |
| `DELETE /v1/publications/{id}/grants/{user}` | owner | remove access |
| `GET /s/{id}` | reader | transcript viewer |

Denied, missing, expired, and revoked publications return the same 404 response.
HTTP errors contain stable codes and request IDs, never database errors or
session contents. Logs omit bodies, query strings, tokens, titles, and transcripts.
No transcript HTML is trusted, media URLs are not fetched, file references are
not opened, and transcript commands never run on the server.

## Launch gates

- Cross-account tests for every publication route, owner-only mutations, expiry,
  revocation, idempotency, limits, and malformed archives.
- Browser login, CLI approval, publish, private/unlisted read, download, continue,
  fork, and revoke exercised with synthetic sessions on two isolated machines.
- Deployed HTTPS origin, GitHub OAuth application, encrypted private bucket,
  Postgres, secret management, backup retention and restore rehearsal.
- Operator-published privacy/terms, support address, abuse handling, quotas,
  availability targets, and a staged pilot before public availability claims.
- Fresh tagged binary installation on Linux, macOS, and Windows; real installed
  harness smoke tests are required separately from codec fixture tests.

Teams have an immutable owner account and `writer` or `reader` members. Owners
administer team publications and membership, writers publish and read, and readers
read. Removing membership immediately denies private team publications unless an
independent per-publication grant still applies. Team IDs are separate quota and
ownership scopes. Each account can create five teams; each personal/team scope
allows 100 active publications and 256 MiB. Membership uses stable account IDs;
per-publication invitations resolve current GitHub handles to stable IDs.

`GET/POST /v1/teams` lists/creates teams. `GET/POST
/v1/teams/{team}/members` lists/sets membership; `DELETE
/v1/teams/{team}/members/{user}` removes a member. Publishing accepts optional
`team`; only its owner/writers may publish. Team ownership transfer, billing,
automatic artifact file upload, email delivery, and public search are not enabled.
