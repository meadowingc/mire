package reaper

import (
	"log"
	"sort"
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

func New(db *sqlite.DB) *Reaper {
	r := &Reaper{
		feeds:        make(map[string]*FeedHolder),
		saverChannel: make(chan *PostSaveRequest),
		db:           db,
	}

	go r.start()
	go r.startDbSaver()

	return r
}

// Start initializes the reaper by populating a list of feeds from the database
// and periodically refreshes all feeds every hour, if the feeds are stale.
// reaper should only ever be started once (in New)
func (r *Reaper) start() {
	urls := r.db.GetAllFeedURLs()

	for _, url := range urls {
		// Setting FeedLink lets us defer fetching
		feed := &gofeed.Feed{
			FeedLink: url,
		}

		r.feeds[url] = &FeedHolder{
			Feed:        feed,
			LastFetched: time.Now().Add(-timeToBecomeStale), // force refresh
		}
	}

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

func (r *Reaper) updateFeedAndSaveNewItemsToDb(f *gofeed.Feed) {
	originalListOfItems := f.Items

	fp := gofeed.NewParser()
	newF, err := fp.ParseURL(f.FeedLink)
	newF.FeedLink = f.FeedLink // sometimes this gets overwritten for some reason

	if err != nil {
		r.handleFeedFetchFailure(newF.FeedLink, err)
	} else {
		r.feeds[newF.FeedLink].Feed = newF
		r.feeds[newF.FeedLink].LastFetched = time.Now()
	}

	newItems := []*gofeed.Item{}
	for _, item := range newF.Items {
		isNew := true
		for _, originalItem := range originalListOfItems {
			if item.Link == originalItem.Link {
				isNew = false
				break
			}
		}
		if isNew {
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
}

// UpdateAll fetches every feed & attempts updating them
// asynchronously, then prints the duration of the sync
func (r *Reaper) refreshAllFeeds() {
	start := time.Now()

	semaphore := make(chan struct{}, 20)

	for i := range r.feeds {
		if r.feeds[i].LastFetched.Add(timeToBecomeStale).Before(time.Now()) {
			semaphore <- struct{}{} // acquire a token
			go func(feedHolder *FeedHolder) {
				defer func() { <-semaphore }() // release the token when done
				r.updateFeedAndSaveNewItemsToDb(feedHolder.Feed)
			}(r.feeds[i])
		}
	}

	// wait for all goroutines to finish
	for i := 0; i < cap(semaphore); i++ {
		semaphore <- struct{}{}
	}

	log.Printf("reaper: refresh complete in %s\n", time.Since(start))
}

func (r *Reaper) handleFeedFetchFailure(url string, err error) {
	log.Printf("[err] reaper: fetch failure '%s': %s\n", url, err)
	err = r.db.SetFeedFetchError(url, err.Error())
	if err != nil {
		log.Printf("[err] reaper: could not set feed fetch error '%s'\n", err)
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

// GetUserFeeds returns a list of feeds
func (r *Reaper) GetUserFeeds(username string) []*gofeed.Feed {
	urls := r.db.GetUserFeedURLs(username)
	var result []*gofeed.Feed
	for _, u := range urls {
		// feeds in the db are guaranteed to be in reaper
		result = append(result, r.feeds[u].Feed)
	}

	r.SortFeeds(result)
	return result
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

// Fetch attempts to fetch a feed from a given url, marshal
// it into a feed object, and manage it via reaper.
func (r *Reaper) Fetch(url string) error {
	fp := gofeed.NewParser()
	feed, err := fp.ParseURL(url)
	if err != nil {
		return err
	}

	r.feeds[url] = &FeedHolder{
		Feed:        feed,
		LastFetched: time.Now(),
	}

	return nil
}
