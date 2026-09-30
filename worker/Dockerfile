# Build the stub agent as a static Go binary, then ship it inside the
# python toolchain image. Rebuild if you change stub.go or the toolchain.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY stub.go .
RUN CGO_ENABLED=0 go build -trimpath -o /stub_agent stub.go

FROM python:3.12-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        git nodejs npm \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir pytest

COPY --from=build /stub_agent /opt/stub_agent

WORKDIR /work
CMD ["sh"]
