package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	"github.com/robfig/cron/v3"
	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"
	_ "modernc.org/sqlite"
)

type Config struct {
	BotToken        string
	BotOwnerIDs     map[int64]struct{}
	Port            string
	BaseURL         string
	Timezone        string
	EnableGroupLock bool
	DataDir         string
	ReportDir       string
	DatabasePath    string
}

type GroupSettings struct {
	ChatID           int64
	Title            string
	IsActive         bool
	FounderUserID    sql.NullInt64
	FounderName      sql.NullString
	BookkeepingMode  string
	FeeRate          string
	ManualRate       sql.NullString
	UseRealtimeRate  bool
	LastReportToken  sql.NullString
	CreatedAt        string
	UpdatedAt        string
}

type RoleRecord struct {
	ChatID      int64
	UserID      int64
	Username    sql.NullString
	DisplayName string
	Role        string
	CreatedAt   string
	UpdatedAt   string
}

type MemberRecord struct {
	ChatID      int64
	UserID      int64
	Username    sql.NullString
	DisplayName string
	LastSeenAt  string
}

type LedgerEntry struct {
	ID             int64
	ChatID         int64
	ArchiveToken   sql.NullString
	BusinessDate   string
	Kind           string
	SignedAmount   string
	CustomRate     sql.NullString
	EffectiveRate  string
	FeeRate        string
	GrossCNY       string
	NetCNY         string
	OperatorUserID int64
	OperatorName   string
	TargetUserID   sql.NullInt64
	TargetName     sql.NullString
	RawText        string
	CreatedAt      string
}

type BillArchive struct {
	Token        string
	ChatID       int64
	GroupTitle   string
	BusinessDate string
	EntryCount   int
	DepositTotal string
	PayoutTotal  string
	BalanceTotal string
	HTMLPath     string
	XLSXPath     string
	CreatedAt    string
}

type Totals struct {
	Deposit string
	Payout  string
	Balance string
}

type RateCache struct {
	mu        sync.Mutex
	Value     string
	ExpiresAt time.Time
}

type App struct {
	cfg       Config
	db        *sql.DB
	bot       *tgbotapi.BotAPI
	loc       *time.Location
	rateCache *RateCache
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("未加载到 .env，继续读取系统环境变量: %v", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}

	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		log.Fatalf("加载时区失败: %v", err)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("创建 data 目录失败: %v", err)
	}
	if err := os.MkdirAll(cfg.ReportDir, 0o755); err != nil {
		log.Fatalf("创建 reports 目录失败: %v", err)
	}

	db, err := sql.Open("sqlite", cfg.DatabasePath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	if err := initSchema(db); err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}

	bot, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		log.Fatalf("创建 Telegram Bot 失败: %v", err)
	}
	bot.Debug = false

	app := &App{
		cfg: cfg,
		db:  db,
		bot: bot,
		loc: loc,
		rateCache: &RateCache{},
	}

	go app.startHTTPServer()
	go app.startCron()

	if err := app.run(); err != nil {
		log.Fatalf("机器人退出: %v", err)
	}
}

func loadConfig() (Config, error) {
	cfg := Config{
		BotToken:        strings.TrimSpace(os.Getenv("BOT_TOKEN")),
		BotOwnerIDs:     map[int64]struct{}{},
		Port:            envOrDefault("PORT", "3100"),
		BaseURL:         strings.TrimRight(strings.TrimSpace(os.Getenv("BASE_URL")), "/"),
		Timezone:        envOrDefault("TIMEZONE", "Asia/Shanghai"),
		EnableGroupLock: envOrDefault("ENABLE_GROUP_LOCK", "true") == "true",
		DataDir:         filepath.Join(".", "data"),
		ReportDir:       filepath.Join(".", "reports"),
	}
	cfg.DatabasePath = filepath.Join(cfg.DataDir, "ledger.db")

	if cfg.BotToken == "" {
		return cfg, errors.New("缺少 BOT_TOKEN")
	}

	for _, part := range strings.Split(strings.TrimSpace(os.Getenv("BOT_OWNER_IDS")), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return cfg, fmt.Errorf("BOT_OWNER_IDS 包含非法值: %s", part)
		}
		cfg.BotOwnerIDs[id] = struct{}{}
	}

	return cfg, nil
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func initSchema(db *sql.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS group_settings (
	chat_id INTEGER PRIMARY KEY,
	title TEXT NOT NULL,
	is_active INTEGER NOT NULL DEFAULT 0,
	founder_user_id INTEGER,
	founder_name TEXT,
	bookkeeping_mode TEXT NOT NULL DEFAULT 'original',
	fee_rate TEXT NOT NULL DEFAULT '0',
	manual_rate TEXT,
	use_realtime_rate INTEGER NOT NULL DEFAULT 1,
	last_report_token TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS group_roles (
	chat_id INTEGER NOT NULL,
	user_id INTEGER NOT NULL,
	username TEXT,
	display_name TEXT NOT NULL,
	role TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (chat_id, user_id)
);

CREATE TABLE IF NOT EXISTS group_members (
	chat_id INTEGER NOT NULL,
	user_id INTEGER NOT NULL,
	username TEXT,
	display_name TEXT NOT NULL,
	last_seen_at TEXT NOT NULL,
	PRIMARY KEY (chat_id, user_id)
);

CREATE TABLE IF NOT EXISTS ledger_entries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	chat_id INTEGER NOT NULL,
	archive_token TEXT,
	business_date TEXT NOT NULL,
	kind TEXT NOT NULL,
	signed_amount TEXT NOT NULL,
	custom_rate TEXT,
	effective_rate TEXT NOT NULL,
	fee_rate TEXT NOT NULL,
	gross_cny TEXT NOT NULL,
	net_cny TEXT NOT NULL,
	operator_user_id INTEGER NOT NULL,
	operator_name TEXT NOT NULL,
	target_user_id INTEGER,
	target_name TEXT,
	raw_text TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS bill_archives (
	token TEXT PRIMARY KEY,
	chat_id INTEGER NOT NULL,
	group_title TEXT NOT NULL,
	business_date TEXT NOT NULL,
	entry_count INTEGER NOT NULL,
	deposit_total TEXT NOT NULL,
	payout_total TEXT NOT NULL,
	balance_total TEXT NOT NULL,
	html_path TEXT NOT NULL,
	xlsx_path TEXT NOT NULL,
	created_at TEXT NOT NULL
);`

	_, err := db.Exec(schema)
	return err
}

func (a *App) run() error {
	log.Printf("机器人 %s 已启动", a.bot.Self.UserName)

	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 30
	updates := a.bot.GetUpdatesChan(updateConfig)

	for update := range updates {
		if update.Message == nil {
			continue
		}
		if err := a.handleMessage(update.Message); err != nil {
			log.Printf("处理消息失败: chat=%d user=%d err=%v", update.Message.Chat.ID, update.Message.From.ID, err)
		}
	}

	return nil
}

func (a *App) startHTTPServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"time": time.Now().UTC().Format(time.RFC3339),
		})
	})
	mux.Handle("/reports/", http.StripPrefix("/reports/", http.FileServer(http.Dir(a.cfg.ReportDir))))

	addr := "127.0.0.1:" + a.cfg.Port
	log.Printf("HTTP 报表服务已启动: http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("HTTP 服务失败: %v", err)
	}
}

func (a *App) startCron() {
	c := cron.New(cron.WithLocation(a.loc))
	_, err := c.AddFunc("0 3 * * *", func() {
		if err := a.autoArchiveAllGroups(context.Background()); err != nil {
			log.Printf("自动归档失败: %v", err)
		}
	})
	if err != nil {
		log.Fatalf("创建 cron 失败: %v", err)
	}
	c.Start()
}

func (a *App) handleMessage(msg *tgbotapi.Message) error {
	if msg.Text == "" || msg.From == nil {
		return nil
	}

	chatType := msg.Chat.Type
	text := strings.TrimSpace(msg.Text)

	if chatType == "private" {
		return a.handlePrivateMessage(msg, text)
	}

	if chatType != "group" && chatType != "supergroup" {
		return nil
	}

	a.recordMember(msg.Chat.ID, msg.From)
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		a.recordMember(msg.Chat.ID, msg.ReplyToMessage.From)
	}
	return a.handleGroupMessage(msg, text)
}

func (a *App) handlePrivateMessage(msg *tgbotapi.Message, text string) error {
	if strings.HasPrefix(text, "/start") {
		lines := []string{
			"Go 版免费记账机器人已启动。",
			"把机器人拉进群后关闭 Group Privacy，并给管理员权限。",
			"",
			"群内常用命令：",
			"开始 / 停止",
			"设置费率10",
			"设置汇率7 / 设置实时汇率",
			"设置原始模式 / 设置计数模式 / 设置回复模式",
			"+1000 / +1000/7.5 / 下发100",
			"显示账单 / 显示完整账单 / 保存账单",
			"z100",
			"(100+10.5)+5*10",
			"",
			"私聊管理员可用：/broadcast 文本内容",
		}
		if _, err := a.sendText(msg.Chat.ID, strings.Join(lines, "\n")); err != nil {
			return err
		}
		return nil
	}

	if strings.HasPrefix(text, "/broadcast") {
		return a.handleBroadcast(msg)
	}

	if matched, amount := parseRateQuery(text); matched {
		rate, err := a.fetchRealtimeRate()
		if err != nil {
			return a.replyText(msg, "查询汇率失败: "+err.Error())
		}
		cny := amount.Mul(rate)
		return a.replyText(msg, fmt.Sprintf("当前 USDT/CNY: %s\n%sU ≈ %s 元", rate.StringFixed(4), amount.StringFixed(2), cny.StringFixed(2)))
	}

	if looksLikeExpression(text) {
		result, err := evaluateExpression(text)
		if err != nil {
			return a.replyText(msg, "计算失败: "+err.Error())
		}
		return a.replyText(msg, "计算结果: "+result.StringFixed(2))
	}

	return nil
}

func (a *App) handleBroadcast(msg *tgbotapi.Message) error {
	if _, ok := a.cfg.BotOwnerIDs[msg.From.ID]; !ok {
		return a.replyText(msg, "你没有群发权限。")
	}

	content := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(msg.Text, "/broadcast"), "@"+a.bot.Self.UserName))
	content = strings.TrimSpace(content)

	if content == "" && msg.ReplyToMessage != nil {
		content = strings.TrimSpace(msg.ReplyToMessage.Text)
		if content == "" {
			content = strings.TrimSpace(msg.ReplyToMessage.Caption)
		}
	}

	if content == "" {
		return a.replyText(msg, "请使用 /broadcast 文本内容，或回复一条文字/带说明的媒体消息再发送 /broadcast。")
	}

	rows, err := a.db.Query(`SELECT chat_id FROM group_settings ORDER BY chat_id ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	total := 0
	success := 0
	for rows.Next() {
		var chatID int64
		if err := rows.Scan(&chatID); err != nil {
			return err
		}
		total++
		if _, err := a.sendText(chatID, content); err == nil {
			success++
		} else {
			log.Printf("群发失败 chat=%d err=%v", chatID, err)
		}
	}

	return a.replyText(msg, fmt.Sprintf("群发完成，成功 %d/%d 个群。", success, total))
}

func (a *App) handleGroupMessage(msg *tgbotapi.Message, text string) error {
	group, err := a.getOrCreateGroup(msg.Chat, nil)
	if err != nil {
		return err
	}
	role, _ := a.getRole(msg.Chat.ID, msg.From.ID)
	normalized := removeSpaces(text)

	if normalized == "开始" || normalized == "上课" {
		if !group.FounderUserID.Valid {
			group, err = a.getOrCreateGroup(msg.Chat, msg.From)
			if err != nil {
				return err
			}
			role = &RoleRecord{Role: "owner"}
		}
		if !hasMinimumRole(role, "operator") {
			return a.replyText(msg, "只有操作人或权限人可以开始记账。")
		}
		if err := a.updateGroupActive(msg.Chat.ID, true); err != nil {
			return err
		}
		if a.cfg.EnableGroupLock {
			_ = a.setGroupLock(msg.Chat.ID, true)
		}
		return a.replyText(msg, "记账已开始。发送 +1000 / 下发100 即可记账。")
	}

	if normalized == "停止" || normalized == "下课" {
		if !hasMinimumRole(role, "operator") {
			return a.replyText(msg, "只有操作人或权限人可以停止记账。")
		}
		if err := a.updateGroupActive(msg.Chat.ID, false); err != nil {
			return err
		}
		if a.cfg.EnableGroupLock {
			_ = a.setGroupLock(msg.Chat.ID, false)
		}
		return a.archiveAndReply(msg, "记账已停止，今日账单已归档。")
	}

	if !group.FounderUserID.Valid {
		return a.replyText(msg, "请先发送“开始”初始化本群记账机器人。")
	}

	if normalized == "显示操作人" {
		if !hasMinimumRole(role, "operator") {
			return a.replyText(msg, "只有操作人或权限人可查看角色列表。")
		}
		return a.replyRoles(msg)
	}

	if strings.HasPrefix(text, "设置操作人") || strings.HasPrefix(text, "新增权限人") || strings.HasPrefix(text, "删除操作人") || strings.HasPrefix(text, "删除权限人") {
		if !hasMinimumRole(role, "manager") {
			return a.replyText(msg, "只有权限人可以管理角色。")
		}
		return a.handleRoleCommand(msg, text)
	}

	if value, ok := parseNumericAfterPrefix(text, "设置费率"); ok {
		if !hasMinimumRole(role, "operator") {
			return a.replyText(msg, "只有操作人或权限人可以设置费率。")
		}
		return a.setFeeRate(msg, value)
	}

	if value, ok := parseNumericAfterPrefix(text, "设置汇率"); ok {
		if !hasMinimumRole(role, "operator") {
			return a.replyText(msg, "只有操作人或权限人可以设置汇率。")
		}
		return a.setManualRate(msg, value)
	}

	if normalized == "设置实时汇率" || normalized == "切换实时汇率" {
		if !hasMinimumRole(role, "operator") {
			return a.replyText(msg, "只有操作人或权限人可以切换汇率模式。")
		}
		if _, err := a.db.Exec(`UPDATE group_settings SET use_realtime_rate = 1, updated_at = ? WHERE chat_id = ?`, a.nowISO(), msg.Chat.ID); err != nil {
			return err
		}
		rate, err := a.fetchRealtimeRate()
		if err != nil {
			return a.replyText(msg, "已切换为实时汇率，但当前拉取汇率失败。")
		}
		return a.replyText(msg, fmt.Sprintf("已切换为实时汇率，当前约 %s。", rate.StringFixed(4)))
	}

	if mode, ok := parseMode(normalized); ok {
		if !hasMinimumRole(role, "operator") {
			return a.replyText(msg, "只有操作人或权限人可以切换记账模式。")
		}
		if _, err := a.db.Exec(`UPDATE group_settings SET bookkeeping_mode = ?, updated_at = ? WHERE chat_id = ?`, mode, a.nowISO(), msg.Chat.ID); err != nil {
			return err
		}
		return a.replyText(msg, "记账模式已切换为 "+mode+"。")
	}

	if matched, amount, customRate := parseDeposit(text); matched {
		if !hasMinimumRole(role, "operator") {
			return a.replyText(msg, "只有操作人或权限人可以记账。")
		}
		return a.handleLedgerRecord(msg, amount, customRate, "deposit")
	}

	if matched, amount := parsePayout(text); matched {
		if !hasMinimumRole(role, "operator") {
			return a.replyText(msg, "只有操作人或权限人可以记账。")
		}
		return a.handleLedgerRecord(msg, amount, "", "payout")
	}

	if normalized == "显示账单" || normalized == "+0" {
		if !hasMinimumRole(role, "operator") {
			return a.replyText(msg, "只有操作人或权限人可以查看账单。")
		}
		return a.replyRecentBills(msg)
	}

	if normalized == "显示完整账单" {
		if !hasMinimumRole(role, "operator") {
			return a.replyText(msg, "只有操作人或权限人可以查看完整账单。")
		}
		return a.replyFullBills(msg)
	}

	if normalized == "保存账单" {
		if !hasMinimumRole(role, "operator") {
			return a.replyText(msg, "只有操作人或权限人可以保存账单。")
		}
		return a.archiveAndReply(msg, "账单已保存。")
	}

	if normalized == "删除账单" {
		if !hasMinimumRole(role, "manager") {
			return a.replyText(msg, "只有权限人可以删除当天账单。")
		}
		result, err := a.db.Exec(`DELETE FROM ledger_entries WHERE chat_id = ? AND archive_token IS NULL`, msg.Chat.ID)
		if err != nil {
			return err
		}
		affected, _ := result.RowsAffected()
		return a.replyText(msg, fmt.Sprintf("已删除当天未保存账单，共 %d 条。", affected))
	}

	if normalized == "删除历史账单" {
		if !hasMinimumRole(role, "manager") {
			return a.replyText(msg, "只有权限人可以删除所有账单。")
		}
		return a.deleteAllBills(msg.Chat.ID, msg)
	}

	if matched, amount := parseRateQuery(text); matched {
		rate, err := a.fetchRealtimeRate()
		if err != nil {
			return a.replyText(msg, "查询汇率失败: "+err.Error())
		}
		cny := amount.Mul(rate)
		return a.replyText(msg, fmt.Sprintf("当前 USDT/CNY: %s\n%sU ≈ %s 元", rate.StringFixed(4), amount.StringFixed(2), cny.StringFixed(2)))
	}

	if looksLikeExpression(text) {
		result, err := evaluateExpression(text)
		if err != nil {
			return a.replyText(msg, "计算失败: "+err.Error())
		}
		return a.replyText(msg, "计算结果: "+result.StringFixed(2))
	}

	return nil
}

func (a *App) handleRoleCommand(msg *tgbotapi.Message, text string) error {
	target, err := a.extractTargetUser(msg, text)
	if err != nil {
		return a.replyText(msg, err.Error())
	}

	switch {
	case strings.HasPrefix(text, "设置操作人"):
		return a.upsertRoleAndReply(msg, target, "operator", "操作人")
	case strings.HasPrefix(text, "新增权限人"):
		return a.upsertRoleAndReply(msg, target, "manager", "权限人")
	case strings.HasPrefix(text, "删除操作人"), strings.HasPrefix(text, "删除权限人"):
		role, err := a.getRole(msg.Chat.ID, target.UserID)
		if err != nil {
			return a.replyText(msg, "该用户当前没有角色。")
		}
		if role.Role == "owner" {
			return a.replyText(msg, "首个权限人不可删除。")
		}
		if _, err := a.db.Exec(`DELETE FROM group_roles WHERE chat_id = ? AND user_id = ?`, msg.Chat.ID, target.UserID); err != nil {
			return err
		}
		return a.replyText(msg, fmt.Sprintf("已删除 %s 的角色权限。", mentionFromMember(target)))
	default:
		return nil
	}
}

func (a *App) upsertRoleAndReply(msg *tgbotapi.Message, target MemberRecord, role, roleName string) error {
	now := a.nowISO()
	_, err := a.db.Exec(`
INSERT INTO group_roles (chat_id, user_id, username, display_name, role, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(chat_id, user_id) DO UPDATE SET
	username = excluded.username,
	display_name = excluded.display_name,
	role = excluded.role,
	updated_at = excluded.updated_at
`, msg.Chat.ID, target.UserID, nullableStringValue(target.Username), target.DisplayName, role, now, now)
	if err != nil {
		return err
	}
	return a.replyText(msg, fmt.Sprintf("已设置 %s 为%s。", mentionFromMember(target), roleName))
}

func (a *App) setFeeRate(msg *tgbotapi.Message, value decimal.Decimal) error {
	_, err := a.db.Exec(`UPDATE group_settings SET fee_rate = ?, updated_at = ? WHERE chat_id = ?`, value.StringFixed(2), a.nowISO(), msg.Chat.ID)
	if err != nil {
		return err
	}
	return a.replyText(msg, "费率已设置为 "+value.StringFixed(2)+"%。")
}

func (a *App) setManualRate(msg *tgbotapi.Message, value decimal.Decimal) error {
	_, err := a.db.Exec(`UPDATE group_settings SET manual_rate = ?, use_realtime_rate = 0, updated_at = ? WHERE chat_id = ?`, value.StringFixed(4), a.nowISO(), msg.Chat.ID)
	if err != nil {
		return err
	}
	return a.replyText(msg, "固定汇率已设置为 "+value.StringFixed(4)+"。")
}

func (a *App) handleLedgerRecord(msg *tgbotapi.Message, amount decimal.Decimal, customRate string, kind string) error {
	group, err := a.getGroup(msg.Chat.ID)
	if err != nil {
		return err
	}
	if !group.IsActive {
		return a.replyText(msg, "当前未开始记账，请先发送“开始”。")
	}

	rate, err := a.resolveRate(group, customRate)
	if err != nil {
		return err
	}
	fee := mustDecimal(group.FeeRate)
	gross := amount.Abs().Mul(rate)
	net := gross.Mul(decimal.NewFromInt(1).Sub(fee.Div(decimal.NewFromInt(100))))

	var targetUserID any
	var targetName any
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		targetUserID = msg.ReplyToMessage.From.ID
		targetName = telegramUserDisplayName(msg.ReplyToMessage.From)
	}

	_, err = a.db.Exec(`
INSERT INTO ledger_entries (
	chat_id, archive_token, business_date, kind, signed_amount, custom_rate, effective_rate,
	fee_rate, gross_cny, net_cny, operator_user_id, operator_name, target_user_id, target_name, raw_text, created_at
) VALUES (?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.Chat.ID,
		a.currentBusinessDate(),
		kind,
		amount.StringFixed(2),
		nullIfEmpty(customRate),
		rate.StringFixed(4),
		fee.StringFixed(2),
		gross.StringFixed(2),
		net.StringFixed(2),
		msg.From.ID,
		telegramUserDisplayName(msg.From),
		targetUserID,
		targetName,
		msg.Text,
		a.currentDateTimeText(),
	)
	if err != nil {
		return err
	}

	entries, err := a.getCurrentEntries(msg.Chat.ID)
	if err != nil {
		return err
	}
	line := formatEntryLine(entries[len(entries)-1], len(entries), group.BookkeepingMode)
	totals := summarizeEntries(entries)
	body := fmt.Sprintf("%s\n\n今日合计\n入款: %sU\n下发: %sU\n结余: %sU", line, totals.Deposit, totals.Payout, totals.Balance)
	return a.replyText(msg, body)
}

func (a *App) replyRoles(msg *tgbotapi.Message) error {
	rows, err := a.db.Query(`SELECT chat_id, user_id, username, display_name, role, created_at, updated_at
FROM group_roles WHERE chat_id = ?
ORDER BY CASE role WHEN 'owner' THEN 1 WHEN 'manager' THEN 2 ELSE 3 END, updated_at ASC`, msg.Chat.ID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var lines []string
	index := 1
	for rows.Next() {
		var role RoleRecord
		if err := rows.Scan(&role.ChatID, &role.UserID, &role.Username, &role.DisplayName, &role.Role, &role.CreatedAt, &role.UpdatedAt); err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%d. %s - %s", index, mentionFromRole(role), role.Role))
		index++
	}
	if len(lines) == 0 {
		lines = []string{"当前没有设置角色。"}
	}
	return a.replyText(msg, strings.Join(lines, "\n"))
}

func (a *App) replyRecentBills(msg *tgbotapi.Message) error {
	group, err := a.getGroup(msg.Chat.ID)
	if err != nil {
		return err
	}
	entries, err := a.getCurrentEntries(msg.Chat.ID)
	if err != nil {
		return err
	}
	totals := summarizeEntries(entries)
	if len(entries) == 0 {
		return a.replyText(msg, fmt.Sprintf("当前没有未保存账单。\n\n当前统计\n入款: %sU\n下发: %sU\n结余: %sU", totals.Deposit, totals.Payout, totals.Balance))
	}

	start := 0
	if len(entries) > 6 {
		start = len(entries) - 6
	}
	lines := make([]string, 0, len(entries[start:])+4)
	for i, entry := range entries[start:] {
		lines = append(lines, formatEntryLine(entry, i+1, group.BookkeepingMode))
	}
	lines = append(lines, "", "当前统计", "入款: "+totals.Deposit+"U", "下发: "+totals.Payout+"U", "结余: "+totals.Balance+"U")
	return a.replyText(msg, strings.Join(lines, "\n"))
}

func (a *App) replyFullBills(msg *tgbotapi.Message) error {
	entries, err := a.getCurrentEntries(msg.Chat.ID)
	if err != nil {
		return err
	}
	totals := summarizeEntries(entries)

	archives, err := a.getArchives(msg.Chat.ID)
	if err != nil {
		return err
	}

	lines := []string{
		fmt.Sprintf("当前未保存账单: %d 条", len(entries)),
		"当前入款: " + totals.Deposit + "U",
		"当前下发: " + totals.Payout + "U",
		"当前结余: " + totals.Balance + "U",
		"",
		"最近历史账单:",
	}
	if len(archives) == 0 {
		lines = append(lines, "暂无历史账单")
	} else {
		limit := len(archives)
		if limit > 10 {
			limit = 10
		}
		for _, archive := range archives[:limit] {
			lines = append(lines, fmt.Sprintf("%s | %d条 | 结余 %sU", archive.BusinessDate, archive.EntryCount, archive.BalanceTotal))
			if url := a.reportURL(archive.Token); url != "" {
				lines = append(lines, "网页: "+url)
			}
			if url := a.xlsxURL(archive.Token); url != "" {
				lines = append(lines, "Excel: "+url)
			}
		}
	}

	if err := a.replyText(msg, strings.Join(lines, "\n")); err != nil {
		return err
	}

	if len(archives) > 0 && fileExists(archives[0].XLSXPath) {
		doc := tgbotapi.NewDocument(msg.Chat.ID, tgbotapi.FilePath(archives[0].XLSXPath))
		_, err = a.bot.Send(doc)
		return err
	}
	return nil
}

func (a *App) archiveAndReply(msg *tgbotapi.Message, prefix string) error {
	archive, err := a.createArchive(msg.Chat.ID)
	if err != nil {
		return err
	}
	if archive == nil {
		return a.replyText(msg, prefix+"\n当前没有可保存账单。")
	}

	lines := []string{
		prefix,
		"业务日期: " + archive.BusinessDate,
		"入款: " + archive.DepositTotal + "U",
		"下发: " + archive.PayoutTotal + "U",
		"结余: " + archive.BalanceTotal + "U",
		fmt.Sprintf("条数: %d", archive.EntryCount),
	}
	if url := a.reportURL(archive.Token); url != "" {
		lines = append(lines, "网页账单: "+url)
	}
	if url := a.xlsxURL(archive.Token); url != "" {
		lines = append(lines, "Excel 导出: "+url)
	}
	lines = append(lines, "本地 HTML: "+archive.HTMLPath, "本地 Excel: "+archive.XLSXPath)

	if err := a.replyText(msg, strings.Join(lines, "\n")); err != nil {
		return err
	}

	if fileExists(archive.XLSXPath) {
		doc := tgbotapi.NewDocument(msg.Chat.ID, tgbotapi.FilePath(archive.XLSXPath))
		_, err = a.bot.Send(doc)
		return err
	}
	return nil
}

func (a *App) autoArchiveAllGroups(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `SELECT chat_id FROM group_settings ORDER BY chat_id ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var chatID int64
		if err := rows.Scan(&chatID); err != nil {
			return err
		}
		archive, err := a.createArchive(chatID)
		if err != nil {
			log.Printf("自动归档失败 chat=%d err=%v", chatID, err)
			continue
		}
		if archive == nil {
			continue
		}
		lines := []string{
			"03:00 自动归档已完成。",
			"业务日期: " + archive.BusinessDate,
			"入款: " + archive.DepositTotal + "U",
			"下发: " + archive.PayoutTotal + "U",
			"结余: " + archive.BalanceTotal + "U",
		}
		if url := a.reportURL(archive.Token); url != "" {
			lines = append(lines, "网页账单: "+url)
		}
		if url := a.xlsxURL(archive.Token); url != "" {
			lines = append(lines, "Excel 导出: "+url)
		}
		if _, err := a.sendText(chatID, strings.Join(lines, "\n")); err != nil {
			log.Printf("归档通知发送失败 chat=%d err=%v", chatID, err)
		}
	}
	return nil
}

func (a *App) createArchive(chatID int64) (*BillArchive, error) {
	group, err := a.getGroup(chatID)
	if err != nil {
		return nil, err
	}
	entries, err := a.getCurrentEntries(chatID)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}

	totals := summarizeEntries(entries)
	token := fmt.Sprintf("%d%d", time.Now().Unix(), time.Now().Nanosecond())
	archive := &BillArchive{
		Token:        token,
		ChatID:       chatID,
		GroupTitle:   group.Title,
		BusinessDate: a.currentBusinessDate(),
		EntryCount:   len(entries),
		DepositTotal: totals.Deposit,
		PayoutTotal:  totals.Payout,
		BalanceTotal: totals.Balance,
		HTMLPath:     filepath.Join(a.cfg.ReportDir, token+".html"),
		XLSXPath:     filepath.Join(a.cfg.ReportDir, token+".xlsx"),
		CreatedAt:    a.nowISO(),
	}

	if err := a.saveArchiveFiles(archive, entries); err != nil {
		return nil, err
	}

	tx, err := a.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`INSERT INTO bill_archives (
token, chat_id, group_title, business_date, entry_count, deposit_total, payout_total, balance_total, html_path, xlsx_path, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		archive.Token, archive.ChatID, archive.GroupTitle, archive.BusinessDate, archive.EntryCount, archive.DepositTotal,
		archive.PayoutTotal, archive.BalanceTotal, archive.HTMLPath, archive.XLSXPath, archive.CreatedAt,
	); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(`UPDATE ledger_entries SET archive_token = ? WHERE chat_id = ? AND archive_token IS NULL`, archive.Token, chatID); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(`UPDATE group_settings SET last_report_token = ?, updated_at = ? WHERE chat_id = ?`, archive.Token, a.nowISO(), chatID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return archive, nil
}

func (a *App) saveArchiveFiles(archive *BillArchive, entries []LedgerEntry) error {
	if err := os.WriteFile(archive.HTMLPath, []byte(a.renderHTML(archive, entries)), 0o644); err != nil {
		return err
	}

	file := excelize.NewFile()
	sheet := file.GetSheetName(0)
	headers := []string{"序号", "时间", "类型", "操作人", "标记对象", "金额U", "汇率", "费率", "参考金额", "到账金额", "原命令"}
	for idx, header := range headers {
		cell, _ := excelize.CoordinatesToCellName(idx+1, 1)
		file.SetCellValue(sheet, cell, header)
	}
	for i, entry := range entries {
		row := i + 2
		values := []any{
			i + 1,
			entry.CreatedAt,
			map[bool]string{true: "入款", false: "下发"}[entry.Kind == "deposit"],
			entry.OperatorName,
			nullString(entry.TargetName, ""),
			entry.SignedAmount,
			entry.EffectiveRate,
			entry.FeeRate + "%",
			entry.GrossCNY,
			entry.NetCNY,
			entry.RawText,
		}
		for col, value := range values {
			cell, _ := excelize.CoordinatesToCellName(col+1, row)
			file.SetCellValue(sheet, cell, value)
		}
	}
	return file.SaveAs(archive.XLSXPath)
}

func (a *App) renderHTML(archive *BillArchive, entries []LedgerEntry) string {
	const tpl = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>{{.Archive.GroupTitle}} - 账单 {{.Archive.BusinessDate}}</title>
  <style>
    body { font-family: Arial, sans-serif; margin: 24px; background: #f7f8fa; color: #111827; }
    .card { background: #fff; border-radius: 12px; padding: 20px; box-shadow: 0 6px 24px rgba(15, 23, 42, 0.08); margin-bottom: 16px; }
    .summary { display: flex; flex-wrap: wrap; gap: 12px; }
    .pill { background: #eef2ff; color: #3730a3; padding: 10px 12px; border-radius: 10px; font-weight: 600; }
    table { width: 100%; border-collapse: collapse; background: #fff; }
    th, td { border-bottom: 1px solid #e5e7eb; padding: 10px; font-size: 14px; text-align: left; }
    th { background: #f3f4f6; }
  </style>
</head>
<body>
  <div class="card">
    <h1>{{.Archive.GroupTitle}} 账单</h1>
    <p>业务日期: {{.Archive.BusinessDate}} | 生成时间: {{.GeneratedAt}}</p>
  </div>
  <div class="card summary">
    <div class="pill">入款合计: {{.Archive.DepositTotal}}U</div>
    <div class="pill">下发合计: {{.Archive.PayoutTotal}}U</div>
    <div class="pill">结余: {{.Archive.BalanceTotal}}U</div>
    <div class="pill">条数: {{.Archive.EntryCount}}</div>
  </div>
  <table>
    <thead>
      <tr>
        <th>#</th>
        <th>时间</th>
        <th>类型</th>
        <th>操作人</th>
        <th>标记对象</th>
        <th>金额(U)</th>
        <th>汇率</th>
        <th>费率</th>
        <th>参考金额</th>
        <th>到账金额</th>
        <th>原命令</th>
      </tr>
    </thead>
    <tbody>
    {{range $idx, $entry := .Entries}}
      <tr>
        <td>{{inc $idx}}</td>
        <td>{{$entry.CreatedAt}}</td>
        <td>{{if eq $entry.Kind "deposit"}}入款{{else}}下发{{end}}</td>
        <td>{{$entry.OperatorName}}</td>
        <td>{{nullStr $entry.TargetName}}</td>
        <td>{{$entry.SignedAmount}}</td>
        <td>{{$entry.EffectiveRate}}</td>
        <td>{{$entry.FeeRate}}%</td>
        <td>{{$entry.GrossCNY}}</td>
        <td>{{$entry.NetCNY}}</td>
        <td>{{$entry.RawText}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
</body>
</html>`

	funcs := template.FuncMap{
		"inc": func(i int) int { return i + 1 },
		"nullStr": func(value sql.NullString) string {
			if value.Valid && value.String != "" {
				return value.String
			}
			return "-"
		},
	}

	var builder strings.Builder
	t := template.Must(template.New("report").Funcs(funcs).Parse(tpl))
	_ = t.Execute(&builder, map[string]any{
		"Archive":     archive,
		"Entries":     entries,
		"GeneratedAt": a.currentDateTimeText(),
	})
	return builder.String()
}

func (a *App) deleteAllBills(chatID int64, msg *tgbotapi.Message) error {
	archives, err := a.getArchives(chatID)
	if err != nil {
		return err
	}
	for _, archive := range archives {
		if fileExists(archive.HTMLPath) {
			_ = os.Remove(archive.HTMLPath)
		}
		if fileExists(archive.XLSXPath) {
			_ = os.Remove(archive.XLSXPath)
		}
	}

	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM ledger_entries WHERE chat_id = ?`, chatID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM bill_archives WHERE chat_id = ?`, chatID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE group_settings SET last_report_token = NULL, updated_at = ? WHERE chat_id = ?`, a.nowISO(), chatID); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	return a.replyText(msg, "已删除该群全部账单和历史归档。")
}

func (a *App) getOrCreateGroup(chat *tgbotapi.Chat, bootstrapUser *tgbotapi.User) (*GroupSettings, error) {
	group, err := a.getGroup(chat.ID)
	if err == nil {
		_, _ = a.db.Exec(`UPDATE group_settings SET title = ?, updated_at = ? WHERE chat_id = ?`, chat.Title, a.nowISO(), chat.ID)
		return a.getGroup(chat.ID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	now := a.nowISO()
	var founderID any
	var founderName any
	if bootstrapUser != nil {
		founderID = bootstrapUser.ID
		founderName = telegramUserDisplayName(bootstrapUser)
	}

	_, err = a.db.Exec(`
INSERT INTO group_settings (
	chat_id, title, is_active, founder_user_id, founder_name, bookkeeping_mode, fee_rate, manual_rate, use_realtime_rate, last_report_token, created_at, updated_at
) VALUES (?, ?, 0, ?, ?, 'original', '0', NULL, 1, NULL, ?, ?)`,
		chat.ID, chat.Title, founderID, founderName, now, now,
	)
	if err != nil {
		return nil, err
	}

	if bootstrapUser != nil {
		_, err = a.db.Exec(`
INSERT INTO group_roles (chat_id, user_id, username, display_name, role, created_at, updated_at)
VALUES (?, ?, ?, ?, 'owner', ?, ?)
ON CONFLICT(chat_id, user_id) DO UPDATE SET
	username = excluded.username,
	display_name = excluded.display_name,
	role = excluded.role,
	updated_at = excluded.updated_at
`,
			chat.ID, bootstrapUser.ID, nullIfEmpty(bootstrapUser.UserName), telegramUserDisplayName(bootstrapUser), now, now,
		)
		if err != nil {
			return nil, err
		}
	}

	return a.getGroup(chat.ID)
}

func (a *App) getGroup(chatID int64) (*GroupSettings, error) {
	row := a.db.QueryRow(`SELECT chat_id, title, is_active, founder_user_id, founder_name, bookkeeping_mode, fee_rate, manual_rate, use_realtime_rate, last_report_token, created_at, updated_at
FROM group_settings WHERE chat_id = ?`, chatID)
	var group GroupSettings
	var isActive int
	var useRealtime int
	if err := row.Scan(
		&group.ChatID,
		&group.Title,
		&isActive,
		&group.FounderUserID,
		&group.FounderName,
		&group.BookkeepingMode,
		&group.FeeRate,
		&group.ManualRate,
		&useRealtime,
		&group.LastReportToken,
		&group.CreatedAt,
		&group.UpdatedAt,
	); err != nil {
		return nil, err
	}
	group.IsActive = isActive == 1
	group.UseRealtimeRate = useRealtime == 1
	return &group, nil
}

func (a *App) updateGroupActive(chatID int64, active bool) error {
	value := 0
	if active {
		value = 1
	}
	_, err := a.db.Exec(`UPDATE group_settings SET is_active = ?, updated_at = ? WHERE chat_id = ?`, value, a.nowISO(), chatID)
	return err
}

func (a *App) getRole(chatID, userID int64) (*RoleRecord, error) {
	row := a.db.QueryRow(`SELECT chat_id, user_id, username, display_name, role, created_at, updated_at FROM group_roles WHERE chat_id = ? AND user_id = ?`, chatID, userID)
	var role RoleRecord
	if err := row.Scan(&role.ChatID, &role.UserID, &role.Username, &role.DisplayName, &role.Role, &role.CreatedAt, &role.UpdatedAt); err != nil {
		return nil, err
	}
	return &role, nil
}

func (a *App) recordMember(chatID int64, user *tgbotapi.User) {
	if user == nil {
		return
	}
	_, err := a.db.Exec(`
INSERT INTO group_members (chat_id, user_id, username, display_name, last_seen_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(chat_id, user_id) DO UPDATE SET
	username = excluded.username,
	display_name = excluded.display_name,
	last_seen_at = excluded.last_seen_at
`, chatID, user.ID, nullIfEmpty(user.UserName), telegramUserDisplayName(user), a.nowISO())
	if err != nil {
		log.Printf("记录成员失败: %v", err)
	}
}

func (a *App) extractTargetUser(msg *tgbotapi.Message, text string) (MemberRecord, error) {
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		user := msg.ReplyToMessage.From
		return MemberRecord{
			ChatID:      msg.Chat.ID,
			UserID:      user.ID,
			Username:    toNullString(user.UserName),
			DisplayName: telegramUserDisplayName(user),
			LastSeenAt:  a.nowISO(),
		}, nil
	}

	re := regexp.MustCompile(`@([A-Za-z0-9_]{5,32})`)
	matches := re.FindStringSubmatch(text)
	if len(matches) < 2 {
		return MemberRecord{}, errors.New("请用回复消息，或使用群内已出现过的 @username 来指定目标。")
	}

	row := a.db.QueryRow(`SELECT chat_id, user_id, username, display_name, last_seen_at FROM group_members WHERE chat_id = ? AND lower(username) = lower(?)`, msg.Chat.ID, matches[1])
	var member MemberRecord
	if err := row.Scan(&member.ChatID, &member.UserID, &member.Username, &member.DisplayName, &member.LastSeenAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MemberRecord{}, errors.New("机器人未记录到该 @username，请先让对方在群里发言一次，再用回复消息设置。")
		}
		return MemberRecord{}, err
	}
	return member, nil
}

func (a *App) getCurrentEntries(chatID int64) ([]LedgerEntry, error) {
	rows, err := a.db.Query(`SELECT id, chat_id, archive_token, business_date, kind, signed_amount, custom_rate, effective_rate, fee_rate, gross_cny, net_cny, operator_user_id, operator_name, target_user_id, target_name, raw_text, created_at
FROM ledger_entries WHERE chat_id = ? AND archive_token IS NULL ORDER BY id ASC`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

func (a *App) getArchives(chatID int64) ([]BillArchive, error) {
	rows, err := a.db.Query(`SELECT token, chat_id, group_title, business_date, entry_count, deposit_total, payout_total, balance_total, html_path, xlsx_path, created_at
FROM bill_archives WHERE chat_id = ? ORDER BY created_at DESC`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var archives []BillArchive
	for rows.Next() {
		var archive BillArchive
		if err := rows.Scan(
			&archive.Token, &archive.ChatID, &archive.GroupTitle, &archive.BusinessDate, &archive.EntryCount,
			&archive.DepositTotal, &archive.PayoutTotal, &archive.BalanceTotal, &archive.HTMLPath, &archive.XLSXPath, &archive.CreatedAt,
		); err != nil {
			return nil, err
		}
		archives = append(archives, archive)
	}
	return archives, nil
}

func scanEntries(rows *sql.Rows) ([]LedgerEntry, error) {
	var entries []LedgerEntry
	for rows.Next() {
		var entry LedgerEntry
		if err := rows.Scan(
			&entry.ID, &entry.ChatID, &entry.ArchiveToken, &entry.BusinessDate, &entry.Kind,
			&entry.SignedAmount, &entry.CustomRate, &entry.EffectiveRate, &entry.FeeRate,
			&entry.GrossCNY, &entry.NetCNY, &entry.OperatorUserID, &entry.OperatorName,
			&entry.TargetUserID, &entry.TargetName, &entry.RawText, &entry.CreatedAt,
		); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func summarizeEntries(entries []LedgerEntry) Totals {
	deposit := decimal.Zero
	payoutSigned := decimal.Zero
	balance := decimal.Zero
	for _, entry := range entries {
		amount := mustDecimal(entry.SignedAmount)
		balance = balance.Add(amount)
		if entry.Kind == "deposit" {
			deposit = deposit.Add(amount)
		}
		if entry.Kind == "payout" {
			payoutSigned = payoutSigned.Add(amount)
		}
	}
	return Totals{
		Deposit: deposit.StringFixed(2),
		Payout:  payoutSigned.Neg().StringFixed(2),
		Balance: balance.StringFixed(2),
	}
}

func formatEntryLine(entry LedgerEntry, index int, mode string) string {
	typeLabel := "下发"
	if entry.Kind == "deposit" {
		typeLabel = "入款"
	}
	target := ""
	if entry.TargetName.Valid && entry.TargetName.String != "" {
		target = " -> " + entry.TargetName.String
	}
	base := fmt.Sprintf("%d. %s%s %sU", index, typeLabel, target, mustDecimal(entry.SignedAmount).StringFixed(2))
	switch mode {
	case "count":
		return fmt.Sprintf("%s | 操作人: %s", base, entry.OperatorName)
	case "reply":
		return fmt.Sprintf("%s | 汇率: %s | 回复标记: %s", base, mustDecimal(entry.EffectiveRate).StringFixed(4), nullString(entry.TargetName, "无"))
	default:
		return fmt.Sprintf("%s | 汇率: %s | 费率: %s%% | 到账: %s元", base, mustDecimal(entry.EffectiveRate).StringFixed(4), mustDecimal(entry.FeeRate).StringFixed(2), mustDecimal(entry.NetCNY).StringFixed(2))
	}
}

func parseRateQuery(text string) (bool, decimal.Decimal) {
	re := regexp.MustCompile(`^[zZ](\d+(?:\.\d+)?)$`)
	matches := re.FindStringSubmatch(strings.TrimSpace(text))
	if len(matches) != 2 {
		return false, decimal.Zero
	}
	value, err := decimal.NewFromString(matches[1])
	if err != nil {
		return false, decimal.Zero
	}
	return true, value
}

func parseNumericAfterPrefix(text, prefix string) (decimal.Decimal, bool) {
	normalized := removeSpaces(text)
	prefix = removeSpaces(prefix)
	if !strings.HasPrefix(normalized, prefix) {
		return decimal.Zero, false
	}
	value := strings.TrimPrefix(normalized, prefix)
	if value == "" {
		return decimal.Zero, false
	}
	number, err := decimal.NewFromString(value)
	if err != nil {
		return decimal.Zero, false
	}
	return number, true
}

func parseDeposit(text string) (bool, decimal.Decimal, string) {
	re := regexp.MustCompile(`^\+(-?\d+(?:\.\d+)?)(?:/(\d+(?:\.\d+)?))?$`)
	matches := re.FindStringSubmatch(removeSpaces(text))
	if len(matches) == 0 {
		return false, decimal.Zero, ""
	}
	amount, err := decimal.NewFromString(matches[1])
	if err != nil {
		return false, decimal.Zero, ""
	}
	customRate := ""
	if len(matches) > 2 {
		customRate = matches[2]
	}
	return true, amount, customRate
}

func parsePayout(text string) (bool, decimal.Decimal) {
	re := regexp.MustCompile(`^下发(-?\d+(?:\.\d+)?)$`)
	matches := re.FindStringSubmatch(removeSpaces(text))
	if len(matches) == 0 {
		return false, decimal.Zero
	}
	number, err := decimal.NewFromString(matches[1])
	if err != nil {
		return false, decimal.Zero
	}
	if number.IsNegative() {
		return true, number.Abs()
	}
	return true, number.Neg()
}

func parseMode(normalized string) (string, bool) {
	modes := map[string]string{
		"设置原始模式": "original",
		"切换原始模式": "original",
		"设置计数模式": "count",
		"切换计数模式": "count",
		"设置回复模式": "reply",
		"切换回复模式": "reply",
	}
	mode, ok := modes[normalized]
	return mode, ok
}

func looksLikeExpression(text string) bool {
	trimmed := strings.TrimSpace(text)
	if !strings.ContainsAny(trimmed, "+-*/") {
		return false
	}
	for _, r := range trimmed {
		if !(r >= '0' && r <= '9') && !strings.ContainsRune("+-*/(). ", r) {
			return false
		}
	}
	return true
}

func evaluateExpression(expression string) (decimal.Decimal, error) {
	clean := removeSpaces(expression)
	if clean == "" {
		return decimal.Zero, errors.New("表达式为空")
	}

	var tokens []string
	current := strings.Builder{}
	for _, r := range clean {
		if (r >= '0' && r <= '9') || r == '.' {
			current.WriteRune(r)
			continue
		}
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
		tokens = append(tokens, string(r))
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}

	output := make([]string, 0, len(tokens))
	ops := []string{}
	precedence := map[string]int{"+": 1, "-": 1, "*": 2, "/": 2}
	prevKind := ""

	for _, token := range tokens {
		if isNumberToken(token) {
			output = append(output, token)
			prevKind = "number"
			continue
		}
		if token == "(" {
			ops = append(ops, token)
			prevKind = "("
			continue
		}
		if token == ")" {
			for len(ops) > 0 && ops[len(ops)-1] != "(" {
				output = append(output, ops[len(ops)-1])
				ops = ops[:len(ops)-1]
			}
			if len(ops) == 0 {
				return decimal.Zero, errors.New("括号不匹配")
			}
			ops = ops[:len(ops)-1]
			prevKind = ")"
			continue
		}
		if (token == "+" || token == "-") && (prevKind == "" || prevKind == "(" || prevKind == "operator") {
			output = append(output, "0")
		}
		for len(ops) > 0 && ops[len(ops)-1] != "(" && precedence[ops[len(ops)-1]] >= precedence[token] {
			output = append(output, ops[len(ops)-1])
			ops = ops[:len(ops)-1]
		}
		ops = append(ops, token)
		prevKind = "operator"
	}

	for len(ops) > 0 {
		if ops[len(ops)-1] == "(" {
			return decimal.Zero, errors.New("括号不匹配")
		}
		output = append(output, ops[len(ops)-1])
		ops = ops[:len(ops)-1]
	}

	stack := []decimal.Decimal{}
	for _, token := range output {
		if isNumberToken(token) {
			number, err := decimal.NewFromString(token)
			if err != nil {
				return decimal.Zero, err
			}
			stack = append(stack, number)
			continue
		}
		if len(stack) < 2 {
			return decimal.Zero, errors.New("表达式非法")
		}
		b := stack[len(stack)-1]
		a := stack[len(stack)-2]
		stack = stack[:len(stack)-2]
		switch token {
		case "+":
			stack = append(stack, a.Add(b))
		case "-":
			stack = append(stack, a.Sub(b))
		case "*":
			stack = append(stack, a.Mul(b))
		case "/":
			if b.Equal(decimal.Zero) {
				return decimal.Zero, errors.New("不能除以 0")
			}
			stack = append(stack, a.Div(b))
		default:
			return decimal.Zero, errors.New("不支持的运算符")
		}
	}
	if len(stack) != 1 {
		return decimal.Zero, errors.New("表达式非法")
	}
	return stack[0], nil
}

func isNumberToken(token string) bool {
	if token == "" {
		return false
	}
	_, err := decimal.NewFromString(token)
	return err == nil
}

func (a *App) resolveRate(group *GroupSettings, customRate string) (decimal.Decimal, error) {
	if customRate != "" {
		return decimal.NewFromString(customRate)
	}
	if group.UseRealtimeRate {
		return a.fetchRealtimeRate()
	}
	if group.ManualRate.Valid && group.ManualRate.String != "" {
		return decimal.NewFromString(group.ManualRate.String)
	}
	return a.fetchRealtimeRate()
}

func (a *App) fetchRealtimeRate() (decimal.Decimal, error) {
	a.rateCache.mu.Lock()
	if a.rateCache.Value != "" && time.Now().Before(a.rateCache.ExpiresAt) {
		value := a.rateCache.Value
		a.rateCache.mu.Unlock()
		return decimal.NewFromString(value)
	}
	a.rateCache.mu.Unlock()

	req, err := http.NewRequest(http.MethodGet, "https://api.coingecko.com/api/v3/simple/price?ids=tether&vs_currencies=cny", nil)
	if err != nil {
		return decimal.Zero, err
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if value := a.rateCache.Value; value != "" {
			return decimal.NewFromString(value)
		}
		return decimal.NewFromFloat(7), nil
	}
	defer resp.Body.Close()

	var data struct {
		Tether struct {
			CNY float64 `json:"cny"`
		} `json:"tether"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		if value := a.rateCache.Value; value != "" {
			return decimal.NewFromString(value)
		}
		return decimal.NewFromFloat(7), nil
	}

	if data.Tether.CNY <= 0 {
		if value := a.rateCache.Value; value != "" {
			return decimal.NewFromString(value)
		}
		return decimal.NewFromFloat(7), nil
	}

	value := decimal.NewFromFloat(data.Tether.CNY).StringFixed(4)
	a.rateCache.mu.Lock()
	a.rateCache.Value = value
	a.rateCache.ExpiresAt = time.Now().Add(1 * time.Minute)
	a.rateCache.mu.Unlock()
	return decimal.NewFromString(value)
}

func (a *App) setGroupLock(chatID int64, active bool) error {
	if !a.cfg.EnableGroupLock {
		return nil
	}

	permissions := tgbotapi.ChatPermissions{}
	if active {
		permissions = tgbotapi.ChatPermissions{
			CanSendMessages:       true,
			CanSendAudios:         true,
			CanSendDocuments:      true,
			CanSendPhotos:         true,
			CanSendVideos:         true,
			CanSendVideoNotes:     true,
			CanSendVoiceNotes:     true,
			CanSendPolls:          true,
			CanSendOtherMessages:  true,
			CanAddWebPagePreviews: true,
			CanInviteUsers:        true,
		}
	}

	config := tgbotapi.NewSetChatPermissions(chatID, permissions)
	_, err := a.bot.Request(config)
	return err
}

func (a *App) sendText(chatID int64, text string) (tgbotapi.Message, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = ""
	return a.bot.Send(msg)
}

func (a *App) replyText(msg *tgbotapi.Message, text string) error {
	reply := tgbotapi.NewMessage(msg.Chat.ID, text)
	reply.ReplyToMessageID = msg.MessageID
	_, err := a.bot.Send(reply)
	return err
}

func (a *App) currentBusinessDate() string {
	return time.Now().In(a.loc).Format("2006-01-02")
}

func (a *App) currentDateTimeText() string {
	return time.Now().In(a.loc).Format("2006-01-02 15:04:05")
}

func (a *App) nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func (a *App) reportURL(token string) string {
	if a.cfg.BaseURL == "" {
		return ""
	}
	return a.cfg.BaseURL + "/reports/" + token + ".html"
}

func (a *App) xlsxURL(token string) string {
	if a.cfg.BaseURL == "" {
		return ""
	}
	return a.cfg.BaseURL + "/reports/" + token + ".xlsx"
}

func removeSpaces(text string) string {
	return strings.Join(strings.Fields(text), "")
}

func mustDecimal(value string) decimal.Decimal {
	d, err := decimal.NewFromString(value)
	if err != nil {
		return decimal.Zero
	}
	return d
}

func hasMinimumRole(role *RoleRecord, minimum string) bool {
	if role == nil {
		return false
	}
	return roleWeight(role.Role) >= roleWeight(minimum)
}

func roleWeight(role string) int {
	switch role {
	case "owner":
		return 3
	case "manager":
		return 2
	case "operator":
		return 1
	default:
		return 0
	}
}

func telegramUserDisplayName(user *tgbotapi.User) string {
	if user == nil {
		return "未指定"
	}
	name := strings.TrimSpace(strings.TrimSpace(user.FirstName + " " + user.LastName))
	if name != "" {
		return name
	}
	if user.UserName != "" {
		return "@" + user.UserName
	}
	return strconv.FormatInt(user.ID, 10)
}

func mentionFromRole(role RoleRecord) string {
	if role.Username.Valid && role.Username.String != "" {
		return "@" + role.Username.String
	}
	return fmt.Sprintf("%s(%d)", role.DisplayName, role.UserID)
}

func mentionFromMember(member MemberRecord) string {
	if member.Username.Valid && member.Username.String != "" {
		return "@" + member.Username.String
	}
	return fmt.Sprintf("%s(%d)", member.DisplayName, member.UserID)
}

func toNullString(value string) sql.NullString {
	if strings.TrimSpace(value) == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func nullIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableStringValue(value sql.NullString) any {
	if value.Valid && value.String != "" {
		return value.String
	}
	return nil
}

func nullString(value sql.NullString, fallback string) string {
	if value.Valid && value.String != "" {
		return value.String
	}
	return fallback
}

func fileExists(filePath string) bool {
	info, err := os.Stat(filePath)
	return err == nil && !info.IsDir()
}

func sortEntriesByID(entries []LedgerEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ID < entries[j].ID
	})
}

func safeFloatToDecimal(value float64) decimal.Decimal {
	return decimal.NewFromFloat(math.Round(value*10000) / 10000)
}
