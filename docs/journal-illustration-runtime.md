# Journal illustration runtime

The feature is disabled by default. No deployed environment is changed by this implementation.

Explicit Journal configuration:

- `JOURNAL_IMAGE_ENABLED=true`
- `JOURNAL_IMAGE_BASE_URL`: HTTPS provider API base URL
- `JOURNAL_IMAGE_API_KEY`: secret injected through deployment secret storage
- `JOURNAL_IMAGE_MODEL_ID`: the configured single-image model
- `JOURNAL_IMAGE_MONTHLY_LIMIT`: positive image allowance per subscription account
- `JOURNAL_IMAGE_MAX_CNY_PER_IMAGE`: positive conservative cost ceiling for one request

The price bound must cover the configured model, output size and all billable inputs. There is no default image price. Review it against the provider account's actual pricing before enablement and whenever model or pricing changes.

MySQL, payload encryption, current Journal subscription and explicit image consent are required. The existing project monetary controller and concurrency limit cover images together with Health and Journal text/speech; a separate independent spend ceiling is not created. The project controller must be configured even in development when image generation is enabled.

The image adapter reserves its maximum amount before calling the provider. A denied reservation never calls the provider and allows the worker to release the user's image allowance. Once dispatched, the image response is not an invoice: success and ambiguous failure retain the conservative monetary amount, with concurrency released. Failed settlement retains the durable reservation and emits a content-free error. Actual billing reconciliation remains a separate pending integration; do not interpret these conservative values as an exact invoice.

The scheduler starts after router construction, recovers only queued requests, and stops before database closure. It never resubmits running or uncertain requests. Expiry releases only known-undispatched image allowances and removes private payloads; late success records usage without restoring delivery.

Before deployment: complete image-specific consent transport and App confirmation, verify migrations and real provider behavior, review prices/limits and billing reconciliation, and validate the full user flow. Local runtime and adapter tests alone are not deployment or paid-provider acceptance.
