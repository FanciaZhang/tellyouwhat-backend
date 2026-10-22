-- Preserve published-client scopes and admit the scopes already accepted by the API.
-- Check the definition so replay also recovers a committed DDL without its checkpoint.
SET @privacy_consent_scope_upgrade = IF(
    EXISTS (
        SELECT 1 FROM information_schema.CHECK_CONSTRAINTS
        WHERE CONSTRAINT_SCHEMA = DATABASE()
          AND CONSTRAINT_NAME = 'privacy_consents_scope_chk'
          AND CHECK_CLAUSE LIKE '%guardian_consent%'
          AND CHECK_CLAUSE LIKE '%journal_illustration%'
    ),
    'SELECT 1',
    'ALTER TABLE privacy_consents
        DROP CHECK privacy_consents_scope_chk,
        ADD CONSTRAINT privacy_consents_scope_chk CHECK (scope IN (
            ''adult'', ''age_14_plus'', ''guardian_consent'', ''privacy_and_terms'',
            ''lifetime_byok'', ''managed_subscription'', ''free_managed_recognition'',
            ''sensitive_health_ai'', ''journal_illustration''
        ))'
);
PREPARE privacy_consent_scope_upgrade FROM @privacy_consent_scope_upgrade;
EXECUTE privacy_consent_scope_upgrade;
DEALLOCATE PREPARE privacy_consent_scope_upgrade;
