package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/avast/retry-go"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

var botToken = flag.String("token", "", "telegram bot token.")
var apiEndpoint = flag.String("api", "https://api.telegram.org", "baseUrl of telegram bot api")
var savePath = flag.String("output", "./downloads", "save path for files")
var retryAttempts = flag.Int("attempts", 10, "number of attempts to download files")
var localApi = flag.Bool("use_local", false, "set this to true if you're using local bot api")
var concurrentSignal chan struct{}
var client *http.Client

type MediaFile struct {
	FileName string
	FileID   string
}

var pathRegex = regexp.MustCompile("https://.+/(/.+$)")

func main() {
	concurrent := flag.Int("conc", 4, "number of concurrent downloads")
	_permittedUsers := flag.String("sudoers", "", "permitted users. Split by ,")
	flag.Parse()
	os.MkdirAll(*savePath, 0755)
	concurrentSignal = make(chan struct{}, *concurrent)
	client = &http.Client{}
	if *botToken == "" || *apiEndpoint == "" {
		panic("Invalid botToken or apiEndpoint.")
	}
	permittedUsers := strings.Split(*_permittedUsers, ",")
	if *_permittedUsers == "" {
		log.Println("WARNING: Sudoers are not set. Everyone can request your bot to download sth.")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	serverURL := *apiEndpoint + "/bot"
	b, err := bot.New(*botToken, bot.WithServerURL(serverURL),
		bot.WithDefaultHandler(func(ctx context.Context, b *bot.Bot, update *models.Update) {
			if update.Message == nil {
				return
			}
			id := strconv.FormatInt(update.Message.From.ID, 10)
			if *_permittedUsers != "" {
				if slices.Index(permittedUsers, id) == -1 {
					log.Printf("Failed to validate %v: %v", update.Message.From.Username, id)
					return
				}
			}
			log.Printf("[%s] %s", update.Message.From.Username, update.Message.Text)
			handleDocument(ctx, b, update)
			handleAudio(ctx, b, update)
			handleVideo(ctx, b, update)
			handlePhoto(ctx, b, update)
		}))
	if err != nil {
		log.Panic(err)
	}
	log.Printf("Authorized on account %s", b.ID())
	b.Start(ctx)
}

func handleDocument(ctx context.Context, b *bot.Bot, update *models.Update) {
	document := update.Message.Document
	if document == nil {
		return
	}
	go handleFile(ctx, b, update, MediaFile{FileID: document.FileID, FileName: document.FileName})
}

func handleAudio(ctx context.Context, b *bot.Bot, update *models.Update) {
	audio := update.Message.Audio
	if audio == nil {
		return
	}
	go handleFile(ctx, b, update, MediaFile{FileID: audio.FileID, FileName: audio.FileName})
}

func handleVideo(ctx context.Context, b *bot.Bot, update *models.Update) {
	video := update.Message.Video
	if video == nil {
		return
	}
	var best *models.VideoQuality = nil
	for _, quality := range video.Qualities {
		if best == nil {
			best = &quality
			continue
		}
		if quality.FileSize > best.FileSize {
			best = &quality
		}
	}
	fileId := video.FileID
	if best != nil {
		fileId = best.FileID
	}
	go handleFile(ctx, b, update, MediaFile{
		FileID:   fileId,
		FileName: video.FileName,
	})
}

func handlePhoto(ctx context.Context, b *bot.Bot, update *models.Update) {
	photo := update.Message.Photo
	if photo == nil {
		return
	}
	p := slices.MaxFunc(photo, func(a, b models.PhotoSize) int {
		return cmp.Compare(b.FileSize, a.FileSize)
	})
	go handleFile(ctx, b, update, MediaFile{
		time.Unix(int64(update.Message.Date), 0).String(),
		p.FileID,
	})
}

var downloading int32 = 0

func handleFile(ctx context.Context, b *bot.Bot, update *models.Update, media MediaFile) {
	mediaFileName := media.FileName
	if mediaFileName == "" {
		mediaFileName = media.FileID
	}
	if update.Message.ForwardOrigin != nil {
		if origin := update.Message.ForwardOrigin.MessageOriginChat; origin != nil {
			mediaFileName = fmt.Sprintf("%v %v - %v", origin.SenderChat.FirstName, origin.SenderChat.LastName, mediaFileName)
		}
	}
	log.Printf("(%v) Found new media: %v, sent from %v", media.FileID, mediaFileName, update.Message.Chat.Username)
	filePath := path.Join(*savePath, media.FileID+"."+mediaFileName)
	atomic.AddInt32(&downloading, 1)
	defer atomic.AddInt32(&downloading, -1)
	if f, err := os.Stat(filePath); err == nil {
		sendMessage(ctx, b, update.Message.Chat.ID, fmt.Sprintf("%v already downloaded! (size: %v)", mediaFileName, f.Size()))
		return
	}
	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   fmt.Sprintf("Enqueued %v. %v are downloading now", mediaFileName, downloading),
	})

	file, err := b.GetFile(ctx, &bot.GetFileParams{FileID: media.FileID})
	if err != nil {
		sendMessage(ctx, b, update.Message.Chat.ID, fmt.Sprintf("(%v) Failed to fetch direct url of %v, err: %v", media.FileID, mediaFileName, err))
		return
	}
	url := b.FileDownloadLink(file)
	log.Println("Resolved url for ", media.FileID, " is: ", url)
	if !*localApi {
		downloadTask(ctx, b, mediaFileName, url, filePath, update.Message.Chat.ID)
		return
	}
	matches := pathRegex.FindStringSubmatch(url)
	if matches == nil || len(matches) != 2 {
		sendMessage(ctx, b, update.Message.Chat.ID, fmt.Sprintf("(%v) Cannot resolve local url %v", media.FileID, mediaFileName))
		return
	}
	log.Println("Moving from", matches[1], "to", filePath)
	var copyErr error
	if src, err := os.Open(matches[1]); err == nil {
		if dst, err := os.Create(filePath); err == nil {
			_, copyErr = src.WriteTo(dst)
			_ = dst.Close()
		}
		_ = src.Close()
	}
	if copyErr != nil {
		sendMessage(ctx, b, update.Message.Chat.ID, fmt.Sprintf("(%v) Cannot move file for %v. err: %v", media.FileID, mediaFileName, copyErr))
		return
	}
	sendMessage(ctx, b, update.Message.Chat.ID, fmt.Sprintf("%v is saved successfully", mediaFileName))
}

func downloadTask(ctx context.Context, b *bot.Bot, name string, url string, filePath string, chatId int64) {
	concurrentSignal <- struct{}{}
	defer func() { <-concurrentSignal }()
	if _, err := os.Stat(filePath); err == nil {
		return
	}
	attempt := 0
	_ = retry.Do(func() error {
		resp, err := client.Get(url)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			return errors.New(resp.Status)
		}
		file, err := os.Create(filePath)
		if err != nil {
			return err
		}
		written, err := io.Copy(file, resp.Body)
		if err != nil {
			return err
		}
		sendMessage(ctx, b, chatId, fmt.Sprintf("%v has been downloaded! (%vB)", name, written))
		return nil
	}, retry.RetryIf(func(err error) bool {
		if attempt > *retryAttempts {
			sendMessage(ctx, b, chatId, fmt.Sprintf("Failed to download %v, err: %v", name, err))
			return false
		}
		log.Printf("Attempt %v failed, retrying. Error: %v", attempt, err)
		attempt++
		return true
	}))
}

func sendMessage(ctx context.Context, b *bot.Bot, chat int64, text string) {
	log.Printf("(-> %v) %v", chat, text)
	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chat,
		Text:   text,
	})
}
