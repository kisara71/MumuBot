package memory

import (
	"context"
	"errors"
	"fmt"
	"mumu-bot/internal/config"
	"mumu-bot/internal/session"
	"mumu-bot/internal/utils"
	"mumu-bot/internal/vector"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// EmbeddingProvider 向量嵌入接口
type EmbeddingProvider interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

type vectorStore interface {
	Insert(ctx context.Context, memoryID uint, refID string, memType string, embedding []float64) (int64, error)
	Search(ctx context.Context, embedding []float64, refID string, memType string, topK int, threshold float64) ([]vector.SearchResult, error)
	Delete(ctx context.Context, memoryIDs []uint) error
	DeleteByRef(ctx context.Context, refID string) error
	Close() error
	GetConfig() *vector.MilvusConfig
}

// Manager 记忆系统管理器
type Manager struct {
	db              *gorm.DB
	embedding       EmbeddingProvider
	milvus          vectorStore // Memory 向量存储
	styleCardMilvus vectorStore // StyleCard 向量存储
	cleanupStop     chan struct{}
}

func buildLikeQuery(columns []string, keywords []string) (string, []interface{}) {
	if len(columns) == 0 || len(keywords) == 0 {
		return "", nil
	}
	likeConditions := make([]string, 0, len(keywords))
	args := make([]interface{}, 0, len(columns)*len(keywords))
	for _, kw := range keywords {
		columnConds := make([]string, 0, len(columns))
		pattern := "%" + kw + "%"
		for _, column := range columns {
			columnConds = append(columnConds, column+" LIKE ?")
			args = append(args, pattern)
		}
		likeConditions = append(likeConditions, "("+strings.Join(columnConds, " OR ")+")")
	}
	return strings.Join(likeConditions, " OR "), args
}

func reverseMessageLogs(items []MessageLog) {
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
}

// NewManager 创建记忆管理器
func NewManager(embedding EmbeddingProvider) (*Manager, error) {
	if embedding == nil {
		return nil, fmt.Errorf("embedding 未初始化，Milvus 为强制依赖")
	}

	// 构建 MySQL DSN
	cfg := config.Get()
	mysqlCfg := cfg.Memory.MySQL
	if mysqlCfg.Host == "" {
		mysqlCfg.Host = "127.0.0.1"
	}
	if mysqlCfg.Port == 0 {
		mysqlCfg.Port = 3306
	}
	if mysqlCfg.DBName == "" {
		mysqlCfg.DBName = "mumu_bot"
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		mysqlCfg.User,
		mysqlCfg.Password,
		mysqlCfg.Host,
		mysqlCfg.Port,
		mysqlCfg.DBName,
	)

	db, err := gorm.Open(mysql.Open(dsn))
	if err != nil {
		return nil, fmt.Errorf("连接 MySQL 数据库失败: %w", err)
	}

	// 迁移所有表
	if err := db.AutoMigrate(
		&Memory{},
		&UserProfile{},
		&StyleCard{},
		&Jargon{},
		&MessageLog{},
		&Sticker{},
		&MoodState{},
		&LearningState{},
	); err != nil {
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	milvusCfg := &vector.MilvusConfig{
		Address:        cfg.Memory.Milvus.Address,
		DBName:         cfg.Memory.Milvus.DBName,
		CollectionName: cfg.Memory.Milvus.CollectionName,
		VectorDim:      cfg.Memory.Milvus.VectorDim,
		MetricType:     cfg.Memory.Milvus.MetricType,
	}
	milvusClient, err := vector.NewMilvusClient(milvusCfg)
	if err != nil {
		return nil, fmt.Errorf("连接记忆 Milvus 失败: %w", err)
	}

	styleMilvusCfg := &vector.MilvusConfig{
		Address:        cfg.Memory.Milvus.Address,
		DBName:         cfg.Memory.Milvus.DBName,
		CollectionName: styleCardCollectionName(cfg.Memory.Milvus.CollectionName),
		VectorDim:      cfg.Memory.Milvus.VectorDim,
		MetricType:     cfg.Memory.Milvus.MetricType,
	}
	styleMilvusClient, err := vector.NewMilvusClient(styleMilvusCfg)
	if err != nil {
		_ = milvusClient.Close()
		return nil, fmt.Errorf("连接风格卡片 Milvus 失败: %w", err)
	}

	zap.L().Info("Milvus 向量存储已连接",
		zap.String("memory_collection", milvusCfg.CollectionName),
		zap.String("style_card_collection", styleMilvusCfg.CollectionName))

	m := &Manager{
		db:              db,
		embedding:       embedding,
		milvus:          milvusClient,
		styleCardMilvus: styleMilvusClient,
		cleanupStop:     make(chan struct{}),
	}

	// 启动消息日志清理任务
	m.startMessageLogCleanup()

	// 启动情绪衰减任务
	m.startMoodDecay()

	return m, nil
}

// ==================== 短期记忆 ====================

// AddMessage 添加消息到短期记忆
func (m *Manager) AddMessage(msg MessageLog) error {
	return m.db.Create(&msg).Error
}

// GetRecentMessages 获取最近的消息记录
func (m *Manager) GetRecentMessages(ref ConversationRef, limit, offset int) []MessageLog {
	var dbMsgs []MessageLog
	q := scopeMessageLogs(ref, m.db.Model(&MessageLog{})).Order("created_at DESC").Limit(limit)
	if offset > 0 {
		q = q.Offset(offset)
	}
	q.Find(&dbMsgs)

	reverseMessageLogs(dbMsgs)
	return dbMsgs
}

// GetMessagesAfterID 获取指定消息ID之后的消息
func (m *Manager) GetMessagesAfterID(ref ConversationRef, selfID int64, lastID uint, limit int) ([]MessageLog, error) {
	var dbMsgs []MessageLog
	q := scopeMessageLogs(ref, m.db.Model(&MessageLog{})).
		Where("id > ? AND user_id != ?", lastID, selfID).
		Order("id ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Find(&dbMsgs).Error
	return dbMsgs, err
}

// GetMessageCountByTime 获取指定用户在指定作用域一段时间内的消息数量
func (m *Manager) GetMessageCountByTime(ref ConversationRef, userID int64, startTime time.Time) (int64, error) {
	var count int64
	err := scopeMessageLogs(ref, m.db.Model(&MessageLog{})).
		Where("user_id = ? AND created_at >= ?", userID, startTime).
		Count(&count).Error
	return count, err
}

// ==================== 长期记忆 ====================
// TODO 私聊长期记忆

// SearchSimilarMemoriesByConversation 按会话引用和记忆类型搜索相似记忆
func (m *Manager) SearchSimilarMemoriesByConversation(ctx context.Context, text string, ref ConversationRef, memType MemoryType, limit int, threshold float64) ([]Memory, error) {
	if m.milvus == nil || m.embedding == nil {
		return nil, errors.New("向量检索未启用")
	}
	if limit <= 0 {
		limit = 15
	}

	emb, err := m.embedding.Embed(ctx, text)
	if err != nil {
		return nil, err
	}

	results, err := m.milvusVectorSearch(ctx, emb, ref.ID(), normalizeMemoryTypeFilter(memType), limit, threshold)
	if err != nil {
		return nil, err
	}

	return results, nil
}

// UpdateMemoryContent 更新记忆内容（用于合并）
func (m *Manager) UpdateMemoryContent(ctx context.Context, id uint, newContent string) error {
	// 更新数据库
	if err := m.db.Model(&Memory{}).Where("id = ?", id).Update("content", newContent).Error; err != nil {
		return err
	}

	// 更新向量（先删后增）
	if m.milvus != nil && m.embedding != nil {
		_ = m.milvus.Delete(ctx, []uint{id})

		emb, err := m.embedding.Embed(ctx, newContent)
		if err == nil {
			var mem Memory
			if err := m.db.First(&mem, id).Error; err == nil {
				_, _ = m.milvus.Insert(ctx, id, mem.ConversationID, string(mem.Type), emb)
			}
		}
	}
	return nil
}

// DeleteMemory 删除记忆
func (m *Manager) DeleteMemory(ctx context.Context, id uint) error {
	if err := m.db.Delete(&Memory{}, id).Error; err != nil {
		return err
	}
	if m.milvus != nil {
		_ = m.milvus.Delete(ctx, []uint{id})
	}
	return nil
}

// SaveMemory 保存长期记忆
func (m *Manager) SaveMemory(ctx context.Context, mem *Memory) error {
	// 生成 embedding
	var embedding []float64
	if m.embedding != nil {
		if emb, err := m.embedding.Embed(ctx, mem.Content); err == nil {
			embedding = emb
		}
	}

	// 保存到 MySQL
	if err := m.db.Save(mem).Error; err != nil {
		return err
	}

	// 保存向量到 Milvus
	if m.milvus != nil && len(embedding) > 0 {
		if _, err := m.milvus.Insert(ctx, mem.ID, mem.ConversationID, string(mem.Type), embedding); err != nil {
			// 向量插入失败只记录日志，不影响主流程
			zap.L().Error("Milvus 插入向量失败", zap.Error(err))
		}
	}

	return nil
}

// QueryMemoryByConversation 查询相关记忆
func (m *Manager) QueryMemoryByConversation(ctx context.Context, query string, ref ConversationRef, memType MemoryType, limit int) ([]Memory, error) {
	// 尝试 Milvus 向量搜索
	if m.milvus != nil && m.embedding != nil {
		if emb, err := m.embedding.Embed(ctx, query); err == nil {
			if results, err := m.milvusVectorSearch(ctx, emb, ref.ID(), normalizeMemoryTypeFilter(memType), limit, 0.7); err == nil && len(results) > 0 {
				return results, nil
			}
		}
	}

	// 回退到关键词搜索
	var memories []Memory
	q := scopeSession(ref, m.db.Model(&Memory{}), sessionFields)
	if memType != "" {
		q = q.Where("type = ?", memType)
	}
	keywords := strings.Fields(query)
	if len(keywords) == 0 {
		return memories, nil
	}
	condition, args := buildLikeQuery([]string{"content"}, keywords)
	err := q.Where(condition, args...).
		Order("importance DESC, updated_at DESC").
		Limit(limit).
		Find(&memories).Error
	if err != nil {
		return memories, err
	}

	if len(memories) > 0 {
		memoryIDs := make([]uint, 0, len(memories))
		for _, mem := range memories {
			memoryIDs = append(memoryIDs, mem.ID)
		}
		_ = m.db.Model(&Memory{}).Where("id IN ?", memoryIDs).Updates(map[string]any{
			"access_count": gorm.Expr("access_count + 1"),
		}).Error
	}

	return memories, nil
}

// startMessageLogCleanup 启动消息日志清理定时任务
func (m *Manager) startMessageLogCleanup() {
	cleanupCfg := config.Get().Memory.MessageLogCleanup
	enabled := true
	if cleanupCfg.Enabled != nil {
		enabled = *cleanupCfg.Enabled
	}
	if !enabled {
		return
	}

	intervalHours := cleanupCfg.IntervalHours
	if intervalHours <= 0 {
		intervalHours = 6
	}
	keepLatest := cleanupCfg.KeepLatest
	if keepLatest <= 0 {
		keepLatest = 500
	}

	// 启动后立即清理一次
	go m.cleanupMessageLogs(keepLatest)

	ticker := time.NewTicker(time.Duration(intervalHours) * time.Hour)
	go func() {
		for {
			select {
			case <-ticker.C:
				m.cleanupMessageLogs(keepLatest)
			case <-m.cleanupStop:
				ticker.Stop()
				return
			}
		}
	}()
}

// cleanupMessageLogs 清理消息日志，仅保留每个会话最新的 keepLatest 条
func (m *Manager) cleanupMessageLogs(keepLatest int) {
	if keepLatest <= 0 {
		return
	}

	type logScope struct {
		ConversationID string
	}

	var scopes []logScope
	if err := m.db.Model(&MessageLog{}).
		Where("conversation_id <> ''").
		Select("conversation_id").
		Group("conversation_id").
		Scan(&scopes).Error; err != nil {
		zap.L().Warn("清理消息日志失败：获取会话列表失败", zap.Error(err))
		return
	}

	for _, scope := range scopes {
		ref, ok := session.ParseRefID(scope.ConversationID)
		if !ok {
			continue
		}

		var keepIDs []uint
		if err := scopeMessageLogs(ref, m.db.Model(&MessageLog{})).
			Order("created_at DESC").
			Limit(keepLatest).
			Pluck("id", &keepIDs).Error; err != nil {
			zap.L().Warn("清理消息日志失败：获取保留ID失败", zap.String("source", string(ref.Source)), zap.String("id", ref.ID()), zap.Error(err))
			continue
		}
		if len(keepIDs) == 0 {
			continue
		}

		result := scopeMessageLogs(ref, m.db.Where("id NOT IN ?", keepIDs)).Delete(&MessageLog{})
		if result.Error != nil {
			zap.L().Warn("清理消息日志失败：删除旧记录失败", zap.String("source", string(ref.Source)), zap.String("id", ref.ID()), zap.Error(result.Error))
			continue
		}
		if result.RowsAffected > 0 {
			zap.L().Info("消息日志已清理", zap.String("source", string(ref.Source)), zap.String("id", ref.ID()), zap.Int("deleted", int(result.RowsAffected)))
		}
	}
}

// milvusVectorSearch 使用 Milvus 进行向量搜索并返回完整的 Memory 对象
func (m *Manager) milvusVectorSearch(ctx context.Context, queryEmb []float64, refID string, memType string, limit int, threshold float64) ([]Memory, error) {
	results, err := m.milvus.Search(ctx, queryEmb, refID, memType, limit, threshold)
	if err != nil {
		return nil, err
	}

	if len(results) == 0 {
		return nil, nil
	}

	// 获取对应的记忆
	memoryIDs := make([]uint, 0, len(results))
	for _, r := range results {
		memoryIDs = append(memoryIDs, r.MemoryID)
	}

	var memories []Memory
	if err := m.db.Where("id IN ?", memoryIDs).Find(&memories).Error; err != nil {
		return nil, err
	}

	// 按照搜索结果的顺序排序
	memoryMap := make(map[uint]Memory)
	for _, mem := range memories {
		memoryMap[mem.ID] = mem
	}

	sortedMemories := make([]Memory, 0, len(results))
	for _, r := range results {
		if mem, ok := memoryMap[r.MemoryID]; ok {
			m.db.Model(&mem).Updates(map[string]any{
				"access_count": gorm.Expr("access_count + 1"),
			})
			sortedMemories = append(sortedMemories, mem)
		}
	}

	return sortedMemories, nil
}

func normalizeMemoryTypeFilter(memType MemoryType) string {
	return strings.TrimSpace(string(memType))
}

// ==================== 风格卡片 ====================

func (m *Manager) SaveStyleCardCandidate(ctx context.Context, card *StyleCard) (bool, error) {
	if card == nil {
		return false, nil
	}
	if m.embedding == nil || m.styleCardMilvus == nil {
		return false, fmt.Errorf("风格卡片向量依赖未初始化")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	card.Intent = strings.TrimSpace(card.Intent)
	card.Tone = strings.TrimSpace(card.Tone)
	card.TriggerRule = strings.TrimSpace(card.TriggerRule)
	card.AvoidRule = strings.TrimSpace(card.AvoidRule)
	card.Example = strings.TrimSpace(card.Example)
	card.SourceExcerpt = strings.TrimSpace(card.SourceExcerpt)

	if !IsValidStyleIntent(card.Intent) || !IsValidStyleTone(card.Tone) {
		return false, fmt.Errorf("非法的风格标签")
	}
	if card.TriggerRule == "" || card.AvoidRule == "" || card.Example == "" {
		return false, fmt.Errorf("风格卡片缺少必填字段")
	}

	queryText := styleCardVectorText(card)
	embedding, err := m.embedding.Embed(ctx, queryText)
	if err != nil {
		return false, err
	}

	var existing StyleCard
	searchResults, err := m.styleCardMilvus.Search(
		ctx,
		embedding,
		card.ConversationID,
		styleCardVectorKey(card.Intent, card.Tone),
		3,
		0.92,
	)
	if err != nil {
		return false, err
	}
	for _, result := range searchResults {
		if err := m.db.First(&existing, result.MemoryID).Error; err == nil {
			break
		}
	}
	if existing.ID != 0 {
		updates := map[string]any{
			"evidence_count": gorm.Expr("evidence_count + 1"),
			"updated_at":     time.Now(),
		}
		if nextStatus := styleCardStatusOnNewEvidence(existing.Status); nextStatus != existing.Status {
			updates["status"] = nextStatus
		}
		if shouldPreferLongerText(existing.TriggerRule, card.TriggerRule) {
			updates["trigger_rule"] = card.TriggerRule
		}
		if shouldPreferLongerText(existing.AvoidRule, card.AvoidRule) {
			updates["avoid_rule"] = card.AvoidRule
		}
		if shouldPreferShorterText(existing.Example, card.Example) {
			updates["example"] = card.Example
		}
		if mergedExcerpt := mergeStyleCardSourceExcerpt(existing.SourceExcerpt, card.SourceExcerpt); mergedExcerpt != strings.TrimSpace(existing.SourceExcerpt) {
			updates["source_excerpt"] = mergedExcerpt
		}
		if err := m.db.Model(&existing).Updates(updates).Error; err != nil {
			return false, err
		}
		if err := m.db.First(&existing, existing.ID).Error; err == nil {
			if err := m.refreshStyleCardVector(ctx, &existing); err != nil {
				return false, err
			}
		}
		return false, nil
	}

	if card.Status == "" {
		card.Status = StyleCardStatusCandidate
	}
	if card.EvidenceCount <= 0 {
		card.EvidenceCount = 1
	}
	if err := m.db.Create(card).Error; err != nil {
		return false, err
	}
	if err := m.insertStyleCardVector(ctx, card, embedding); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) SearchStyleCards(groupID int64, keyword string, limit int) ([]StyleCard, error) {
	var cards []StyleCard
	q := m.db.Model(&StyleCard{}).Where("status = ?", StyleCardStatusActive)
	if strings.TrimSpace(keyword) != "" {
		keywords := strings.Fields(keyword)
		condition, args := buildLikeQuery([]string{"trigger_rule", "avoid_rule", "example", "source_excerpt"}, keywords)
		q = q.Where(condition, args...)
	}
	if limit <= 0 {
		limit = 10
	}

	orderGroup := fmt.Sprintf("CASE WHEN group_id = %d THEN 0 ELSE 1 END", groupID)
	err := q.Order(orderGroup).
		Order("evidence_count DESC").
		Order("use_count DESC").
		Order("updated_at DESC").
		Limit(limit).
		Find(&cards).Error
	return cards, err
}

func (m *Manager) ListUncheckedStyleCards(groupID int64, limit int) ([]StyleCard, error) {
	var cards []StyleCard
	q := m.db.Where("status = ?", StyleCardStatusCandidate)
	if groupID != 0 {
		q = q.Where("group_id = ?", groupID)
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Order("updated_at DESC").Find(&cards).Error
	return cards, err
}

func (m *Manager) ReviewStyleCards(ids []uint, approve bool) error {
	if len(ids) == 0 {
		return nil
	}

	var cards []StyleCard
	if err := m.db.Where("id IN ?", ids).Find(&cards).Error; err != nil {
		return err
	}

	for _, card := range cards {
		nextStatus := StyleCardStatusRejected
		if approve {
			nextStatus = StyleCardStatusCandidate
			if card.EvidenceCount >= 2 {
				nextStatus = StyleCardStatusActive
			}
		}
		if err := m.db.Model(&StyleCard{}).Where("id = ?", card.ID).Updates(map[string]any{
			"status":     nextStatus,
			"updated_at": time.Now(),
		}).Error; err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) ListActiveStyleCardsByIntent(intent string, groupID int64, tone string, limit int) ([]StyleCard, error) {
	var cards []StyleCard
	if limit <= 0 {
		limit = 3
	}

	orderGroup := fmt.Sprintf("CASE WHEN group_id = %d THEN 0 ELSE 1 END", groupID)
	escapedTone := strings.ReplaceAll(tone, "'", "''")
	orderTone := fmt.Sprintf("CASE WHEN tone = '%s' THEN 0 ELSE 1 END", escapedTone)

	err := m.db.Model(&StyleCard{}).
		Where("status = ? AND intent = ?", StyleCardStatusActive, strings.TrimSpace(intent)).
		Order(orderGroup).
		Order(orderTone).
		Order("evidence_count DESC").
		Order("use_count DESC").
		Order("updated_at DESC").
		Limit(limit).
		Find(&cards).Error
	return cards, err
}

func (m *Manager) IncrementStyleCardUsage(ids []uint) error {
	if len(ids) == 0 {
		return nil
	}

	now := time.Now()
	return m.db.Model(&StyleCard{}).
		Where("id IN ?", ids).
		Updates(map[string]any{
			"use_count":    gorm.Expr("use_count + 1"),
			"last_used_at": &now,
		}).Error
}

func styleCardCollectionName(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "mumu_memories"
	}
	return base + "_style_cards"
}

func styleCardStatusOnNewEvidence(status StyleCardStatus) StyleCardStatus {
	if status == StyleCardStatusRejected {
		return StyleCardStatusCandidate
	}
	return status
}

func styleCardVectorKey(intent, tone string) string {
	return strings.TrimSpace(intent) + "|" + strings.TrimSpace(tone)
}

func styleCardVectorText(card *StyleCard) string {
	if card == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf(
		"intent:%s\ntone:%s\ntrigger:%s\navoid:%s\nexample:%s",
		card.Intent,
		card.Tone,
		card.TriggerRule,
		card.AvoidRule,
		card.Example,
	))
}

func (m *Manager) insertStyleCardVector(ctx context.Context, card *StyleCard, embedding []float64) error {
	if card == nil || m.styleCardMilvus == nil {
		return nil
	}
	if _, err := m.styleCardMilvus.Insert(ctx, card.ID, card.ConversationID, styleCardVectorKey(card.Intent, card.Tone), embedding); err != nil {
		return fmt.Errorf("插入风格卡片向量失败: %w", err)
	}
	return nil
}

func (m *Manager) refreshStyleCardVector(ctx context.Context, card *StyleCard) error {
	if card == nil || m.embedding == nil || m.styleCardMilvus == nil {
		return nil
	}
	if err := m.styleCardMilvus.Delete(ctx, []uint{card.ID}); err != nil {
		return fmt.Errorf("删除风格卡片旧向量失败: %w", err)
	}
	embedding, err := m.embedding.Embed(ctx, styleCardVectorText(card))
	if err != nil {
		return err
	}
	return m.insertStyleCardVector(ctx, card, embedding)
}

func shouldPreferShorterText(existing, candidate string) bool {
	existing = strings.TrimSpace(existing)
	candidate = strings.TrimSpace(candidate)
	switch {
	case candidate == "":
		return false
	case existing == "":
		return true
	default:
		return len([]rune(candidate)) < len([]rune(existing))
	}
}

func shouldPreferLongerText(existing, candidate string) bool {
	existing = strings.TrimSpace(existing)
	candidate = strings.TrimSpace(candidate)
	switch {
	case candidate == "":
		return false
	case existing == "":
		return true
	default:
		return len([]rune(candidate)) > len([]rune(existing))
	}
}

func mergeStyleCardSourceExcerpt(existing, candidate string) string {
	var parts []string
	if strings.TrimSpace(existing) != "" {
		items := strings.Split(existing, "|")
		parts = make([]string, 0, len(items))
		for _, item := range items {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			parts = append(parts, item)
		}
	}
	candidate = strings.TrimSpace(candidate)
	if candidate != "" {
		parts = append(parts, candidate)
	}
	if len(parts) > 3 {
		parts = parts[len(parts)-3:]
	}
	return strings.Join(parts, "|")
}

// ==================== 黑话管理 ====================

// SearchJargons 搜索黑话（通过关键词匹配，本群优先）
func (m *Manager) SearchJargonsByConversation(ref ConversationRef, keyword string, limit int) ([]Jargon, error) {
	var jargons []Jargon
	q := scopeSession(ref, m.db.Model(&Jargon{}).Where("rejected = ?", false), sessionFields)

	// 使用 strings.Fields 切割关键词，挨个模糊匹配
	if keyword != "" {
		keywords := strings.Fields(keyword)
		if len(keywords) > 0 {
			condition, args := buildLikeQuery([]string{"content"}, keywords)
			q = q.Where(condition, args...)
		}
	}

	switch {
	case ref.IsGroup():
		err := q.Order(fmt.Sprintf("CASE WHEN group_id = %d THEN 0 ELSE 1 END, checked DESC", ref.GroupID)).
			Limit(limit).Find(&jargons).Error
		return jargons, err
	case ref.IsPrivate():
		err := q.Order("checked DESC").Limit(limit).Find(&jargons).Error
		return jargons, err
	default:
		err := q.Order("checked DESC").Limit(limit).Find(&jargons).Error
		return jargons, err
	}
}

// SaveJargon 保存黑话/术语
func (m *Manager) SaveJargon(jargon *Jargon) error {
	var existing Jargon
	q := scopeSession(conversationRefFromJargon(jargon), m.db.Model(&Jargon{}), sessionFields).Where("content = ?", jargon.Content)
	err := q.First(&existing).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		return m.db.Create(jargon).Error
	} else if err != nil {
		return err
	}

	updates := map[string]any{
		"meaning":  jargon.Meaning,
		"context":  jargon.Context,
		"checked":  false, // 重置审核状态
		"rejected": false,
	}
	return m.db.Model(&existing).Updates(updates).Error
}

func conversationRefFromJargon(jargon *Jargon) ConversationRef {
	if jargon == nil {
		return AllConversationRef()
	}
	if jargon.GroupID > 0 {
		return GroupConversationRef(jargon.GroupID)
	}
	if jargon.UserID > 0 {
		return PrivateConversationRef(jargon.UserID)
	}
	return AllConversationRef()
}

// BatchReviewJargon 批量审核黑话
func (m *Manager) BatchReviewJargon(ids []uint, approve bool) error {
	if len(ids) == 0 {
		return nil
	}
	updates := map[string]any{
		"checked": true,
	}
	if approve {
		updates["rejected"] = false
	} else {
		updates["rejected"] = true
	}
	return m.db.Model(&Jargon{}).Where("id IN ?", ids).Updates(updates).Error
}

// GetUncheckedJargons 获取待审核的黑话
func (m *Manager) GetUncheckedJargonsByConversation(ref ConversationRef, limit int) ([]Jargon, error) {
	var jargons []Jargon
	err := scopeSession(ref, m.db.Model(&Jargon{}), sessionFields).
		Where("checked = ?", false).
		Limit(limit).
		Find(&jargons).Error
	return jargons, err
}

// GetAllApprovedJargons 获取所有已审核通过的黑话（用于构建 AC 自动机）
func (m *Manager) GetAllApprovedJargons() ([]Jargon, error) {
	var jargons []Jargon
	err := m.db.Where("checked = ? AND rejected = ?", true, false).Find(&jargons).Error
	return jargons, err
}

// ==================== 成员画像 ====================

// GetMemberProfile 获取成员画像
func (m *Manager) GetMemberProfile(userID int64) (*UserProfile, error) {
	var profile UserProfile
	err := m.db.Where("user_id = ?", userID).First(&profile).Error
	if err != nil {
		return nil, err
	}
	return &profile, nil
}

// GetOrCreateMemberProfile 获取或创建成员画像
func (m *Manager) GetOrCreateMemberProfile(userID int64, nickname string) (*UserProfile, error) {
	var profile UserProfile
	err := m.db.Where("user_id = ?", userID).First(&profile).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		profile = UserProfile{
			UserID:      userID,
			Nickname:    nickname,
			Activity:    0.5, // 初始活跃度
			Intimacy:    0.3, // 初始亲密度
			Trust:       0.4,
			Familiarity: 0.2,
			Respect:     0.5,
			LastSpeak:   time.Now(),
		}
		if err := m.db.Create(&profile).Error; err != nil {
			return nil, err
		}
		return &profile, nil
	}
	return &profile, err
}

// UpdateUserProfile 更新用户画像
func (m *Manager) UpdateUserProfile(profile *UserProfile) error {
	// 计算活跃度：基于最近发言时间和消息数量
	// 活跃度衰减：每天降低0.1，最低0.1
	daysSinceLastSpeak := time.Since(profile.LastSpeak).Hours() / 24
	if daysSinceLastSpeak > 0 {
		profile.Activity -= 0.1 * daysSinceLastSpeak
		if profile.Activity < 0.1 {
			profile.Activity = 0.1
		}
	}
	// 发言增加活跃度
	if time.Since(profile.LastSpeak) < time.Hour {
		profile.Activity += 0.05
		if profile.Activity > 1.0 {
			profile.Activity = 1.0
		}
	}
	return m.db.Save(profile).Error
}

// ==================== 统计 ====================

// GetStats 获取统计信息
func (m *Manager) GetStats() map[string]int64 {
	stats := make(map[string]int64)
	var memories, members, messages, styleCards, jargons int64
	m.db.Model(&Memory{}).Count(&memories)
	m.db.Model(&UserProfile{}).Count(&members)
	m.db.Model(&MessageLog{}).Count(&messages)
	m.db.Model(&StyleCard{}).Count(&styleCards)
	m.db.Model(&Jargon{}).Count(&jargons)
	stats["memories"] = memories
	stats["members"] = members
	stats["messages"] = messages
	stats["style_cards"] = styleCards
	stats["jargons"] = jargons
	return stats
}

// ==================== 列表查询（供管理界面用）====================

func (m *Manager) ListMemoriesByConversation(ref ConversationRef, memType string, page, pageSize int) ([]Memory, int64, error) {
	var items []Memory
	var total int64

	q := scopeSession(ref, m.db.Model(&Memory{}), sessionFields)
	if memType != "" {
		q = q.Where("type = ?", memType)
	}
	q.Count(&total)

	err := q.Order("updated_at DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&items).Error
	return items, total, err
}

func (m *Manager) ListMemberProfiles(page, pageSize int) ([]UserProfile, int64, error) {
	var items []UserProfile
	var total int64

	q := m.db.Model(&UserProfile{})
	q.Count(&total)

	err := q.Order("msg_count DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&items).Error
	return items, total, err
}

func (m *Manager) ListMessageLogsByConversation(ref ConversationRef, page, pageSize int) ([]MessageLog, int64, error) {
	var items []MessageLog
	var total int64

	q := scopeMessageLogs(ref, m.db.Model(&MessageLog{}))
	q.Count(&total)

	err := q.Order("created_at DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&items).Error
	return items, total, err
}

// GetMessageLogByID 根据消息ID获取消息日志
func (m *Manager) GetMessageLogByID(messageID string) (*MessageLog, error) {
	var log MessageLog
	err := m.db.Where("message_id = ?", messageID).First(&log).Error
	if err != nil {
		return nil, err
	}
	return &log, nil
}

// Close 关闭连接
func (m *Manager) Close() error {
	// 停止清理任务
	if m.cleanupStop != nil {
		close(m.cleanupStop)
		m.cleanupStop = nil
	}
	// 关闭 Milvus 连接
	if m.milvus != nil {
		_ = m.milvus.Close()
	}
	// 关闭 MySQL 连接
	if sqlDB, err := m.db.DB(); err == nil {
		return sqlDB.Close()
	}
	return nil
}

func (m *Manager) GetDB() *gorm.DB { return m.db }

// ==================== 表情包管理 ====================

// SaveSticker 保存表情包（通过哈希去重）
func (m *Manager) SaveSticker(sticker *Sticker) (bool, error) {
	// 先检查哈希是否已存在
	var existing Sticker
	err := m.db.Where("file_hash = ?", sticker.FileHash).First(&existing).Error
	if err == nil {
		// 已存在，返回重复标记
		return true, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}

	// 不存在，创建新记录
	if err := m.db.Create(sticker).Error; err != nil {
		return false, err
	}
	return false, nil
}

// GetStickerByID 根据ID获取表情包
func (m *Manager) GetStickerByID(id uint) (*Sticker, error) {
	var sticker Sticker
	err := m.db.First(&sticker, id).Error
	if err != nil {
		return nil, err
	}
	return &sticker, nil
}

// SearchStickers 搜索表情包
func (m *Manager) SearchStickers(keyword string, limit int) ([]Sticker, error) {
	var stickers []Sticker
	q := m.db.Model(&Sticker{})
	if keyword != "" {
		keywords := strings.Fields(keyword)
		likeConditions := make([]string, 0, len(keywords))
		args := make([]interface{}, 0, len(keywords))
		for _, kw := range keywords {
			likeConditions = append(likeConditions, "description LIKE ?")
			args = append(args, "%"+kw+"%")
		}
		q = q.Where(strings.Join(likeConditions, " OR "), args...)
	}
	err := q.Order("use_count DESC, updated_at DESC").Limit(limit).Find(&stickers).Error
	return stickers, err
}

// UpdateStickerUsage 更新表情包使用记录
func (m *Manager) UpdateStickerUsage(id uint) error {
	return m.db.Model(&Sticker{}).Where("id = ?", id).Updates(map[string]any{
		"use_count": gorm.Expr("use_count + 1"),
	}).Error
}

// GetStickerByHash 通过哈希获取表情包
func (m *Manager) GetStickerByHash(hash string) (*Sticker, error) {
	var sticker Sticker
	err := m.db.Where("file_hash = ?", hash).First(&sticker).Error
	if err != nil {
		return nil, err
	}
	return &sticker, nil
}

// ==================== 情绪状态管理 ====================

// startMoodDecay 启动情绪衰减定时任务（每分钟执行一次）
func (m *Manager) startMoodDecay() {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for {
			select {
			case <-ticker.C:
				if err := m.ApplyMoodDecay(); err != nil {
					zap.L().Error("情绪衰减失败", zap.Error(err))
				}
			case <-m.cleanupStop:
				ticker.Stop()
				return
			}
		}
	}()
	zap.L().Info("情绪衰减任务已启动")
}

// GetMoodState 获取某个用户的情绪状态
func (m *Manager) GetMoodState(userID int64) (*MoodState, error) {
	if userID == 0 {
		return nil, fmt.Errorf("userID 不能为空")
	}
	var mood MoodState
	err := m.db.Where("user_id = ?", userID).First(&mood).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		mood = MoodState{
			UserID:      userID,
			Valence:     0.0,
			Energy:      0.5,
			Sociability: 0.5,
			Irritation:  0.0,
			Curiosity:   0.5,
		}
		if err := m.db.Create(&mood).Error; err != nil {
			return nil, err
		}
		return &mood, nil
	}
	if err != nil {
		return nil, err
	}
	return &mood, nil
}

// UpdateMoodState 更新某个用户的情绪状态（增量更新）
func (m *Manager) UpdateMoodState(userID int64, valenceDelta, energyDelta, sociabilityDelta, irritationDelta, curiosityDelta float64, reason string) (*MoodState, error) {
	mood, err := m.GetMoodState(userID)
	if err != nil {
		return nil, err
	}

	mood.Valence = utils.ClampFloat64(mood.Valence+valenceDelta, -1.0, 1.0)
	mood.Energy = utils.ClampFloat64(mood.Energy+energyDelta, 0.0, 1.0)
	mood.Sociability = utils.ClampFloat64(mood.Sociability+sociabilityDelta, 0.0, 1.0)
	mood.Irritation = utils.ClampFloat64(mood.Irritation+irritationDelta, 0.0, 1.0)
	mood.Curiosity = utils.ClampFloat64(mood.Curiosity+curiosityDelta, 0.0, 1.0)
	mood.LastReason = reason

	if err := m.db.Save(mood).Error; err != nil {
		return nil, err
	}
	return mood, nil
}

// ApplyMoodDecay 应用情绪自然衰减
func (m *Manager) ApplyMoodDecay() error {
	var moods []MoodState
	if err := m.db.Find(&moods).Error; err != nil {
		return err
	}
	for i := range moods {
		moods[i].Valence *= 0.95
		moods[i].Energy += (0.5 - moods[i].Energy) * 0.05
		moods[i].Sociability += (0.5 - moods[i].Sociability) * 0.05
		moods[i].Irritation += (0.0 - moods[i].Irritation) * 0.08
		moods[i].Curiosity += (0.5 - moods[i].Curiosity) * 0.05
		if err := m.db.Save(&moods[i]).Error; err != nil {
			return err
		}
	}
	return nil
}

// ==================== 学习状态管理 ====================

// GetLearningState 获取群组的学习进度
func (m *Manager) GetLearningState(groupID int64) (*LearningState, error) {
	var state LearningState
	err := m.db.Where("group_id = ?", groupID).First(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &LearningState{GroupID: groupID}, nil
	}
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// UpdateLearningState 更新群组的学习进度
func (m *Manager) UpdateLearningState(groupID int64, lastMessageID uint) error {
	var state LearningState
	err := m.db.Where("group_id = ?", groupID).First(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		state = LearningState{
			GroupID:       groupID,
			LastMessageID: lastMessageID,
		}
		return m.db.Create(&state).Error
	}
	if err != nil {
		return err
	}

	state.LastMessageID = lastMessageID
	return m.db.Save(&state).Error
}
