# Causely investigator on kagent: Go A2A<->ACP adapter + Hermes Agent, plus
# the CLIs the agent uses from its terminal tool: git, gh, kubectl, ripgrep.
# Multi-platform (linux/amd64, linux/arm64): the Go adapter is cross-compiled
# on the build host; the runtime stage is built per target platform.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine3.23@sha256:f70850ef6ce341e8b6427f677049145d63c0b7ce002d4d7e3d464828b1b96dcd AS builder
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY *.go ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -o /out/causely-hermes .

FROM ghcr.io/astral-sh/uv:0.12.3@sha256:2d890623d310b57771ce840f0da5eed5fc6d657da05ffaa45d82797b53fa3abc AS uv

FROM python:3.14.10-slim-trixie@sha256:9aaa474097d094040649f2162a627c0049b408f52138d980916788d80f04802f

ARG TARGETARCH
ARG VERSION=0.1.0
# Bump deliberately; every download is verified against a checksum.
ARG KUBECTL_VERSION=v1.37.0
ARG GH_VERSION=2.80.0
# Hermes Agent is released as GitHub tags only (PyPI stops at 0.19.0, and the
# project refuses wheel/sdist builds). The source archive is pinned by sha256
# and installed the way upstream does: a source checkout plus a venv synced
# from its own uv.lock, which pins every dependency by hash.
ARG HERMES_VERSION=0.23.3
ARG HERMES_SHA256=061442bf28ee1abd47ef7d6884c5fd906ee0f478b4c3813a5b899ba16544f150
# The commit the v${HERMES_VERSION} tag points at, for Hermes' install stamp.
ARG HERMES_COMMIT=caafe5389ae585ef77387a962aee05b1bcf17029

RUN apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates curl git jq ripgrep xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 65532 kagent \
    && useradd --uid 65532 --gid 65532 --home-dir /data --shell /usr/sbin/nologin kagent \
    && mkdir -p /data/hermes /data/work \
    && chown -R 65532:65532 /data

# kubectl, checksum-verified.
RUN set -eu; \
    cd /tmp; \
    curl -fsSLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl"; \
    echo "$(curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256")  kubectl" | sha256sum -c -; \
    install -m 0755 kubectl /usr/local/bin/kubectl; \
    rm kubectl

# gh, checksum-verified, installed off PATH; /usr/local/bin/gh is the wrapper
# that supplies the token (see gh-wrapper.sh).
RUN set -eu; \
    cd /tmp; \
    tarball="gh_${GH_VERSION}_linux_${TARGETARCH}.tar.gz"; \
    curl -fsSLO "https://github.com/cli/cli/releases/download/v${GH_VERSION}/${tarball}"; \
    curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_checksums.txt" | grep " ${tarball}\$" | sha256sum -c -; \
    mkdir -p /opt/gh; \
    tar -xzf "${tarball}" -C /opt/gh --strip-components=1; \
    rm "${tarball}"
COPY --chmod=0755 gh-wrapper.sh /usr/local/bin/gh
# git authenticates to GitHub through gh, so the token is only ever read
# from the file by the wrapper, never placed in git config or a remote URL.
RUN git config --system credential.https://github.com.helper '' \
    && git config --system --add credential.https://github.com.helper '!/usr/local/bin/gh auth git-credential' \
    && git config --system init.defaultBranch main

COPY --from=uv /uv /usr/local/bin/uv
RUN set -eu; \
    curl -fsSL -o /tmp/hermes.tar.gz "https://github.com/NousResearch/hermes-agent/archive/refs/tags/v${HERMES_VERSION}.tar.gz"; \
    echo "${HERMES_SHA256}  /tmp/hermes.tar.gz" | sha256sum -c -; \
    mkdir -p /opt/hermes; \
    tar -xzf /tmp/hermes.tar.gz -C /opt/hermes --strip-components=1; \
    rm /tmp/hermes.tar.gz; \
    cd /opt/hermes; \
    rm -rf apps contributors evals tests tests-js website; \
    UV_PROJECT_ENVIRONMENT=/opt/hermes/.venv UV_PYTHON_DOWNLOADS=never UV_NO_CACHE=1 \
        uv sync --frozen --no-dev --python /usr/local/bin/python \
        --extra acp --extra anthropic --extra mcp; \
    # The archive has no .git, so Hermes reads its version from this stamp
    # (hermes_cli/version_info.py), as in upstream's own image.
    /opt/hermes/.venv/bin/python scripts/write_install_stamp.py --output install-stamp.json \
        --commit "${HERMES_COMMIT}" --base-version "${HERMES_VERSION}" --display-version "${HERMES_VERSION}" \
        --source docker --distribution docker --update-mechanism external; \
    /opt/hermes/.venv/bin/hermes --version | grep -F "${HERMES_VERSION}"

COPY skills /opt/causely/skills
COPY --from=builder /out/causely-hermes /usr/local/bin/causely-hermes

ENV HERMES_HOME=/data/hermes \
    HOME=/data \
    PATH=/opt/hermes/.venv/bin:/usr/local/bin:/usr/bin:/bin
WORKDIR /data/work
USER 65532:65532

LABEL org.opencontainers.image.source="https://github.com/causely-oss/background-agents" \
      org.opencontainers.image.description="Causely investigator: Hermes Agent as a kagent BYO Harness" \
      org.opencontainers.image.version="$VERSION"

ENTRYPOINT ["/usr/local/bin/causely-hermes"]
