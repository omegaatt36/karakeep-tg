# karakeep-tg

Receives Karakeep's `crawled` webhook, waits for the AI summary, and sends the article to Telegram.

```sh
cp .env.example .env
task run
task        # fmt, lint, test
```

## Setup

1. Create a bot with BotFather; add it as admin to the target channel.
2. In Karakeep settings, add a webhook for `crawled` with URL `http://karakeep-tg:8080/webhook/karakeep` and the same token as `WEBHOOK_TOKEN`.
3. Karakeep must allow the hostname: `CRAWLER_ALLOWED_INTERNAL_HOSTNAMES=karakeep-tg`.
4. After the first release, set the GHCR package `karakeep-tg` to public (package settings, Change visibility).

Configuration lives in `.env.example`; `TELEGRAM_CHAT_IDS` is a comma-separated list.
