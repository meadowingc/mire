BEGIN TRANSACTION;

-- create new table with unique compound constraint
CREATE TABLE IF NOT EXISTS new_post (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    feed_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    url TEXT NOT NULL,
    published_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(feed_id, url)
);

-- Copy the data from the old table to the new one
INSERT INTO new_post SELECT * FROM post;

-- Delete the old table
DROP TABLE post;

-- Rename the new table to the old table's name
ALTER TABLE new_post RENAME TO post;

COMMIT;