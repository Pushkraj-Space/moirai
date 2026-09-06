# Security Policy

## Supported versions

Security fixes are released for the latest minor release on the default branch.
Users should upgrade to the newest available version before reporting an issue.

## Reporting a vulnerability

Do not report vulnerabilities in a public issue. Use
[GitHub private vulnerability reporting](https://github.com/october-dev/moirai/security/advisories/new)
and include the affected version, impact, reproduction steps, and a minimal
sanitized fixture when possible.

Do not use real credentials, private sessions, or customer data in a report.

## Security boundary

Moirai parses untrusted session data and writes to local harness stores. Its
security controls include bounded parsing, canonical validation, archive
integrity checks, guarded store paths, rejection of symlink traversal, atomic
private file writes, fresh identity on handoff, and confirmation for deletion.

Moirai does not:

- authenticate to remote chat services;
- copy harness credentials, cookies, tokens, or account databases;
- execute commands found inside transcript content;
- infer whether ordinary message or tool content contains a project secret;
- provide confidentiality or authenticity through its archive checksum.

An archive SHA-256 value detects accidental or unsophisticated modification; it
is not a signature. Protect archives using normal filesystem and transport
access controls.

## Useful reports

The hosted backend and website are maintained separately. This repository's
publish client assists with local review and known token patterns; it cannot
prove that tool output, inline media, or ordinary text is free of secrets.
Review archives before uploading and use a trusted service origin. Report hosted
service issues to its operator.

- path traversal, unsafe symlink handling, or unintended deletion;
- command execution caused by imported content;
- reads or writes outside a selected harness store;
- archive verification bypasses;
- denial of service that bypasses configured input limits;
- cross-session identity or provenance corruption;
- secrets copied from an unrelated account or session.
