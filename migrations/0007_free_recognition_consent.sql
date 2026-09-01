ALTER TABLE privacy_consents
    DROP CHECK privacy_consents_chk_1,
    ADD CONSTRAINT privacy_consents_chk_1 CHECK (scope IN (
        'adult',
        'privacy_and_terms',
        'lifetime_byok',
        'managed_subscription',
        'free_managed_recognition',
        'sensitive_health_ai'
    ));
