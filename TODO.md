Things to do

# Improving memory usage and persistence
- [ ] Reaper should save posts into DB instead of keeping them in memory
    - Memory is a reason, but the main one is that feeds frequently don't
      include ALL the posts, but only a subset (eg, last 10 for Bearblog).
    - [ ] User page should load posts from DB, not from Reaper 
- [ ] Reaper should not maintain a in-memory list of the known feeds. Mainly
  because there's no reason to do so.

# Tracking read status in user page
- [ ] We'll probably need a table to track which users read which posts
- [ ] Visiting a page should tigger something to know that the user has "read"
  that post
    - We might need JS for this one :/
- [ ] Clicking on something (an envelope emoji?) should mark the post as
  read/unread.