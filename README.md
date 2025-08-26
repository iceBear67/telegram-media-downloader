# telegram-media-downloader-bot

发文件给 Bot 然后自动下载，特么这么简单的事情我翻遍整个 GitHub 都没几个能用的，我请问呢？？？

Bot API 默认限制大小 20M, 如果想要突破限制请使用 [tgbotapi](https://tdlib.github.io/telegram-bot-api/build.html) 搭配 `-api` 使用。

```
 % ./bot --help
Usage of ./bot:
  -api string
        baseUrl of telegram bot api (default "https://api.telegram.org/bot%s/%s")
  -attempts int
        number of attempts to download files (default 10)
  -conc int
        number of concurrent downloads (default 4)
  -output string
        save path for files (default "./downloads")
  -sudoers string
        permitted users. Split by ,
  -token string
        telegram bot token.
  -use_local
        set this to true if you're using local bot api
```