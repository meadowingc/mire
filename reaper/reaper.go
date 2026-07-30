package reaper

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"codeberg.org/meadowingc/mire/constants"
	"codeberg.org/meadowingc/mire/sqlite"
	"github.com/mmcdole/gofeed"
)

const timeToBecomeStale = 3 * time.Hour

type PostSaveRequest struct {
	FeedLink string
	Title    string
	Link     string
	Date     time.Time
}

type Reaper struct {
	// tracks when each feed (map key = feed url) was last fetched; feed
	// metadata lives in the database (feed.title/description/link), not here
	feeds map[string]time.Time

	saverChannel chan *PostSaveRequest

	db *sqlite.DB
}

var mutex = make(chan struct{}, 1)

func New(db *sqlite.DB) *Reaper {
	// open up mutex (non-blocking: the token is already there if this is
	// not the first reaper started in this process)
	select {
	case mutex <- struct{}{}:
	default:
	}

	r := &Reaper{
		feeds:        make(map[string]time.Time),
		saverChannel: make(chan *PostSaveRequest),
		db:           db,
	}

	go r.start()
	go r.startDbSaver()

	return r
}

func lock() {
	<-mutex
}

func unlock() {
	mutex <- struct{}{}
}

// Start initializes the reaper by populating a list of feeds from the database
// and periodically refreshes all feeds every hour, if the feeds are stale.
// reaper should only ever be started once (in New)
func (r *Reaper) start() {
	if !constants.DEBUG_MODE {
		urls := r.db.GetAllFeedURLs()

		lock()
		for _, url := range urls {
			// trigger immediate refresh by setting the last fetch time to the
			// past (one second past the staleness boundary so the strict
			// Before comparison still holds when time.Now() has coarse
			// granularity, e.g. on Windows where two calls can return the
			// same instant)
			r.feeds[url] = time.Now().Add(-timeToBecomeStale - time.Second)
		}
		unlock()
	}

	for {
		r.refreshAllFeeds()
		time.Sleep(10 * time.Minute)
	}
}

// startDbSaver accumulates incoming posts and writes them to the database in
// batches (one transaction per flush) since a transaction per post is far
// more expensive. Posts are batched per feed, so the buffer is flushed
// whenever the feed changes, the buffer fills up, or the flush interval
// elapses.
func (r *Reaper) startDbSaver() {
	const (
		flushInterval = 5 * time.Second
		flushSize     = 100
	)

	buffer := make([]*PostSaveRequest, 0, flushSize)

	flush := func() {
		if len(buffer) == 0 {
			return
		}
		posts := make([]*sqlite.Post, len(buffer))
		for i, req := range buffer {
			posts[i] = &sqlite.Post{
				Title:             req.Title,
				URL:               req.Link,
				PublishedDatetime: req.Date,
			}
		}
		if inserted := r.db.SavePosts(buffer[0].FeedLink, posts); inserted > 0 {
			log.Printf("reaper: saved %d new posts for feed %s\n", inserted, buffer[0].FeedLink)
		}
		buffer = buffer[:0]
	}

	ticker := time.NewTicker(flushInterval)
	for {
		select {
		case item := <-r.saverChannel:
			if len(buffer) > 0 && buffer[0].FeedLink != item.FeedLink {
				flush()
			}
			buffer = append(buffer, item)
			if len(buffer) >= flushSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (r *Reaper) sanitizeFeedItems(feed *gofeed.Feed) {
	whitespaceRegexp := regexp.MustCompile(`\s+`)
	seen := make(map[string]bool)
	uniqueItems := make([]*gofeed.Item, 0)

	for _, item := range feed.Items {
		// collapse all whitespace and newlines to a single whitespace in item title
		item.Title = whitespaceRegexp.ReplaceAllString(item.Title, " ")
		item.Title = strings.TrimSpace(item.Title)

		// if the item doesn't have a title, we just set it to "[untitled]"
		if item.Title == "" {
			item.Title = "[untitled]"
		}

		// strip whitespaces in item link
		item.Link = strings.TrimSpace(item.Link)

		// if link is not a valid http(s) link then we just skip it
		if !strings.HasPrefix(item.Link, "http://") && !strings.HasPrefix(item.Link, "https://") {
			continue
		}

		// if the item doesn't have a parsed date, try to parse it
		if item.PublishedParsed == nil {
			parsedDate, err := r.db.TryParseDate(item.Published)
			if err != nil {
				log.Printf("[err] reaper: could not parse date '%s' for item '%s' in feed '%s'\n", item.Published, item.Title, feed.FeedLink)
				item.PublishedParsed = &time.Time{}
			} else {
				item.PublishedParsed = &parsedDate
			}
		}

		// if the link is not in the seen map, add it to uniqueItems and mark it as seen
		if !seen[item.Link] {
			seen[item.Link] = true

			if item.Link != "" {
				// we don't really need to keep the whole item
				uniqueItems = append(uniqueItems, &gofeed.Item{
					Title:           item.Title,
					Link:            item.Link,
					Published:       item.Published,
					PublishedParsed: item.PublishedParsed,
				})
			}
		}
	}

	// replace the items in the feed with the unique items
	feed.Items = uniqueItems
}

func (r *Reaper) updateFeedAndSaveNewItemsToDb(feedLink string) {
	if _, ok := r.feeds[feedLink]; !ok {
		log.Printf("[err] reaper:updateFeedAndSaveNewItemsToDb → Tied to fetch a feed that is not known to Reaper")
		return
	}

	// refresh last attempted refresh time for feed, independently of whether
	// the fetch succeeds or not
	fetchTime := time.Now()
	lock()
	r.feeds[feedLink] = fetchTime
	unlock()

	newF, err := r.rawFetchFeed(feedLink)

	if err != nil {
		r.handleFeedFetchFailure(feedLink, err)
		return
	}

	newF.FeedLink = feedLink // sometimes this gets overwritten for some reason

	// record the successful refresh (timestamp + cleared error + metadata)
	r.db.UpdateFeedRefreshState(feedLink, "", fetchTime)
	r.db.UpdateFeedMetadata(feedLink, newF.Title, newF.Description, newF.Link)

	r.sanitizeFeedItems(newF)

	// queue every item for saving; SavePosts skips the ones already in the
	// database, so there's no need to diff against previously seen items here
	r.queueFeedItemsForSaving(feedLink, newF.Items)
}

func (r *Reaper) queueFeedItemsForSaving(feedLink string, items []*gofeed.Item) {
	for _, item := range items {
		r.saverChannel <- &PostSaveRequest{
			FeedLink: feedLink,
			Title:    item.Title,
			Link:     item.Link,
			Date:     *item.PublishedParsed,
		}
	}
}

// UpdateAll fetches every feed & attempts updating them
// asynchronously, then prints the duration of the sync
func (r *Reaper) refreshAllFeeds() {
	start := time.Now()
	semaphore := make(chan struct{}, 5)
	var wg sync.WaitGroup

	for feedLink, lastFetched := range r.feeds {
		// if the feed is stale, update it
		if lastFetched.Add(timeToBecomeStale).Before(start) {
			semaphore <- struct{}{} // acquire a token
			wg.Add(1)               // increment the WaitGroup counter

			go func(feedLink string) {
				defer func() {
					<-semaphore // release the token when done
					wg.Done()   // decrement the WaitGroup counter
				}()

				// wait a random amount of time so we spread out the fetches as
				// time goes on (we don't want to do "burst" of fetches every
				// `timeToBecomeStale`)
				time.Sleep(time.Duration(10+rand.Intn(20)) * time.Millisecond)

				r.updateFeedAndSaveNewItemsToDb(feedLink)
			}(feedLink)
		}
	}

	wg.Wait() // wait for all goroutines to finish

	log.Printf("reaper: refresh complete in %s\n", time.Since(start))
}

func (r *Reaper) handleFeedFetchFailure(url string, err error) {
	pc, file, line, ok := runtime.Caller(1)
	callerInfo := ""
	if ok {
		fullFuncName := runtime.FuncForPC(pc).Name()
		parts := strings.Split(fullFuncName, ".")
		lastPart := parts[len(parts)-1]             // get the last part, which is "(*Reaper).updateFeedAndSaveNewItemsToDb"
		funcName := strings.Split(lastPart, "(")[0] // split on "(" and get the first part, which is the function name
		cwd, _ := os.Getwd()
		relativePath, _ := filepath.Rel(cwd, file)
		callerInfo = fmt.Sprintf(" (called from %s#%s:%d)", relativePath, funcName, line)
	}

	log.Printf("[warning] reaper: fetch failure '%s': %s%s\n", url, err, callerInfo)
	r.db.UpdateFeedRefreshState(url, err.Error(), time.Now())
}

// HasFeed checks whether a given url is tracked by the reaper.
func (r *Reaper) HasFeed(url string) bool {
	if _, ok := r.feeds[url]; ok {
		return true
	}
	return false
}

func (r *Reaper) RemoveFeed(url string) {
	if !r.HasFeed(url) {
		log.Printf("[err] reaper: tried to remove non-existent feed '%s'\n", url)
		return
	}

	lock()
	delete(r.feeds, url)
	unlock()
}

func (r *Reaper) rawFetchFeed(url string) (*gofeed.Feed, error) {
	fp := gofeed.NewParser()

	// Be a nice internet citizen and add how a descriptive user agent header
	// with subscriber stats.
	// https://www.lesswrong.com/posts/djn3nJnnHYX7tReFa/looking-at-rss-user-agents
	numSubscribersForFeed := r.db.GetNumSubscribersForFeed(url)
	fp.UserAgent = fmt.Sprintf("Mire (+https://mire.meadow.cafe) - %d subscribers", numSubscribersForFeed)

	return fp.ParseURL(url)
}

// Fetch fetches a feed from a given url, tracks it in the reaper, stores its
// metadata in the database, and queues its items for saving. The fully
// parsed feed (including items) is returned for immediate use — the reaper
// itself never retains anything but the last-fetch time in memory.
//
// The feed is tracked even when the fetch fails (with a stale timestamp) so
// the scheduler keeps retrying it, mirroring what subscribing expects.
func (r *Reaper) Fetch(url string) (*gofeed.Feed, error) {
	feed, err := r.rawFetchFeed(url)
	if err != nil {
		// register the feed as stale so it stays on the refresh schedule
		lock()
		if _, tracked := r.feeds[url]; !tracked {
			r.feeds[url] = time.Now().Add(-timeToBecomeStale)
		}
		unlock()
		return nil, err
	}

	feed.FeedLink = url // sometimes this gets overwritten for some reason

	r.sanitizeFeedItems(feed)

	r.db.UpdateFeedMetadata(url, feed.Title, feed.Description, feed.Link)

	lock()
	r.feeds[url] = time.Now()
	unlock()

	r.queueFeedItemsForSaving(url, feed.Items)

	return feed, nil
}
