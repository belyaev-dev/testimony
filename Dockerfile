# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.24.0
ARG ALLURE2_VERSION=2.38.1

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags='-s -w' -o /out/testimony ./cmd/testimony

FROM debian:bookworm-slim AS runtime
ARG ALLURE2_VERSION
ARG APP_UID=10001
ARG APP_GID=10001

LABEL org.opencontainers.image.title="testimony" \
      org.opencontainers.image.description="Testimony server with the Allure 2 + JRE runtime" \
      org.opencontainers.image.vendor="testimony-dev" \
      org.opencontainers.image.source="https://github.com/testimony-dev/testimony"

ENV TESTIMONY_SERVER_HOST=0.0.0.0 \
    TESTIMONY_SERVER_PORT=8080 \
    TESTIMONY_SQLITE_PATH=/var/lib/testimony/data/testimony.sqlite \
    TESTIMONY_TEMP_DIR=/var/lib/testimony/tmp \
    TESTIMONY_GENERATE_VARIANT=allure2 \
    TESTIMONY_GENERATE_CLI_PATH=/usr/local/bin/allure

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        tini \
        unzip \
        openjdk-17-jre-headless \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL -o /tmp/allure.zip "https://repo.maven.apache.org/maven2/io/qameta/allure/allure-commandline/${ALLURE2_VERSION}/allure-commandline-${ALLURE2_VERSION}.zip" \
    && unzip -q /tmp/allure.zip -d /opt \
    && ln -s "/opt/allure-${ALLURE2_VERSION}/bin/allure" /usr/local/bin/allure \
    && rm -f /tmp/allure.zip

RUN groupadd --gid "${APP_GID}" testimony \
    && useradd --uid "${APP_UID}" --gid testimony --create-home --home-dir /home/testimony --shell /usr/sbin/nologin testimony \
    && install -d -o testimony -g testimony /var/lib/testimony/data /var/lib/testimony/tmp

COPY --from=builder /out/testimony /usr/local/bin/testimony

USER testimony:testimony
WORKDIR /var/lib/testimony

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/bin/sh", "-c", "curl --fail --silent http://127.0.0.1:${TESTIMONY_SERVER_PORT}/healthz >/dev/null || exit 1"]

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/testimony"]
