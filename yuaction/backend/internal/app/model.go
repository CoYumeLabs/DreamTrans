package app

import "time"

type Question struct {
	ID         string    `json:"id"`
	Content    string    `json:"content"`
	Status     string    `json:"status"`
	SegmentID  string    `json:"segmentId,omitempty"`
	SegmentIDs []string  `json:"segmentIds,omitempty"`
	QuotedText string    `json:"quotedText,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Final recognition fragments form stable caption cards. Appended fragments
// update a card without duplicating its identity; translations key by language.
type Segment struct {
	ID                string            `json:"id"`
	Text              string            `json:"text"`
	Translation       string            `json:"translation,omitempty"`
	Source            string            `json:"source"`
	CreatedAt         time.Time         `json:"createdAt"`
	StartTime         float64           `json:"startTime,omitempty"`
	EndTime           float64           `json:"endTime,omitempty"`
	Speaker           string            `json:"speaker,omitempty"`
	Archived          bool              `json:"archived,omitempty"`
	UpdatedAt         time.Time         `json:"updatedAt,omitempty"`
	Parts             int               `json:"parts,omitempty"`
	Translations      map[string]string `json:"translations,omitempty"`
	TranslationErrors map[string]string `json:"translationErrors,omitempty"`
}

type Room struct {
	HandoffUntil  time.Time  `json:"handoffUntil,omitempty"`
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
