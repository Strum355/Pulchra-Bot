package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/bwmarrin/discordgo"
	"github.com/radovskyb/watcher"
)

var (
	dg      *discordgo.Session
	events  []watcher.Event
	conf    *config
	logF    *os.File
	counter uint
)

type config struct {
	Token         string   `toml:"token"`
	Dir           string   `toml:"dir"`
	Color         int      `toml:"color"`
	WatchDelay    int      `toml:"watch_delay"`
	MessageDelay  int      `toml:"message_frequency"`
	Channels      []string `toml:"channels"`
	Prefix        string   `toml:"prefix"`
	AdminChannel  string   `toml:"admin_channel"`
	ScreenshotDir string   `toml:"screenshot_dir"`
	YourID        string   `toml:"user_id"`
}

func main() {
	w := watcher.New()
	runtime.GOMAXPROCS(1)

	var err error
	//logF = os.Stdout
	logF, err = os.OpenFile("log.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer logF.Close()

	if _, err = toml.DecodeFile("config.toml", &conf); err != nil {
		fmt.Fprintln(logF, err)
		os.Exit(1)
	}

	conf.ScreenshotDir, _ = filepath.Abs(conf.ScreenshotDir)

	dg, err = discordgo.New("Bot " + conf.Token)
	if err != nil {
		fmt.Fprintln(logF, err)
		os.Exit(1)
	}

	if err = dg.Open(); err != nil {
		fmt.Fprintln(logF, err)
		os.Exit(1)
	}
	defer dg.Close()

	dg.AddHandler(message)

	w.Ignore("config.toml", "log.txt")

	go func() {
		for {
			select {
			case event := <-w.Event:
				events = append(events, event)
			case err := <-w.Error:
				sendError(err)
				os.Exit(1)
			case <-w.Closed:
				return
			}
		}
	}()

	go func() {
		for {
			sendMessages()
			time.Sleep(time.Second * time.Duration(conf.MessageDelay))
		}
	}()

	if err := w.AddRecursive(conf.Dir); err != nil {
		fmt.Fprintln(logF, err)
		os.Exit(1)
	}

	if err := w.AddRecursive(conf.ScreenshotDir); err != nil {
		fmt.Fprintln(logF, err)
		os.Exit(1)
	}

	fmt.Fprintln(logF, "Started")

	if err := w.Start(time.Second * time.Duration(conf.WatchDelay)); err != nil {
		fmt.Fprintln(logF, err)
		os.Exit(1)
	}
}

func message(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.Bot || !strings.HasPrefix(m.Content, conf.Prefix) || m.Author.ID != conf.YourID {
		return
	}

	if strings.TrimPrefix(m.Content, conf.Prefix) == "reload" {
		if _, err := toml.DecodeFile("config.toml", &conf); err != nil {
			sendError(err)
			s.ChannelMessageSend(m.ChannelID, "Error reloading config"+err.Error())
			return
		}
		s.ChannelMessageSend(m.ChannelID, "Reloaded config")
	}
}

func sendError(e error) {
	fmt.Fprintln(logF, e)

	c, err := dg.UserChannelCreate(conf.YourID)
	if err != nil {
		fmt.Fprintln(logF, err)
		os.Exit(1)
	}

	if _, err = dg.ChannelMessageSend(c.ID, "Watcher Bot had an issue. Check the logs"); err != nil {
		fmt.Fprintln(logF, err)
		os.Exit(1)
	}
}

func sendMessages() {
	if len(events) == 0 {
		return
	}

	fields := make([][]*discordgo.MessageEmbedField, 1)
	var total uint16
	var currIndex uint8
	wasImage := false

	for _, event := range events {
		if event.IsDir() {
			continue
		}

		if strings.HasPrefix(event.Path, conf.ScreenshotDir) && event.Op == watcher.Create {
			go postToAdmin(event)
			wasImage = true
		} else {
			processFileUpdates(event, fields, total, currIndex)
			wasImage = false
		}
	}

	if !wasImage {
		for _, channel := range conf.Channels { 
			for _, field := range fields {
				dg.ChannelMessageSendEmbed(channel, &discordgo.MessageEmbed{
					Title:  "Pulchra Latest Updates",
					Fields: field,
					Color:  conf.Color,
				})
			}
		}
	}

	events = []watcher.Event{}
}

func processFileUpdates(e watcher.Event, fields [][]*discordgo.MessageEmbedField, total uint16, currIndex uint8) {
	if e.Op == watcher.Write {
		return
	}

	name := e.String() + func() string {
		if e.IsDir() {
			return " Directory"
		}
		return " File"
	}()

	total += uint16(len(name))
	total += uint16(len(e.Path))

	if total >= 2000 {
		currIndex++
		fields = append(fields, []*discordgo.MessageEmbedField{})
	}

	fields[currIndex] = append(fields[currIndex], &discordgo.MessageEmbedField{
		Name: name,
		Value: e.Path,
	})
}

func postToAdmin(e watcher.Event) {
	f, err := os.Open(e.Path)
	if err != nil {
		sendError(err)
		return
	}
	defer f.Close()

	count := counter
	counter++

	if _, err := dg.ChannelMessageSendComplex(conf.AdminChannel, &discordgo.MessageSend{
		Content: fmt.Sprintf("#%d", count),
		Files: []*discordgo.File{
			{Name: filepath.Base(e.Path), Reader: f},
		},
	}); err != nil {
		sendError(err)
		return
	}

	for {
		runtime.Gosched()

		msg := <-nextMessageCreate(dg)
		if msg.Author.ID != conf.YourID ||msg.ChannelID != conf.AdminChannel || !strings.HasPrefix(msg.Content, fmt.Sprintf("#%d", count)) {
			continue
		}

		if _, err := f.Seek(0, 0); err != nil {
			sendError(err)
			return
		}

		for _, channel := range conf.Channels {
			if _, err := dg.ChannelMessageSendComplex(channel, &discordgo.MessageSend{
				Content: strings.TrimPrefix(msg.Content, fmt.Sprintf("#%d", count)),
				Files: []*discordgo.File{
					{Name: filepath.Base(e.Path), Reader: f},
				},
			}); err != nil {
				sendError(err)
			}
		}
		return
	}
}

func nextMessageCreate(s *discordgo.Session) chan *discordgo.MessageCreate {
	out := make(chan *discordgo.MessageCreate)
	s.AddHandlerOnce(func(_ *discordgo.Session, e *discordgo.MessageCreate) {
		out <- e
	})
	return out
}
