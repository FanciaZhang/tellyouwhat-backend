-- Orphan revocation fences expire only after all pre-fence assertions are stale.
ALTER TABLE album_apple_identity_state
 ADD KEY album_identity_retirement(revoked_before,subject_sha256);
