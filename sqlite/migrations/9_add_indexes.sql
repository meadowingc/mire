BEGIN TRANSACTION;

-- dedupe subscribe rows (same user subscribed to same feed more than once),
-- keeping the oldest row
DELETE FROM subscribe
WHERE id NOT IN (
    SELECT MIN(id) FROM subscribe GROUP BY user_id, feed_id
);

-- dedupe post_read rows, keeping the most recently written row
DELETE FROM post_read
WHERE id NOT IN (
    SELECT MAX(id) FROM post_read GROUP BY user_id, post_id
);

-- speeds up every per-user subscription lookup/join
CREATE UNIQUE INDEX IF NOT EXISTS idx_subscribe_user_feed ON subscribe(user_id, feed_id);

-- speeds up per-feed subscriber counts and orphan-feed cleanup
CREATE INDEX IF NOT EXISTS idx_subscribe_feed_id ON subscribe(feed_id);

-- speeds up every read-status check/toggle and the post_read joins in the
-- timeline queries
CREATE UNIQUE INDEX IF NOT EXISTS idx_post_read_user_post ON post_read(user_id, post_id);

-- speeds up post lookups by url (GetPostId fallback)
CREATE INDEX IF NOT EXISTS idx_post_url ON post(url);

-- speeds up per-feed post listings ordered by date
CREATE INDEX IF NOT EXISTS idx_post_feed_published ON post(feed_id, published_at);

-- speeds up recent-posts scans (discover page oversampling)
CREATE INDEX IF NOT EXISTS idx_post_published_at ON post(published_at);

COMMIT;
