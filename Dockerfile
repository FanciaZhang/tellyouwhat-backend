# syntax=docker/dockerfile:1.7
FROM golang:1.26.6-bookworm AS build

ARG SERVICE=gateway
ARG GOPROXY=https://proxy.golang.org,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN GOPROXY="${GOPROXY}" go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/service ./cmd/${SERVICE}

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/service /service
COPY deploy/schema-manifest.json /config/schema-manifest.json
COPY deploy/Apple_App_Attestation_Root_CA.pem /config/Apple_App_Attestation_Root_CA.pem
ENV SCHEMA_MANIFEST_PATH=/config/schema-manifest.json
ENV APP_ATTEST_ROOT_PEM_PATH=/config/Apple_App_Attestation_Root_CA.pem
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/service"]
