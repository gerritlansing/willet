# willet

A GitHub Actions [runner scale set](https://github.com/actions/scaleset) that runs every job in a fresh, ephemeral [microsandbox](https://github.com/superradcompany/microsandbox) microVM.

It works like [actions-runner-controller](https://github.com/actions/actions-runner-controller), but runs on a single Linux host with KVM instead of Kubernetes. Each job gets hardware isolation, with its own guest kernel, instead of a container.

```
GitHub ──long poll──▶ willet ──JIT config──▶ microVM (ghcr.io/actions/actions-runner)
                         │                           └─ run.sh: one job, then exits
                         └─ destroys the VM when the runner exits
```

## Requirements

- Linux with KVM (`/dev/kvm`). macOS on Apple Silicon may work but is untested.
- Go 1.27.1 or later, with CGO and a C toolchain, to build.
- The microsandbox runtime, which the daemon installs into `~/.microsandbox` on first start. An existing install must match the SDK version in `go.mod` (currently v0.7.3); run `msb self update` to align it.
- A GitHub App (recommended) or personal access token that can manage self-hosted runners for the target organization, repository or enterprise. See [GitHub's docs](https://docs.github.com/en/actions/tutorials/use-actions-runner-controller/authenticate-to-the-api).

## Quick start

```sh
make build
cp willet.env.example willet.env   # set URL, NAME and credentials
chmod 600 willet.env
./bin/willet --env-file willet.env
```

Then target the scale set from a workflow:

```yaml
jobs:
  build:
    runs-on: msb   # the scale set name, or one of WILLET_LABELS if set
```

## Configuration

[`willet.env.example`](willet.env.example) documents every setting. Each setting is an environment variable `WILLET_<NAME>` and a matching flag `--<name>` (for example `WILLET_MAX_RUNNERS` and `--max-runners`). Precedence, highest first:

1. command-line flags, handy for one-off overrides such as `--log-level debug`;
2. the process environment;
3. the env file, given with `--env-file` or `WILLET_ENV_FILE`.

Unknown `WILLET_*` keys in the env file stop startup, so a typo can't silently fall back to a default. Run `willet --help` for the full list.

Keep the GitHub App private key in its own file (`WILLET_APP_PRIVATE_KEY_FILE`), because env files can't hold multi-line values.

**Sizing:** `WILLET_MAX_RUNNERS` is a hard cap on VMs in any state (booting, running a job, or being torn down). Size the host for `WILLET_MAX_RUNNERS × WILLET_MEMORY`, plus one short-lived VM during image refreshes.

### Runner image and build environment

VMs boot GitHub's runner image, `ghcr.io/actions/actions-runner:latest`, unless you set `WILLET_RUNNER_IMAGE`. That image is deliberately minimal: Ubuntu, the runner, `git`, `curl`, `jq` and a few basics, but no language toolchains or build tools.

Jobs can install what they need at run time, with `setup-*` actions or `sudo apt-get install`. But every job starts in a fresh VM, so it downloads and installs those tools again every time. For tools most of your jobs use, it's cheaper to build them into an image once:

```dockerfile
FROM ghcr.io/actions/actions-runner:latest
USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential zip \
 && rm -rf /var/lib/apt/lists/*
USER runner
```

Push it to a registry and point `WILLET_RUNNER_IMAGE` at it. `setup-*` actions still work on top, for example for version matrices.

Any image works if `WILLET_RUNNER_DIR` (default `/home/runner`) contains the runner's `run.sh`, executable by `WILLET_RUNNER_USER` (default `runner`). Building `FROM ghcr.io/actions/actions-runner` keeps that layout. At startup the daemon boots the image and checks this as the configured user, so a broken image fails immediately, not on the first job. The daemon re-pulls your tag daily, but your image only gets a new runner version when you rebuild it. Rebuild at least every few weeks, since GitHub stops sending jobs to runners more than 30 days out of date (see [Keeping the runner up to date](#keeping-the-runner-up-to-date)).

For a private registry, set `WILLET_REGISTRY_USERNAME` and `WILLET_REGISTRY_PASSWORD_FILE`; for a private GHCR package, use a token with `read:packages`. The credentials are only used on the host to pull the image. They are never stored in microsandbox's database or passed into VMs. Without them, microsandbox falls back to the service user's OS keyring and Docker credential helpers.

### Docker in jobs

Set `WILLET_DOCKER=true` to support `container:` jobs, service containers and Docker container actions. Each VM then runs its own Docker daemon, as GitHub-hosted runners do. The daemon starts before the runner, keeps its data on a separate ext4 disk (`WILLET_DOCKER_DISK`, default 20 GiB, sparse), and is removed with the VM.

The image must include `dockerd` and `iptables`. GitHub's stock image has `dockerd` but not `iptables`, because it's designed for actions-runner-controller, which runs the daemon in a separate container. Use the project image in [`images/runner`](images/runner/Dockerfile), which adds `iptables` and a few build tools, or add `iptables` to your own. The project image isn't published yet, so for now build and push it yourself:

```sh
docker build -t registry.example.com/willet-runner images/runner
docker push registry.example.com/willet-runner
```

At startup the daemon checks that Docker starts in the image and that the runner user can use it.

Pulls from Docker Hub count against its anonymous rate limit for your host's IP address. For busy hosts, log in within workflows (`docker/login-action`) or use a registry mirror.

### Keeping the runner up to date

Ephemeral runners don't update themselves, and GitHub [stops sending jobs](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#runner-software-updates-on-self-hosted-runners) to a runner more than 30 days behind the latest release. New runner versions come out every 3–7 weeks.

- **Tagged image (the default, `:latest`):** every `WILLET_IMAGE_REFRESH_INTERVAL` (default 24h) the daemon re-pulls the tag and starts new runners from the digest it now resolves to. Running jobs keep their image, old images are removed once unused, and a failed refresh keeps the last good image.
- **Digest-pinned image (`repo@sha256:…`):** used exactly as given and never refreshed; updating it within the 30 days is up to you.

`msb self update` updates the microsandbox runtime, not the runner image.

### Private networks and GitHub Enterprise Server

Runner VMs can reach the public internet and DNS. Private ranges, loopback, link-local and cloud metadata addresses, and the host itself are blocked ([microsandbox network security](https://docs.microsandbox.dev/security/network)). To let jobs reach a GitHub Enterprise Server (GHES), package mirror or other private service, allow its addresses:

```sh
WILLET_NETWORK_ALLOW=10.0.5.10,10.20.0.0/16:443
```

Each entry is an IP address or CIDR, optionally with `:port` or `:port-range`; IPv6 with a port uses brackets (`[fd00::1]:443`). Hostnames, and entries that would allow every address, are rejected.

**Hosts reached by name need their address allowed without a port**, because microsandbox's DNS rebinding protection drops private DNS answers unless an entry covers the address on all ports. For `ghes.example.com` at `10.0.5.10`:

| `WILLET_NETWORK_ALLOW` | `WILLET_DNS_REBIND_PROTECTION` | Jobs can reach |
|---|---|---|
| `10.0.5.10` | `true` (default) | every port on `10.0.5.10` |
| `10.0.5.10:443` | `false` | only port 443 on `10.0.5.10` |

Either way, connections stay limited to the public internet and your allow list. Turning rebinding protection off gives up a defence against a malicious public domain resolving to one of your allowed private addresses. Choose based on your network; the daemon logs a warning when protection is off.

At startup the daemon checks, from inside a VM, that the server in `WILLET_URL` resolves and accepts connections. If it doesn't, the daemon stops and says what to allow.

## Running as a service

Create a service user in the `kvm` group, install the binary and configuration, and add a systemd unit:

```sh
sudo useradd --system --create-home --groups kvm willet
sudo install -m 755 bin/willet /usr/local/bin/
sudo install -m 600 willet.env /etc/willet.env
```

```ini
# /etc/systemd/system/willet.service
[Unit]
Description=GitHub Actions scale set on microsandbox
After=network-online.target
Wants=network-online.target

[Service]
User=willet
EnvironmentFile=/etc/willet.env
StateDirectory=willet
ExecStart=/usr/local/bin/willet
Restart=always
# Restart slowly after a failure instead of hitting systemd's start rate limit.
RestartSec=30
KillSignal=SIGTERM
# The daemon's own shutdown takes at most 60s; this leaves headroom.
TimeoutStopSec=90

[Install]
WantedBy=multi-user.target
```

- `EnvironmentFile` is read by systemd as root, so `/etc/willet.env` can stay root-owned with mode 600. The service user must be able to read the GitHub App private key file.
- The service user's `~/.microsandbox` holds the runtime and image cache.
- `StateDirectory=` creates `/var/lib/willet` for the single-instance lock. Outside systemd the lock lives in `$XDG_STATE_HOME/willet`, which defaults to `~/.local/state/willet`.

## How it works

- **Startup:** the daemon takes a lock for its scale set (registration URL, runner group and name), so a second daemon for the same scale set on the host exits immediately. It pins the runner image, checks it and the network from a test VM, gets or creates the scale set, and opens GitHub's message session. Only then does it remove VMs left over from a crash, and only its own.
- **Scaling:** the target is `min(WILLET_MAX_RUNNERS, WILLET_MIN_RUNNERS + assigned jobs)`. For each missing runner, the daemon requests a just-in-time runner config from GitHub, boots a VM (about 300 ms once the image is cached), and starts `run.sh`. The config is passed only to that process, so it is never stored in microsandbox's catalog.
- **After a job:** the ephemeral runner exits and its VM is destroyed, disk and all. A runner still alive two minutes after its job completed is destroyed anyway. Surplus idle runners are removed; GitHub refuses to remove a runner with an assigned job, so none is lost to a race.
- **Failures:** a VM keeps its capacity slot until it is confirmed destroyed, and failed teardowns are retried, so the host is never oversubscribed.
- **Shutdown** (SIGINT/SIGTERM): within one minute, the daemon stops the image refresh, destroys all VMs and deregisters idle runners, then closes the GitHub session (and, with `WILLET_DELETE_ON_EXIT`, deletes the scale set). A slow or unresponsive GitHub API can't delay VM teardown; GitHub cleans up anything left behind.

## Limitations

- **One host per scale set.** GitHub allows one message session per scale set, so a second host using the same scale set waits, with a warning, and does nothing. Give each host its own scale set. The same wait happens briefly after a crash, until GitHub expires the old session.
- **Runner output logs are unmasked.** With `WILLET_LOG_RUNNER_OUTPUT=true`, job output is logged without GitHub's secret masking, so treat those logs as sensitive.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and tests.

## Security

Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)

willet is an independent project, not affiliated with or endorsed by GitHub or microsandbox.
