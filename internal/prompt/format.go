package prompt

import (
	"fmt"
	"strings"
	"time"

	"github.com/6tail/lunar-go/calendar"
)

func (b *Builder) writeSharedThinkPrefix(out *strings.Builder, ctx *Context) {
	out.WriteString("时间：" + currentTimeContext() + "\n")
	if ctx != nil && ctx.MoodState != nil {
		out.WriteString(formatMoodPrompt(ctx.MoodState))
	}
}

func currentTimeContext() string {
	now := time.Now()
	weekday := now.Weekday()
	weekStr := [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}
	solar := calendar.NewSolarFromDate(now)
	lunar := solar.GetLunar()
	return fmt.Sprintf("%s %s %02d:%02d | %s", now.Format("2006-01-02"), weekStr[weekday], now.Hour(), now.Minute(), lunar.String())
}

func formatMoodPrompt(mood *MoodInfo) string {
	return fmt.Sprintf("情绪：心情=%.2f 精力=%.2f 社交=%.2f 烦躁=%.2f 好奇=%.2f\n",
		mood.Valence, mood.Energy, mood.Sociability, mood.Irritation, mood.Curiosity)
}
