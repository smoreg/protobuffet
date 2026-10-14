# syntax=docker/dockerfile:1

# The registry shells out to buf for compatibility checks. The binary comes
# from the official image rather than being compiled here: building buf from
# source needs more memory than a small CI runner has, and it is the same
# static binary either way.
FROM bufbuild/buf:1.72.0 AS buf

FROM golang:1.27.2-alpine AS build
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

# The data directory is created here so the final image carries it owned by
# the nonroot user. Left to the runtime, an anonymous volume is created as
# root and the server refuses to start, which is correct but unhelpful for
# anyone running the image straight from the README.
RUN mkdir -p /data-skeleton

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/buffetd /usr/local/bin/buffetd
COPY --from=build /out/buffet /usr/local/bin/buffet
COPY --from=buf /usr/local/bin/buf /usr/local/bin/buf
COPY --from=build --chown=65532:65532 /data-skeleton /data

USER nonroot:nonroot
VOLUME /data
EXPOSE 8080

# distroless has no shell and no curl, so the check runs the binary itself.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/buffet", "version"]
ENV BUFFET_DATA=/data BUFFET_LISTEN=:8080
ENTRYPOINT ["/usr/local/bin/buffetd"]
