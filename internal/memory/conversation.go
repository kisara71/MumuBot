package memory

import (
	"github.com/kisara71/luma/internal/session"

	"gorm.io/gorm"
)

// All private data access is scoped at the repository boundary. An invalid or
// non-private ref matches nothing instead of silently becoming a global query.
func scopeSession(ref session.Ref, q *gorm.DB) *gorm.DB {
	if ref.Valid() {
		return q.Where("user_id = ?", ref.UserID)
	}
	return q.Where("1 = 0")
}

func scopeMessageLogs(ref session.Ref, q *gorm.DB) *gorm.DB {
	if ref.Valid() {
		return q.Where("conversation_id = ?", ref.ID())
	}
	return q.Where("1 = 0")
}
