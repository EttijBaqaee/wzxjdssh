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
RUN go mod verify && go test -mod=readonly -race -c -o /verify.test . && CGO_ENABLED=0 go build -mod=readonly -o /wzxjdssh-test .
RUN chown -R 1000:1000 /src
ENV HOME=/tmp GOCACHE=/tmp/go-build WZX_TEST_BINARY=/wzxjdssh-test
USER 1000:1000
CMD ["sh", "-c", "cp go.mod go.sum /results/; /verify.test -test.v -test.timeout=180s > /results/go-test.log 2>&1; rc=$?; cat /results/go-test.log; printf '%s\\n' \"$rc\" > /results/exit-code; exit \"$rc\""]

FROM source AS builder
ARG VERSION=1.0.2
RUN go mod verify && CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /wzxjdssh . && mkdir -m 0700 /empty-data

FROM scratch AS runtime
ARG VERSION=1.0.2
LABEL org.opencontainers.image.title="wzxjdssh" org.opencontainers.image.version="${VERSION}" org.opencontainers.image.description="SSH transport honeypot with delayed authentication and optional banner tarpit"
COPY --from=builder /wzxjdssh /wzxjdssh
COPY --from=builder --chown=65532:65532 /empty-data /data
COPY configs/wzxjdssh.conf /etc/wzxjdssh/config
ENV WZXJDSSH_CONFIG=/etc/wzxjdssh/config
USER 65532:65532
VOLUME ["/data"]
EXPOSE 2222/tcp
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 CMD ["/wzxjdssh", "-health"]
ENTRYPOINT ["/wzxjdssh"]
