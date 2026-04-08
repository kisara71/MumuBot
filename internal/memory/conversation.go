package memory

import (
	"mumu-bot/internal/session"

	"gorm.io/gorm"
)

type SessionScopeFields struct {
	GroupField   string
	PrivateField string
}

var sessionFields = SessionScopeFields{
	GroupField:   "group_id",
	PrivateField: "user_id",
}

func scopeSession(ref session.Ref, q *gorm.DB, fields SessionScopeFields) *gorm.DB {
	switch {
	case ref.IsGroup():
		return q.Where(fields.GroupField+" = ?", ref.GroupID)
	case ref.IsPrivate():
		return q.Where(fields.PrivateField+" = ?", ref.UserID)
	default:
		return q
	}
}

func scopeMessageLogs(ref session.Ref, q *gorm.DB) *gorm.DB {
	if id := ref.ID(); id != "" {
		return q.Where("conversation_id = ?", id)
	}
	return q
}
