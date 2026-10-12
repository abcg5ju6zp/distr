DROP TABLE SupportBundleCollectionScript;
DROP TABLE SupportBundleCollectionEnvVar;
DROP TABLE SupportBundleCollection;

DROP INDEX idx_support_bundle_resource_bundle_stable_key;

-- Rows that only carry an outcome never held content and cannot satisfy the restored constraint.
DELETE FROM SupportBundleResource WHERE content IS NULL AND content_enc IS NULL;

ALTER TABLE SupportBundleResource
    DROP COLUMN stable_key,
    DROP COLUMN kind,
    DROP COLUMN required,
    DROP COLUMN status,
    DROP COLUMN content_sha256,
    DROP COLUMN size_bytes,
    DROP COLUMN failure_reason,
    DROP COLUMN updated_at;

ALTER TABLE SupportBundleResource DROP CONSTRAINT SupportBundleResource_content_encryption;
ALTER TABLE SupportBundleResource
    ADD CONSTRAINT SupportBundleResource_content_encryption
        CHECK (num_nonnulls(content, content_enc) = 1);

DROP TYPE support_bundle_resource_kind;
DROP TYPE support_bundle_resource_status;
