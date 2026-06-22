# Contributing to knarr

Thanks for your interest in contributing! knarr is developed in the open with the
goal of homing under the [composefs](https://github.com/composefs) organization,
and aims to follow that community's conventions. Please also read the
[composefs contribution guide](https://github.com/composefs/composefs/blob/main/CONTRIBUTING.md),
which this guide builds on.

By participating, you agree to abide by our [Code of Conduct](CODE-OF-CONDUCT.md).

## Before you start

- If you plan to contribute a large change, please get in touch **before**
  submitting a pull request — file an issue describing your proposed change so we
  can align on the approach first. Large or high-impact pull requests submitted
  without prior discussion may be closed.
- Keep each pull request focused on a single change. Submit independent changes as
  separate pull requests.

## Developer Certificate of Origin (DCO)

All commits **must** be signed off under the
[Developer Certificate of Origin](https://developercertificate.org/). Sign off by
adding a `Signed-off-by` trailer with your real name and email (no pseudonyms or
anonymous contributions):

```
Signed-off-by: Jane Developer <jane@example.com>
```

The easiest way is to pass `-s` when committing:

```bash
git commit -s -m "pull: Short imperative subject"
```

The sign-off certifies that you wrote the patch or otherwise have the right to
submit it under the project's open source license. Commits without a valid
sign-off cannot be merged.

## Automated and AI-generated contributions

- **Pull requests must be opened by a human.** Any use of automation to create
  pull requests must be approved by a maintainer in advance.
- **All AI-generated or AI-assisted content** — code, commit messages, PR
  descriptions, or comments — **must be reviewed by the human author before it is
  submitted.** The human author is responsible for the correctness and quality of
  everything in their pull request.

If you use a coding agent in this repository, it must also follow the rules in
[AGENTS.md](AGENTS.md).

## Submitting a pull request

1. Read the [Code of Conduct](CODE-OF-CONDUCT.md) and [LICENSE](LICENSE).
2. [Fork](../../fork) and clone the repository.
3. Create a branch off `main`:
   ```bash
   git checkout -b my-change main
   ```
4. Make your change, add tests, and make sure everything passes (see below).
5. Commit with a sign-off and a clear commit message (see below). Each commit
   should build and tell a coherent story.
6. Keep your branch current:
   ```bash
   git pull --rebase upstream main
   ```
7. Push to your fork and [open a pull request](../../pulls) against `main`.

## Git commit style

knarr follows the composefs (Linux-kernel) commit style:

- **Subject:** `<context>: Capitalized imperative subject`, where `<context>` is
  the area of change (e.g. `pull`, `mount`, `cas`, `cmd/knarr`, `docs`). Aim for
  under 50 characters (72 max), and do not end it with a period.
- **Body:** separate it from the subject with a blank line, wrap it at 72
  characters, and explain *what* changed and *why*.
- If your commit fixes an issue, end the body with `Closes: #<number>`.

See [How to Write a Git Commit Message](https://chris.beams.io/posts/git-commit/)
for more.

## Code style and checks

- Go code uses standard formatting. Run `gofmt` (CI rejects unformatted code) and
  `go vet ./...`.
- Run the relevant tests before submitting:
  ```bash
  make vet
  make test       # unit tests
  make build
  ```
- Changes that touch knarr's privileged paths (mount, fs-verity, CAS build) must
  pass the privileged test target, which requires root or a full-mapping user
  namespace:
  ```bash
  sudo -E PATH=$PATH make root-test   # or: podman unshare make root-test
  ```
- Every Go source file carries the Apache-2.0 SPDX header
  (`// SPDX-License-Identifier: Apache-2.0`).

## Continuous integration

CI runs `gofmt`, `go vet`, `make build`, and the unit tests. A pull request is
expected to be green before review.

## Reporting security issues

Please do **not** report security vulnerabilities through public issues or pull
requests. See [SECURITY.md](SECURITY.md) for the coordinated-disclosure process.
