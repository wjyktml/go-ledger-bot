# Go 版 Telegram 免费记账机器人

这是一个用 Go 语言编写的 Telegram 群记账机器人，按 `@jz7x24bot` 公开说明实现同类核心功能。

## 已实现功能

- 群内开始/停止记账
- 操作人、权限人、首个权限人角色体系
- 设置费率
- 设置固定汇率、切换实时汇率
- 原始模式、计数模式、回复模式
- `+1000` / `+1000/7.5` / `+-1000`
- `下发100` / `下发-100`
- `显示账单`
- `显示完整账单`
- `保存账单`
- `删除账单`
- `删除历史账单`
- `z100` 汇率换算
- 四则运算计算器
- 每日 `03:00` 自动归档
- HTML 账单导出
- Excel 账单导出
- 私聊 `/broadcast 文本内容` 群发

## 项目结构

```text
.
├─ cmd/
│  └─ bot/
│     └─ main.go
├─ .env.example
├─ .gitignore
├─ DEPLOY.md
├─ README.md
├─ go.mod
└─ go.sum
```

## 技术栈

- Telegram Bot API: `github.com/go-telegram-bot-api/telegram-bot-api/v5`
- 数据库: SQLite
- SQLite 驱动: `modernc.org/sqlite`
- Excel 导出: `github.com/xuri/excelize/v2`
- 小数精度: `github.com/shopspring/decimal`
- 定时任务: `github.com/robfig/cron/v3`

## 环境要求

- Go 1.22+
- 可公网访问的域名
- Telegram 机器人 Token

## 配置

复制环境变量模板：

```bash
cp .env.example .env
```

编辑 `.env`：

```env
BOT_TOKEN=你的机器人token
BOT_OWNER_IDS=你的Telegram数字ID
PORT=3100
BASE_URL=https://bot.yourdomain.com
TIMEZONE=Asia/Shanghai
ENABLE_GROUP_LOCK=true
```

参数说明：

- `BOT_TOKEN`：BotFather 生成的 Token
- `BOT_OWNER_IDS`：可私聊使用 `/broadcast` 的管理员 ID，多个用逗号分隔
- `PORT`：本地监听端口，默认 `3100`
- `BASE_URL`：对外访问地址，用于账单链接
- `TIMEZONE`：时区，默认 `Asia/Shanghai`
- `ENABLE_GROUP_LOCK`：开始/停止时是否尝试切换群权限

## 本地运行

安装依赖：

```bash
go mod tidy
```

运行：

```bash
go run -mod=mod ./cmd/bot
```

或编译：

```bash
go build -mod=mod -o telegram-ledger-bot-go ./cmd/bot
./telegram-ledger-bot-go
```

## 私聊命令

- `/start`
- `/broadcast 文本内容`

## 群内命令

### 基础控制

- `开始`
- `上课`
- `停止`
- `下课`

### 角色管理

- `设置操作人`
- `新增权限人`
- `删除操作人`
- `删除权限人`
- `显示操作人`

建议直接回复某人消息执行角色命令，最稳定。

### 配置命令

- `设置费率10`
- `设置汇率7`
- `设置实时汇率`
- `切换实时汇率`
- `设置原始模式`
- `切换原始模式`
- `设置计数模式`
- `切换计数模式`
- `设置回复模式`
- `切换回复模式`

### 记账命令

- `+1000`
- `+1000/7.5`
- `+-1000`
- `下发100`
- `下发-100`

### 账单命令

- `显示账单`
- `+0`
- `显示完整账单`
- `保存账单`
- `删除账单`
- `删除历史账单`

### 辅助命令

- `z100`
- `(100+10.5)+5*10`

## 数据目录

程序运行后会自动生成：

- `data/ledger.db`
- `reports/*.html`
- `reports/*.xlsx`

## 说明

- `@username` 方式设置角色需要目标用户先在群里发言过
- 最稳的方式始终是“回复目标用户消息后发送角色命令”
- 如果你要公网访问 HTML/Excel 报表，必须正确配置 `BASE_URL`

## 部署文档

完整服务器部署教程见 [DEPLOY.md](file:///c:/Users/wjykt/Desktop/%E6%9C%BA%E5%99%A8%E4%BA%BA/DEPLOY.md)。
