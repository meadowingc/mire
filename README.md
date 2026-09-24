# Mire

A simple, multi-tenant feed reader

- features:
  - rss and atom support
  - minimal, simple, reliable, fast
  - refresh your feeds automatically
  - display a chronological list of feed items
  - track read status for each post
  - open source & free of charge forever

- anti-features:
  - no tags
  - no options
  - minimal javascript
  - no comments, upvotes, or ranks

Feeds are checked every ten minutes and normally fetched no more often than
every three hours. When a feed responds with `Cache-Control: max-age`, Mire
waits until that freshness period has elapsed if it is longer than three
hours. If there is no usable `max-age`, Mire uses `Expires` instead. These
per-feed deadlines survive restarts; new feeds can still be fetched on first
subscription. Actual requests may occur later than the deadline because
the scheduler checks every ten minutes.
