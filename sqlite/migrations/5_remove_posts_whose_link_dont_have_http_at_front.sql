-- we had some posts whose links started with `//` instead of `http(s)://`,
-- which was breaking stuff. This migration is meant to remove them from the
-- database.

DELETE FROM post_read
WHERE post_id IN (
    SELECT id FROM post
    WHERE url NOT LIKE 'http://%' AND url NOT LIKE 'https://%'
);

DELETE FROM post
WHERE url NOT LIKE 'http://%' AND url NOT LIKE 'https://%';