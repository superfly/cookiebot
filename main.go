package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/superfly/macaroon"
	"github.com/superfly/macaroon/flyio"
	"github.com/superfly/macaroon/tp"
)

const (
	MacaroonLocation = "https://cookiebot.fly.dev/ticket"
	TestChannel      = "C0603H9UZA4" // i should be purged i should be flogged
)

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

type chanAsk struct {
	slackts    string
	name       string
	reply      chan chanReply
	pollSecret string
	ts         time.Time
	kill       bool
}

type chanReply struct {
	name, slackts string
	answer        bool
}

type Bot struct {
	tp             *tp.TP
	macaroonSecret []byte
	signingSecret  string
	api            *slack.Client
	asks           chan chanAsk
	reacts         chan chanReply
}

func (b *Bot) WaitLoop(ctx context.Context) {
	asks := []*chanAsk{}

	tick := time.NewTicker(5 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return

		case reply := <-b.reacts:
			for _, ask := range asks {
				if ask.slackts == reply.slackts {
					if ask.reply != nil {
						ask.reply <- reply
					}
					if ask.pollSecret != "" {
						if reply.answer {
							b.tp.DischargePoll(ask.pollSecret)
						} else {
							b.tp.AbortPoll(ask.pollSecret, fmt.Sprintf("rejected by @%s", reply.name))
						}
					}
				}
			}

		case newAsk := <-b.asks:
			switch {
			case newAsk.kill:
				newAsks := []*chanAsk{}

				for _, ask := range asks {
					if ask.slackts != newAsk.slackts {
						newAsks = append(newAsks, ask)
					}
				}

				asks = newAsks

			default:
				newAsk.ts = time.Now()
				asks = append(asks, &newAsk)
			}

		case <-tick.C:
			newAsks := []*chanAsk{}

			thresh := time.Now().Add(-5 * time.Minute)

			for _, ask := range asks {
				if !ask.ts.Before(thresh) {
					newAsks = append(newAsks, ask)
				}
			}

			asks = newAsks
		}
	}
}

func (b *Bot) PostAttenuate(w http.ResponseWriter, r *http.Request) {
	slog.Info("incoming", "remote", r.RemoteAddr, "method", r.Method, "uri", r.URL)

	var buf strings.Builder
	if _, err := io.Copy(&buf, io.LimitReader(r.Body, 10000)); e500(w, "read", err) {
		return
	}

	permTok, dissToks, err := flyio.ParsePermissionAndDischargeTokens(buf.String())
	if e500(w, "parse", err) {
		return
	}

	// discourage sending of entire token...
	if len(dissToks) != 0 {
		http.Error(w, "only send permission token!", http.StatusBadRequest)
	}

	perm, err := macaroon.Decode(permTok)
	if e500(w, "decode", err) {
		return
	}

	if err := perm.Add3P(b.macaroonSecret, MacaroonLocation); e500(w, "attenuate", err) {
		return
	}

	permStr, err := perm.String()
	if e500(w, "encode", err) {
		return
	}

	if _, err := w.Write([]byte(permStr)); e("write", err) {
		return
	}
}

func (b *Bot) PostEvent(w http.ResponseWriter, r *http.Request) {
	slog.Info("incoming", "remote", r.RemoteAddr, "method", r.Method, "uri", r.URL)

	body, err := ioutil.ReadAll(r.Body)
	if e500(w, "read", err) {
		return
	}

	sv, err := slack.NewSecretsVerifier(r.Header, b.signingSecret)
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

	if evt.Type == slackevents.URLVerification {
		var r *slackevents.ChallengeResponse
		err := json.Unmarshal([]byte(body), &r)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text")
		w.Write([]byte(r.Challenge))
	}

	slog.Info("event rx", "event", evt)

	if evt.Type == slackevents.CallbackEvent {
		innerEvent := evt.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
			ch, ts, err := b.api.PostMessage(ev.Channel, slack.MsgOptionText("Yes, hello.", false))
			if err != nil {
				e("post", err)
			}

			slog.Info("posted", "ch", ch, "ts", ts)

		case *slackevents.ReactionAddedEvent:
			switch ev.Reaction {
			case "+1", "celeryman", "celebrate", "yes":
				b.reacts <- chanReply{
					name:    ev.User,
					answer:  true,
					slackts: ev.Item.Timestamp,
				}

			default:
				b.reacts <- chanReply{
					name:    ev.User,
					answer:  false,
					slackts: ev.Item.Timestamp,
				}
			}

			slog.Info("reaction", "ev", ev)
		}
	}
}

type TicketRequest struct {
	Name   string `json:"name"`
	Ticket []byte `json:"ticket"`
}

type TicketReply struct {
	Respondant string `json:"respondant"`
	Discharge  []byte `json:"discharge"`
	Approved   bool   `json:"approved"`
}

func (b *Bot) PostTicket(w http.ResponseWriter, r *http.Request) {
	slog.Info("incoming", "remote", r.RemoteAddr, "method", r.Method, "uri", r.URL)

	tr := TicketRequest{}

	err := json.NewDecoder(r.Body).Decode(&tr)
	if e500(w, "decode ticket json", err) {
		return
	}

	cavs, dm, err := macaroon.DischargeTicket(b.macaroonSecret, MacaroonLocation, tr.Ticket)
	if e500(w, "decode ticket", err) {
		return
	}
	if len(cavs) != 0 {
		http.Error(w, "unsupported caveats in 3p caveat", http.StatusBadRequest)
		return
	}

	discharge, err := dm.Encode()
	if e500(w, "encode discharge", err) {
		return
	}

	_, ts, err := b.api.PostMessage(TestChannel,
		slack.MsgOptionText(fmt.Sprintf(":interrobang: @%s would like to deploy. :+1: or :-1:?", tr.Name), false))
	if err != nil {
		e("post", err)
	}

	ask := chanAsk{
		slackts: ts,
		name:    tr.Name,
		reply:   make(chan chanReply),
	}

	b.asks <- ask

	defer func() {
		ask.kill = true
		b.asks <- ask
	}()

	select {
	case reply := <-ask.reply:
		u, err := b.api.GetUserInfo(reply.name)
		if err == nil {
			reply.name = u.Name
		}

		trep := &TicketReply{
			Respondant: reply.name,
			Approved:   reply.answer,
		}

		if reply.answer {
			trep.Discharge = discharge
		}

		json.NewEncoder(w).Encode(trep)

	case <-r.Context().Done():
		fmt.Fprintf(w, "timed out without response")
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (b *Bot) HandleDischargeInit(w http.ResponseWriter, r *http.Request) {
	slog.Info("incoming", "remote", r.RemoteAddr, "method", r.Method, "uri", r.URL)

	switch cavs, err := tp.CaveatsFromRequest(r); {
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case len(cavs) != 0:
		http.Error(w, "unsupported caveats in 3p caveat", http.StatusBadRequest)
		return
	}

	_, ts, err := b.api.PostMessage(TestChannel,
		slack.MsgOptionText(":interrobang: attempting to deploy. :+1: or :-1:?", false))
	if err != nil {
		e("post", err)
	}

	pollSecret := b.tp.RespondPoll(w, r)
	if pollSecret == "" {
		return
	}

	b.asks <- chanAsk{
		slackts:    ts,
		pollSecret: pollSecret,
	}
}

func main() {
	s64 := os.Getenv("MACAROON_SECRET")
	if s64 == "" {
		slog.Error("no MACAROON_SECRET")
		return
	}

	secret, err := base64.StdEncoding.DecodeString(s64)
	if e("decode MACAROON_SECRET", err) {
		return
	}

	if len(secret) != 32 {
		slog.Error("MACAROON_SECRET should be 32 bytes")
		return
	}

	store, err := tp.NewMemoryStore(nil, 1000)
	if e("create memory store", err) {
		return
	}

	b := &Bot{
		macaroonSecret: secret,
		signingSecret:  os.Getenv("SLACK_SIGNING_SECRET"),
		api:            slack.New(os.Getenv("SLACK_BOT_TOKEN")),
		asks:           make(chan chanAsk),
		reacts:         make(chan chanReply),
		tp: &tp.TP{
			Location: MacaroonLocation,
			Key:      secret,
			Store:    store,
			Log:      logrus.StandardLogger(),
		},
	}

	http.HandleFunc("/attenuate", b.PostAttenuate)
	http.HandleFunc("/events-endpoint", b.PostEvent)
	http.HandleFunc("/ticket", b.PostTicket)

	http.Handle(MacaroonLocation+"/"+tp.InitPath, b.tp.InitRequestMiddleware(
		http.HandlerFunc(b.HandleDischargeInit),
	))

	http.HandleFunc(MacaroonLocation+"/"+tp.PollPathPrefix, b.tp.HandlePollRequest)

	slog.Info("server listening", "port", ":3000")

	http.ListenAndServe(":3000", nil)
}
