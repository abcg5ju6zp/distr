ALTER TABLE DeploymentRevision DROP COLUMN automatic_update_batch_item_id;

DROP TABLE AutomaticUpdateBatchItem;
DROP TABLE AutomaticUpdateBatch;
DROP TYPE AUTOMATIC_UPDATE_BATCH_ITEM_STATUS;
