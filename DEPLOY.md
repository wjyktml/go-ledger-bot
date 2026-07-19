# Go 版服务器部署教程

本文档是 Go 版 Telegram 记账机器人的完整部署教程，包含：

- Linux 服务器初始化
- Go 环境安装
- 项目上传
- 环境变量配置
- 编译与运行
- systemd 守护
- Nginx 反向代理
- HTTPS 证书
- 防火墙配置
- BotFather 设置
- 故障排查

适用系统：

- Ubuntu 22.04 / 24.04
- Debian 12

## 一、部署架构

推荐部署方式：

- Go 程序本地监听 `127.0.0.1:3100`
- Nginx 对外提供 `80/443`
- Nginx 反向代理到 Go 程序
- systemd 负责进程守护
- SQLite 存储账单数据
- `reports/` 对外静态提供 HTML 与 Excel 报表

推荐目录：

```bash
/www/wwwroot/go-ledger-bot
```

## 二、准备信息

部署前准备：

- 一台 Linux 服务器
- 一个域名，并解析到服务器 IP
- Telegram 机器人 Token
- 你的 Telegram 数字 ID

## 三、初始化服务器

更新系统：

```bash
sudo apt update
sudo apt -y upgrade
```

安装基础工具：

```bash
sudo apt -y install curl wget git unzip vim ufw nginx
```

创建目录：

```bash
sudo mkdir -p /www/wwwroot/go-ledger-bot
sudo chown -R $USER:$USER /www/wwwroot/go-ledger-bot
cd /www/wwwroot/go-ledger-bot
```

## 四、安装 Go

### 方案 A：用官方二进制安装

```bash
cd /tmp
wget https://go.dev/dl/go1.24.6.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.24.6.linux-amd64.tar.gz
```

写入环境变量：

```bash
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
```

检查版本：

```bash
go version
```

## 五、上传项目

你可以用以下任意方式上传：

- `git clone`
- WinSCP
- SFTP
- 宝塔文件管理器

最终目录应至少包含：

```bash
go.mod
cmd/
.env.example
README.md
DEPLOY.md
```

进入项目目录：

```bash
cd /www/wwwroot/go-ledger-bot
```

## 六、配置环境变量

复制模板：

```bash
cp .env.example .env
```

编辑：

```bash
vim .env
```

示例：

```env
BOT_TOKEN=123456789:your_real_bot_token
BOT_OWNER_IDS=123456789
PORT=3100
BASE_URL=https://bot.yourdomain.com
TIMEZONE=Asia/Shanghai
ENABLE_GROUP_LOCK=true
```

说明：

- `BOT_TOKEN`：Telegram 机器人 Token
- `BOT_OWNER_IDS`：可私聊使用 `/broadcast` 的管理员 ID
- `PORT`：程序监听端口，默认 `3100`
- `BASE_URL`：外网访问地址，账单导出链接会用到
- `TIMEZONE`：时区
- `ENABLE_GROUP_LOCK`：开始/停止时是否尝试修改群权限

## 七、下载依赖并编译

### 1. 拉依赖

```bash
go mod tidy
```

### 2. 编译程序

```bash
go build -mod=mod -o telegram-ledger-bot-go ./cmd/bot
```

编译成功后会生成：

```bash
./telegram-ledger-bot-go
```

### 3. 先手动运行测试

```bash
./telegram-ledger-bot-go
```

如果看到类似输出，说明启动正常：

```text
HTTP 报表服务已启动: http://127.0.0.1:3100
机器人 xxxxx_bot 已启动
```

按 `Ctrl + C` 停止。

## 八、配置 systemd 开机自启

创建服务文件：

```bash
sudo vim /etc/systemd/system/go-ledger-bot.service
```

写入：

```ini
[Unit]
Description=Go Telegram Ledger Bot
After=network.target

[Service]
Type=simple
WorkingDirectory=/www/wwwroot/go-ledger-bot
ExecStart=/www/wwwroot/go-ledger-bot/telegram-ledger-bot-go
Restart=always
RestartSec=5
User=root
Environment=HOME=/root

[Install]
WantedBy=multi-user.target
```

说明：

- 如果你不是 `root` 运行，请把 `User=root` 改成你的实际用户
- `ExecStart` 必须写成编译后二进制的完整路径

启动并设为开机自启：

```bash
sudo systemctl daemon-reload
sudo systemctl enable go-ledger-bot
sudo systemctl start go-ledger-bot
sudo systemctl status go-ledger-bot
```

查看日志：

```bash
journalctl -u go-ledger-bot -f
```

## 九、配置 Nginx 反向代理

创建站点配置：

```bash
sudo vim /etc/nginx/sites-available/go-ledger-bot
```

写入：

```nginx
server {
    listen 80;
    server_name bot.yourdomain.com;

    client_max_body_size 20m;

    location / {
        proxy_pass http://127.0.0.1:3100;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

启用：

```bash
sudo ln -sf /etc/nginx/sites-available/go-ledger-bot /etc/nginx/sites-enabled/go-ledger-bot
sudo nginx -t
sudo systemctl reload nginx
```

测试：

```bash
curl http://127.0.0.1:3100/health
curl http://bot.yourdomain.com/health
```

## 十、配置 HTTPS

安装 certbot：

```bash
sudo apt -y install certbot python3-certbot-nginx
```

申请证书：

```bash
sudo certbot --nginx -d bot.yourdomain.com
```

申请成功后，把 `.env` 中的：

```env
BASE_URL=http://bot.yourdomain.com
```

改成：

```env
BASE_URL=https://bot.yourdomain.com
```

然后重启服务：

```bash
sudo systemctl restart go-ledger-bot
```

## 十一、防火墙

如果开启了 UFW：

```bash
sudo ufw allow OpenSSH
sudo ufw allow 'Nginx Full'
sudo ufw enable
sudo ufw status
```

说明：

- 不需要把 `3100` 开放到公网
- 只开放 `22`、`80`、`443`

## 十二、Telegram 配置

打开 `@BotFather`：

### 1. 创建机器人

```text
/newbot
```

拿到 Token 后写进 `.env`。

### 2. 关闭群隐私模式

依次操作：

- `/mybots`
- 选择你的机器人
- `Bot Settings`
- `Group Privacy`
- `Turn off`

### 3. 把机器人拉进群

加到群后给管理员权限，建议至少开启：

- 删除消息
- 禁言成员
- 邀请用户
- 管理群组

如果你要用“开始/停止”控制群权限，机器人必须具备足够群管理权限。

## 十三、部署完成后的验证流程

### 1. 健康检查

浏览器打开：

```text
https://bot.yourdomain.com/health
```

应返回 JSON。

### 2. 私聊机器人

发送：

```text
/start
```

应返回说明。

### 3. 群内测试

按顺序测试：

```text
开始
设置费率10
设置汇率7
+1000
下发100
显示账单
保存账单
显示完整账单
```

### 4. 检查生成文件

```bash
ls -la data
ls -la reports
```

应看到：

- `data/ledger.db`
- `reports/*.html`
- `reports/*.xlsx`

## 十四、更新代码

进入项目目录：

```bash
cd /www/wwwroot/go-ledger-bot
```

重新编译：

```bash
go build -mod=mod -o telegram-ledger-bot-go ./cmd/bot
```

重启：

```bash
sudo systemctl restart go-ledger-bot
```

## 十五、备份建议

建议每天备份这些内容：

- `.env`
- `data/`
- `reports/`
- 编译后二进制

可直接执行：

```bash
tar -czf /root/go-ledger-bot-backup-$(date +%F).tar.gz \
  /www/wwwroot/go-ledger-bot/.env \
  /www/wwwroot/go-ledger-bot/data \
  /www/wwwroot/go-ledger-bot/reports \
  /www/wwwroot/go-ledger-bot/telegram-ledger-bot-go
```

## 十六、常见问题

### 1. 机器人收不到群消息

排查：

- `Group Privacy` 是否关闭
- 机器人是否已进群
- 是否给了管理员权限
- 群消息是否为普通文本

### 2. 账单链接打不开

排查：

- `BASE_URL` 是否正确
- 域名是否解析到服务器
- Nginx 是否正常
- `80/443` 是否放行
- HTTPS 是否已启用

### 3. 程序启动失败

查看日志：

```bash
journalctl -u go-ledger-bot -f
```

常见原因：

- `.env` 缺失
- `BOT_TOKEN` 错误
- 端口被占用
- 目录权限不够

### 4. 03:00 自动归档没有执行

检查：

- `.env` 的 `TIMEZONE` 是否正确
- 服务器时间是否正确
- 进程是否一直运行
- 当天是否存在未保存账单

查看时间：

```bash
date
timedatectl
```

### 5. `设置操作人 @xxx` 不生效

这是 Telegram 侧限制，机器人不一定能仅凭文本 `@username` 拿到用户 ID。

最稳方式：

- 先让目标用户在群里发言
- 直接回复对方消息发送 `设置操作人`

## 十七、最简上线命令

如果你想最快上线，可以直接按下面顺序执行：

```bash
sudo apt update && sudo apt -y upgrade
sudo apt -y install curl wget git unzip vim ufw nginx
cd /tmp
wget https://go.dev/dl/go1.24.6.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.24.6.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
sudo mkdir -p /www/wwwroot/go-ledger-bot
sudo chown -R $USER:$USER /www/wwwroot/go-ledger-bot
cd /www/wwwroot/go-ledger-bot
```

上传代码后：

```bash
cp .env.example .env
vim .env
go mod tidy
go build -mod=mod -o telegram-ledger-bot-go ./cmd/bot
./telegram-ledger-bot-go
```

确认正常后：

```bash
sudo vim /etc/systemd/system/go-ledger-bot.service
sudo systemctl daemon-reload
sudo systemctl enable go-ledger-bot
sudo systemctl start go-ledger-bot
sudo vim /etc/nginx/sites-available/go-ledger-bot
sudo ln -sf /etc/nginx/sites-available/go-ledger-bot /etc/nginx/sites-enabled/go-ledger-bot
sudo nginx -t
sudo systemctl reload nginx
sudo apt -y install certbot python3-certbot-nginx
sudo certbot --nginx -d bot.yourdomain.com
sudo systemctl restart go-ledger-bot
```
