# Contributing to Accelero

Thanks for your interest in improving Accelero! This document covers how to get
set up, the conventions the project follows, and how to get a change merged.

## Ground rules

- **Be respectful.** All interaction is governed by our [Code of Conduct](CODE_OF_CONDUCT.md).
- **Security issues do not go in public issues.** See [SECURITY.md](SECURITY.md)
  for how to report a vulnerability privately.
- **Discuss large changes first.** For anything beyond a bug fix or small
  improvement, open an issue describing the problem before writing code — it
  saves everyone time if the approach needs adjusting.

## Prerequisites

- **Go** 1.25 or newer (`go version`).
- **Docker** with a reachable daemon — the test suite and manual runs talk to it.
- Optionally **mkdocs** if you're editing the docs site.

## Getting started

```bash
git clone https://github.com/arbianshkodra/accelero.git
cd accelero

# Build
go build ./...

# Run the full test suite (talks to your local Docker daemon)
go test ./...

# Run locally (requires an API key)
API_KEY=dev-key go run ./cmd/main.go
```

See [`README.md`](README.md) for the quick-start and [`docs/`](docs/) for the
full documentation.

## Development workflow

1. **Branch off `dev`.** `dev` is the default branch and the base for all pull
   requests. Use a descriptive prefix: `feat/`, `fix/`, `chore/`, or `docs/`.
2. **Make focused changes.** One logical change per pull request keeps reviews
   fast and history readable.
3. **Match the surrounding code.** Follow the naming, comment density, and idioms
   already present in the file you're editing.
4. **Add tests.** New behaviour needs coverage; bug fixes should include a test
   that fails before the fix. The project keeps a per-package test summary in
   [`CLAUDE.md`](CLAUDE.md) — add a line there when you add a test file.
5. **Update docs.** If you add or change configuration, endpoints, or behaviour,
   update `docs/`, `.env.example`, and the relevant `README.md`/`CLAUDE.md`
   sections in the same PR.

## Before you open a pull request

Run the same checks CI runs, from the repository root:

```bash
go build ./...
go vet ./...
staticcheck ./...   # CI uses dominikh/staticcheck-action
go test ./...
```

CI runs lint (staticcheck) and the test suite on Linux, macOS, and Windows
against Go 1.25.x. All must pass before a PR can merge.

## Commit and PR conventions

- Write commit subjects in the imperative mood, prefixed by type, e.g.
  `feat: per-stack secrets`, `fix: rollback capturing post-failure state`.
- Explain the *why* in the body when it isn't obvious from the diff.
- In the PR description, cover what changed, why, and how you verified it
  (tests, manual steps, or both).
- Keep the PR scoped to a single concern; split unrelated changes.

## Reporting bugs and requesting features

Use the issue templates when opening an issue. For bugs, include your Accelero
version, how you're running it (Docker/binary), the relevant compose/stack
config with **secrets redacted**, and the logs around the failure.

## License

By contributing, you agree that your contributions are licensed under the
project's [Apache License 2.0](LICENSE).
