"""
Will print out all the feeds that have posted a post more recent than the specified number of days.
"""

from pathlib import Path
import sys
import sqlite3
from datetime import datetime, timedelta
from urllib.parse import urlparse


def normalize_feed_url(feed_url):
    feed_url = feed_url.strip()
    feed_url = feed_url.rstrip("/")
    feed_url = feed_url.rstrip("?")
    feed_url = feed_url.rstrip("&")

    # normalize weird bearblog feeds endings
    # - *bearblog.dev/feed
    # - *bearblog.dev/feed/
    # - *bearblog.dev/feed/?type=rss
    if ".bearblog.dev" in feed_url or "/feed/?type=rss" in feed_url:
        feed_url = feed_url.split("?")[0]
        feed_url = feed_url.rstrip("/")
        feed_url = feed_url.replace("/rss", "/feed")


    return feed_url


def main(days):
    if not days.isdigit():
        print("Usage: export_all_recent_feeds.py <days>")
        sys.exit(1)

    days = int(days)
    cutoff_date = datetime.now() - timedelta(days=days)
    cutoff_date_str = cutoff_date.strftime("%Y-%m-%d %H:%M:%S")

    print(
        f"Fetching feeds with posts more recent than {cutoff_date_str}", file=sys.stderr
    )

    conn = sqlite3.connect("mire.db")
    cursor = conn.cursor()

    cursor.execute(
        """
        SELECT DISTINCT f.id, f.url
        FROM feed f
        JOIN post p ON f.id = p.feed_id
        WHERE p.published_at > ?
    """,
        (cutoff_date_str,),
    )
    rows = cursor.fetchall()

    column_names = [description[0] for description in cursor.description]
    feeds = [
        {column_names[i]: row[i] for i in range(len(column_names))} for row in rows
    ]

    # remove spammy feeds
    spammy_feeds = (
        Path("sqlite/sqlite.go")
        .read_text()
        .split("var listOfSpammyFeeds = []string{")[1]
        .split("}")[0]
        .split("\n")
    )
    spammy_feeds = [
        feed.strip().replace('"', "").rstrip(",")
        for feed in spammy_feeds
        if feed.strip()
    ]
    print(
        f"Found {len(spammy_feeds)} spammy feed from Go source",
        file=sys.stderr,
    )

    feed_urls = [normalize_feed_url(feed["url"]) for feed in feeds]

    def is_spammy(feed):
        return any(spammy_feed in feed for spammy_feed in spammy_feeds)

    feed_urls = [feed_url for feed_url in feed_urls if not is_spammy(feed_url)]

    feed_urls = set(feed_urls)
    feed_urls = sorted(feed_urls)

    # Group feeds by domain
    feeds_by_domain = {}
    for feed_url in feed_urls:
        domain = urlparse(feed_url).netloc
        if domain not in feeds_by_domain:
            feeds_by_domain[domain] = []
        feeds_by_domain[domain].append(feed_url)

    print(
        f"Found {len(feed_urls)} feeds with posts more recent than {days} days",
        file=sys.stderr,
    )

    # export all feeds
    for feed_url in feed_urls:
        print(feed_url)

    # now report on the domains that have multiple feeds
    for domain, feeds in feeds_by_domain.items():
        if len(feeds) > 1:
            print(f"\n{domain}", file=sys.stderr)
            for feed in feeds:
                print(f"  '{feed}'", file=sys.stderr)

    conn.close()


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("Usage: export_all_recent_feeds.py <days>")
        sys.exit(1)

    main(sys.argv[1])
