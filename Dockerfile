# syntax=docker/dockerfile:1

# The registry shells out to buf for compatibility checks, so buf is built
# from source here rather than pulled from a release tarball: one toolchain,
# no extra download at runtime, and it stays a static binary.
FROM golang:1.26.8-alpine AS build
ARG VERSION=dev
WORKDIR /src

RUN --mount=type=cache,target=/go/pkg/mod \
    go install github.com/bufbuild/buf/cmd/buf@v1.72.0

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/buffetd ./cmd/buffetd && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/buffet ./cmd/buffet

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/buffetd /usr/local/bin/buffetd
COPY --from=build /out/buffet /usr/local/bin/buffet
COPY --from=build /go/bin/buf /usr/local/bin/buf

USER nonroot:nonroot
VOLUME /data
EXPOSE 8080
ENV BUFFET_DATA=/data BUFFET_LISTEN=:8080
ENTRYPOINT ["/usr/local/bin/buffetd"]
