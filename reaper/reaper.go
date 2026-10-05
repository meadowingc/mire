package reaper

import (
	"context"
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
	// Next eligible fetch time for each feed (keyed by URL).
	feeds map[string]time.Time

	saverChannel chan *PostSaveRequest

	db            *sqlite.DB
	ctx           context.Context
	cancel        context.CancelFunc
	schedulerDone chan struct{}
	saverDone     chan struct{}
	closed        chan struct{}
	closeOnce     sync.Once
	fetchMu       sync.Mutex
	stopping      bool
	fetches       sync.WaitGroup
}

var mutex = make(chan struct{}, 1)

func New(db *sqlite.DB) *Reaper {
	return NewWithPolling(db, true)
}

func NewWithPolling(db *sqlite.DB, polling bool) *Reaper {
	// open up mutex (non-blocking: the token is already there if this is
	// not the first reaper started in this process)
	select {
	case mutex <- struct{}{}:
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &Reaper{
		feeds:        make(map[string]time.Time),
		saverChannel: make(chan *PostSaveRequest),
		db:           db,
		ctx:          ctx, cancel: cancel,
		schedulerDone: make(chan struct{}), saverDone: make(chan struct{}), closed: make(chan struct{}),
	}

	go func() {
		defer close(r.schedulerDone)
		if polling {
			r.start()
		}
	}()
	go r.startDbSaver()

	return r
}

func (r *Reaper) requestContext() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

func (r *Reaper) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		r.fetchMu.Lock()
		r.stopping = true
		r.cancel()
		r.fetchMu.Unlock()
		go func() {
			<-r.schedulerDone
			r.fetches.Wait()
			close(r.saverChannel)
			<-r.saverDone
			close(r.closed)
		}()
	})
	select {
	case <-r.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func lock() {
	<-mutex
}

func unlock() {
	mutex <- struct{}{}
}

// Start initializes the reaper from persisted deadlines and checks due feeds.
// reaper should only ever be started once (in New)
func (r *Reaper) start() {
	if !constants.DEBUG_MODE {
		r.loadFeedDeadlines()
	}

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		if r.ctx.Err() != nil {
			return
		}
		r.refreshAllFeeds()
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Reaper) loadFeedDeadlines() {
	deadlines := r.db.GetFeedPollDeadlines()
	lock()
	for url, deadline := range deadlines {
		if _, tracked := r.feeds[url]; !tracked {
			r.feeds[url] = deadline
		}
	}
	unlock()
}

// startDbSaver accumulates incoming posts and writes them to the database in
// batches (one transaction per flush) since a transaction per post is far
// more expensive. Posts are batched per feed, so the buffer is flushed
// whenever the feed changes, the buffer fills up, or the flush interval
// elapses.
func (r *Reaper) startDbSaver() {
	defer close(r.saverDone)
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
	defer ticker.Stop()
	for {
		select {
		case item, open := <-r.saverChannel:
			if !open {
				flush()
				return
			}
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
	lock()
	if _, ok := r.feeds[feedLink]; !ok {
		unlock()
		log.Printf("[err] reaper:updateFeedAndSaveNewItemsToDb → Tied to fetch a feed that is not known to Reaper")
		return
	}

	fetchTime := time.Now()
	r.feeds[feedLink] = fetchTime.Add(timeToBecomeStale)
	unlock()

	newF, freshness, err := r.rawFetchFeed(feedLink)

	if err != nil {
		if r.requestContext().Err() != nil {
			return
		}
		r.handleFeedFetchFailure(feedLink, err)
		return
	}

	newF.FeedLink = feedLink // sometimes this gets overwritten for some reason

	// record the successful refresh (timestamp + cleared error + metadata)
	r.recordRefresh(feedLink, "", fetchTime, freshness)
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
	defer wg.Wait()

	lock()
	due := make([]string, 0)
	for feedLink, deadline := range r.feeds {
		if !deadline.After(start) {
			due = append(due, feedLink)
		}
	}
	unlock()

	for _, feedLink := range due {
		lock()
		deadline, tracked := r.feeds[feedLink]
		if !tracked || deadline.After(time.Now()) {
			unlock()
			continue
		}
		r.feeds[feedLink] = time.Now().Add(timeToBecomeStale)
		unlock()
		select {
		case semaphore <- struct{}{}:
		case <-r.requestContext().Done():
			return
		}
		wg.Add(1) // increment the WaitGroup counter

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
	r.recordRefresh(url, err.Error(), time.Now(), time.Time{})
}

// HasFeed checks whether a given url is tracked by the reaper.
func (r *Reaper) HasFeed(url string) bool {
	lock()
	defer unlock()
	if _, ok := r.feeds[url]; ok {
		return true
	}
	return false
}

func (r *Reaper) RemoveFeed(url string) {
	lock()
	if _, ok := r.feeds[url]; !ok {
		unlock()
		log.Printf("[err] reaper: tried to remove non-existent feed '%s'\n", url)
		return
	}

	delete(r.feeds, url)
	unlock()
}

func (r *Reaper) recordRefresh(url, fetchErr string, fetchedAt, freshness time.Time) {
	deadline := fetchedAt.Add(timeToBecomeStale)
	if freshness.After(deadline) {
		deadline = freshness
	}
	lock()
	if _, tracked := r.feeds[url]; tracked {
		r.feeds[url] = deadline
	}
	unlock()
	r.db.UpdateFeedRefreshState(url, fetchErr, fetchedAt, deadline)
}

// Fetch fetches a feed from a given url, tracks it in the reaper, stores its
// metadata in the database, and queues its items for saving. The fully
// parsed feed (including items) is returned for immediate use — the reaper
// itself never retains anything but the last-fetch time in memory.
//
// The feed is tracked even when the fetch fails so the scheduler retries
// after the ordinary three-hour interval.
func (r *Reaper) Fetch(url string) (*gofeed.Feed, error) {
	r.fetchMu.Lock()
	if r.stopping {
		r.fetchMu.Unlock()
		return nil, context.Canceled
	}
	r.fetches.Add(1)
	r.fetchMu.Unlock()
	defer r.fetches.Done()
	fetchedAt := time.Now()
	feed, freshness, err := r.rawFetchFeed(url)
	if err != nil {
		if r.requestContext().Err() != nil {
			return nil, err
		}
		lock()
		r.feeds[url] = fetchedAt.Add(timeToBecomeStale)
		unlock()
		r.db.UpdateFeedRefreshState(url, err.Error(), fetchedAt, fetchedAt.Add(timeToBecomeStale))
		return nil, err
	}

	feed.FeedLink = url // sometimes this gets overwritten for some reason

	r.sanitizeFeedItems(feed)

	r.db.UpdateFeedMetadata(url, feed.Title, feed.Description, feed.Link)

	lock()
	r.feeds[url] = fetchedAt.Add(timeToBecomeStale)
	unlock()
	r.recordRefresh(url, "", fetchedAt, freshness)

	r.queueFeedItemsForSaving(url, feed.Items)

	return feed, nil
}
