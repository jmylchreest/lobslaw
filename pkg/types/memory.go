package types

import "time"

type Metadata map[string]string

// VectorRecord is a text item indexed by its embedding. SourceIDs is
// the consolidation provenance: empty for originals, populated for
// records produced by dream/REM — forget cascades through it.
type VectorRecord struct {
	ID        string    `json:"id"`
	Embedding []float32 `json:"embedding"`
	Text      string    `json:"text"`
	Metadata  Metadata  `json:"metadata,omitempty"`
	Scope     string    `json:"scope,omitempty"`
	Retention Retention `json:"retention"`
	SourceIDs []string  `json:"source_ids,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type EpisodicRecord struct {
	ID         string    `json:"id"`
	Event      string    `json:"event"`
	Context    string    `json:"context,omitempty"`
	Importance int       `json:"importance"`
	Timestamp  time.Time `json:"ts"`
	Tags       []string  `json:"tags,omitempty"`
	Retention  Retention `json:"retention"`
	SourceIDs  []string  `json:"source_ids,omitempty"`
}

// ForgetQuery is AND-semantics across set fields.
type ForgetQuery struct {
	Query  string    `json:"query,omitempty"`
	Before time.Time `json:"before,omitempty"`
	Tags   []string  `json:"tags,omitempty"`
}
