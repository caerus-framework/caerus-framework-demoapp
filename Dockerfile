# Multi-stage image for "does the binary boot in a container?" checks.
# Day-to-day DX is still host `make run` against Compose deps — see README.

FROM golang:1.26-alpine AS build
WORKDIR /src

# Workspace siblings are expected next to this module when building from the
# monorepo root context. Build with:
#   docker build -f caerus-framework-demoapp/Dockerfile -t caerus-demoapp .
# from the workspace root (so replace directives resolve).

COPY caerus-framework ./caerus-framework
COPY caerus-framework-configuration ./caerus-framework-configuration
COPY caerus-framework-logs ./caerus-framework-logs
COPY caerus-framework-observability ./caerus-framework-observability
COPY caerus-framework-http ./caerus-framework-http
COPY caerus-framework-postgresql ./caerus-framework-postgresql
COPY caerus-framework-valkey ./caerus-framework-valkey
COPY caerus-framework-vpq ./caerus-framework-vpq
COPY caerus-framework-demoapp ./caerus-framework-demoapp

WORKDIR /src/caerus-framework-demoapp
RUN CGO_ENABLED=0 go build -o /demoapp ./cmd/demoapp

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /demoapp /usr/local/bin/demoapp
COPY caerus-framework-demoapp/config /config
ENV DEMOAPP_CONFIG_DIR=/config
EXPOSE 9090 8081
ENTRYPOINT ["demoapp"]
CMD ["serve"]
