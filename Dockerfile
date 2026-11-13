ARG BUILDER_IMAGE=golang:1.26.11-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM --platform=$BUILDPLATFORM ${BUILDER_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
ENV GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 GOMAXPROCS=2 GOFLAGS=-p=2
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY api/ ./api/
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY pkg/ ./pkg/
COPY migrations/ ./migrations/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /out/root/app/bin && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -mod=readonly -trimpath \
      -ldflags="-s -w -X main.Name=ani-resource-service -X main.Version=${VERSION}" \
      -o /out/root/app/bin/server ./cmd/ani-resource-service && \
    ln -s server /out/root/app/bin/ani-resource-service && \
    ln -s app/bin/server /out/root/ani-resource-service

FROM ${RUNTIME_IMAGE} AS service
ARG VERSION=dev
ARG SOURCE_REVISION
ARG SOURCE_SNAPSHOT
LABEL org.opencontainers.image.title="ani-resource-service" \
      org.opencontainers.image.source="https://github.com/zhangzhe-ctrl/ani-resource-service" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${SOURCE_REVISION}" \
      io.ani.source-snapshot="${SOURCE_SNAPSHOT}"
WORKDIR /app
# Keep the two existing deployment command paths as aliases of the common entry.
COPY --from=build --chmod=0555 /out/root/ /
COPY --chmod=0444 THIRD_PARTY_NOTICES.go-kratos-layout.txt /app/notices/go-kratos-layout.txt
USER 10001:10001
EXPOSE 19090 19091 9446
ENTRYPOINT ["/app/bin/server"]
CMD ["-conf", "/app/configs"]
