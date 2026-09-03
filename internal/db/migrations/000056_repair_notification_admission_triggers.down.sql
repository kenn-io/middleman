-- Migration 55's valid trigger contract already targets spoke preparation.
-- Preserve that contract on downgrade instead of restoring the broken preview
-- reference to forge_node_preparation.
DROP TRIGGER forge_notification_ack_admission_update;
DROP TRIGGER forge_notification_ack_admission_insert;

CREATE TRIGGER forge_notification_ack_admission_insert
AFTER INSERT ON forge_notification_items
WHEN NEW.source_ack_queued_at IS NOT NULL
 AND NEW.source_ack_synced_at IS NULL
BEGIN
    UPDATE forge_spoke_preparation
    SET ack_generation = ack_generation + 1,
        updated_at = datetime('now')
    WHERE singleton_id = 1;

    INSERT INTO forge_notification_ack_admissions (
        notification_id, generation, queued_at
    )
    SELECT NEW.id, ack_generation, NEW.source_ack_queued_at
    FROM forge_spoke_preparation
    WHERE singleton_id = 1
    ON CONFLICT(notification_id) DO UPDATE SET
        generation = excluded.generation,
        queued_at = excluded.queued_at;
END;

CREATE TRIGGER forge_notification_ack_admission_update
AFTER UPDATE OF source_ack_queued_at, source_ack_synced_at
ON forge_notification_items
WHEN NEW.source_ack_queued_at IS NOT NULL
 AND NEW.source_ack_synced_at IS NULL
 AND (
     OLD.source_ack_queued_at IS NULL
     OR OLD.source_ack_synced_at IS NOT NULL
     OR OLD.source_ack_queued_at <> NEW.source_ack_queued_at
 )
BEGIN
    UPDATE forge_spoke_preparation
    SET ack_generation = ack_generation + 1,
        updated_at = datetime('now')
    WHERE singleton_id = 1;

    INSERT INTO forge_notification_ack_admissions (
        notification_id, generation, queued_at
    )
    SELECT NEW.id, ack_generation, NEW.source_ack_queued_at
    FROM forge_spoke_preparation
    WHERE singleton_id = 1
    ON CONFLICT(notification_id) DO UPDATE SET
        generation = excluded.generation,
        queued_at = excluded.queued_at;
END;
