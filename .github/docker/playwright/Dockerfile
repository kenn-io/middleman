# syntax=docker/dockerfile:1

ARG BUN_IMAGE=oven/bun:1.4.0@sha256:5ff609364c049b54eb0ff560ec96319729a972078ef2c755d758f0c6ef89c2d6
ARG PLAYWRIGHT_BASE_IMAGE=mcr.microsoft.com/playwright:v1.62.1-noble@sha256:dcc5531e97840b9b5e794f2814476b21571c5124a3fca2267d73041f56e7580e
ARG VITE_PLUS_VERSION=0.3.0

FROM ${BUN_IMAGE} AS bun
FROM ${PLAYWRIGHT_BASE_IMAGE}

ARG VITE_PLUS_VERSION

LABEL org.opencontainers.image.source="https://github.com/kenn-io/middleman"

ENV BUN_INSTALL=/usr/local \
    DEBIAN_FRONTEND=noninteractive

COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun

RUN node -e 'if (Number(process.versions.node.split(".")[0]) < 24) process.exit(1)' \
    && bun --version \
    && bun add --global --ignore-scripts "vite-plus@${VITE_PLUS_VERSION}" \
    && vp --version \
    && sed -i \
      -e 's|http://azure.archive.ubuntu.com/ubuntu|https://mirrors.edge.kernel.org/ubuntu|g' \
      -e 's|http://archive.ubuntu.com/ubuntu|https://mirrors.edge.kernel.org/ubuntu|g' \
      -e 's|http://security.ubuntu.com/ubuntu|https://mirrors.edge.kernel.org/ubuntu|g' \
      /etc/apt/sources.list.d/ubuntu.sources \
    && apt-get \
      -o Acquire::Retries=5 \
      -o Acquire::ForceIPv4=true \
      -o Acquire::http::Timeout=30 \
      -o Acquire::https::Timeout=30 \
      update \
    && apt-get \
      -o Acquire::Retries=5 \
      -o Acquire::ForceIPv4=true \
      -o Acquire::http::Timeout=30 \
      -o Acquire::https::Timeout=30 \
      install -y --no-install-recommends poppler-utils tmux \
    && rm -rf /var/lib/apt/lists/*
