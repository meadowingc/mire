package reaper

import (
	"log"
	"sort"
	"time"

	"codeberg.org/meadowingc/mire/rss"
	"codeberg.org/meadowingc/mire/sqlite"
)

type PostSaveRequest struct {
	FeedURL string
	Title   string
	Link    string
	Date    time.Time
}

type Reaper struct {
	// internal list of all rss feeds where the map
	// key represents the url of the feed (which should be unique)
	feeds map[string]*rss.Feed

	saverChannel chan *PostSaveRequest

	db *sqlite.DB
}

func New(db *sqlite.DB) *Reaper {
	r := &Reaper{
		feeds:        make(map[string]*rss.Feed),
		saverChannel: make(chan *PostSaveRequest),
		db:           db,
	}

	go r.start()
	go r.startDbSaver()

	return r
}

// Start initializes the reaper by populating a list of feeds from the database
// and periodically refreshes all feeds every hour, if the feeds are
// stale.
// reaper should only ever be started once (in New)
func (r *Reaper) start() {
	urls := r.db.GetAllFeedURLs()

	for _, url := range urls {
		// Setting UpdateURL lets us defer fetching
		feed := &rss.Feed{
			UpdateURL: url,
		}
		r.feeds[url] = feed
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
			r.db.SavePost(item.FeedURL, item.Title, item.Link, item.Date)
		default:
			time.Sleep(10 * time.Second)
		}
	}
}

// Add the given rss feed to Reaper for maintenance.
func (r *Reaper) addFeed(f *rss.Feed) {
	r.feeds[f.UpdateURL] = f
}

func (r *Reaper) updateFeedAndSaveNewItemsToDb(f *rss.Feed) {
	originalListOfItems := f.Items

	err := f.Update()
	if err != nil {
		r.handleFeedFetchFailure(f.UpdateURL, err)
	}

	newItems := []*rss.Item{}
	for _, item := range f.Items {
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
		log.Printf("Saving %d new items for feed %s\n", len(newItems), f.UpdateURL)

		for _, newItem := range newItems {
			r.saverChannel <- &PostSaveRequest{
				FeedURL: f.UpdateURL,
				Title:   newItem.Title,
				Link:    newItem.Link,
				Date:    newItem.Date,
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
		if r.feeds[i].Stale() {
			semaphore <- struct{}{} // acquire a token
			go func(feed *rss.Feed) {
				defer func() { <-semaphore }() // release the token when done
				r.updateFeedAndSaveNewItemsToDb(feed)
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

func (r *Reaper) GetFeed(url string) *rss.Feed {
	return r.feeds[url]
}

// GetUserFeeds returns a list of feeds
func (r *Reaper) GetUserFeeds(username string) []*rss.Feed {
	urls := r.db.GetUserFeedURLs(username)
	var result []*rss.Feed
	for _, u := range urls {
		// feeds in the db are guaranteed to be in reaper
		result = append(result, r.feeds[u])
	}

	r.SortFeeds(result)
	return result
}

func (r *Reaper) GetAllFeeds() []*rss.Feed {
	var result []*rss.Feed
	for _, f := range r.feeds {
		result = append(result, f)
	}

	return result
}

func (r *Reaper) SortFeeds(f []*rss.Feed) {
	sort.Slice(f, func(i, j int) bool {
		return f[i].UpdateURL < f[j].UpdateURL
	})
}

func (r *Reaper) SortFeedItemsByDate(feeds []*rss.Feed) []*rss.Item {
	var posts []*rss.Item
	for _, f := range feeds {
		posts = append(posts, f.Items...)
	}

	return r.SortItemsByDate(posts)
}

func (r *Reaper) SortItemsByDate(posts []*rss.Item) []*rss.Item {
	sort.Slice(posts, func(i, j int) bool {
		return posts[i].Date.After(posts[j].Date)
	})
	return posts
}

// Fetch attempts to fetch a feed from a given url, marshal
// it into a feed object, and manage it via reaper.
func (r *Reaper) Fetch(url string) error {
	feed, err := rss.Fetch(url)
	if err != nil {
		return err
	}

	r.addFeed(feed)

	return nil
}
