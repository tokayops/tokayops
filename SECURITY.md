# Security Policy

## Reporting a vulnerability

Please do not open a public issue for security problems.

Report privately through GitHub's [security advisory
form](https://github.com/tokayops/tokayops/security/advisories/new). The report
stays visible only to you and the maintainers until an advisory is published.

Include what you need to make the problem reproducible: the deployed build
(`curl https://<your-host>/api/version`, or the `sha-<commit>` image tag),
configuration relevant to the issue, and the steps you took. A proof of concept
helps, but a clear description is enough.

You will get an acknowledgement within 3 working days and an assessment within
10. We will tell you what we intend to do and when, and credit you in the
advisory unless you would rather stay anonymous.

## Supported versions

Only the newest release receives fixes, security ones included, and they ship as
a patch on top of it (`0.1.0` -> `0.1.1`). Older releases, `develop` builds and
`sha-<commit>` images are not patched - upgrade to the newest release instead.
See the [release policy](README.md#releases).

## Scope

In scope: authentication and session handling, RBAC enforcement, the
Alertmanager and Telegram webhook endpoints, Slack request signature
verification, outbound webhook SSRF protection, encryption of stored
integration secrets, and injection or privilege escalation through the API.

Out of scope: findings that require an already-compromised host or database,
denial of service through sheer volume, missing hardening headers with no
demonstrated impact, and anything in a deployment's own reverse proxy or
network configuration.

## Operator notes

Two properties of a TokayOps deployment are the operator's responsibility, and
neither is a vulnerability in the product:

- **`ENCRYPTION_KEY` cannot be rotated.** It decrypts every stored integration
  secret. Keep a backup outside the server; if it is lost, all integrations must
  be recreated.
- **The application must run behind TLS.** `APP_ENV=production` sets Secure
  cookies and enables CSRF, both of which assume HTTPS. The shipped compose file
  binds its ports to loopback for exactly this reason.
