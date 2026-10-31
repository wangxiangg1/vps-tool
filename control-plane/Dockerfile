# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG TARGETOS
ARG TARGETARCH
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /src
COPY control-plane/go.mod control-plane/go.sum ./control-plane/
RUN cd control-plane && go mod download
COPY web ./web
COPY control-plane ./control-plane
RUN rm -rf control-plane/web/* && cp -a web/. control-plane/web/
RUN cd control-plane && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w -buildid=' -o /out/vps-server ./cmd/vps-server

FROM alpine:3.20
RUN addgroup -S app && adduser -S -G app app && mkdir -p /data && chown app:app /data
COPY --from=build /out/vps-server /usr/local/bin/vps-server
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
USER app
WORKDIR /app
ENV VPS_TOOL_DB_PATH=/data/vps-tool.sqlite3
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["/usr/local/bin/vps-server", "-health"]
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/vps-server"]
