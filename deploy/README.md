# Production deployment

The launch topology is one Tencent Cloud Lighthouse instance running Caddy,
gateway, and worker. TDSQL-C MySQL and TencentDB for Redis remain private
managed services connected through CCN. TOS and Ark remain managed Volcengine
services.

Use [`tencent/README.md`](tencent/README.md) for provisioning, DNS, TLS,
migration, maintenance, acceptance, and scaling instructions.

The previous all-in-one topology remains available in
[`single-server/README.md`](single-server/README.md). Its MySQL and Redis
services use the `local-storage` Compose profile.

[`../Contracts/HTTP/HealthAPI/openapi.yaml`](../Contracts/HTTP/HealthAPI/openapi.yaml)
is the canonical public HTTP contract. It generates the Gin router and client
SDKs; it is not imported into an API Gateway for the Lighthouse topology.
