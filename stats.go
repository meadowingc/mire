package main

import "time"

type MireSiteStats struct {
	LastComputed   time.Time
	TotalUsers     int
	NumReadPosts   int
	NumUniqueFeeds int
}

var globalStats *MireSiteStats = &MireSiteStats{}

func statsCalculatorProcess(s *Site) {
	for {
		globalStats.LastComputed = time.Now()
		globalStats.NumReadPosts = s.db.GetGlobalNumReadPosts()
		globalStats.NumUniqueFeeds = s.db.GetGlobalNumUniqueFeeds()
		globalStats.TotalUsers = s.db.GetGlobalNumUsers()

		time.Sleep(24 * time.Hour)
	}
}
