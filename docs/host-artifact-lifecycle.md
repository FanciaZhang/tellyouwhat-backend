# Shared-host retention and recovery

The 2026-10-13 disk alert came from repeated deployments retaining their outputs. CI produced production images; Journal development produced one Linux binary per start; Birthday and Albums test builds added their own image families. No producer had a complete host-retention step. The scheduled maintenance job also collided with the deployment reconciler's operations lock and exited instead of waiting.

## Ownership and retention

| Producer | Required entry point | Retention |
| --- | --- | --- |
| Production CI | `release.py deploy` | Current and verified previous slot, immutable image digests, active candidate/recovery references |
| Shared Journal recording processing | Production `release.py deploy` or owned platform-test deployment provisions its mount; gateway and `cmd/maintenance` own content retention | Private environment directory outside slots; 24-hour task lifetime, active execution locks protected; deletion fences and lock files retained. Not a disposable deployment artifact or configuration-backup input |
| Journal development | JournalApp `scripts/journal-development.py` | Authenticated current service and stopped previous service, matching binary and base image |
| Birthday development | Owned producer invocation below | Registered current and previous images |
| Albums test | Owned producer invocation below | Registered current and previous images; schema compatibility must be checked separately |
| Albums production | First data/worker creation uses `release.py deploy --albums-resources <SHA> --source-run <CI run>`; shared API uses the normal release; direct Compose activation is prohibited | Owner `albums-production`; current/previous `tellyouwhat-albums-production-*` and matching `.albums-production/releases/<SHA>` selections |
| Platform test | Owned producer invocation with `--owner platform-test` | Registered current and previous `tellyouwhat-platform-test-*` images and matching configuration |
| Apple development delivery | Owned producer invocation with `--owner apple-development` | Current/previous `tellyouwhat-apple-development:*` and matching configuration; IPA/grant expiry is collected separately by this owner's registered image |
| Host maintenance | `operations.py maintenance` | Removes scoped retired artifacts older than 48 hours OR outside the newest five versions per owner; build cache target 2 GB |
| Host journal | systemd-journald | 512 MB, 14 days, with 1 GB free-space reservation |

An artifact expires after 48 hours OR when its revision falls outside the newest five versions of its owner. Service images sharing a revision count as one version. Release directories and compiler-cache generations each use the same five-version/time boundary. Live and stopped-container references, rollback configuration, and owner registrations take precedence. Unmanaged images, source, credentials, volumes and state are preserved. Registration records contain owner, source revision/run, paths and image references; repeating the same deployment does not overwrite its previous selection.

Full image inventory includes legacy intermediate images. Known platform/Albums compiler commands establish build ownership; untagged outputs inherit owner and revision from a matching source-tar hash. Their unreferenced parent chains are removed child-first only when every child is also selected. Unknown images and shared parents remain. Historical lifecycle-labelled outputs without recoverable owner/revision metadata are adopted only by full maintenance after 48 hours; their age cannot be replaced by a guessed version rank. Deletion rechecks the plan and aliases and never forces image removal or implicitly prunes parents.

Each producer must finish its own cleanup before claiming completion. Nightly maintenance repairs missed cleanup and reports partial failure. Disk capacity alone does not mean a backup or a deployment succeeded. New production releases require at least 8 GiB and 20 percent disk space free.

For Birthday and Albums, first register the already verified current selection. Subsequent producers use the shared lock order and register only after readiness succeeds:

Run the wrapper and registration as the backend directory owner (`ubuntu`), including when calling the Python functions directly. A different effective user is rejected before the producer runs or any recovery registration is replaced. Root-owned mode-600 records prevent the scheduled operator from reading the protected selections.

```sh
python3 /opt/tellyouwhat/backend/deploy/tencent/ops_artifacts.py \
  --apply --register --owner albums-test --source "$SOURCE_SHA" --source-run "$RUN_ID" \
  --image "tellyouwhat-albums-test-albumgateway:$SOURCE_SHA" \
  --image "tellyouwhat-albums-test-albumworker:$SOURCE_SHA" \
  --image "tellyouwhat-albums-test-migrate:$SOURCE_SHA" \
  --verify-url http://127.0.0.1:18090/readyz \
  --run bash /opt/tellyouwhat/albums-test/deploy-verified.sh
```

`deploy-verified.sh` is the producer's reviewed command, not an existing bundled script. It must drain schema-incompatible processes, migrate only its own test database, and recover its prior service on failure. Use Birthday's image and `http://127.0.0.1:18788/readyz` for that owner. The wrapper does not make an old application compatible with a newer database schema.

The independent `compose.platform-test.yaml` stack uses owner `platform-test` and readiness `http://127.0.0.1:18790/readyz`. Install the reviewed operations version that recognizes this owner before running its producer. Use the wrapper as `ubuntu`; do not reuse the production release entry point to start a test stack. Registration protects its current/previous images and configuration, while volumes and credentials remain outside disposable cleanup. Its default subnet is `172.29.93.0/24`; `172.29.92.0/24` is already used by Albums development. The actual host parser and all existing network ranges were checked in `docs/evidence/platform-test-network-2026-10-16.json`. The Compose file and owner support alone do not prove backup or deployment acceptance.

`platform_test_deploy.py --source <full SHA>` bootstraps the selected test gateway, MySQL and Redis. It accepts private source/configuration hashes under `.platform-test/releases/<SHA>`, uses the exact source's memory-bounded Dockerfile, and validates `.dev` identities, production App Attest, the test subnet and independent credentials before any build. Retained Birthday-only selections start just the gateway. The new maintenance topology requires Health's isolated test media credentials and worker; Journal admission additionally checks its existing restricted resources. Readiness checks every selected Host, worker lifecycle and applicable recording mount. Existing data refuses reinitialization. `--update-from <exact current SHA>` preserves database/Redis credentials, verifies an authenticated offsite recovery selection, and restores the matched previous process/configuration on failure; only the explicit transitions below are admitted. This does not prove cloud IAM isolation or real device registration.

The shared backup task includes a separate `platform_test_artifacts.py` checkpoint after this stack is registered. It reuses the existing local-first control-plane row allowlist, with a test-only SELECT account and an independent SQL encryption key. The encrypted SQL/manifest and source-bound configuration travel inside the existing authenticated COS recovery bundle; its inner key stays in `/opt/tellyouwhat/platform-test/backup.env` and separate restricted escrow. App identities, purchases, AI requests and media rows are deliberately not restored, matching the existing production retention policy. An isolated restore and actual timer evidence remain separate acceptance requirements.

Platform updates compare every selected migration and the actual applied names. The only presently reviewed additive transition is `0065_journal_illustration_jobs.sql` at its pinned SHA-256 in `platform_test_schema.py`. After this migration, old API recovery retains the new table; the state records its actual schema even if the candidate fails. Never roll back this table or restore the control-plane-only checkpoint over live test data. The only reviewed topology transition is the exact Compose pair in `platform_test_storage.py`, adding the private development recording mount and maintenance service; unknown changes fail closed. Deploy a mounted Journal baseline with generation disabled before enabling recordings, so rollback retains deletion and retention capability.

The existing host maintenance operation runs the registered test selection's maintenance image independently of API availability. Its image is part of `platform-test` current/previous registration, and its mount/configuration is already included in the selected source/Compose backup. A test cleanup failure is reported as overall maintenance failure without skipping other cleanup. No new scheduler or backup copy of temporary recording content is introduced. A real timer run and real mounted-container acceptance remain deployment gates.

After the four test hosts point exclusively to the shared gateway, run `python3 deploy/tencent/retire_test_apis.py --source <active-shared-test-SHA>` as the backend owner. Install the matched scripts through the normal production release first. This registered platform producer stops only the named old Journal, two Birthday and development Albums API containers; it preserves their containers, images, files and volumes, and leaves Albums data services and media workers running. The older Albums stack still serving the formal domain is outside this operation.

Retirement checks active proxy routes, the shared runtime and the old containers' registered image IDs. Authenticated offsite recovery is required before and after stopping; failure restarts only the previously running targets. The Journal recovery reader validates the pinned configuration and ledger for stopped containers as well, so scheduled backup does not require keeping an unused API running. A successful retirement is recorded in `.operations/test-api-retirement.json`; the owner wrapper then performs scoped artifact cleanup. Real retirement and the next scheduled backup cycle remain separate acceptance checks.

Apple development delivery uses owner `apple-development`, readiness `http://127.0.0.1:18791/readyz`, and private data at `.apple-delivery/data`. Its recovery state must bind the cleanup image to the current owner registration. Scheduled operations report expired lease/package cleanup failures; dry-run remains read-only. Installation links are credentials and must not appear in proxy logs. See [Apple development delivery](apple-development-delivery.md) for the supported transport and remaining deployment acceptance.

Its `apple_delivery_deploy.py` entry point invokes that same owner wrapper, checks file hashes and operator ownership, and restores the prior image/configuration if activation fails. Retired source-bound directories under `.apple-delivery/releases/<40-hex-source>` are collected after 48 hours or outside the newest five versions unless protected by current/previous or a live container reference. The private data directory and unknown files are excluded. Encrypted offsite configuration snapshots include both protected service binaries and their matched configurations; authenticated per-file hashes are verified before recovery.

## Albums production recovery preparation

`albums_artifacts.py` and `albums_checkpoint.py` now serve both Albums environments; the shared backup task includes `recovery_albums_production` only when its registered state exists. Production uses `albums_production`, SELECT-only account `albums_production_backup`, container `tellyouwhat-albums-production-mysql-1`, and private `/opt/tellyouwhat/albums-production/` inputs. Development keeps its existing test database, wrapper, account and archive section. A wrapper or database account from the other environment is rejected.

The production section preserves source-bound current and previous configuration, required login keys, enabled subscription/push/share credentials, an encrypted full Albums SQL snapshot and the deletion ledger. Inner SQL decryption keys remain in separate restricted escrow; they are omitted from the recovery bundle. Missing required keys, modified files or unregistered selections block backup. Outer recovery archives remain bounded at 512 MiB to accommodate the independent source selections and encrypted database checkpoints; individual source archives and checkpoints retain their 64/32 MiB limits. This is a bounded small-service implementation, not evidence that larger datasets have passed recovery.

Owner cleanup includes only retired production images and source-bound release directories after 48 hours or outside the newest five versions. It preserves current/previous selections and container references, and does not collect volumes, credentials, state or the development stack. The development deployment wrapper cannot activate this production owner. Production deployment, actual database grants, initial offsite backup/restore and scheduled-run acceptance remain pending; this code has not been installed on the host.

## Backups and deployment

The release entry point holds the operations lock across backup authentication and migration and records backup filename/hash against the exact release and source run. A failed export, authentication, COS upload or readback blocks migration. The lightweight slot reconciler uses only the deployment lock, so it can continue during a scheduled backup. Backup, maintenance and restore jobs wait up to 10 minutes and retry bounded failures twice at two-minute intervals.

Production releases use a production-only checkpoint: the shared production SQL policy, current/previous runtimes and any registered production Albums storage remain covered. Independent test, development and Apple-delivery checkpoints and test-routing validation belong to the scheduled whole-host backup. A broken independent test selection cannot block a production application checkpoint. Production checkpoint and offsite receipts are separate from daily backup receipts, so deployment cannot hide a failed scheduled backup.

`ops_common.install_operations` updates shared operations tooling while retaining registered test producers' `platform_test_*.py` and `test_routes.py` in place. Production packaging never assembles a mixed staged toolchain or replaces those independently installed readers. Test producers must use a reviewed source compatible with the installed shared operations API; earlier development branches do not authorize replacing shared backup policy.

Database export keeps every table schema and only the established control-plane row allowlist. It does not reintroduce App identities, consent, entitlements, AI/media tasks or private App data. See `data-retention.md` for the independent native-database snapshot boundary. Production database restoration is never automatic.

Offsite configuration is `/opt/tellyouwhat/backup/cos.env` (operator owned, mode 600). After activation, `.operations/offsite-required` makes a missing config/SDK an error. The dedicated private bucket is `tellyouwhat-backups-1251995834`, prefix `backups/`, with 14-day expiry. The account's [policy](../deploy/tencent/backup-cos/policy.json) contains object upload/read/head and bucket ACL/lifecycle reads. It cannot delete objects, change policy or retention, create buckets, or access other buckets.

Each offsite snapshot consists of encrypted SQL, an encrypted configuration bundle for current/previous image selections, and the authenticated manifest uploaded last. Every object is read back and compared before the snapshot is successful. Configuration includes environment, signing material, isolated service credentials and operations scripts. The decryption key must be retained separately from the archive. Existing image digests remain in the registry and current/previous images remain on the host; the configuration bundle is not a Docker image export.

Managed test routing adds the actual native Caddy root, test fragment and routing state to the encrypted configuration bundle. Their individual hashes are included in the authenticated manifest and checked during recovery verification. Missing, modified or symlinked managed files block backup; older manifests without this optional selection remain readable. This preserves routing configuration, not the test services' databases or media; each test producer still owns its data backup acceptance.

To bootstrap an empty recovery host, install the reviewed operations source and run `install-operations.sh`; it installs the pinned COS SDK when offsite configuration is present. Provision the original backup decryption key from the separately protected copy. Run the isolated restore drill before applying any data or switching public traffic. When offsite backup is enabled, the drill downloads COS objects into a fresh private directory, authenticates the complete pair, validates the configuration archive, and restores SQL into a disposable MySQL container with no network or production mount. It compares every table and row count, then removes the container and temporary plaintext.

## Acceptance

- Actual filesystem recovery and unchanged live services.
- Protected current/previous image and configuration references survive cleanup.
- Failed backup blocks migration; failed activation keeps prior artifacts and recovers its service.
- Fresh backup/maintenance and an isolated restore pass; offsite success requires remote readback.
- Verify two consecutive scheduled daily backup/maintenance runs after the fix. A manual `systemctl start` is not scheduled-run evidence.
- Native cloud snapshots are verified independently for schedule, retention and actual successful records. A successful local drill is not proof of a native cloud snapshot restore.

Baseline evidence: system disk 85 percent used, 342 images, 98 Journal binaries, build-cache logical size 7.65 GB. After initial cleanup, disk usage was 36 percent with about 25 GB available, 47 images and about 2 GB build cache. Docker's virtual sizes overlap; physical `df` recovery is the capacity evidence.
