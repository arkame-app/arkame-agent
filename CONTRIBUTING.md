# Contributing to Arkame Agent

Thanks for taking the time. This is a small project maintained by one person,
so a little context up front saves both of us time.

## Before you start

- **Bugs:** open an issue with the bug template. The agent version
  (`/usr/local/bin/arkame-agent version`, or `/opt/arkame/bin/...` /
  `~/.local/bin/...` depending on the install; on Windows
  `& "C:\Program Files\Arkame\arkame-agent.exe" version`; in Docker
  `sudo docker exec arkame-agent arkame-agent version`), the OS, the install
  method (Docker or native) and the relevant log lines make most reports
  actionable on the first read.
- **Features and larger changes:** open an issue (or a Discussion) first and
  describe the problem you want to solve. The agent is driven by the Arkame
  panel, and many changes need a matching change on the panel side, which is not
  in this repository. Agreeing on the approach before you write code avoids a
  pull request that cannot be merged.
- **Security issues:** do not open a public issue. See [SECURITY.md](SECURITY.md).
- **Questions:** use GitHub Discussions.

## Development setup

You need Go 1.25 or newer. Docker is optional, for the bucket tests.

```bash
make build      # bin/arkame-agent
make lint       # go vet + gofmt
make test       # go test -race with coverage
```

Tests that touch a real bucket (retention purge, for example) are skipped unless
`ARKAME_TEST_S3_ENDPOINT` points to an S3-compatible server. CI uses RustFS:

```bash
docker run -d --name s3 -p 9000:9000 \
  -e RUSTFS_ACCESS_KEY=arkametest -e RUSTFS_SECRET_KEY=arkametest123 \
  docker.io/rustfs/rustfs:1.0.0
ARKAME_TEST_S3_ENDPOINT=http://127.0.0.1:9000 go test -race ./...
```

The agent runs on Linux, macOS and Windows, and the service code is specific to
each. CI cross-compiles all six targets; you can do the same locally:

```bash
for t in "linux amd64" "linux arm64" "darwin amd64" "darwin arm64" "windows amd64" "windows arm64"; do
  set -- $t; GOOS=$1 GOARCH=$2 CGO_ENABLED=0 go build -o /dev/null ./... || break
done
```

If you change `install.sh`, run `shellcheck install.sh`.

Code layout and architecture notes are in [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).

## Pull requests

- Keep each pull request focused on one change.
- Add or update tests for behavior changes. For a bug fix, a test that fails
  without the fix is the most convincing evidence.
- Run `make lint` and `make test` before pushing; CI runs the same checks.
- Explain *why* in the description, not only what changed.
- Do not change the archive names in `.goreleaser.yaml`: the installers build
  download URLs from them.
- Anything that changes what the agent sends to the panel (`internal/api`) or
  what it does to the user's files or bucket gets extra scrutiny. Call it out in
  the description.

## Language

Issues and pull requests can be written in English, Portuguese or Spanish.
Much of the existing code comments, CLI messages and commit history are in
Brazilian Portuguese; new code comments in either English or Portuguese are fine.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE), the same license as the project.
