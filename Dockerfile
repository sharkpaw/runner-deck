# Manager: multi-stage build, built on BUILDPLATFORM and cross-compiled for TARGETOS/TARGETARCH
ARG BUILDPLATFORM
FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY scripts ./scripts
ARG VERSION=dev
# Empty when not passed; version-kit leaves empty fields out of its output
ARG COMMIT=
ARG BUILD_DATE=
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags "-X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildDate=${BUILD_DATE}" -o runner-manager ./cmd/runner-manager

FROM ubuntu:24.04
LABEL org.opencontainers.image.title="Runner Deck Manager" \
      org.opencontainers.image.description="GitHub Actions runner manager"

# The package list lives in scripts/apt-packages.txt, shared by the Manager and Runner images.
COPY scripts/apt-packages.txt /tmp/apt-packages.txt
RUN apt-get update \
    && awk '!/^[[:space:]]*#/ && NF' /tmp/apt-packages.txt \
       | xargs -r apt-get install -y --no-install-recommends \
    && rm -rf /var/lib/apt/lists/* /tmp/apt-packages.txt
RUN install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod 644 /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" > /etc/apt/sources.list.d/docker.list \
    && apt-get update && apt-get install -y --no-install-recommends docker-ce-cli \
    && rm -rf /var/lib/apt/lists/*

# Without container mode runner processes live in this container; with it the Manager creates
# runner containers through the host's Docker. Both need the mounted docker.sock, so app (UID 1001)
# must be in the socket's group. Defaults to 999; override with --build-arg DOCKER_GID=<host GID>
# or group_add in compose.
ARG DOCKER_GID=999
RUN groupadd -g 1001 app && useradd -r -u 1001 -g app -d /app -s /bin/bash app \
    && if ! getent group "${DOCKER_GID}" >/dev/null; then groupadd -g "${DOCKER_GID}" docker; fi \
    && usermod -aG "$(getent group "${DOCKER_GID}" | cut -d: -f1)" app
# Hosted runners offer passwordless sudo and many workflows call `sudo apt-get install -y ...`.
# Installing sudo without sudoers would leave a command that exists but cannot be used.
# Turn it off with --build-arg ALLOW_SUDO=false when you need a stricter boundary.
#
# This does not really widen what a job can do: app already owns the work directory. The real
# boundary is the container (use runners.resources in container mode) and whether the host's
# docker.sock is mounted.
ARG ALLOW_SUDO=true
RUN if [ "${ALLOW_SUDO}" = "true" ]; then \
        echo "app ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/runner-deck-app \
        && chmod 0440 /etc/sudoers.d/runner-deck-app \
        && visudo -c -f /etc/sudoers.d/runner-deck-app; \
    fi

# tini as PID 1 does two things the Go process cannot. It reaps orphans: `cmd &` in a job,
# daemonized tools and the Gradle daemon get handed to PID 1, and Go only waits for the child it
# started, so the rest become zombies. Runners live long, so zombies pile up across jobs and use
# up runners.resources.pids_limit, until fork fails with "Resource temporarily unavailable".
# It also forwards SIGTERM to the direct child.
# Not listed in scripts/apt-packages.txt: that list mirrors the tools jobs get from
# actions/runner-images, while tini is part of the runtime.
RUN apt-get update \
    && apt-get install -y --no-install-recommends tini \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/runner-manager .
COPY config.yaml.example ./config/config.yaml
COPY scripts/install-runner.sh /app/scripts/install-runner.sh
RUN sed -i 's|base_path: ./runners|base_path: /app/runners|' config/config.yaml \
    && mkdir -p /app/runners \
    && chmod +x /app/scripts/install-runner.sh && chown -R app:app /app

USER app
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/usr/bin/tini", "--", "./runner-manager"]
CMD ["-config", "/app/config/config.yaml"]
