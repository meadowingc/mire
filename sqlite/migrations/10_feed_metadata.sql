-- Adds feed metadata columns so feed title/description/site-link can be
-- served from the database instead of being kept in reaper memory.
-- Existing rows get NULLs; the reaper backfills them on each successful
-- refresh (self-healing within one refresh cycle).

ALTER TABLE feed ADD COLUMN title TEXT;
ALTER TABLE feed ADD COLUMN description TEXT;
ALTER TABLE feed ADD COLUMN link TEXT;
