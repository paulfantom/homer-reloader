# Based on https://actuated.dev/blog/multi-arch-docker-github-actions

FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.23-alpine AS builder
ARG TARGETPLATFORM
ARG BUILDPLATFORM
ARG TARGETOS
ARG TARGETARCH

WORKDIR /go/src/github.com/paulfantom/reloader

COPY .  .

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
  go build -ldflags "-s -w" \
  -a -o /usr/bin/reloader .

#FROM --platform=${BUILDPLATFORM:-linux/amd64} gcr.io/distroless/static:nonroot
FROM --platform=${BUILDPLATFORM:-linux/amd64} alpine:3.18.0

LABEL org.opencontainers.image.source=https://github.com/paulfantom/homer-reloader

WORKDIR /
COPY --from=builder /usr/bin/reloader /
#USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/reloader"]