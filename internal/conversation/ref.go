package conversation

import "fmt"

type MessageSource string

const (
	MessageSourceUnknown MessageSource = "unknown"
	MessageSourceGroup   MessageSource = "group"
	MessageSourcePrivate MessageSource = "private"
)

type Ref struct {
	Source  MessageSource `json:"source"`
	GroupID int64         `json:"group_id,omitempty"`
	UserID  int64         `json:"user_id,omitempty"`
}

func AllConversationRef() Ref {
	return Ref{}
}

func GroupConversationRef(groupID int64) Ref {
	return Ref{Source: MessageSourceGroup, GroupID: groupID}
}

func PrivateConversationRef(userID int64) Ref {
	return Ref{Source: MessageSourcePrivate, UserID: userID}
}

func (ref Ref) IsGroup() bool {
	return ref.Source == MessageSourceGroup && ref.GroupID > 0
}

func (ref Ref) IsPrivate() bool {
	return ref.Source == MessageSourcePrivate && ref.UserID > 0
}

func (ref Ref) ID() string {
	if ref.IsGroup() {
		return fmt.Sprintf("group_%d", ref.GroupID)
	}
	if ref.IsPrivate() {
		return fmt.Sprintf("private_%d", ref.UserID)
	}
	return ""
}

func ParseRefID(id string) (Ref, bool) {
	var groupID int64
	if _, err := fmt.Sscanf(id, "group_%d", &groupID); err == nil && groupID > 0 {
		return GroupConversationRef(groupID), true
	}

	var userID int64
	if _, err := fmt.Sscanf(id, "private_%d", &userID); err == nil && userID > 0 {
		return PrivateConversationRef(userID), true
	}

	return Ref{}, false
}
