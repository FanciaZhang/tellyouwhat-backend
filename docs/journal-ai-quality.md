# Journal AI completion and publication checks

## App behavior

Tags and book recommendations save as soon as organization succeeds. Optional scene extraction runs independently. Scene `succeeded` includes a valid null result (no appropriate scene); `failed` is retained separately in SwiftData. A failed scene can be retried from the reading page or the next analysis of unchanged content without paying for organization again. Reopening an interrupted request resumes it. A late reply cannot overwrite a changed, deleted or protected document or a withdrawn consent. If newer content has already been organized, the next scene request uses that content.

## One published Journal policy

`journal.illustrationSuggestionPrompt` and `journal.illustrationGenerationPrompt` are editable on the admin 配图 page. Scene extraction and speech-assisted scene suggestions use the request's pinned revision. Image admission stores the compiled prompt and revision ID with the task. A retry keeps that frozen version; a new task uses the latest published or restored version.

Migration 0015 adopts the previously fixed image instructions into a new Journal baseline revision. It preserves all prior revision documents and all non-Journal policies. It does not change the published organization wording. Historical revisions from before this schema retain their audit content; to restore one, supply both new fields in a new draft. New-schema revisions use normal draft/evaluate/publish restoration.

The private Journal server reads an independent `config/prompt.json` through a read-only directory mount. It refuses database/Redis configuration, validates the complete initial Journal policy and retains its last good revision after later refresh failures. The first isolated revision copies the then-published policy once; future production publications do not change development configuration. Deployment reads provider parameters from a private `runtime.env` and never inspects the production gateway's environment.

The evaluation worker additionally needs `JOURNAL_IMAGE_MODEL_ID` and `JOURNAL_IMAGE_MAX_COST_NANOS` (positive, at most 1000000000). It uses the existing Journal provider URL/key. Missing image configuration fails that evaluation and blocks publication. Each image reserves at most CNY 1 within the existing evaluation budget; displayed cost is an estimate, not a provider invoice.

## Required regression cases

The existing eight built-ins plus twelve fixed quality samples fit the twenty-sample batch. Both Lite and Pro check short text, already-collected books, user-owned book names, unsupported relationship inference and concise new book titles. Scene and image samples check grounded extraction and actual image generation. The gate requires the exact fixed sample definitions; an edited copy is not coverage.

Organization checks inspect both the model result before curation and the curated result. Filtering cannot turn a raw quality failure into a passing publication check. Generated images appear in the existing encrypted seven-day evaluation records for human inspection. Text-model scoring is not presented as an image-quality pass.

## Verification and rollout

Focused Go tests, isolated MySQL tests, synthetic real-provider tests, App result application/persistence tests and iPhone 17 UI screenshots cover this milestone. Deployment, Prompt publication and user screenshot acceptance are separate steps. Production publication: run migrations, configure the evaluation image model/cost, then evaluate and publish the intended organization draft through the admin Passkey flow. Private development configuration is managed independently. Do not overwrite the existing published policy with source defaults.
