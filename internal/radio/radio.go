// Package radio holds the station domain types shared by the store and the Go API.
//
// The JSON tags on these types ARE the public contract: they mirror, field by field, what the
// legacy PHP API emits. Renaming a field or changing its type is a breaking change that the
// shadow comparison will flag, so treat them as frozen until the legacy API is gone.
package radio

import "time"

// Program is a recurring show in the weekly schedule.
type Program struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Host    string `json:"host"`
	Weekday int    `json:"weekday"` // 0 = Sunday, like Postgres EXTRACT(DOW)
	// Times are "HH:MM" strings, not time.Time: that is what the legacy API returns.
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
}

// Track is an item of the music library.
type Track struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	// Pointer so a missing album serializes as null, exactly like PHP, instead of "".
	Album           *string  `json:"album"`
	DurationSeconds int      `json:"duration_seconds"`
	Tags            []string `json:"tags"`
}

// Play is one broadcast of a track. It is internal; the API shapes the response itself.
type Play struct {
	Track    Track
	PlayedAt time.Time
}
