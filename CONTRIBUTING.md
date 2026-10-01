# Contributing to OneCamp

Thank you for helping. A few things make a contribution easy to accept.

## Before you start

- **Bugs:** open an issue with what you did, what you expected and what
  happened, plus the version (Admin, then Health and updates).
- **Features:** open a Discussion first. OneCamp is built by a very small team,
  and agreeing on the shape before the code saves both of us time.
- **Security problems:** never in a public issue. See [SECURITY.md](SECURITY.md).

## Making a change

- Go 1.25. Run `make verify` before opening a pull request: it runs vet, gofmt,
  the unit tests and the guard tests that keep a few promises honest (no
  dead exports, the public route list, and others).
- Integration tests need Docker: `go test -tags=integration ./tests/integration/...`.
- Keep changes focused. One pull request, one purpose.
- Database changes are numbered migrations in `migrations/`, with a `down` file.
- Comments explain *why*, not what. Read a few files first and match their style.

## The contributor licence agreement

OneCamp is dual licensed: AGPL-3.0 for everyone, and a commercial licence that
pays for its development. For your code to ship in both, we need your
permission to license it that way. By opening a pull request you agree to the
[Contributor Licence Agreement](CLA.md), and the pull request template asks you
to confirm it.

Sign off every commit (`git commit -s`), which certifies the
[Developer Certificate of Origin](https://developercertificate.org/): that you
wrote the change or otherwise have the right to submit it.
