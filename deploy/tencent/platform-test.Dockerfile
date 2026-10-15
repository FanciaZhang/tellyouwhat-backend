# syntax=docker/dockerfile:1.7
FROM golang:1.26.6-bookworm AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOMAXPROCS=1 GOMEMLIMIT=512MiB GOFLAGS=-p=1
WORKDIR /src
COPY go.mod go.sum ./
RUN GOPROXY="${GOPROXY}" go mod download
COPY . .
# Reuse the compiler cache across the isolated platform service images.
RUN set -e; for service in gateway worker migrate servicecheck; do \
      CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/${service} ./cmd/${service}; \
    done

FROM scratch AS runtime
LABEL cn.tellyouwhat.operations-automation="1" cn.tellyouwhat.lifecycle="1"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /out/servicecheck /servicecheck
COPY deploy/schema-manifest.json /config/schema-manifest.json
COPY deploy/Apple_App_Attestation_Root_CA.pem /config/Apple_App_Attestation_Root_CA.pem
COPY deploy/apple-app-store-roots.pem /config/apple-app-store-roots.pem
ENV HEALTH_SCHEMA_MANIFEST_PATH=/config/schema-manifest.json APP_ATTEST_ROOT_PEM_PATH=/config/Apple_App_Attestation_Root_CA.pem
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/service"]

FROM runtime AS gateway
COPY --from=build /out/gateway /service
FROM runtime AS worker
COPY --from=build /out/worker /service
FROM runtime AS migrate
COPY --from=build /out/migrate /service
