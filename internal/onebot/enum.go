package onebot

type MessageSource string

const (
	MessageSourceUnknown MessageSource = "unknown"
	MessageSourceGroup                 = "group"
	MessageSourcePrivate               = "private"
)

type NoticeEventType string

const (
	NoticeEventTypeFriendAdd NoticeEventType = "friend_add"
	NoticeEventTypeGroupBan  NoticeEventType = "group_ban"
)
