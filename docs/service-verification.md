# Live service verification

Run the production object-storage check with the deployed image and configuration:

```sh
docker compose --env-file .env.production -f compose.production.yaml \
  run --rm --no-deps --entrypoint /servicecheck gateway
```

Add `--models` to also call Health text capture, photo capture, cup estimation, and Journal Lite/Pro. This mode consumes provider tokens and uses only synthetic content.

The checker generates its own image and object identifiers, uses the same upload authorization and model clients as the gateway, verifies byte-for-byte retrieval, rejects anonymous object access, and deletes its temporary object. It does not create App Attest identities, subscriptions, user records, or free quota in production. Output contains pass/fail results without object URLs, keys, prompts, or model responses.

A successful result verifies storage and provider connectivity. App consent, authentication, purchase, entitlement synchronization, and user confirmation require separate journey acceptance. Public HTTPS and Apple server callbacks are separate release gates.
