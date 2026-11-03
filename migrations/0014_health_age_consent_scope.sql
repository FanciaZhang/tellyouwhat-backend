-- Keep legacy scopes while admitting the current Health age confirmation.
-- A single ALTER preserves enforcement throughout the upgrade. If the DDL
-- committed before its migration checkpoint, replay keeps the new constraint.
SET @health_consent_scope_upgrade = IF(
    EXISTS (
        SELECT 1 FROM information_schema.TABLE_CONSTRAINTS
        WHERE CONSTRAINT_SCHEMA = DATABASE()
          AND TABLE_NAME = 'privacy_consents'
          AND CONSTRAINT_NAME = 'privacy_consents_scope_chk'
          AND CONSTRAINT_TYPE = 'CHECK'
    ),
    'SELECT 1',
    'ALTER TABLE privacy_consents
        DROP CHECK privacy_consents_chk_1,
        ADD CONSTRAINT privacy_consents_scope_chk CHECK (scope IN (
            ''adult'', ''age_14_plus'', ''privacy_and_terms'', ''lifetime_byok'',
            ''managed_subscription'', ''free_managed_recognition'', ''sensitive_health_ai''
        ))'
);
PREPARE health_consent_scope_upgrade FROM @health_consent_scope_upgrade;
EXECUTE health_consent_scope_upgrade;
DEALLOCATE PREPARE health_consent_scope_upgrade;
