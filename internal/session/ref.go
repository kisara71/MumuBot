package session

import "fmt"

// Ref identifies one private relationship. There is no source/type dimension:
// Luma only accepts direct messages.
type Ref struct {
	UserID int64 `json:"user_id"`
}

func NewRef(userID int64) Ref { return Ref{UserID: userID} }

func (ref Ref) Valid() bool { return ref.UserID > 0 }

func (ref Ref) ID() string {
	if !ref.Valid() {
		return ""
	}
	return fmt.Sprintf("user_%d", ref.UserID)
}

func ParseRefID(id string) (Ref, bool) {
	var userID int64
	if _, err := fmt.Sscanf(id, "user_%d", &userID); err == nil && userID > 0 {
		return NewRef(userID), true
	}
	return Ref{}, false
}
