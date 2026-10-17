# Build the stub agent as a static Go binary, then ship it inside the
# python toolchain image. Rebuild if you change stub.go or the toolchain.
# Worker image version: bump the minifactory.worker-version LABEL below
# and workerImageVersion in workerimage.go together whenever this file
# changes; `minifactory init` rebuilds images whose label does not match
# and `minifactory doctor` reports them stale.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY stub.go .
RUN CGO_ENABLED=0 go build -trimpath -o /stub_agent stub.go

FROM python:3.12-slim

LABEL minifactory.worker-version="1"

# Node 24 LTS via NodeSource: Debian's nodejs is too old for the agent
# CLIs (@anthropic-ai/claude-code requires node >=22.0.0). NodeSource's
# nodejs bundles npm, so the distro `npm` package is not installed.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git gnupg \
    && curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir pytest

# Vendor agent CLIs: real agents (claude/codex/opencode) run inside this
# sandbox, so the image carries them. Rebuild the image
# (docker build -t factory-worker-go worker/) to pick up new CLI versions.
RUN npm install -g @anthropic-ai/claude-code @openai/codex opencode-ai

COPY --from=build /stub_agent /opt/stub_agent

WORKDIR /work
CMD ["sh"]
