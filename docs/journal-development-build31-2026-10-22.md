# Journal development admission and read-aloud recovery

The shared test platform deployed `ea1c005b08b0049913a1c645ba54f14538e474fb` from `c512264f544b4d14becbb3a29c4869299cccf770`. The only runtime change added Journal development build 31 to `JOURNAL_ALLOWED_APP_BUILDS`; the four App readiness checks, registered cleanup and authenticated offsite backup/readback succeeded. Production selection and Caddy hashes were unchanged. The published voice catalogue retained 466 entries.

On the physical development App 1.0 (31), a real Apple Sandbox monthly purchase and separate resynchronization/restore produced successful transaction-sync requests. Its accelerated expiry is independent of whether an already synthesized paragraph remains available locally.

The subsequent resume failure is a client receipt-state problem: request `1fc19fd1-6742-4cbf-9805-7ca5a6ac4c39` was locally cancelled on October 21, reused for successful server synthesis on October 22, but never cached because the local completion path only accepted pending receipts. Later status requests correctly reported that the transient server buffer was unavailable. Fixing the App must preserve task identity during recovery and request explicit confirmation before new synthesis when old audio is truly unavailable. No entitlement grant or database edit is needed.

Build 32 admission is the next runtime-only change for verifying that client repair. It must use the registered shared-platform producer and retain the existing resources, administrator state, production selection and voice catalogue. Admission alone is not device acceptance.
