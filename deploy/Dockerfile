# syntax=docker/dockerfile:1
ARG GO_IMAGE=golang:1.26.10-bookworm@sha256:5de7c99eb78ca5e515a4c340235d0955ec5964a5071c492c15503d7c267da473
FROM ${GO_IMAGE} AS cpa-build
RUN apt-get update && apt-get install -y --no-install-recommends python3 && rm -rf /var/lib/apt/lists/*
COPY deploy/collect_notices.py /collect_notices.py
WORKDIR /src/cpa
COPY cpa/go.mod cpa/go.sum ./
RUN go mod download
COPY cpa/ ./
ARG SOURCE_COMMIT=unknown
RUN CGO_ENABLED=1 go build -trimpath -buildvcs=false -ldflags="-X main.Version=8.0.75-bps.4 -X main.Commit=${SOURCE_COMMIT}" -o /out/CLIProxyAPI ./cmd/server && python3 /collect_notices.py /out/notices

FROM ${GO_IMAGE} AS plugin-build
RUN apt-get update && apt-get install -y --no-install-recommends python3 && rm -rf /var/lib/apt/lists/*
COPY deploy/collect_notices.py /collect_notices.py
WORKDIR /src/plugin
COPY plugin/go.mod plugin/go.sum ./
RUN go mod download
COPY plugin/ ./
RUN CGO_ENABLED=1 go build -trimpath -buildvcs=false -buildmode=c-shared -o /out/cpa-bps.so ./cmd/cpa-bps && go build -trimpath -buildvcs=false -o /out/bps-document-worker ./cmd/bps-document-worker && python3 /collect_notices.py /out/notices

FROM ${GO_IMAGE} AS manager-build
RUN apt-get update && apt-get install -y --no-install-recommends python3 && rm -rf /var/lib/apt/lists/*
COPY deploy/collect_notices.py /collect_notices.py
WORKDIR /src/manager
COPY manager/apps/manager-server/go.mod manager/apps/manager-server/go.sum ./
RUN go mod download
COPY manager/apps/manager-server/ ./
ARG SOURCE_COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-X github.com/seakee/cpa-manager-plus/apps/manager-server/internal/buildinfo.Version=v1.14.10-bps.1 -X github.com/seakee/cpa-manager-plus/apps/manager-server/internal/buildinfo.SourceCommit=${SOURCE_COMMIT}" -o /out/cpa-manager-plus ./cmd/cpa-manager-plus && python3 /collect_notices.py /out/notices

FROM python:3.12-slim-bookworm AS panel-build
COPY panel/base.html /panel/base.html
COPY plugin/panel/ /patch/
RUN python /patch/patch_manager.py --input /panel/base.html --output /panel/management.html

FROM python:3.12-slim-bookworm AS api
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata libstdc++6 fonts-noto-cjk && rm -rf /var/lib/apt/lists/*
RUN python -m pip install --no-cache-dir --disable-pip-version-check pypdfium2==5.12.1 xlrd==2.0.2
COPY --from=cpa-build /out/notices /opt/cpa-bps/notices/cpa-dependencies
COPY --from=plugin-build /out/notices /opt/cpa-bps/notices/plugin-dependencies
COPY --from=cpa-build /out/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI
COPY --from=plugin-build /out/cpa-bps.so /opt/cpa-bps/plugins/cpa-bps.so
COPY --from=plugin-build /out/bps-document-worker /opt/cpa-bps/bps-document-worker
COPY --from=panel-build /panel/management.html /opt/cpa-bps/management.html
COPY plugin/internal/bpsprotocol/LICENSE /opt/cpa-bps/notices/Basispoints-MIT.txt
COPY plugin/LICENSE-CLIProxyAPI /opt/cpa-bps/notices/CPA-MIT.txt
COPY panel/LICENSE /opt/cpa-bps/notices/Manager-MIT.txt
ENV TZ=Asia/Shanghai MANAGEMENT_STATIC_PATH=/opt/cpa-bps/management.html
WORKDIR /CLIProxyAPI
EXPOSE 8317
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=5 CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8317/', timeout=4).read(1024)"
ENTRYPOINT ["./CLIProxyAPI"]

FROM debian:bookworm-slim AS manager
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata wget && rm -rf /var/lib/apt/lists/*
COPY --from=manager-build /out/notices /opt/cpa-bps/notices/manager-dependencies
COPY --from=manager-build /out/cpa-manager-plus /usr/local/bin/cpa-manager-plus
COPY --from=panel-build /panel/management.html /opt/cpa-bps/management.html
COPY panel/LICENSE /opt/cpa-bps/LICENSE
ENV TZ=Asia/Shanghai PANEL_PATH=/opt/cpa-bps/management.html HTTP_ADDR=0.0.0.0:18317 USAGE_DB_PATH=/data/usage.sqlite USAGE_DATA_DIR=/data USAGE_COLLECTOR_MODE=http
WORKDIR /app
EXPOSE 18317
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=5 CMD wget -qO- http://127.0.0.1:18317/health || exit 1
ENTRYPOINT ["cpa-manager-plus"]
