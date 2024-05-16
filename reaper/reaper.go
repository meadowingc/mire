package reaper

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

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

type FeedHolder struct {
	Feed        *gofeed.Feed
	LastFetched time.Time
}

type Reaper struct {
	// internal list of all rss feeds where the map
	// key represents the url of the feed (which should be unique)
	feeds map[string]*FeedHolder

	saverChannel chan *PostSaveRequest

	db *sqlite.DB
}

var mutex = make(chan struct{}, 1)

func New(db *sqlite.DB) *Reaper {
	mutex <- struct{}{}

	r := &Reaper{
		feeds:        make(map[string]*FeedHolder),
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
	urls := r.db.GetAllFeedURLs()

	lock()
	for _, url := range urls {
		// Setting FeedLink lets us defer fetching
		feed := &gofeed.Feed{
			FeedLink: url,
		}

		lastRefreshed := r.db.GetFeedLastRefreshTime(feed.FeedLink)
		r.feeds[url] = &FeedHolder{
			Feed:        feed,
			LastFetched: lastRefreshed,
		}
	}
	unlock()

	for {
		r.refreshAllFeeds()
		time.Sleep(1 * time.Hour)
	}
}

func (r *Reaper) startDbSaver() {
	for {
		select {
		case item := <-r.saverChannel:
			r.db.SavePost(item.FeedLink, item.Title, item.Link, item.Date)
		default:
			time.Sleep(10 * time.Second)
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

func (r *Reaper) updateFeedAndSaveNewItemsToDb(fh *FeedHolder) {
	f := fh.Feed

	if _, ok := r.feeds[f.FeedLink]; !ok {
		log.Printf("[err] reaper:updateFeedAndSaveNewItemsToDb → Tied to fetch a feed that is not known to Reaper")
		return
	}

	// refresh last attempted refresh time for feed, independently of whether
	// the fetch succeeds or not
	fetchTime := time.Now()
	lock()
	r.feeds[f.FeedLink].LastFetched = fetchTime
	unlock()
	r.db.UpdateFeedLastRefreshTime(f.FeedLink, fetchTime)

	originalItemsMap := make(map[string]*gofeed.Item)
	for _, item := range f.Items {
		originalItemsMap[item.Link] = item
	}

	newF, err := r.rawFetchFeed(f.FeedLink)

	if err != nil {
		r.handleFeedFetchFailure(f.FeedLink, err)
		return
	}

	// otherwise tell the DB that we successfully fetched the feed
	err = r.db.SetFeedFetchError(f.FeedLink, "")
	if err != nil {
		log.Printf("[err] reaper: could not clear feed fetch error '%s'\n", err)
	}

	r.sanitizeFeedItems(newF)

	if newF.PublishedParsed == nil {
		parsedDate, err := r.db.TryParseDate(newF.Published)
		if err == nil {
			// we don't log an error here since we don't really care if the feed
			// has a date or not
			newF.PublishedParsed = &parsedDate
		}
	}

	newF.FeedLink = f.FeedLink // sometimes this gets overwritten for some reason

	if !r.HasFeed(newF.FeedLink) {
		// NOTE: this should never happen, but if it does, we should add the
		// feed to the reaper so that we can track it
		log.Printf("[err] reaper: feed not tracked by reaper but fetched '%s'\n", newF.FeedLink)
		log.Printf("[err. cont] reaper: adding feed '%s' to reaper\n", newF.FeedLink)
		r.AddFeedStub(newF.FeedLink)
	}

	lock()
	r.feeds[newF.FeedLink].Feed = newF
	unlock()

	newItems := []*gofeed.Item{}
	for _, item := range newF.Items {
		if _, exists := originalItemsMap[item.Link]; !exists {
			newItems = append(newItems, item)
		}
	}

	if len(newItems) > 0 {
		log.Printf("Saving %d new items for feed %s\n", len(newItems), newF.FeedLink)

		for _, newItem := range newItems {
			r.saverChannel <- &PostSaveRequest{
				FeedLink: newF.FeedLink,
				Title:    newItem.Title,
				Link:     newItem.Link,
				Date:     *newItem.PublishedParsed,
			}
		}
	}

	fh.LastFetched = time.Now()
}

// UpdateAll fetches every feed & attempts updating them
// asynchronously, then prints the duration of the sync
func (r *Reaper) refreshAllFeeds() {
	start := time.Now()
	semaphore := make(chan struct{}, 5)
	var wg sync.WaitGroup

	for feedLink := range r.feeds {
		// if the feed is stale, update it
		if r.feeds[feedLink].LastFetched.Add(timeToBecomeStale).Before(start) {
			semaphore <- struct{}{} // acquire a token
			wg.Add(1)               // increment the WaitGroup counter

			go func(feedHolder *FeedHolder) {
				defer func() {
					<-semaphore // release the token when done
					wg.Done()   // decrement the WaitGroup counter
				}()

				// wait a random amount of time so we spread out the fetches as
				// time goes on (we don't want to do "burst" of fetches every
				// `timeToBecomeStale`)
				time.Sleep(time.Duration(10+rand.Intn(20)) * time.Millisecond)

				r.updateFeedAndSaveNewItemsToDb(feedHolder)
			}(r.feeds[feedLink])
		}
	}

	wg.Wait() // wait for all goroutines to finish

	log.Printf("reaper: refresh complete in %s\n", time.Since(start))
}

func (r *Reaper) handleFeedFetchFailure(url string, err error) {
	pc, file, line, ok := runtime.Caller(1)
	callerInfo := ""
	if ok {
		funcName := runtime.FuncForPC(pc).Name()
		cwd, _ := os.Getwd()
		relativePath, _ := filepath.Rel(cwd, file)
		callerInfo = fmt.Sprintf(" (called from %s:%d, function: %s)", relativePath, line, funcName)
	}

	log.Printf("[warning] reaper: fetch failure '%s': %s%s\n", url, err, callerInfo)
	err = r.db.SetFeedFetchError(url, err.Error())
	if err != nil {
		log.Printf("[err] reaper: could not set feed fetch error '%s'%s\n", err, callerInfo)
	}
}

// HasFeed checks whether a given url is represented
// in the reaper cache.
func (r *Reaper) HasFeed(url string) bool {
	if _, ok := r.feeds[url]; ok {
		return true
	}
	return false
}

func (r *Reaper) GetFeed(url string) *gofeed.Feed {
	return r.feeds[url].Feed
}

func (r *Reaper) GetAllFeeds() []*gofeed.Feed {
	var result []*gofeed.Feed
	for _, f := range r.feeds {
		result = append(result, f.Feed)
	}

	return result
}

func (r *Reaper) SortFeeds(f []*gofeed.Feed) {
	sort.Slice(f, func(i, j int) bool {
		return f[i].FeedLink < f[j].FeedLink
	})
}

func (r *Reaper) SortFeedItemsByDate(feeds []*gofeed.Feed) []*gofeed.Item {
	var posts []*gofeed.Item
	for _, f := range feeds {
		posts = append(posts, f.Items...)
	}

	return r.SortItemsByDate(posts)
}

func (r *Reaper) SortItemsByDate(posts []*gofeed.Item) []*gofeed.Item {
	sort.Slice(posts, func(i, j int) bool {
		return posts[i].PublishedParsed.After(*posts[j].PublishedParsed)
	})
	return posts
}

func (r *Reaper) AddFeedStub(url string) {
	if r.HasFeed(url) {
		return
	}

	lock()
	r.feeds[url] = &FeedHolder{
		Feed:        &gofeed.Feed{FeedLink: url},
		LastFetched: time.Now().Add(-timeToBecomeStale), // force refresh
	}
	unlock()
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
	fp.UserAgent = fmt.Sprintf("Mire (+https://mire.meadowing.club) - %d subscribers", numSubscribersForFeed)

	return fp.ParseURL(url)
}

// Fetch attempts to fetch a feed from a given url, marshal
// it into a feed object, and manage it via reaper.
func (r *Reaper) Fetch(url string) error {
	feed, err := r.rawFetchFeed(url)
	if err != nil {
		return err
	}

	feed.FeedLink = url // sometimes this gets overwritten for some reason

	r.sanitizeFeedItems(feed)

	lock()
	r.feeds[url] = &FeedHolder{
		Feed:        feed,
		LastFetched: time.Now(),
	}
	unlock()

	return nil
}
