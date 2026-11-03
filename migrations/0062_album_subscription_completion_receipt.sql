-- Full known-chain verification at a specific subscription revision. This is
-- not a receipt for identity, media or backup erasure.
ALTER TABLE album_storage_accounts
 ADD COLUMN subscription_checked_revision BIGINT NULL,
 ADD COLUMN subscription_checked_at DATETIME(6) NULL,
 ADD CONSTRAINT album_subscription_completion_receipt CHECK (
   (subscription_checked_revision IS NULL AND subscription_checked_at IS NULL) OR
   (subscription_checked_revision IS NOT NULL AND subscription_checked_at IS NOT NULL
    AND deletion_requested_at IS NOT NULL
    AND subscription_checked_revision=subscription_revision AND subscription_checked_at>=deletion_requested_at));

-- Earlier individual acknowledgements did not persist the complete snapshot.
-- Re-query Apple instead of inventing a complete receipt from those rows.
UPDATE album_deletion_subscription_jobs
 SET acknowledged_at=NULL,verified_transaction_id=NULL,verified_signed_at=NULL,
     lease_token=NULL,lease_until=NULL,available_at=LEAST(available_at,CURRENT_TIMESTAMP(6))
 WHERE acknowledged_at IS NOT NULL;
