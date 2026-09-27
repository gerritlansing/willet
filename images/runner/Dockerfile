# Default runner image for willet: GitHub's actions-runner image plus
# iptables, which Docker in the VM needs for container networking, and a few
# common build tools. Extend it with FROM for your project's own tools.
ARG BASE=ghcr.io/actions/actions-runner:latest
FROM ${BASE}

USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      iptables \
      build-essential \
      zip \
      xz-utils \
      zstd \
 && rm -rf /var/lib/apt/lists/*
USER runner
