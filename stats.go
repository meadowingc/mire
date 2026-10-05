package main

import (
	"context"
	"time"
)

type MireSiteStats struct {
	LastComputed   time.Time
	TotalUsers     int
	NumReadPosts   int
	NumUniqueFeeds int
}

func statsCalculatorProcess(ctx context.Context, s *Site) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		stats := MireSiteStats{
			LastComputed:   time.Now(),
			NumReadPosts:   s.db.GetGlobalNumReadPosts(),
			NumUniqueFeeds: s.db.GetGlobalNumUniqueFeeds(),
			TotalUsers:     s.db.GetGlobalNumUsers(),
		}
		s.statsMu.Lock()
		s.stats = stats
		s.statsMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
