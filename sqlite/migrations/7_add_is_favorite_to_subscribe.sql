BEGIN TRANSACTION;

-- Add is_favorite column to subscribe table
ALTER TABLE subscribe ADD COLUMN is_favorite BOOLEAN NOT NULL DEFAULT 0;

COMMIT;
