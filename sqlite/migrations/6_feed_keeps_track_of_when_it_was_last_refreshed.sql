-- We haven't refreshed on insert
ALTER TABLE feed ADD COLUMN last_refreshed DATETIME DEFAULT '1970-01-01 00:00:00';