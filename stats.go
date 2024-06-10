package main

import "time"

type MireSiteStats struct {
	LastComputed   time.Time
	TotalUsers     int
	NumReadPosts   int
	NumUniqueFeeds int
}

var globalSiteStats *MireSiteStats = &MireSiteStats{}

func statsCalculatorProcess(s *Site) {
	for {
		globalSiteStats.LastComputed = time.Now()
		globalSiteStats.NumReadPosts = s.db.GetGlobalNumReadPosts()
		globalSiteStats.NumUniqueFeeds = s.db.GetGlobalNumUniqueFeeds()
		globalSiteStats.TotalUsers = s.db.GetGlobalNumUsers()

		time.Sleep(6 * time.Hour)
	}
}
