package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/superfly/macaroon/tp"
	"gopkg.in/yaml.v3"
)

type ask struct {
	slackTS    string
	pollSecret string
	ts         time.Time
}

type reply struct {
	name    string
	slackTS string
	answer  bool
}

type bot struct {
	config  *config
	tp      *tp.TP
	api     *slack.Client
	asks    chan *ask
	replies chan *reply
	*http.ServeMux
}

func newBot(c *config) *bot {
	store, _ := tp.NewMemoryStore(nil, 1000)

	b := &bot{
		config:  c,
		api:     slack.New(c.SlackBotToken),
		asks:    make(chan *ask),
		replies: make(chan *reply),
		tp: &tp.TP{
			Location: c.MacaroonLocation,
			Key:      c.getMacaroonSecret(),
			Store:    store,
			Log:      logrus.StandardLogger(),
		},
		ServeMux: http.NewServeMux(),
	}

	b.ServeMux.HandleFunc("/events-endpoint", b.PostEvent)

	b.ServeMux.Handle("/ticket"+tp.InitPath, b.tp.InitRequestMiddleware(
		http.HandlerFunc(b.HandleDischargeInit),
	))

	b.ServeMux.HandleFunc("/ticket"+tp.PollPathPrefix, b.tp.HandlePollRequest)

	b.ServeMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		slog.Info("incoming", "remote", r.RemoteAddr, "method", r.Method, "uri", r.URL, "handler", "404")
		w.WriteHeader(http.StatusNotFound)
	})

	return b
}

func (b *bot) waitLoop() {
	asks := map[string]*ask{}
	tick := time.NewTicker(5 * time.Second)

	for {
		select {
		case reply := <-b.replies:
			a, ok := asks[reply.slackTS]
			if !ok {
				continue
			}

			if reply.answer {
				e("discharge", b.tp.DischargePoll(a.pollSecret))
			} else {
				e("abort", b.tp.AbortPoll(a.pollSecret, fmt.Sprintf("rejected by @%s", reply.name)))
			}

		case a := <-b.asks:
			asks[a.slackTS] = a

		case <-tick.C:
			maps.DeleteFunc(asks, func(_ string, a *ask) bool {
				if a.ts.After(time.Now().Add(-2 * time.Minute)) {
					return false
				}

				e("abort", b.tp.AbortPoll(a.pollSecret, "timeout"))

				return true
			})
		}
	}
}

func (b *bot) PostEvent(w http.ResponseWriter, r *http.Request) {
	slog.Info("incoming", "remote", r.RemoteAddr, "method", r.Method, "uri", r.URL, "handler", "post-event")

	body, err := io.ReadAll(r.Body)
	if e500(w, "read", err) {
		return
	}

	sv, err := slack.NewSecretsVerifier(r.Header, b.config.SlackSigningSecret)
	if e500(w, "verify", err) {
		return
	}

	_, err = sv.Write(body)
	if e500(w, "verify", err) {
		return
	}

	err = sv.Ensure()
	if e500(w, "verify", err) {
		return
	}

	evt, err := slackevents.ParseEvent(json.RawMessage(body), slackevents.OptionNoVerifyToken())
	if e500(w, "parse", err) {
		return
	}

	slog.Info("event rx", "event", evt)

	switch evt.Type {
	case slackevents.URLVerification:
		var r slackevents.ChallengeResponse

		if err := json.Unmarshal([]byte(body), &r); e500(w, "unmarshal", err) {
			return
		}

		w.Header().Set("Content-Type", "text")
		w.Write([]byte(r.Challenge))

	case slackevents.CallbackEvent:
		switch ev := evt.InnerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
			ch, ts, err := b.api.PostMessageContext(r.Context(), ev.Channel, slack.MsgOptionText("Yes, hello.", false))
			if err != nil {
				e("post", err)
				return
			}

			slog.Info("posted", "ch", ch, "ts", ts)

		case *slackevents.ReactionAddedEvent:
			switch reaction, _, _ := strings.Cut(ev.Reaction, "::"); reaction {
			case "+1", "celeryman", "celebrate", "yes":
				b.replies <- &reply{
					name:    ev.User,
					answer:  true,
					slackTS: ev.Item.Timestamp,
				}

			default:
				b.replies <- &reply{
					name:    ev.User,
					answer:  false,
					slackTS: ev.Item.Timestamp,
				}
			}

			slog.Info("reaction", "ev", ev)
		}
	}
}

func (b *bot) HandleDischargeInit(w http.ResponseWriter, r *http.Request) {
	slog.Info("incoming", "remote", r.RemoteAddr, "method", r.Method, "uri", r.URL, "handler", "discharge-init")

	switch cavs, err := tp.CaveatsFromRequest(r); {
	case err != nil:
		e("caveats from request", err)
		b.tp.RespondError(w, r, http.StatusBadRequest, err.Error())
		return
	case len(cavs) != 0:
		e("caveats in request", err)
		b.tp.RespondError(w, r, http.StatusBadRequest, "unsupported caveats in 3p caveat")
		return
	}

	_, ts, err := b.api.PostMessage(b.config.SlackChannel,
		slack.MsgOptionText(":interrobang: login attempt. :+1: or :-1:?", false))
	if err != nil {
		e("post", err)
		b.tp.RespondError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	pollSecret := b.tp.RespondPoll(w, r)
	if pollSecret == "" {
		return
	}

	b.asks <- &ask{
		slackTS:    ts,
		pollSecret: pollSecret,
		ts:         time.Now(),
	}
}

func main() {
	c, err := loadConfig()
	if e("load config", err) {
		return
	}

	b := newBot(c)

	go b.waitLoop()

	e("serve", http.ListenAndServe(":3000", b))
}

type config struct {
	MacaroonLocation   string `yaml:"macaroon_location"`
	MacaroonSecret     string `yaml:"macaroon_secret"`
	SlackChannel       string `yaml:"slack_channel"`
	SlackSigningSecret string `yaml:"slack_signing_secret"`
	SlackBotToken      string `yaml:"slack_bot_token"`
}

const configPath = "cookiebot.yml"

func loadConfig() (*config, error) {
	y, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	ys := os.ExpandEnv(string(y))

	var c config
	if err := yaml.Unmarshal([]byte(ys), &c); err != nil {
		return nil, err
	}

	if err := c.validate(); err != nil {
		return nil, err
	}

	return &c, nil
}

func (c *config) validate() error {
	switch {
	case c.MacaroonLocation == "":
		return errors.New("config is missing MacaroonLocation")
	case c.MacaroonSecret == "":
		return errors.New("config is missing MacaroonSecret")
	case c.SlackChannel == "":
		return errors.New("config is missing SlackChannel")
	case c.SlackSigningSecret == "":
		return errors.New("config is missing SlackSigningSecret")
	case c.SlackBotToken == "":
		return errors.New("config is missing SlackBotToken")
	}

	if s, _ := base64.StdEncoding.DecodeString(c.MacaroonSecret); len(s) != 32 {
		return errors.New("invalid MacaroonSecret")
	}

	return nil
}

func (c *config) getMacaroonSecret() []byte {
	s, _ := base64.StdEncoding.DecodeString(c.MacaroonSecret)
	return s
}

func e(desc string, err error) bool {
	if err == nil {
		return false
	}

	slog.Error(desc, "error", err)
	return true
}

func e500(w http.ResponseWriter, desc string, err error) bool {
	if e(desc, err) {
		w.WriteHeader(http.StatusInternalServerError)
		return true
	}

	return false
}
