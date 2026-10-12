-- An automatic application upgrade is one batch: it fixes the version ordering, the entitlements
-- and the set of deployments it acts on, and it records one result per deployment. Revisions
-- created before a later deployment failed therefore stay in place instead of being rolled back
-- with the triggering request, and retrying the batch only touches the deployments no revision
-- was created for.

CREATE TYPE AUTOMATIC_UPDATE_BATCH_ITEM_STATUS AS ENUM (
  -- The deployment has not been processed yet.
  'pending',
  -- The single automatic-update revision of the item was created.
  'upgraded',
  -- A deterministic result that needs no revision: already on the target version, not entitled to
  -- it, automatic updates turned off, or a newer revision (manual or from another batch) won.
  'skipped',
  -- The deployment values cannot be applied to the target version; retrying cannot change that.
  'incompatible',
  -- A transient failure (target/secrets/license data unavailable, revision write lost); the
  -- deployment is still unconfirmed and the item may be claimed by the next retry sweep.
  'retryable'
);

CREATE TABLE AutomaticUpdateBatch (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  created_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
  completed_at TIMESTAMP,
  organization_id UUID NOT NULL REFERENCES Organization (id) ON DELETE CASCADE,
  application_id UUID NOT NULL REFERENCES Application (id) ON DELETE CASCADE,
  triggered_by_user_account_id UUID REFERENCES UserAccount (id) ON DELETE SET NULL,
  -- Set when the batch catches a single deployment up after automatic updates were enabled on it;
  -- NULL for an application-wide upgrade.
  only_deployment_id UUID REFERENCES Deployment (id) ON DELETE SET NULL,
  -- The strategy and the application's versions in ASC ordering as of batch creation fix the
  -- ordering the whole batch shares, even if a version is renamed afterwards.
  versioning_strategy VERSIONING_STRATEGY NOT NULL,
  version_ids UUID[] NOT NULL DEFAULT '{}',
  total_count INTEGER NOT NULL DEFAULT 0,
  upgraded_count INTEGER NOT NULL DEFAULT 0,
  skipped_count INTEGER NOT NULL DEFAULT 0,
  incompatible_count INTEGER NOT NULL DEFAULT 0,
  retryable_count INTEGER NOT NULL DEFAULT 0,
  CONSTRAINT AutomaticUpdateBatch_counts_complete CHECK (
    upgraded_count >= 0
      AND skipped_count >= 0
      AND incompatible_count >= 0
      AND retryable_count >= 0
      AND upgraded_count + skipped_count + incompatible_count + retryable_count <= total_count
  )
);

CREATE INDEX fk_AutomaticUpdateBatch_organization_id ON AutomaticUpdateBatch (organization_id);
CREATE INDEX fk_AutomaticUpdateBatch_application_id ON AutomaticUpdateBatch (application_id);
CREATE INDEX idx_AutomaticUpdateBatch_running
  ON AutomaticUpdateBatch (created_at)
  WHERE completed_at IS NULL;

CREATE TABLE AutomaticUpdateBatchItem (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  created_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
  updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
  batch_id UUID NOT NULL REFERENCES AutomaticUpdateBatch (id) ON DELETE CASCADE,
  deployment_id UUID NOT NULL REFERENCES Deployment (id) ON DELETE CASCADE,
  deployment_target_id UUID NOT NULL REFERENCES DeploymentTarget (id) ON DELETE CASCADE,
  status AUTOMATIC_UPDATE_BATCH_ITEM_STATUS NOT NULL DEFAULT 'pending',
  -- Snapshot the item's decision was made from.
  current_revision_id UUID REFERENCES DeploymentRevision (id) ON DELETE SET NULL,
  current_version_id UUID REFERENCES ApplicationVersion (id) ON DELETE SET NULL,
  target_version_id UUID REFERENCES ApplicationVersion (id) ON DELETE SET NULL,
  application_entitlement_id UUID REFERENCES ApplicationEntitlement (id) ON DELETE SET NULL,
  -- The one automatic-update revision created for this item, if any.
  revision_id UUID REFERENCES DeploymentRevision (id) ON DELETE SET NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  -- Machine-readable reason for the status, e.g. 'values_incompatible' or 'target_unavailable'.
  message TEXT
);

CREATE UNIQUE INDEX AutomaticUpdateBatchItem_batch_deployment_unique
  ON AutomaticUpdateBatchItem (batch_id, deployment_id);
CREATE INDEX fk_AutomaticUpdateBatchItem_batch_id ON AutomaticUpdateBatchItem (batch_id);
CREATE INDEX fk_AutomaticUpdateBatchItem_deployment_id ON AutomaticUpdateBatchItem (deployment_id);
CREATE INDEX idx_AutomaticUpdateBatchItem_open
  ON AutomaticUpdateBatchItem (batch_id, created_at)
  WHERE status IN ('pending', 'retryable');
CREATE UNIQUE INDEX uq_AutomaticUpdateBatchItem_revision
  ON AutomaticUpdateBatchItem (revision_id)
  WHERE revision_id IS NOT NULL;

ALTER TABLE DeploymentRevision
  ADD COLUMN automatic_update_batch_item_id UUID
  REFERENCES AutomaticUpdateBatchItem (id) ON DELETE SET NULL;

-- At most one revision per batch item: it is the stable identity an uncertain insert is looked up
-- by, so a lost response can never produce a duplicate revision.
CREATE UNIQUE INDEX uq_DeploymentRevision_automatic_update_batch_item
  ON DeploymentRevision (automatic_update_batch_item_id)
  WHERE automatic_update_batch_item_id IS NOT NULL;
CREATE INDEX fk_DeploymentRevision_automatic_update_batch_item
  ON DeploymentRevision (automatic_update_batch_item_id)
  WHERE automatic_update_batch_item_id IS NOT NULL;
