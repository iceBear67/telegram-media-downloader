package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/avast/retry-go"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

var botToken = flag.String("token", "", "telegram bot token.")
var apiEndpoint = flag.String("api", tgbotapi.APIEndpoint, "baseUrl of telegram bot api")
var savePath = flag.String("output", "./downloads", "save path for files")
var retryAttempts = flag.Int("attempts", 10, "number of attempts to download files")
var localApi = flag.Bool("use_local", false, "set this to true if you're using local bot api")
var bot *tgbotapi.BotAPI
var concurrentSignal chan struct{}
var client *http.Client

var localApiFileURLRegex *regexp.Regexp

type MediaFile struct {
	FileName string
	FileID   string
}

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
	if *localApi {
		regex := strings.ReplaceAll(tgbotapi.FileEndpoint, ".", "\\.")
		regex = strings.ReplaceAll(tgbotapi.FileEndpoint, "/", "\\/")
		regex = strings.Replace(regex, "%s", "([a-zA-Z:0-9_]+)?", 1)
		regex = strings.Replace(regex, "%s", "(.*)", 1)
		localApiFileURLRegex = regexp.MustCompile(regex)
	}
	permittedUsers := strings.Split(*_permittedUsers, ",")
	if *_permittedUsers == "" {
		log.Println("WARNING: Sudoers are not set. Everyone can request your bot to download sth.")
	}

	_bot, err := tgbotapi.NewBotAPIWithAPIEndpoint(*botToken, *apiEndpoint)
	bot = _bot
	if err != nil {
		log.Panic(err)
	}
	log.Printf("Authorized on account %s", bot.Self.UserName)
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)
	for update := range updates {
		if update.Message != nil { // If we got a message
			id := strconv.FormatInt(update.Message.From.ID, 10)
			if *_permittedUsers != "" {
				if slices.Index(permittedUsers, id) == -1 {
					log.Printf("Failed to validate %v: %v", update.Message.From.UserName, id)
					continue
				}
			}
			log.Printf("[%s] %s", update.Message.From.UserName, update.Message.Text)
			handleDocument(update)
			handleAudio(update)
			handleVideo(update)
			handleAudio(update)
			handlePhoto(update)
		}
	}
}

func handleDocument(update tgbotapi.Update) {
	document := update.Message.Document
	if document == nil {
		return
	}
	go handleFile(update, MediaFile{FileID: document.FileID, FileName: document.FileName})
}

func handleAudio(update tgbotapi.Update) {
	audio := update.Message.Audio
	if audio == nil {
		return
	}
	go handleFile(update, MediaFile{FileID: audio.FileID, FileName: audio.FileName})
}

func handleVideo(update tgbotapi.Update) {
	video := update.Message.Video
	if video == nil {
		return
	}
	go handleFile(update, MediaFile{
		FileID:   video.FileID,
		FileName: video.FileName,
	})
}

func handlePhoto(update tgbotapi.Update) {
	photo := update.Message.Photo
	if photo == nil {
		return
	}
	for i := range photo {
		p := photo[i]
		go handleFile(update, MediaFile{
			p.FileID,
			p.FileUniqueID,
		})
	}
}

func handleFile(update tgbotapi.Update, media MediaFile) {
	mediaFileName := media.FileName
	forwardFrom := update.Message.ForwardFromChat
	if forwardFrom != nil {
		mediaFileName = fmt.Sprintf("%v %v - %v", forwardFrom.FirstName, forwardFrom.LastName, mediaFileName)
	}
	log.Printf("(%v) Found new media: %v, sent from %v", media.FileID, mediaFileName, update.FromChat().UserName)
	url, err := bot.GetFileDirectURL(media.FileID)
	if err != nil {
		sendMessage(update.Message.Chat.ID, fmt.Sprintf("(%v) Failed to fetch direct url of %v, err: %v", media.FileID, mediaFileName, err))
		return
	}
	log.Println("Resolved url for ", media.FileID, " is: ", url)
	go bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("Enqueued %v. %v are downloading now", mediaFileName, len(concurrentSignal))))
	if localApiFileURLRegex == nil {
		downloadTask(mediaFileName, url, update.Message.Chat.ID)
		return
	}
	matches := localApiFileURLRegex.FindStringSubmatch(url)
	if matches == nil || len(matches) != 3 {
		sendMessage(update.Message.Chat.ID, fmt.Sprintf("(%v) Cannot resolve local url %v", media.FileID, mediaFileName))
		return
	}
	err = os.Rename(matches[2], path.Join(*savePath, mediaFileName))
	if err != nil {
		sendMessage(update.Message.Chat.ID, fmt.Sprintf("(%v) Cannot move file for %v", media.FileID, mediaFileName))
		return
	}
	sendMessage(update.Message.Chat.ID, fmt.Sprintf("%v is saved successfully", mediaFileName))
}

func downloadTask(name string, url string, chatId int64) {
	concurrentSignal <- struct{}{}
	defer func() { <-concurrentSignal }()
	attempt := 0
	_ = retry.Do(func() error {
		resp, err := client.Get(url)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			return errors.New(resp.Status)
		}
		filePath := path.Join(*savePath, name)
		file, err := os.Create(filePath)
		if err != nil {
			return err
		}
		written, err := io.Copy(file, resp.Body)
		if err != nil {
			return err
		}
		sendMessage(chatId, fmt.Sprintf("%v has been downloaded! (%vB)", name, written))
		return nil
	}, retry.RetryIf(func(err error) bool {
		if attempt > *retryAttempts {
			sendMessage(chatId, fmt.Sprintf("Failed to download %v, err: %v", name, err))
			return false
		}
		log.Printf("Attempt %v failed, retrying. Error: %v", attempt, err)
		attempt++
		return true
	}))
}

func sendMessage(chat int64, text string) {
	log.Printf("(-> %v) %v", chat, text)
	msg := tgbotapi.NewMessage(chat, text)
	go bot.Send(msg)
}
