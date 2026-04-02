package memory

import "gorm.io/gorm"

type ConversationScopeFields struct {
	GroupField   string
	PrivateField string
}

var conversationFields = ConversationScopeFields{
	GroupField:   "group_id",
	PrivateField: "user_id",
}

func (ref ConversationRef) Scope(q *gorm.DB, fields ConversationScopeFields) *gorm.DB {
	switch {
	case ref.IsGroup():
		return q.Where(fields.GroupField+" = ?", ref.GroupID)
	case ref.IsPrivate():
		return q.Where(fields.PrivateField+" = ?", ref.UserID)
	default:
		return q
	}
}

func (ref ConversationRef) ScopeMessageLogs(q *gorm.DB) *gorm.DB {
	switch {
	case ref.IsGroup():
		return q.Where("message_source = ? AND group_id = ?", ref.Source, ref.GroupID)
	case ref.IsPrivate():
		return q.Where("message_source = ? AND user_id = ?", ref.Source, ref.UserID)
	default:
		return q
	}
}
