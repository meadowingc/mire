-- Stored independently of last_refreshed so server-provided freshness survives restarts.
ALTER TABLE feed ADD COLUMN next_fetch_at DATETIME;
