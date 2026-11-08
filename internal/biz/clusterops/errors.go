package clusterops

// Error contains only safe, stable public information. Never wrap API bodies.
type Error struct {
	Reason   string
	Metadata map[string]string
}

func (e *Error) Error() string      { return e.Reason }
func NewError(reason string) *Error { return &Error{Reason: reason} }
