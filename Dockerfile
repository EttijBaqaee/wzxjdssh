ARG GO_IMAGE=golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb
FROM ${GO_IMAGE} AS dependencies
WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
RUN go mod download all && go mod verify

FROM dependencies AS source
COPY . .

FROM dependencies AS verify-dependencies
RUN sed -i 's|https://dl-cdn.alpinelinux.org/alpine|https://mirrors.aliyun.com/alpine|g' /etc/apk/repositories && apk add --no-cache gcc musl-dev openssh-client python3
RUN addgroup -g 1000 verify && adduser -D -u 1000 -G verify -h /tmp/verify verify && mkdir -p /results

FROM verify-dependencies AS verify
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build go mod verify && go test -mod=readonly -race -c -o /verify.test . && CGO_ENABLED=0 go build -mod=readonly -o /wzxjdssh-test .
RUN chown -R 1000:1000 /src
ENV HOME=/tmp GOCACHE=/tmp/go-build WZX_TEST_BINARY=/wzxjdssh-test
USER 1000:1000
CMD ["sh", "-c", "cp go.mod go.sum /results/; /verify.test -test.v -test.timeout=180s > /results/go-test.log 2>&1; rc=$?; cat /results/go-test.log; printf '%s\\n' \"$rc\" > /results/exit-code; exit \"$rc\""]

FROM source AS builder
ARG VERSION=2.0.5
RUN --mount=type=cache,target=/root/.cache/go-build go mod verify && CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /wzxjdssh . && mkdir -m 0700 /empty-data

FROM verify-dependencies AS ui-verify
RUN apk add --no-cache chromium font-noto-cjk nodejs npm && npm install --prefix /opt/browser --registry=https://registry.npmmirror.com playwright-core@1.56.1
ENV NODE_PATH=/opt/browser/node_modules HOME=/tmp
USER 1000:1000

FROM scratch AS runtime
ARG VERSION=2.0.5
ARG VCS_REF=unknown
LABEL org.opencontainers.image.revision="${VCS_REF}"
LABEL org.opencontainers.image.title="wzxjdssh" org.opencontainers.image.version="${VERSION}" org.opencontainers.image.licenses="MIT" org.opencontainers.image.source="https://github.com/EttijBaqaee/wzxjdssh" org.opencontainers.image.description="SSH transport honeypot with delayed authentication, SQLite records and Web UI"
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /wzxjdssh /wzxjdssh
COPY --from=builder --chown=65532:65532 /empty-data /data
COPY --from=builder --chown=65532:65532 /empty-data /var/lib/wzxjdssh
COPY configs/wzxjdssh.conf /etc/wzxjdssh/config
COPY LICENSE THIRD-PARTY-NOTICES.txt /licenses/
ENV WZXJDSSH_CONFIG=/etc/wzxjdssh/config
USER 65532:65532
VOLUME ["/data", "/var/lib/wzxjdssh"]
EXPOSE 2222/tcp 2223/tcp
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 CMD ["/wzxjdssh", "-health"]
ENTRYPOINT ["/wzxjdssh"]
