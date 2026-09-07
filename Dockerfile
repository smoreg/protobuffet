# syntax=docker/dockerfile:1

# The registry shells out to buf for compatibility checks. The binary comes
# from the official image rather than being compiled here: building buf from
# source needs more memory than a small CI runner has, and it is the same
# static binary either way.
FROM bufbuild/buf:1.72.0 AS buf

FROM golang:1.26.8-alpine AS build
ARG VERSION=dev
WORKDIR /src

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
COPY --from=buf /usr/local/bin/buf /usr/local/bin/buf

USER nonroot:nonroot
VOLUME /data
EXPOSE 8080
ENV BUFFET_DATA=/data BUFFET_LISTEN=:8080
ENTRYPOINT ["/usr/local/bin/buffetd"]
