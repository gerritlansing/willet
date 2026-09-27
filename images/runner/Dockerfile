# Default runner image for willet: GitHub's actions-runner image plus
# iptables, which Docker in the VM needs for container networking, and the
# common tools that actions and scripts expect without installing them.
# Language toolchains are left to setup-* actions. Extend it with FROM for
# your project's own tools.
ARG BASE=ghcr.io/actions/actions-runner:latest
FROM ${BASE}

USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Signed apt repositories for Docker Compose and the GitHub CLI.
RUN install -m 0755 -d /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
 && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
 && chmod a+r /etc/apt/keyrings/docker.asc /etc/apt/keyrings/githubcli-archive-keyring.gpg \
 && arch=$(dpkg --print-architecture) \
 && codename=$(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}") \
 && printf 'Types: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/docker.asc\n' "$codename" "$arch" \
      > /etc/apt/sources.list.d/docker.sources \
 && echo "deb [arch=$arch signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      iptables \
      docker-compose-plugin \
      gh \
      git-lfs \
      build-essential \
      pkg-config \
      libssl-dev \
      python3-pip \
      python3-venv \
      pipx \
      zip \
      xz-utils \
      zstd \
      wget \
      rsync \
      file \
      time \
      tree \
      sqlite3 \
      dnsutils \
      iputils-ping \
      netcat-openbsd \
 && rm -rf /var/lib/apt/lists/*
USER runner
