-- Complete finalization for support bundle collection:
--   * a fixed collection generation with immutable snapshots of the collection configuration and
--     the enabled custom scripts,
--   * a resource manifest with a stable identity per resource,
--   * idempotent content-addressed uploads with a terminal result per resource
--     (succeeded / skipped / oversized / failed),
--   * an atomic finalize that publishes the manifest only once every required resource is decided.

CREATE TYPE support_bundle_resource_status AS ENUM (
    'pending', 'succeeded', 'skipped', 'oversized', 'failed'
);

CREATE TYPE support_bundle_resource_kind AS ENUM (
    'system_info', 'host_env', 'custom_script', 'container_env', 'container_logs', 'other'
);

-- Manifest items are created (pending) before their content exists, so content becomes optional.
ALTER TABLE SupportBundleResource DROP CONSTRAINT SupportBundleResource_content_encryption;
ALTER TABLE SupportBundleResource
    ADD CONSTRAINT SupportBundleResource_content_encryption
        CHECK (num_nonnulls(content, content_enc) <= 1);

-- The old upload path could insert several rows for the same resource name when a retried request
-- reached the server more than once. Keep only the newest copy of each duplicate before the unique
-- index that makes such retries idempotent is added.
DELETE FROM SupportBundleResource AS a
USING SupportBundleResource AS b
WHERE a.support_bundle_id = b.support_bundle_id
  AND a.name = b.name
  AND (a.created_at, a.id) < (b.created_at, b.id);

ALTER TABLE SupportBundleResource
    ADD COLUMN stable_key TEXT,
    ADD COLUMN kind support_bundle_resource_kind NOT NULL DEFAULT 'other',
    ADD COLUMN required BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN status support_bundle_resource_status NOT NULL DEFAULT 'succeeded',
    ADD COLUMN content_sha256 TEXT,
    ADD COLUMN size_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN failure_reason TEXT,
    ADD COLUMN updated_at TIMESTAMP NOT NULL DEFAULT now();

UPDATE SupportBundleResource SET stable_key = name;
ALTER TABLE SupportBundleResource ALTER COLUMN stable_key SET NOT NULL;

CREATE UNIQUE INDEX idx_support_bundle_resource_bundle_stable_key
    ON SupportBundleResource (support_bundle_id, stable_key);

-- One fixed collection generation per bundle. The limits are the ones the collect script was
-- rendered with; published_at and the counts hold the atomically committed finalize outcome.
CREATE TABLE SupportBundleCollection (
    support_bundle_id UUID PRIMARY KEY REFERENCES SupportBundle (id) ON DELETE CASCADE,
    collection_generation UUID NOT NULL DEFAULT gen_random_uuid(),
    resource_max_bytes INTEGER NOT NULL,
    log_tail_lines INTEGER NOT NULL,
    script_timeout_seconds INTEGER NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    published_at TIMESTAMP,
    finalized_succeeded_count INTEGER NOT NULL DEFAULT 0,
    finalized_skipped_count INTEGER NOT NULL DEFAULT 0,
    finalized_oversized_count INTEGER NOT NULL DEFAULT 0,
    finalized_failed_count INTEGER NOT NULL DEFAULT 0
);

-- Immutable snapshot of the environment variable configuration the bundle was created with.
CREATE TABLE SupportBundleCollectionEnvVar (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    support_bundle_id UUID NOT NULL REFERENCES SupportBundle (id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    redacted BOOLEAN NOT NULL DEFAULT false,
    position INTEGER NOT NULL
);

CREATE INDEX idx_support_bundle_collection_env_var_bundle
    ON SupportBundleCollectionEnvVar (support_bundle_id);

-- Immutable snapshot of the enabled custom scripts. The id is the configuration script id and is
-- the stable identity the matching resource manifest item uses.
CREATE TABLE SupportBundleCollectionScript (
    id UUID NOT NULL,
    support_bundle_id UUID NOT NULL REFERENCES SupportBundle (id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    description TEXT,
    content TEXT NOT NULL,
    position INTEGER NOT NULL,
    PRIMARY KEY (support_bundle_id, id)
);

CREATE INDEX idx_support_bundle_collection_script_bundle
    ON SupportBundleCollectionScript (support_bundle_id);

-- Backfill a collection for every bundle created before this migration. The limits use the
-- built-in defaults; bundles already past collection are treated as published so that nothing is
-- ever deleted as an expired unpublished collection.
INSERT INTO SupportBundleCollection
    (support_bundle_id, resource_max_bytes, log_tail_lines, script_timeout_seconds,
     created_at, published_at)
SELECT id, 1048576, 1000, 60, created_at,
       CASE WHEN status = 'initialized' THEN NULL ELSE now() END
FROM SupportBundle;

INSERT INTO SupportBundleCollectionEnvVar (support_bundle_id, name, redacted, position)
SELECT sb.id, ev.name, ev.redacted,
       row_number() OVER (PARTITION BY sb.id ORDER BY ev.name) - 1
FROM SupportBundle sb
JOIN SupportBundleConfigurationEnvVar ev ON ev.organization_id = sb.organization_id;

INSERT INTO SupportBundleCollectionScript (id, support_bundle_id, name, description, content, position)
SELECT cs.id, sb.id, cs.name, cs.description, cs.content,
       row_number() OVER (PARTITION BY sb.id ORDER BY cs.name) - 1
FROM SupportBundle sb
JOIN SupportBundleConfigurationScript cs
  ON cs.organization_id = sb.organization_id AND cs.enabled;

-- Backfill the required manifest items. Rows the old script already uploaded keep their succeeded
-- status and content; the manifest update only claims the identity for them.
INSERT INTO SupportBundleResource (support_bundle_id, stable_key, name, kind, required, status)
SELECT id, 'system-info', 'system-info',
       'system_info'::support_bundle_resource_kind, true,
       'pending'::support_bundle_resource_status
FROM SupportBundle
UNION ALL
SELECT id, 'host-environment-variables', 'host-environment-variables',
       'host_env'::support_bundle_resource_kind, true,
       'pending'::support_bundle_resource_status
FROM SupportBundle
UNION ALL
SELECT sb.id, 'custom-script:' || cs.id::text, cs.name,
       'custom_script'::support_bundle_resource_kind, true,
       'pending'::support_bundle_resource_status
FROM SupportBundle sb
JOIN SupportBundleConfigurationScript cs
  ON cs.organization_id = sb.organization_id AND cs.enabled
ON CONFLICT (support_bundle_id, stable_key) DO UPDATE
SET kind = EXCLUDED.kind,
    required = EXCLUDED.required;
