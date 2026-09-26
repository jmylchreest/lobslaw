package turn

// TaskScope is server-owned execution identity. A child runner must create a
// new scope; ParentID is attribution and never permission inheritance.
type TaskScope struct {
	ID         string
	Owner      string
	Actor      string
	ClaimToken string
}
