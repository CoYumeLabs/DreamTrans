package app

import "time"

type Question struct {
	ID         string    `json:"id"`
	Content    string    `json:"content"`
	Status     string    `json:"status"`
	SegmentID  string    `json:"segmentId,omitempty"`
	QuotedText string    `json:"quotedText,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Only finalized segments enter the shared room. ID is the provider's stable
// event identifier: retries replace neither content nor sequence.
type Segment struct {
	ID          string    `json:"id"`
	Text        string    `json:"text"`
	Translation string    `json:"translation,omitempty"`
	Source      string    `json:"source"`
	CreatedAt   time.Time `json:"createdAt"`
	StartTime   float64   `json:"startTime,omitempty"`
	EndTime     float64   `json:"endTime,omitempty"`
	Speaker     string    `json:"speaker,omitempty"`
	Archived    bool      `json:"archived,omitempty"`
}

type Room struct {
	Code          string     `json:"code"`
	Title         string     `json:"title"`
	Kind          string     `json:"kind"`
	Status        string     `json:"status"`
	Revision      int64      `json:"revision"`
	CreatedAt     time.Time  `json:"createdAt"`
	Questions     []Question `json:"questions"`
	Segments      []Segment  `json:"segments"`
	Transcription string     `json:"transcription,omitempty"`
}

// Public snapshots include a bounded caption window. Older finalized captions
// remain in persistent state; full archival/export is a later milestone.
func (r Room) Public() Room {
	if len(r.Segments) > 50 {
		r.Segments = r.Segments[len(r.Segments)-50:]
	}
	return r
}
