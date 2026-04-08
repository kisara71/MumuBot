package onebot

import "mumu-bot/internal/session"

type MessageSource = session.MessageSource

const (
	MessageSourceUnknown = session.MessageSourceUnknown
	MessageSourceGroup   = session.MessageSourceGroup
	MessageSourcePrivate = session.MessageSourcePrivate
)

type NoticeEventType string

const (
	NoticeEventTypeFriendAdd NoticeEventType = "friend_add"
	NoticeEventTypeGroupBan  NoticeEventType = "group_ban"
)
