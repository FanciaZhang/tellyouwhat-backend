# Shared Tencent host operations

Only Health has published users and requires backward-compatible App/API upgrades. Albums, Journal and Birthday migrate directly to the target environment and contract; do not add adapters or keep old test routes solely for earlier development builds. This does not authorize deleting existing data, backups or uncommitted source.

For `/opt/tellyouwhat` on the shared production host, the producer owns cleanup of its disposable artifacts. Do not deploy through an unregistered ad hoc Docker/SSH command.

Run artifact registration and its deployment wrapper as the owner of `/opt/tellyouwhat/backend` (`ubuntu` on the current host). Do not run them through `sudo root`: private registration files must remain readable by scheduled operations. Restrict any required sudo use to the individual privileged action inside the producer.

- Production releases use `deploy/tencent/release.py deploy`. Its operations lock spans authenticated backup and migration, and its deployment lock spans activation. Do not run an independent migration or substitute a mutable image tag.
- Journal private development uses JournalApp's `scripts/journal-development.py`. It verifies the authenticated contract, keeps the stopped previous service, records binary hash/source, and invokes scoped cleanup.
- Birthday development, Albums test, platform test and Apple development delivery producers run through `deploy/tencent/ops_artifacts.py --apply --register --owner ... --source ... --source-run ... --image ... --verify-url ... --run <producer command>`. The producer command must perform its own failure recovery and must not include unrelated production changes. See `docs/host-artifact-lifecycle.md` for examples.
- Retain the active version, the verified rollback version, and any running/referenced candidate. A new deployment must include the correct image/configuration/signing material as one recovery selection. A stored older image is not evidence of schema-compatible rollback.
- Delete only scoped, regenerable binaries, retired image tags and build caches. Preserve databases, Docker volumes, development state, credentials, and evidence requested by the user. Never use global `docker system prune --volumes`, image removal with force, or volume deletion as a cleanup shortcut.
- A service can be healthy while cleanup or offsite backup has failed. Record and report those failures; do not declare the deployment complete until its required cleanup and backup checks pass.
