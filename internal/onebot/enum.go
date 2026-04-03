package onebot

import "mumu-bot/internal/conversation"

type MessageSource = conversation.MessageSource

const (
	MessageSourceUnknown = conversation.MessageSourceUnknown
	MessageSourceGroup   = conversation.MessageSourceGroup
	MessageSourcePrivate = conversation.MessageSourcePrivate
)

type NoticeEventType string

const (
	NoticeEventTypeFriendAdd NoticeEventType = "friend_add"
	NoticeEventTypeGroupBan  NoticeEventType = "group_ban"
)
