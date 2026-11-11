ALTER TABLE prompt_eval_samples
 ADD COLUMN voice_debug BOOLEAN NOT NULL DEFAULT FALSE,
 ADD INDEX prompt_eval_sample_library (voice_debug, created_at);
