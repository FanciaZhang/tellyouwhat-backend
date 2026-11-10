-- Preserve existing grants and permit the independent Journal TTS decision.
ALTER TABLE privacy_consents
    DROP CHECK privacy_consents_scope_chk,
    ADD CONSTRAINT privacy_consents_scope_chk CHECK (scope IN (
        'adult', 'age_14_plus', 'privacy_and_terms', 'lifetime_byok',
        'managed_subscription', 'free_managed_recognition', 'sensitive_health_ai',
        'journal_illustration', 'journal_read_aloud'
    ));
