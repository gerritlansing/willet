# Contributing

Thanks for your interest in willet. Bug reports, fixes and improvements are welcome.

- **Bugs and small fixes:** open an issue or a pull request directly.
- **Larger changes:** open an issue first to discuss the approach, so the work isn't wasted.
- **Security issues:** don't open a public issue. Follow [SECURITY.md](SECURITY.md).

## Development setup

You need:

- Linux with KVM (`/dev/kvm`) to run the integration tests. Unit tests run anywhere Go runs.
- Go 1.27.1 or later, with CGO and a C toolchain. With the default `GOTOOLCHAIN=auto`, an older `go` downloads the required version.
- The microsandbox runtime in `~/.microsandbox`, at the same version as the Go SDK in `go.mod` (currently v0.7.3). The daemon installs it on first start; `msb self update` realigns an existing install.

```sh
make build        # bin/willet
make test         # go vet and unit tests with the race detector
make integration  # tests that boot real microVMs; needs KVM and internet access
make vuln         # govulncheck: dependencies and the Go standard library
```

The Docker integration tests build the project runner image inside a microVM (no Docker needed on the host), which takes a minute or two on the first run. The image is cached in `willet-test/` under your user cache directory and rebuilt automatically when the Dockerfile or the upstream base image changes. Delete that directory to force a rebuild.

## Making a change

- **Run the tests.** Every pull request should pass `make test`. Changes to `internal/microvm`, or anything that affects how VMs are created, networked or destroyed, should also pass `make integration`. Say in the pull request which ones you ran.
- **Add a test for each bug fix,** one that fails without the fix. Behaviour that depends on the real runtime belongs in an integration test (`//go:build integration`).
- **Keep to the existing style.** Run `gofmt`, match the surrounding code, and explain *why* in comments rather than *what*.
- **Keep commits focused.** Use an imperative subject line ("Fix …", "Add …") and explain the reason for the change in the body.
- **Update the docs:** [README.md](README.md) and [willet.env.example](willet.env.example) when you change user-visible behaviour or add a setting. A test checks that every setting in the example file exists.

## Dependencies

- **Vulnerabilities:** run `make vuln` after bumping Go or a dependency. Dependabot covers the modules in `go.mod`, but not the Go standard library or toolchain. `govulncheck` is pinned in `go.mod` as a tool, so it is always built with the current toolchain; a separately installed copy fails when its Go version doesn't match.
- **microsandbox:** the Go SDK and the `msb` runtime must be the same version. Bump both together.
- **scaleset:** pinned to v0.4.0, its latest release. Its `main` branch replaces the `listener.Scaler` interface with a single `Scale(ctx, msg)` method, so the next release needs a small adapter in `internal/scaler`.

## Layout

- `cmd/willet`: CLI, configuration, scale set registration, startup and shutdown
- `internal/scaler`: reconciliation and runner lifecycle (implements `listener.Scaler`)
- `internal/microvm`: microsandbox backend: VM boot, runner launch, teardown, image refresh, network policy
- `internal/owner`: scale set identity and the single-instance lock

## License

By contributing, you agree that your contributions are licensed under the project's [MIT License](LICENSE).
