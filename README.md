# willet

A daemon that manages a GitHub Actions [runner scale set](https://github.com/actions/scaleset) and runs every job in a fresh, ephemeral [microsandbox](https://github.com/superradcompany/microsandbox) microVM.

It works like [actions-runner-controller](https://github.com/actions/actions-runner-controller), but runs on a single Linux host with KVM instead of Kubernetes. Each job gets hardware isolation, with its own guest kernel, instead of a container. willet is new (v0.x), so settings may still change between minor versions.

```
GitHub ──long poll──▶ willet ──runner config──▶ microVM (ghcr.io/gerritlansing/willet-runner)
                         │                           └─ run.sh: one job, then exits
                         └─ destroys the VM when the runner exits
```

## Requirements

- Linux on amd64 or arm64 with KVM (`/dev/kvm`) and glibc 2.34 or later (Ubuntu 22.04, Debian 12, RHEL 9 or newer).
- To build from source instead: Go 1.27.1 or later, with CGO and a C toolchain. macOS on Apple Silicon may work from source but is untested.
- The microsandbox runtime, which the daemon installs into `~/.microsandbox` on first start. An existing install must match the SDK version in `go.mod` (currently v0.7.3); run `msb self update` to align it.
- A GitHub App (recommended) or classic personal access token that can manage self-hosted runners for the target repository or organization. Enterprise-level runners need a classic token, because GitHub Apps can't register them. See [GitHub's docs](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/authenticate-to-the-api) for the permissions.

## Quick start

Download a [release](https://github.com/gerritlansing/willet/releases) and, optionally, check that it was built by this repository's release workflow:

```sh
v=0.1.1 arch=amd64   # or arm64
curl -LO https://github.com/gerritlansing/willet/releases/download/v$v/willet_${v}_linux_$arch.tar.gz
gh attestation verify willet_${v}_linux_$arch.tar.gz --repo gerritlansing/willet
tar -xzf willet_${v}_linux_$arch.tar.gz && cd willet_${v}_linux_$arch
```

To build from source instead, run `make build`; the binary is `bin/willet`.

Create the configuration:

```sh
cp willet.env.example willet.env
chmod 600 willet.env
```

Edit `willet.env`: set `WILLET_URL` and `WILLET_NAME`, then either the three `WILLET_APP_*` settings or, for a token, `WILLET_TOKEN` with the `WILLET_APP_*` lines commented out. Start the daemon:

```sh
./willet --env-file willet.env
```

The first start installs the microsandbox runtime and pulls the runner image (about 1 GB). It's ready when it logs `Listening for jobs`. To try it, add this workflow to the repository and run it from the Actions tab:

```yaml
# .github/workflows/willet-test.yml
on: workflow_dispatch
jobs:
  hello:
    runs-on: willet   # WILLET_NAME, or one of WILLET_LABELS if set
    steps:
      - run: echo "Hello from $RUNNER_NAME"
```

## Configuration

[`willet.env.example`](willet.env.example) documents every setting. Each setting is an environment variable `WILLET_<NAME>` and a matching flag `--<name>` (for example `WILLET_MAX_RUNNERS` and `--max-runners`). Precedence, highest first:

1. command-line flags;
2. the process environment;
3. the env file, given with `--env-file` or `WILLET_ENV_FILE`.

Unknown `WILLET_*` keys in the env file stop startup, so a typo can't silently fall back to a default.

Keep the GitHub App private key in its own file (`WILLET_APP_PRIVATE_KEY_FILE`), because env files can't hold multi-line values.

**Sizing:** `WILLET_MAX_RUNNERS` is a hard cap on VMs in any state (booting, running a job, or being torn down). Size the host for `WILLET_MAX_RUNNERS × WILLET_MEMORY`, plus one short-lived VM during image refreshes.

### Runner image and build environment

VMs boot the willet runner image, `ghcr.io/gerritlansing/willet-runner:latest`, unless you set `WILLET_RUNNER_IMAGE`. It is GitHub's runner image, [`ghcr.io/actions/actions-runner`](https://github.com/actions/runner/pkgs/container/actions-runner), plus `iptables` for Docker and a few build tools (`build-essential`, `zip`, `xz-utils`); see [`images/runner`](images/runner/Dockerfile). Other language toolchains aren't included.

The image is rebuilt when GitHub releases a new runner version, for amd64 and arm64, and tagged three ways:

- `2.337.0-1`: one build, never changed. The number after the dash counts changes to the image's recipe.
- `2.337.0`: the newest build of that runner version.
- `latest`: the newest build.

Jobs can install tools at run time with `setup-*` actions or `sudo apt-get install`, but every job starts in a fresh VM and repeats the download. For tools most jobs use, build them into an image once:

```dockerfile
FROM ghcr.io/gerritlansing/willet-runner:latest
USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3-venv \
 && rm -rf /var/lib/apt/lists/*
USER runner
```

Push it to a registry and point `WILLET_RUNNER_IMAGE` at it.

A custom image needs a working actions runner installation in `WILLET_RUNNER_DIR` (default `/home/runner`), with `run.sh` executable by `WILLET_RUNNER_USER` (default `runner`), and, with Docker on, [Docker's requirements](#docker-in-jobs). Building `FROM` the willet image covers all of this. At startup the daemon boots the image and checks it, so a broken image fails immediately, not on the first job. Rebuild it regularly to pick up new runner versions (see [Keeping the runner up to date](#keeping-the-runner-up-to-date)).

For a private registry, set `WILLET_REGISTRY_USERNAME` and `WILLET_REGISTRY_PASSWORD_FILE` (for GHCR, a token with `read:packages`). The credentials are used on the host for the pull only and never reach a VM.

### Docker in jobs

`container:` jobs, service containers and Docker container actions work out of the box: each VM runs its own Docker daemon, as GitHub-hosted runners do. The daemon starts before the runner, adding about a second to each VM's start, keeps its data on a separate ext4 disk (`WILLET_DOCKER_DISK`, default 20 GiB, sparse), and is removed with the VM. Set `WILLET_DOCKER=false` to turn it off.

The image must include `dockerd` and `iptables`. The willet image has both; GitHub's stock image lacks `iptables`, so add it or turn Docker off. At startup the daemon checks that Docker starts and that the runner user can use it.

Pulls from Docker Hub count against its anonymous rate limit for your host's IP address. For busy hosts, log in within workflows (`docker/login-action`) or use a registry mirror.

### Keeping the runner up to date

Ephemeral runners don't update themselves, and GitHub [stops sending jobs](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#runner-software-updates-on-self-hosted-runners) to a runner more than 30 days behind the latest release. New runner versions come out every 3–7 weeks.

- **Tagged image (the default, `:latest`):** every `WILLET_IMAGE_REFRESH_INTERVAL` (default 24h) the daemon re-pulls the tag and starts new runners from the digest it now resolves to. Running jobs keep their image, old images are removed once unused, and a failed refresh keeps the last good image. The willet image's `latest` follows each runner release; a tag of your own only moves when you rebuild and push it.
- **Digest-pinned image (`repo@sha256:…`):** used exactly as given and never refreshed; updating it within the 30 days is up to you.

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
sudo install -m 755 willet /usr/local/bin/   # bin/willet if built from source
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

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now willet
sudo journalctl -u willet -f   # wait for "Listening for jobs"
```

**Stopping or restarting willet interrupts running jobs:** it destroys every VM rather than waiting for jobs to finish.

- `EnvironmentFile` is read by systemd as root, so `/etc/willet.env` can stay root-owned with mode 600. The daemon itself reads the GitHub App private key file and any registry password file, so the service user must be able to read those.
- The service user's `~/.microsandbox` holds the runtime and image cache.
- `StateDirectory=` creates `/var/lib/willet` for the single-instance lock. Outside systemd the lock lives in `$XDG_STATE_HOME/willet`, which defaults to `~/.local/state/willet`.

## How it works

- **Startup:** the daemon takes a per-scale-set lock, so a second daemon for the same scale set on the host exits. It pins the runner image, checks it and the network from a test VM, gets or creates the scale set, opens GitHub's message session, and then removes its own VMs left over from a crash.
- **Scaling:** the target is `min(WILLET_MAX_RUNNERS, WILLET_MIN_RUNNERS + assigned jobs)`. For each missing runner, the daemon requests a single-use runner config from GitHub, boots a VM (about 300 ms once the image is cached, plus about a second to start Docker), and starts `run.sh`.
- **After a job:** the ephemeral runner exits and its VM is destroyed, disk and all. A runner still alive two minutes after its job completed is destroyed anyway. Surplus idle runners are removed.
- **Failures:** a VM keeps its capacity slot until it is confirmed destroyed, and failed teardowns are retried, so the host is never oversubscribed.
- **Shutdown** (SIGINT/SIGTERM): the daemon destroys all VMs, interrupting any running jobs, deregisters idle runners and closes the GitHub session (and, with `WILLET_DELETE_ON_EXIT`, deletes the scale set). It gives up on any step still running after about a minute; VMs it couldn't destroy are removed at the next start, and GitHub cleans up runners left behind.

## Limitations

- **One host per scale set.** GitHub allows one message session per scale set, so a second host using the same scale set waits, with a warning, and does nothing. Give each host its own scale set. The same wait happens briefly after a crash, until GitHub expires the old session.
- **Runner output logs are unmasked.** With `WILLET_LOG_RUNNER_OUTPUT=true`, job output is logged without GitHub's secret masking, so treat those logs as sensitive.

## Contributing

Report bugs and ask questions in [issues](https://github.com/gerritlansing/willet/issues). See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and tests.

## Security

Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)

willet is an independent project, not affiliated with or endorsed by GitHub or microsandbox.
