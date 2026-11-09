-- Widen the enforced scope set atomically; preserve all existing consent rows.
-- Replaying after a committed DDL but before its checkpoint is safe.
ALTER TABLE privacy_consents
    DROP CHECK privacy_consents_scope_chk,
    ADD CONSTRAINT privacy_consents_scope_chk CHECK (scope IN (
        'adult', 'age_14_plus', 'privacy_and_terms', 'lifetime_byok',
        'managed_subscription', 'free_managed_recognition', 'sensitive_health_ai',
        'journal_illustration'
    ));
