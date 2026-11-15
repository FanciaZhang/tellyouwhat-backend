# Journal Ad Hoc 48 test delivery

2026-11-15. The current test selection is based on `31ed4cba393410d4d35b945c0618db5de2a7344e`.

The Journal post-organization change from `e68d577f90ff487901243862a09a001a5982c99c` is applied to that exact running baseline, preserving the existing shared platform, operations recipes, migrations and other App behavior. The App source is `811e5fe03233a101aec5a78975ce270b16d73cb9`, Debug / Ad Hoc 1.0 (48).

The planned runtime change only appends 48 to `JOURNAL_ALLOWED_APP_BUILDS`. Existing budgets, credentials, model choices and cloud feature switches stay in the running selection. Deployment must use the registered platform-test producer, authenticated backup, rollback selection and scoped cleanup. Production is outside this delivery.

Local checks: eight directly related Go packages; 1,140 test events passed, 74 explicit environment/provider checks skipped, zero failures. Earlier real-model evidence for the same hook implementation is retained under `/Users/harborzeng/Documents/JournalAppFixes/2026-11-15-post-organization`; this local result does not prove deployment or real App Attest.

Activation, source/configuration readback, public readiness, post-update backup and package download verification are recorded separately in the App delivery evidence after they actually pass.
