ARG GO_IMAGE=golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb
FROM ${GO_IMAGE} AS dependencies
WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
RUN go mod download all && go mod verify

FROM dependencies AS source
COPY . .

FROM source AS builder
ARG VERSION=2.3.1
RUN --mount=type=cache,target=/root/.cache/go-build go mod verify && CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /wzxjdssh . && mkdir -m 0700 /empty-data

FROM scratch AS runtime
ARG VERSION=2.3.1
ARG VCS_REF=unknown
LABEL org.opencontainers.image.revision="${VCS_REF}"
LABEL org.opencontainers.image.title="wzxjdssh" org.opencontainers.image.version="${VERSION}" org.opencontainers.image.licenses="AGPL-3.0-only" org.opencontainers.image.source="https://github.com/EttijBaqaee/wzxjdssh" org.opencontainers.image.description="SSH transport honeypot with delayed authentication, SQLite records and Web UI"
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /wzxjdssh /wzxjdssh
COPY --from=builder --chown=65532:65532 /empty-data /data
COPY --from=builder --chown=65532:65532 /empty-data /var/lib/wzxjdssh
COPY configs/wzxjdssh.conf /etc/wzxjdssh/config
COPY LICENSE THIRD-PARTY-NOTICES.txt /licenses/
ENV WZXJDSSH_CONFIG=/etc/wzxjdssh/config
ENV SQLITE_TMPDIR=/var/lib/wzxjdssh
USER 65532:65532
VOLUME ["/data", "/var/lib/wzxjdssh"]
EXPOSE 2222/tcp 2223/tcp
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=3s --start-period=300s --retries=3 CMD ["/wzxjdssh", "-health"]
ENTRYPOINT ["/wzxjdssh"]
