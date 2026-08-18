# Contributing to TokayOps

Thanks for looking. TokayOps is source-available under
[FSL-1.1-Apache-2.0](LICENSE.md), not an OSI open source license: you can read,
run, modify and self-host it, but you cannot ship a competing hosted service.
Each release converts to Apache-2.0 two years after it ships. By contributing
you agree your work is licensed on the same terms.

## Before you write code

For anything larger than a bug fix, open an issue first and describe the
problem you hit. Alert routing and on-call scheduling have a lot of load-bearing
edge cases - time zones, rotation boundaries, escalation state - and a short
conversation up front usually saves a rewrite.

Bug reports are most useful with: the build you are running
(`curl https://<your-host>/api/version`), what you expected, what happened, and
the smallest sequence that reproduces it. Do **not** report security issues in a
public issue - see [SECURITY.md](SECURITY.md).

## Branches and releases

`develop` is the working branch and the one CI publishes `:develop` from. Branch
off `develop` and target it with your pull request.

`main` holds the last release. Cutting one means merging `develop` into `main`
and pushing an annotated `vX.Y.Z` tag: the tag is what publishes the versioned
images, moves `:latest`, and creates the GitHub Release. A hotfix branches from
the tag, lands in `main`, gets a patch tag, and is merged back into `develop`.
What the numbers promise is in the [release policy](README.md#releases).

Branch names that CI builds: `feature/**`, `feat/**`, `fix/**`, `hotfix/**`,
`epic*`.

## Changelog

If your change is visible to someone running TokayOps - behaviour, API, config,
or anything that needs a manual step on upgrade - add a line to the `Unreleased`
section of [CHANGELOG.md](CHANGELOG.md) in the same pull request. Refactors and
test-only changes do not need one.

## Development setup

Requirements: Go (version from `go.mod`), Docker, and PostgreSQL 13+ (the
compose file provides one).

```bash
make up                     # Postgres on :5432
export ENCRYPTION_KEY="$(openssl rand -hex 32)"
make run                    # generates swagger docs, then starts the server
```

The UI is at `http://localhost:8080`. To get data to look at:

```bash
make seed                   # demo teams, users and alert groups
```

`make seed` creates demo accounts with a shared, well-known password. It is for
local development only - never run it against a real deployment.

The first user created in a database with no admin is promoted to admin. `seed`
creates users too, so if you seed first, the demo `admin@example.com` takes that
promotion and the account you make afterwards is not an admin. Create your own
account first, or log in as the demo admin.

## Tests

```bash
make test                   # unit tests
make test-integration       # integration tests (starts its own Postgres)
make e2e-test               # Playwright end-to-end suite
```

CI runs `gofmt -l ./cmd ./internal`, `go vet ./...`, the unit tests with
coverage, and the integration suite. Formatting is checked across the whole
tree, so run `gofmt -w` on what you touch.

Please add a test with a behaviour change. Integration tests live under
`internal/integration`, end-to-end specs under `e2e/tests`.

## Commit messages and pull requests

Write commit subjects in the imperative and say what changes, not which files
move: `fix: keep a revision's words with their author`. Do not use em-dashes.

A pull request should explain what problem it solves and how you verified it.
Keep it to one concern. CI builds images on pull requests but does not publish
them, so a PR cannot affect anyone's deployment.

## Things to know before changing them

- **Database schema** lives in `InitDB`; there is no versioned migration system.
  A schema change needs a matching thought about existing deployments, and an
  upgrade note in the changelog if operators have to do anything.
- **Delivery is at-least-once.** Do not build on an exactly-once assumption.
- **Integration secrets** are encrypted with `ENCRYPTION_KEY`, which cannot be
  rotated.
- **Notification channels** go through the provider abstraction in
  `internal/dispatcher`. Read `internal/dispatcher/registry.go` and an existing
  provider (`slack.go`, `telegram.go`) before adding one; ask in an issue if the
  intended shape is not obvious from those.
