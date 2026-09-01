# Production deployment

The launch topology is one Tencent Cloud Lighthouse instance running Caddy,
gateway, worker, and the shared passwordless admin service. TDSQL-C MySQL and
TencentDB for Redis remain private managed services connected through CCN. TOS
and Ark remain managed Volcengine services.

Use [`tencent/README.md`](tencent/README.md) for provisioning, DNS, TLS,
migration, maintenance, acceptance, and scaling instructions.

The application-container topology and operational commands live in
[`single-server/README.md`](single-server/README.md). MySQL and Redis are not
part of the production Compose project.

[`../Contracts/HTTP/HealthAPI/openapi.yaml`](../Contracts/HTTP/HealthAPI/openapi.yaml)
is the canonical Health public HTTP contract. It generates the Gin router and
Swift client; it is not imported into an API Gateway for this topology. Journal
currently exposes only its fixed, typed `journal.organize` transport.
