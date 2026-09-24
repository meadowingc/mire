package reaper

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

var feedHTTPClient = &http.Client{Timeout: 60 * time.Second}

func (r *Reaper) rawFetchFeed(url string) (*gofeed.Feed, time.Time, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("Mire (+https://mire.meadow.cafe) - %d subscribers",
		r.db.GetNumSubscribersForFeed(url)))

	resp, err := feedHTTPClient.Do(req)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, time.Time{}, gofeed.HTTPError{StatusCode: resp.StatusCode, Status: resp.Status}
	}
	receivedAt := time.Now()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, time.Time{}, err
	}
	feed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	if err != nil {
		return nil, time.Time{}, err
	}
	if feed.FeedType == "atom" {
		if subtitle, found, err := plainAtomSubtitle(body); err != nil {
			log.Printf("[warning] reaper: could not decode Atom subtitle for '%s': %v", url, err)
		} else if found {
			feed.Description = subtitle
		}
	}
	return feed, freshnessDeadline(resp.Header, receivedAt), nil
}

// plainAtomSubtitle reads the XML text construct, not the parser's serialized
// markup, so real XML comments disappear but escaped comment-looking text stays.
func plainAtomSubtitle(body []byte) (string, bool, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		switch element := token.(type) {
		case xml.StartElement:
			depth++
			if depth != 2 || element.Name.Space != "http://www.w3.org/2005/Atom" || element.Name.Local != "subtitle" {
				continue
			}
			for _, attribute := range element.Attr {
				if attribute.Name.Local == "type" && attribute.Name.Space == "" &&
					attribute.Value != "" && attribute.Value != "text" {
					return "", false, nil
				}
			}
			var subtitle string
			if err := decoder.DecodeElement(&subtitle, &element); err != nil {
				return "", false, err
			}
			return subtitle, true, nil
		case xml.EndElement:
			depth--
		}
	}
}

func freshnessDeadline(headers http.Header, receivedAt time.Time) time.Time {
	var maxAge int64
	hasMaxAge := false
	noCache := false
	for _, header := range headers.Values("Cache-Control") {
		for _, directive := range strings.Split(header, ",") {
			parts := strings.SplitN(strings.TrimSpace(directive), "=", 2)
			switch strings.ToLower(strings.TrimSpace(parts[0])) {
			case "no-cache", "no-store":
				noCache = true
			case "max-age":
				if len(parts) != 2 {
					log.Printf("[warning] reaper: invalid Cache-Control max-age: %q", directive)
					continue
				}
				value := strings.Trim(strings.TrimSpace(parts[1]), "\"")
				seconds, err := strconv.ParseInt(value, 10, 64)
				if err != nil || seconds < 0 {
					log.Printf("[warning] reaper: invalid Cache-Control max-age: %q", directive)
					continue
				}
				if !hasMaxAge || seconds > maxAge {
					maxAge = seconds
				}
				hasMaxAge = true
			}
		}
	}
	if noCache {
		return time.Time{}
	}

	date, dateErr := http.ParseTime(headers.Get("Date"))
	age := int64(0)
	if value := headers.Get("Age"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			log.Printf("[warning] reaper: invalid Age header: %q", value)
		} else {
			age = parsed
		}
	}
	if dateErr == nil && receivedAt.After(date) {
		if elapsed := int64(receivedAt.Sub(date).Seconds()); elapsed > age {
			age = elapsed
		}
	}

	if hasMaxAge {
		if age >= maxAge {
			return time.Time{}
		}
		return addFreshSeconds(receivedAt, maxAge-age)
	}
	expires := headers.Get("Expires")
	if expires == "" {
		return time.Time{}
	}
	expiry, err := http.ParseTime(expires)
	if err != nil {
		log.Printf("[warning] reaper: invalid Expires header: %q", expires)
		return time.Time{}
	}
	if dateErr == nil {
		if expiry.Before(date) {
			return time.Time{}
		}
		lifetime := int64(expiry.Sub(date).Seconds())
		if lifetime <= age {
			return time.Time{}
		}
		return addFreshSeconds(receivedAt, lifetime-age)
	}
	return expiry
}

func addFreshSeconds(now time.Time, seconds int64) time.Time {
	if seconds > math.MaxInt64-now.Unix() {
		log.Printf("[warning] reaper: freshness deadline exceeds supported timestamp range")
		return time.Time{}
	}
	return time.Unix(now.Unix()+seconds, int64(now.Nanosecond())).UTC()
}
