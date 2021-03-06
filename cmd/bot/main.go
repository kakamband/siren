package main

import (
	"bytes"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/bcmk/go-smtpd/smtpd"
	"github.com/bcmk/siren/lib"
	"github.com/bcmk/siren/payments"
	tg "github.com/bcmk/telegram-bot-api"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
)

var (
	version  = "5.0"
	checkErr = lib.CheckErr
	lerr     = lib.Lerr
	linf     = lib.Linf
	ldbg     = lib.Ldbg
)

type tplData = map[string]interface{}

type timeDiff struct {
	Days        int
	Hours       int
	Minutes     int
	Seconds     int
	Nanoseconds int
}

type notification struct {
	endpoint string
	chatID   int64
	modelID  string
	status   lib.StatusKind
	timeDiff *timeDiff
}

type model struct {
	modelID string
	status  lib.StatusKind
}

type statusChange struct {
	modelID   string
	status    lib.StatusKind
	timestamp int
}

type statRequest struct {
	endpoint string
	writer   http.ResponseWriter
	request  *http.Request
	done     chan bool
}

type ipnRequest struct {
	writer  http.ResponseWriter
	request *http.Request
	done    chan bool
}

type queryDurationsData struct {
	avg   float64
	count int
}

type user struct {
	chatID               int64
	maxModels            int
	reports              int
	blacklist            bool
	showImages           bool
	offlineNotifications bool
}

type worker struct {
	clients                  []*lib.Client
	bots                     map[string]*tg.BotAPI
	db                       *sql.DB
	cfg                      *config
	httpQueriesDuration      time.Duration
	updatesDuration          time.Duration
	changesInPeriod          int
	confirmedChangesInPeriod int
	ourOnline                map[string]bool
	specialModels            map[string]bool
	siteStatuses             map[string]statusChange
	siteOnline               map[string]bool
	tr                       map[string]*lib.Translations
	tpl                      map[string]*template.Template
	modelIDPreprocessing     func(string) string
	checkModel               func(client *lib.Client, modelID string, headers [][2]string, dbg bool, config map[string]string) lib.StatusKind
	onlineModelsAPI          func(
		endpoint string,
		client *lib.Client,
		headers [][2]string,
		debug bool,
		config map[string]string,
	) (
		onlineModels map[string]lib.OnlineModel,
		err error,
	)

	unsuccessfulRequests  []bool
	successfulRequestsPos int
	downloadErrors        []bool
	downloadResultsPos    int
	nextErrorReport       time.Time
	coinPaymentsAPI       *payments.CoinPaymentsAPI
	mailTLS               *tls.Config
	durations             map[string]queryDurationsData
	images                map[string]string
	botNames              map[string]string
	lowPriorityMsg        chan outgoingPacket
	highPriorityMsg       chan outgoingPacket
	outgoingMsgResults    chan msgSendResult
}

type incomingPacket struct {
	message  tg.Update
	endpoint string
}

type outgoingPacket struct {
	message   baseChattable
	endpoint  string
	requested time.Time
}

type email struct {
	chatID   int64
	endpoint string
	email    string
}

type appliedKind int

const (
	invalidReferral appliedKind = iota
	followerExists
	referralApplied
)

const (
	messageSent                = 200
	messageBadRequest          = 400
	messageBlocked             = 403
	messageTooManyRequests     = 429
	messageUnknownError        = -1
	messageUnknownNetworkError = -2
	messageTimeout             = -3
	messageMigrate             = -4
	messageChatNotFound        = -5
)

type msgSendResult struct {
	priority  int
	timestamp int
	result    int
	endpoint  string
	chatID    int64
	delay     int
}

func newWorker() *worker {
	if len(os.Args) != 2 {
		panic("usage: siren <config>")
	}
	cfg := readConfig(os.Args[1])

	var err error
	var mailTLS *tls.Config

	if cfg.Mail != nil && cfg.Mail.Certificate != "" {
		mailTLS, err = loadTLS(cfg.Mail.Certificate, cfg.Mail.CertificateKey)
		checkErr(err)
	}

	var clients []*lib.Client
	for _, address := range cfg.SourceIPAddresses {
		clients = append(clients, lib.HTTPClientWithTimeoutAndAddress(cfg.TimeoutSeconds, address, cfg.EnableCookies))
	}

	telegramClient := lib.HTTPClientWithTimeoutAndAddress(cfg.TelegramTimeoutSeconds, "", false)
	bots := make(map[string]*tg.BotAPI)
	for n, p := range cfg.Endpoints {
		//noinspection GoNilness
		var bot *tg.BotAPI
		bot, err = tg.NewBotAPIWithClient(p.BotToken, tg.APIEndpoint, telegramClient.Client)
		checkErr(err)
		bots[n] = bot
	}
	db, err := sql.Open("sqlite3", cfg.DBPath)
	checkErr(err)
	tr, tpl := lib.LoadAllTranslations(trsByEndpoint(cfg))
	for _, t := range tpl {
		template.Must(t.New("affiliate_link").Parse(cfg.AffiliateLink))
	}
	w := &worker{
		bots:                 bots,
		db:                   db,
		cfg:                  cfg,
		clients:              clients,
		tr:                   tr,
		tpl:                  tpl,
		unsuccessfulRequests: make([]bool, cfg.errorDenominator),
		downloadErrors:       make([]bool, cfg.errorDenominator),
		mailTLS:              mailTLS,
		durations:            map[string]queryDurationsData{},
		images:               map[string]string{},
		botNames:             map[string]string{},
		lowPriorityMsg:       make(chan outgoingPacket, 10000),
		highPriorityMsg:      make(chan outgoingPacket, 10000),
		outgoingMsgResults:   make(chan msgSendResult),
	}

	if cp := cfg.CoinPayments; cp != nil {
		w.coinPaymentsAPI = payments.NewCoinPaymentsAPI(cp.PublicKey, cp.PrivateKey, "https://"+cp.IPNListenURL, cfg.TimeoutSeconds, cfg.Debug)
	}

	switch cfg.Website {
	case "test":
		w.checkModel = lib.CheckModelTest
		w.onlineModelsAPI = lib.TestOnlineAPI
		w.modelIDPreprocessing = lib.CanonicalModelID
	case "bongacams":
		w.checkModel = lib.CheckModelBongaCams
		w.onlineModelsAPI = lib.BongaCamsOnlineAPI
		w.modelIDPreprocessing = lib.CanonicalModelID
	case "chaturbate":
		w.checkModel = lib.CheckModelChaturbate
		w.onlineModelsAPI = lib.ChaturbateOnlineAPI
		w.modelIDPreprocessing = lib.CanonicalModelID
	case "stripchat":
		w.checkModel = lib.CheckModelStripchat
		w.onlineModelsAPI = lib.StripchatOnlineAPI
		w.modelIDPreprocessing = lib.CanonicalModelID
	case "livejasmin":
		w.checkModel = lib.CheckModelLiveJasmin
		w.onlineModelsAPI = lib.LiveJasminOnlineAPI
		w.modelIDPreprocessing = lib.CanonicalModelID
	case "camsoda":
		w.checkModel = lib.CheckModelCamSoda
		w.onlineModelsAPI = lib.CamSodaOnlineAPI
		w.modelIDPreprocessing = lib.CanonicalModelID
	case "flirt4free":
		w.checkModel = lib.CheckModelFlirt4Free
		w.onlineModelsAPI = lib.Flirt4FreeOnlineAPI
		w.modelIDPreprocessing = lib.Flirt4FreeCanonicalModelID
	default:
		panic("wrong website")
	}

	return w
}

func trsByEndpoint(cfg *config) map[string][]string {
	result := make(map[string][]string)
	for k, v := range cfg.Endpoints {
		result[k] = v.Translation
	}
	return result
}

func (w *worker) setWebhook() {
	for n, p := range w.cfg.Endpoints {
		linf("setting webhook for endpoint %s...", n)
		if p.WebhookDomain == "" {
			continue
		}
		if p.CertificatePath == "" {
			var _, err = w.bots[n].SetWebhook(tg.NewWebhook(path.Join(p.WebhookDomain, p.ListenPath)))
			checkErr(err)
		} else {
			var _, err = w.bots[n].SetWebhook(tg.NewWebhookWithCert(path.Join(p.WebhookDomain, p.ListenPath), p.CertificatePath))
			checkErr(err)
		}
		info, err := w.bots[n].GetWebhookInfo()
		checkErr(err)
		if info.LastErrorDate != 0 {
			linf("last webhook error time: %v", time.Unix(int64(info.LastErrorDate), 0))
		}
		if info.LastErrorMessage != "" {
			linf("last webhook error message: %s", info.LastErrorMessage)
		}
		linf("OK")
	}
}

func (w *worker) removeWebhook() {
	for n := range w.cfg.Endpoints {
		linf("removing webhook for endpoint %s...", n)
		_, err := w.bots[n].RemoveWebhook()
		checkErr(err)
		linf("OK")
	}
}

func (w *worker) initBotNames() {
	for n := range w.cfg.Endpoints {
		user, err := w.bots[n].GetMe()
		checkErr(err)
		linf("bot name for endpoint %s: %s", n, user.UserName)
		w.botNames[n] = user.UserName
	}
}

func (w *worker) setCommands() {
	for n := range w.cfg.Endpoints {
		text := templateToString(w.tpl[n], w.tr[n].RawCommands.Key, nil)
		lines := strings.Split(text, "\n")
		var commands []tg.BotCommand
		for _, l := range lines {
			pair := strings.SplitN(l, "-", 2)
			if len(pair) != 2 {
				checkErr(fmt.Errorf("unexpected command pair %q", l))
			}
			pair[0], pair[1] = strings.TrimSpace(pair[0]), strings.TrimSpace(pair[1])
			commands = append(commands, tg.BotCommand{Command: pair[0], Description: pair[1]})
			if w.cfg.Debug {
				ldbg("command %s - %s", pair[0], pair[1])
			}
		}
		linf("setting commands for endpoint %s...", n)
		err := w.bots[n].SetMyCommands(commands)
		checkErr(err)
		linf("OK")
	}
}

func (w *worker) incrementBlock(endpoint string, chatID int64) {
	w.mustExec(`
		insert into block (endpoint, chat_id, block) values (?,?,1)
		on conflict(chat_id, endpoint) do update set block=block+1`,
		endpoint,
		chatID)
}

func (w *worker) resetBlock(endpoint string, chatID int64) {
	w.mustExec("update block set block=0 where endpoint=? and chat_id=?", endpoint, chatID)
}

func (w *worker) sendText(
	queue chan outgoingPacket,
	endpoint string,
	chatID int64,
	notify bool,
	disablePreview bool,
	parse lib.ParseKind,
	text string,
) {
	msg := tg.NewMessage(chatID, text)
	msg.DisableNotification = !notify
	msg.DisableWebPagePreview = disablePreview
	switch parse {
	case lib.ParseHTML, lib.ParseMarkdown:
		msg.ParseMode = parse.String()
	}
	w.enqueueMessage(queue, endpoint, &messageConfig{msg})
}

func (w *worker) sendImage(
	queue chan outgoingPacket,
	endpoint string,
	chatID int64,
	notify bool,
	parse lib.ParseKind,
	text string,
	image []byte,
) {
	fileBytes := tg.FileBytes{Name: "preview", Bytes: image}
	msg := tg.NewPhotoUpload(chatID, fileBytes)
	msg.Caption = text
	msg.DisableNotification = !notify
	switch parse {
	case lib.ParseHTML, lib.ParseMarkdown:
		msg.ParseMode = parse.String()
	}
	w.enqueueMessage(queue, endpoint, &photoConfig{msg})
}

func (w *worker) enqueueMessage(queue chan outgoingPacket, endpoint string, msg baseChattable) {
	select {
	case queue <- outgoingPacket{endpoint: endpoint, message: msg, requested: time.Now()}:
	default:
		lerr("the outgoing message queue is full")
	}
}

func (w *worker) sender(queue chan outgoingPacket, priority int) {
	for packet := range queue {
		now := int(time.Now().Unix())
		delay := 0
	resend:
		for {
			result := w.sendMessageInternal(packet.endpoint, packet.message)
			delay = int(time.Since(packet.requested).Milliseconds())
			w.outgoingMsgResults <- msgSendResult{
				priority:  priority,
				timestamp: now,
				result:    result,
				endpoint:  packet.endpoint,
				chatID:    packet.message.baseChat().ChatID,
				delay:     delay,
			}
			switch result {
			case messageTimeout:
				time.Sleep(1000 * time.Millisecond)
				continue resend
			case messageUnknownNetworkError:
				time.Sleep(1000 * time.Millisecond)
				continue resend
			case messageTooManyRequests:
				time.Sleep(8000 * time.Millisecond)
				continue resend
			default:
				time.Sleep(60 * time.Millisecond)
				break resend
			}
		}
	}
}

func (w *worker) sendMessageInternal(endpoint string, msg baseChattable) int {
	chatID := msg.baseChat().ChatID
	if _, err := w.bots[endpoint].Send(msg); err != nil {
		switch err := err.(type) {
		case tg.Error:
			switch err.Code {
			case messageBlocked:
				if w.cfg.Debug {
					ldbg("cannot send a message, bot blocked")
				}
				return messageBlocked
			case messageTooManyRequests:
				if w.cfg.Debug {
					ldbg("cannot send a message, too many requests")
				}
				return messageTooManyRequests
			case messageBadRequest:
				if err.ResponseParameters.MigrateToChatID != 0 {
					if w.cfg.Debug {
						ldbg("cannot send a message, group migration")
					}
					return messageMigrate
				}
				if err.Message == "Bad Request: chat not found" {
					if w.cfg.Debug {
						ldbg("cannot send a message, chat not found")
					}
					return messageChatNotFound
				}
				lerr("cannot send a message, bad request, code: %d, error: %v", err.Code, err)
				return err.Code
			default:
				lerr("cannot send a message, unknown code: %d, error: %v", err.Code, err)
				return err.Code
			}
		case net.Error:
			if err.Timeout() {
				if w.cfg.Debug {
					ldbg("cannot send a message, timeout")
				}
				return messageTimeout
			}
			lerr("cannot send a message, unknown network error")
			return messageUnknownNetworkError
		default:
			lerr("unexpected error type while sending a message to %d, %v", chatID, err)
			return messageUnknownError
		}
	}
	return messageSent
}

func templateToString(t *template.Template, key string, data map[string]interface{}) string {
	buf := &bytes.Buffer{}
	err := t.ExecuteTemplate(buf, key, data)
	checkErr(err)
	return buf.String()
}

func (w *worker) sendTr(
	queue chan outgoingPacket,
	endpoint string,
	chatID int64,
	notify bool,
	translation *lib.Translation,
	data map[string]interface{},
) {
	tpl := w.tpl[endpoint]
	text := templateToString(tpl, translation.Key, data)
	w.sendText(queue, endpoint, chatID, notify, translation.DisablePreview, translation.Parse, text)
}

func (w *worker) sendTrImage(
	queue chan outgoingPacket,
	endpoint string,
	chatID int64,
	notify bool,
	translation *lib.Translation,
	data map[string]interface{},
	image []byte,
) {
	tpl := w.tpl[endpoint]
	text := templateToString(tpl, translation.Key, data)
	w.sendImage(queue, endpoint, chatID, notify, translation.Parse, text, image)
}

func (w *worker) createDatabase() {
	linf("creating database if needed...")
	for _, prelude := range w.cfg.SQLPrelude {
		w.mustExec(prelude)
	}
	w.mustExec(`create table if not exists schema_version (version integer);`)
	w.applyMigrations()
}

func (w *worker) initCache() {
	start := time.Now()
	w.siteStatuses = w.queryLastStatusChanges()
	w.siteOnline = w.getLastOnlineModels()
	w.ourOnline, w.specialModels = w.queryConfirmedModels()
	elapsed := time.Since(start)
	linf("cache initialized in %d ms", elapsed.Milliseconds())
}

func (w *worker) getLastOnlineModels() map[string]bool {
	res := map[string]bool{}
	for k, v := range w.siteStatuses {
		if v.status == lib.StatusOnline {
			res[k] = true
		}
	}
	return res
}

func (w *worker) lastSeenInfo(modelID string, now int) (begin int, end int, prevStatus lib.StatusKind) {
	query := w.mustQuery(`
		select timestamp, end, prev_status from (
			select
				*,
				lead(timestamp) over (order by timestamp) as end,
				lag(status) over (order by timestamp) as prev_status
			from status_changes
			where model_id=?)
		where status=?
		order by timestamp desc limit 1`,
		modelID,
		lib.StatusOnline)
	defer func() { checkErr(query.Close()) }()
	if !query.Next() {
		return 0, 0, lib.StatusUnknown
	}
	var maybeEnd *int
	var maybePrevStatus *lib.StatusKind
	checkErr(query.Scan(&begin, &maybeEnd, &maybePrevStatus))
	if maybeEnd == nil {
		zero := 0
		maybeEnd = &zero
	}
	if maybePrevStatus == nil {
		unknown := lib.StatusUnknown
		maybePrevStatus = &unknown
	}
	return begin, *maybeEnd, *maybePrevStatus
}

func (w *worker) confirmationSeconds(status lib.StatusKind) int {
	switch status {
	case lib.StatusOnline:
		return w.cfg.StatusConfirmationSeconds.Online
	case lib.StatusOffline:
		return w.cfg.StatusConfirmationSeconds.Offline
	case lib.StatusDenied:
		return w.cfg.StatusConfirmationSeconds.Denied
	case lib.StatusNotFound:
		return w.cfg.StatusConfirmationSeconds.NotFound
	default:
		return 0
	}
}

func (w *worker) updateStatus(insertStatusChangeStmt, updateLastStatusChangeStmt *sql.Stmt, next statusChange) {
	prev := w.siteStatuses[next.modelID]
	if next.status != prev.status {
		w.mustExecPrepared(insertStatusChange, insertStatusChangeStmt, next.modelID, next.status, next.timestamp)
		w.mustExecPrepared(updateLastStatusChange, updateLastStatusChangeStmt, next.modelID, next.status, next.timestamp)
		w.siteStatuses[next.modelID] = next
		if next.status == lib.StatusOnline {
			w.siteOnline[next.modelID] = true
		} else {
			delete(w.siteOnline, next.modelID)
		}
	}
}

func (w *worker) confirm(updateModelStatusStmt *sql.Stmt, now int) []string {
	all, _, _ := hashDiff(w.ourOnline, w.siteOnline)
	var confirmations []string
	for _, c := range all {
		statusChange := w.siteStatuses[c]
		confirmationSeconds := w.confirmationSeconds(statusChange.status)
		durationConfirmed := confirmationSeconds == 0 || (now-statusChange.timestamp >= confirmationSeconds)
		if durationConfirmed {
			if statusChange.status == lib.StatusOnline {
				w.ourOnline[statusChange.modelID] = true
			} else {
				delete(w.ourOnline, statusChange.modelID)
			}
			w.mustExecPrepared(updateModelStatus, updateModelStatusStmt, statusChange.modelID, statusChange.status)
			confirmations = append(confirmations, statusChange.modelID)
		}
	}
	return confirmations
}

func (w *worker) modelsToPoll() (models []string) {
	modelsQuery := w.mustQuery(`
		select distinct model_id from signals
		left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where block.block is null or block.block<?
		order by model_id`,
		w.cfg.BlockThreshold)
	defer func() { checkErr(modelsQuery.Close()) }()
	for modelsQuery.Next() {
		var modelID string
		checkErr(modelsQuery.Scan(&modelID))
		models = append(models, modelID)
	}
	return
}

func (w *worker) usersForModels() (users map[string][]user, endpoints map[string][]string) {
	users = map[string][]user{}
	endpoints = make(map[string][]string)
	chatsQuery := w.mustQuery(`
		select signals.model_id, signals.chat_id, signals.endpoint, users.offline_notifications
		from signals
		join users on users.chat_id=signals.chat_id`)
	defer func() { checkErr(chatsQuery.Close()) }()
	for chatsQuery.Next() {
		var modelID string
		var chatID int64
		var endpoint string
		var offlineNotifications bool
		checkErr(chatsQuery.Scan(&modelID, &chatID, &endpoint, &offlineNotifications))
		users[modelID] = append(users[modelID], user{chatID: chatID, offlineNotifications: offlineNotifications})
		endpoints[modelID] = append(endpoints[modelID], endpoint)
	}
	return
}

func (w *worker) chatsForModel(modelID string) (chats []int64, endpoints []string) {
	chatsQuery := w.mustQuery(`select chat_id, endpoint from signals where model_id=? order by chat_id`, modelID)
	defer func() { checkErr(chatsQuery.Close()) }()
	for chatsQuery.Next() {
		var chatID int64
		var endpoint string
		checkErr(chatsQuery.Scan(&chatID, &endpoint))
		chats = append(chats, chatID)
		endpoints = append(endpoints, endpoint)
	}
	return
}

func (w *worker) broadcastChats(endpoint string) (chats []int64) {
	chatsQuery := w.mustQuery(`select distinct chat_id from signals where endpoint=? order by chat_id`, endpoint)
	defer func() { checkErr(chatsQuery.Close()) }()
	for chatsQuery.Next() {
		var chatID int64
		checkErr(chatsQuery.Scan(&chatID))
		chats = append(chats, chatID)
	}
	return
}

func (w *worker) modelsForChat(endpoint string, chatID int64) []string {
	query := w.mustQuery(`
		select model_id
		from signals
		where chat_id=? and endpoint=?
		order by model_id`,
		chatID,
		endpoint)
	defer func() { checkErr(query.Close()) }()
	var models []string
	for query.Next() {
		var modelID string
		checkErr(query.Scan(&modelID))
		models = append(models, modelID)
	}
	return models
}

func (w *worker) statusesForChat(endpoint string, chatID int64) []model {
	statusesQuery := w.mustQuery(`
		select models.model_id, models.status
		from models
		join signals on signals.model_id=models.model_id
		where signals.chat_id=? and signals.endpoint=?
		order by models.model_id`,
		chatID,
		endpoint)
	defer func() { checkErr(statusesQuery.Close()) }()
	var statuses []model
	for statusesQuery.Next() {
		var modelID string
		var status lib.StatusKind
		checkErr(statusesQuery.Scan(&modelID, &status))
		statuses = append(statuses, model{modelID: modelID, status: status})
	}
	return statuses
}

func (w *worker) notifyOfStatuses(queue chan outgoingPacket, notifications []notification) {
	models := map[string]bool{}
	chats := map[int64]bool{}
	for _, n := range notifications {
		models[n.modelID] = true
		chats[n.chatID] = true
	}
	images := map[string][]byte{}
	users := map[int64]user{}
	for m := range models {
		if url := w.images[m]; url != "" {
			images[m] = w.download(url)
		}
	}
	for c := range chats {
		users[c] = w.mustUser(c)
	}
	for _, n := range notifications {
		var image []byte = nil
		if users[n.chatID].showImages {
			image = images[n.modelID]
		}
		w.notifyOfStatus(queue, n, image)
	}
}

func (w *worker) notifyOfStatus(queue chan outgoingPacket, n notification, image []byte) {
	if w.cfg.Debug {
		ldbg("notifying of status of the model %s", n.modelID)
	}
	data := tplData{"model": n.modelID, "time_diff": n.timeDiff}
	switch n.status {
	case lib.StatusOnline:
		if image == nil {
			w.sendTr(queue, n.endpoint, n.chatID, true, w.tr[n.endpoint].Online, data)
		} else {
			w.sendTrImage(queue, n.endpoint, n.chatID, true, w.tr[n.endpoint].Online, data, image)
		}
	case lib.StatusOffline:
		w.sendTr(queue, n.endpoint, n.chatID, false, w.tr[n.endpoint].Offline, data)
	case lib.StatusDenied:
		w.sendTr(queue, n.endpoint, n.chatID, false, w.tr[n.endpoint].Denied, data)
	}
	w.mustExec("update users set reports=reports+1 where chat_id=?", n.chatID)
}

func (w *worker) subscriptionExists(endpoint string, chatID int64, modelID string) bool {
	count := w.mustInt("select count(*) from signals where chat_id=? and model_id=? and endpoint=?", chatID, modelID, endpoint)
	return count != 0
}

func (w *worker) subscriptionsNumber(endpoint string, chatID int64) int {
	return w.mustInt("select count(*) from signals where chat_id=? and endpoint=?", chatID, endpoint)
}

func (w *worker) user(chatID int64) (user user, found bool) {
	found = w.maybeRecord("select chat_id, max_models, reports, blacklist, show_images, offline_notifications from users where chat_id=?",
		queryParams{chatID},
		record{&user.chatID, &user.maxModels, &user.reports, &user.blacklist, &user.showImages, &user.offlineNotifications})
	return
}

func (w *worker) mustUser(chatID int64) (user user) {
	user, found := w.user(chatID)
	if !found {
		checkErr(fmt.Errorf("user not found: %d", chatID))
	}
	return
}

func (w *worker) addUser(endpoint string, chatID int64) {
	w.mustExec(`insert or ignore into users (chat_id, max_models) values (?, ?)`, chatID, w.cfg.MaxModels)
	w.mustExec(`insert or ignore into emails (endpoint, chat_id, email) values (?, ?, ?)`, endpoint, chatID, uuid.New())
}

func (w *worker) showWeek(endpoint string, chatID int64, modelID string) {
	if modelID != "" {
		w.showWeekForModel(endpoint, chatID, modelID)
		return
	}
	models := w.modelsForChat(endpoint, chatID)
	for _, m := range models {
		w.showWeekForModel(endpoint, chatID, m)
	}
	if len(models) == 0 {
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].ZeroSubscriptions, nil)
	}

}

func (w *worker) showWeekForModel(endpoint string, chatID int64, modelID string) {
	modelID = w.modelIDPreprocessing(modelID)
	if !lib.ModelIDRegexp.MatchString(modelID) {
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].InvalidSymbols, tplData{"model": modelID})
		return
	}
	hours, start := w.week(modelID)
	w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].Week, tplData{
		"hours":   hours,
		"weekday": int(start.UTC().Weekday()),
		"model":   modelID,
	})
}

func (w *worker) addModel(endpoint string, chatID int64, modelID string, now int) bool {
	if modelID == "" {
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].SyntaxAdd, nil)
		return false
	}
	modelID = w.modelIDPreprocessing(modelID)
	if !lib.ModelIDRegexp.MatchString(modelID) {
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].InvalidSymbols, tplData{"model": modelID})
		return false
	}

	if w.subscriptionExists(endpoint, chatID, modelID) {
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].AlreadyAdded, tplData{"model": modelID})
		return false
	}
	subscriptionsNumber := w.subscriptionsNumber(endpoint, chatID)
	user := w.mustUser(chatID)
	if subscriptionsNumber >= user.maxModels {
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].NotEnoughSubscriptions, nil)
		w.subscriptionUsage(endpoint, chatID, true)
		return false
	}
	var confirmedStatus lib.StatusKind
	if w.ourOnline[modelID] {
		confirmedStatus = lib.StatusOnline
	} else if _, ok := w.siteStatuses[modelID]; ok {
		confirmedStatus = lib.StatusOffline
	} else {
		checkedStatus := w.checkModel(w.clients[0], modelID, w.cfg.Headers, w.cfg.Debug, w.cfg.SpecificConfig)
		if checkedStatus == lib.StatusUnknown || checkedStatus == lib.StatusNotFound {
			w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].AddError, tplData{"model": modelID})
			return false
		}
		confirmedStatus = lib.StatusOffline
	}
	w.mustExec("insert into signals (chat_id, model_id, endpoint) values (?,?,?)", chatID, modelID, endpoint)
	w.mustExec("insert or ignore into models (model_id, status) values (?,?)", modelID, confirmedStatus)
	subscriptionsNumber++
	w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].ModelAdded, tplData{"model": modelID})
	w.notifyOfStatuses(w.highPriorityMsg, []notification{{
		endpoint: endpoint,
		chatID:   chatID,
		modelID:  modelID,
		status:   confirmedStatus,
		timeDiff: w.modelTimeDiff(modelID, now)}})
	if subscriptionsNumber >= user.maxModels-w.cfg.HeavyUserRemainder {
		w.subscriptionUsage(endpoint, chatID, true)
	}
	return true
}

func (w *worker) subscriptionUsage(endpoint string, chatID int64, ad bool) {
	subscriptionsNumber := w.subscriptionsNumber(endpoint, chatID)
	user := w.mustUser(chatID)
	tr := w.tr[endpoint].SubscriptionUsage
	if ad {
		tr = w.tr[endpoint].SubscriptionUsageAd
	}
	w.sendTr(w.highPriorityMsg, endpoint, chatID, false, tr, tplData{
		"subscriptions_used":  subscriptionsNumber,
		"total_subscriptions": user.maxModels})
}

func (w *worker) wantMore(endpoint string, chatID int64) {
	w.showReferral(endpoint, chatID)

	if w.cfg.CoinPayments == nil || w.cfg.Mail == nil {
		return
	}

	tpl := w.tpl[endpoint]
	text := templateToString(tpl, w.tr[endpoint].BuyAd.Key, tplData{
		"price":                   w.cfg.CoinPayments.subscriptionPacketPrice,
		"number_of_subscriptions": w.cfg.CoinPayments.subscriptionPacketModelNumber,
	})
	buttonText := templateToString(tpl, w.tr[endpoint].BuyButton.Key, tplData{
		"number_of_subscriptions": w.cfg.CoinPayments.subscriptionPacketModelNumber,
	})

	buttons := [][]tg.InlineKeyboardButton{{tg.NewInlineKeyboardButtonData(buttonText, "buy")}}
	keyboard := tg.NewInlineKeyboardMarkup(buttons...)
	msg := tg.NewMessage(chatID, text)
	msg.ReplyMarkup = keyboard
	w.enqueueMessage(w.highPriorityMsg, endpoint, &messageConfig{msg})
}

func (w *worker) settings(endpoint string, chatID int64) {
	subscriptionsNumber := w.subscriptionsNumber(endpoint, chatID)
	user := w.mustUser(chatID)
	w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].Settings, tplData{
		"subscriptions_used":              subscriptionsNumber,
		"total_subscriptions":             user.maxModels,
		"show_images":                     user.showImages,
		"offline_notifications_supported": w.cfg.OfflineNotifications,
		"offline_notifications":           user.offlineNotifications,
	})
}

func (w *worker) enableImages(endpoint string, chatID int64, showImages bool) {
	w.mustExec("update users set show_images=? where chat_id=?", showImages, chatID)
	w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].OK, nil)
}

func (w *worker) enableOfflineNotifications(endpoint string, chatID int64, offlineNotifications bool) {
	w.mustExec("update users set offline_notifications=? where chat_id=?", offlineNotifications, chatID)
	w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].OK, nil)
}

func (w *worker) removeModel(endpoint string, chatID int64, modelID string) {
	if modelID == "" {
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].SyntaxRemove, nil)
		return
	}
	modelID = w.modelIDPreprocessing(modelID)
	if !lib.ModelIDRegexp.MatchString(modelID) {
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].InvalidSymbols, tplData{"model": modelID})
		return
	}
	if !w.subscriptionExists(endpoint, chatID, modelID) {
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].ModelNotInList, tplData{"model": modelID})
		return
	}
	w.mustExec("delete from signals where chat_id=? and model_id=? and endpoint=?", chatID, modelID, endpoint)
	w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].ModelRemoved, tplData{"model": modelID})
}

func (w *worker) sureRemoveAll(endpoint string, chatID int64) {
	w.mustExec("delete from signals where chat_id=? and endpoint=?", chatID, endpoint)
	w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].AllModelsRemoved, nil)
}

func (w *worker) buy(endpoint string, chatID int64) {
	var buttons [][]tg.InlineKeyboardButton
	for _, c := range w.cfg.CoinPayments.Currencies {
		buttons = append(buttons, []tg.InlineKeyboardButton{tg.NewInlineKeyboardButtonData(c, "buy_with "+c)})
	}

	user := w.mustUser(chatID)
	keyboard := tg.NewInlineKeyboardMarkup(buttons...)
	tpl := w.tpl[endpoint]
	text := templateToString(tpl, w.tr[endpoint].SelectCurrency.Key, tplData{
		"dollars":                 w.cfg.CoinPayments.subscriptionPacketPrice,
		"number_of_subscriptions": w.cfg.CoinPayments.subscriptionPacketModelNumber,
		"total_subscriptions":     user.maxModels + w.cfg.CoinPayments.subscriptionPacketModelNumber,
	})

	msg := tg.NewMessage(chatID, text)
	msg.ReplyMarkup = keyboard
	w.enqueueMessage(w.highPriorityMsg, endpoint, &messageConfig{msg})
}

func (w *worker) email(endpoint string, chatID int64) string {
	username := w.mustString("select email from emails where endpoint=? and chat_id=?", endpoint, chatID)
	return username + "@" + w.cfg.Mail.Host
}

func (w *worker) transaction(uuid string) (status payments.StatusKind, chatID int64, endpoint string, found bool) {
	found = w.maybeRecord("select status, chat_id, endpoint from transactions where local_id=?",
		queryParams{uuid},
		record{&status, &chatID, &endpoint})
	return
}

func (w *worker) buyWith(endpoint string, chatID int64, currency string) {
	found := false
	for _, c := range w.cfg.CoinPayments.Currencies {
		if currency == c {
			found = true
			break
		}
	}
	if !found {
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].UnknownCurrency, nil)
		return
	}

	email := w.email(endpoint, chatID)
	localID := uuid.New()
	transaction, err := w.coinPaymentsAPI.CreateTransaction(w.cfg.CoinPayments.subscriptionPacketPrice, currency, email, localID.String())
	if err != nil {
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].TryToBuyLater, nil)
		lerr("create transaction failed, %v", err)
		return
	}
	kind := "coinpayments"
	timestamp := int(time.Now().Unix())
	w.mustExec(`
		insert into transactions (
			status,
			kind,
			local_id,
			chat_id,
			remote_id,
			timeout,
			amount,
			address,
			dest_tag,
			status_url,
			checkout_url,
			timestamp,
			model_number,
			currency,
			endpoint)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		payments.StatusCreated,
		kind,
		localID,
		chatID,
		transaction.TXNID,
		transaction.Timeout,
		transaction.Amount,
		transaction.Address,
		transaction.DestTag,
		transaction.StatusURL,
		transaction.CheckoutURL,
		timestamp,
		w.cfg.CoinPayments.subscriptionPacketModelNumber,
		currency,
		endpoint)

	w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].PayThis, tplData{
		"price":    transaction.Amount,
		"currency": currency,
		"link":     transaction.CheckoutURL,
	})
}

// calcTimeDiff calculates time difference ignoring summer time and leap seconds
func calcTimeDiff(t1, t2 time.Time) timeDiff {
	var diff timeDiff
	day := int64(time.Hour * 24)
	d := t2.Sub(t1).Nanoseconds()
	diff.Days = int(d / day)
	d -= int64(diff.Days) * day
	diff.Hours = int(d / int64(time.Hour))
	d -= int64(diff.Hours) * int64(time.Hour)
	diff.Minutes = int(d / int64(time.Minute))
	d -= int64(diff.Minutes) * int64(time.Minute)
	diff.Seconds = int(d / int64(time.Second))
	d -= int64(diff.Seconds) * int64(time.Second)
	diff.Nanoseconds = int(d)
	return diff
}

func (w *worker) listModels(endpoint string, chatID int64, now int) {
	type data struct {
		Model    string
		TimeDiff *timeDiff
	}
	statuses := w.statusesForChat(endpoint, chatID)
	var online, offline, denied []data
	for _, s := range statuses {
		data := data{
			Model:    s.modelID,
			TimeDiff: w.modelTimeDiff(s.modelID, now),
		}
		switch s.status {
		case lib.StatusOnline:
			online = append(online, data)
		case lib.StatusDenied:
			denied = append(denied, data)
		default:
			offline = append(offline, data)
		}
	}
	w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].List, tplData{"online": online, "offline": offline, "denied": denied})
}

func (w *worker) modelTimeDiff(modelID string, now int) *timeDiff {
	begin, end, prevStatus := w.lastSeenInfo(modelID, now)
	if end != 0 {
		timeDiff := calcTimeDiff(time.Unix(int64(end), 0), time.Unix(int64(now), 0))
		return &timeDiff
	}
	if begin != 0 && prevStatus != lib.StatusUnknown {
		timeDiff := calcTimeDiff(time.Unix(int64(begin), 0), time.Unix(int64(now), 0))
		return &timeDiff
	}
	return nil
}

func (w *worker) downloadSuccess(success bool) {
	w.downloadErrors[w.downloadResultsPos] = !success
	w.downloadResultsPos = (w.downloadResultsPos + 1) % w.cfg.errorDenominator
}

func (w *worker) download(url string) []byte {
	resp, err := w.clients[0].Client.Get(url)
	if err != nil {
		if w.cfg.Debug {
			ldbg("cannot make image query")
		}
		w.downloadSuccess(false)
		return nil
	}
	defer func() { checkErr(resp.Body.Close()) }()
	if resp.StatusCode != 200 {
		if w.cfg.Debug {
			ldbg("cannot download image data")
		}
		w.downloadSuccess(false)
		return nil
	}
	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(resp.Body)
	if err != nil {
		if w.cfg.Debug {
			ldbg("cannot read image")
		}
		w.downloadSuccess(false)
		return nil
	}
	data := buf.Bytes()
	_, _, err = image.Decode(bytes.NewReader(data))
	if err != nil {
		if w.cfg.Debug {
			ldbg("cannot decode image")
		}
		w.downloadSuccess(false)
		return nil
	}
	w.downloadSuccess(true)
	return data
}

func (w *worker) listOnlineModels(endpoint string, chatID int64, now int) {
	statuses := w.statusesForChat(endpoint, chatID)
	var online []model
	for _, s := range statuses {
		if s.status == lib.StatusOnline {
			online = append(online, s)
		}
	}
	if len(online) > w.cfg.MaxSubscriptionsForPics && chatID < -1 {
		data := tplData{"max_subs": w.cfg.MaxSubscriptionsForPics}
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].TooManySubscriptionsForPics, data)
		return
	}
	for _, s := range online {
		imageURL := w.images[s.modelID]
		var image []byte
		if imageURL != "" {
			image = w.download(imageURL)
		}
		data := tplData{"model": s.modelID, "time_diff": w.modelTimeDiff(s.modelID, now)}
		if image == nil {
			w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].Online, data)
		} else {
			w.sendTrImage(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].Online, data, image)
		}
	}
	if len(online) == 0 {
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].NoOnlineModels, nil)
	}
}

func (w *worker) week(modelID string) ([]bool, time.Time) {
	now := time.Now()
	nowTimestamp := int(now.Unix())
	today := now.Truncate(24 * time.Hour)
	start := today.Add(-6 * 24 * time.Hour)
	weekTimestamp := int(start.Unix())
	query := w.mustQuery(`
		select status, timestamp, prev_status, prev_timestamp
		from(
			select
				*,
				lag(status) over (order by timestamp) as prev_status,
				lag(timestamp) over (order by timestamp) as prev_timestamp
			from status_changes
			where model_id=?)
		where timestamp>=?
		order by timestamp`,
		modelID,
		weekTimestamp)
	var changes []statusChange
	first := true
	for query.Next() {
		var change statusChange
		var firstStatus *lib.StatusKind
		var firstTimestamp *int
		checkErr(query.Scan(&change.status, &change.timestamp, &firstStatus, &firstTimestamp))
		if first && firstStatus != nil && firstTimestamp != nil {
			changes = append(changes, statusChange{status: *firstStatus, timestamp: *firstTimestamp})
			first = false
		}
		changes = append(changes, change)
	}

	changes = append(changes, statusChange{timestamp: nowTimestamp})
	hours := make([]bool, (nowTimestamp-weekTimestamp+3599)/3600)
	for i, c := range changes[:len(changes)-1] {
		if c.status == lib.StatusOnline {
			begin := (c.timestamp - weekTimestamp) / 3600
			if begin < 0 {
				begin = 0
			}
			end := (changes[i+1].timestamp - weekTimestamp + 3599) / 3600
			for j := begin; j < end; j++ {
				hours[j] = true
			}
		}
	}
	return hours, start
}

func (w *worker) feedback(endpoint string, chatID int64, text string) {
	if text == "" {
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].SyntaxFeedback, nil)
		return
	}
	w.mustExec("insert into feedback (endpoint, chat_id, text) values (?, ?, ?)", endpoint, chatID, text)
	w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].Feedback, nil)
	user := w.mustUser(chatID)
	if !user.blacklist {
		w.sendText(w.highPriorityMsg, endpoint, w.cfg.AdminID, true, true, lib.ParseRaw, fmt.Sprintf("Feedback from %d: %s", chatID, text))
	}
}

func (w *worker) setLimit(chatID int64, maxModels int) {
	w.mustExec(`
		insert into users (chat_id, max_models) values (?, ?)
		on conflict(chat_id) do update set max_models=excluded.max_models`,
		chatID,
		maxModels)
}

func (w *worker) unsuccessfulRequestsCount() int {
	var count = 0
	for _, s := range w.unsuccessfulRequests {
		if s {
			count++
		}
	}
	return count
}

func (w *worker) downloadErrorsCount() int {
	var count = 0
	for _, s := range w.downloadErrors {
		if s {
			count++
		}
	}
	return count
}

func (w *worker) userReferralsCount() int {
	return w.mustInt("select coalesce(sum(referred_users), 0) from referrals")
}

func (w *worker) modelReferralsCount() int {
	return w.mustInt("select coalesce(sum(referred_users), 0) from models")
}

func (w *worker) reports() int {
	return w.mustInt("select coalesce(sum(reports), 0) from users")
}

func (w *worker) interactions(endpoint string) map[int]int {
	timestamp := time.Now().Add(time.Hour * -24).Unix()
	query := w.mustQuery("select result, count(*) from interactions where endpoint=? and timestamp>? group by result", endpoint, timestamp)
	defer func() { checkErr(query.Close()) }()
	results := map[int]int{}
	for query.Next() {
		var result int
		var count int
		checkErr(query.Scan(&result, &count))
		results[result] = count
	}
	return results
}

func (w *worker) usersCount(endpoint string) int {
	return w.mustInt("select count(distinct chat_id) from signals where endpoint=?", endpoint)
}

func (w *worker) groupsCount(endpoint string) int {
	return w.mustInt("select count(distinct chat_id) from signals where endpoint=? and chat_id < 0", endpoint)
}

func (w *worker) activeUsersOnEndpointCount(endpoint string) int {
	return w.mustInt(`
		select count(distinct signals.chat_id)
		from signals
		left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where (block.block is null or block.block = 0) and signals.endpoint=?`,
		endpoint)
}

func (w *worker) activeUsersTotalCount() int {
	return w.mustInt(`
		select count(distinct signals.chat_id)
		from signals
		left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where (block.block is null or block.block = 0)`)
}

func (w *worker) modelsCount(endpoint string) int {
	return w.mustInt("select count(distinct model_id) from signals where endpoint=?", endpoint)
}

func (w *worker) modelsToPollOnEndpointCount(endpoint string) int {
	return w.mustInt(`
		select count(distinct signals.model_id)
		from signals
		left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where (block.block is null or block.block < ?) and signals.endpoint=?`,
		w.cfg.BlockThreshold,
		endpoint)
}

func (w *worker) modelsToPollTotalCount() int {
	return w.mustInt(`
		select count(distinct signals.model_id)
		from signals
		left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
		where (block.block is null or block.block < ?)`,
		w.cfg.BlockThreshold)
}

func (w *worker) statusChangesCount() int {
	return w.mustInt("select max(_rowid_) from status_changes")
}

func (w *worker) heavyUsersCount(endpoint string) int {
	return w.mustInt(`
		select count(*) from (
			select 1 from signals
			left join block on signals.chat_id=block.chat_id and signals.endpoint=block.endpoint
			where (block.block is null or block.block = 0) and signals.endpoint=?
			group by signals.chat_id
			having count(*) >= ?);`,
		endpoint,
		w.cfg.MaxModels-w.cfg.HeavyUserRemainder)
}

func (w *worker) transactionsOnEndpoint(endpoint string) int {
	return w.mustInt("select count(*) from transactions where endpoint=?", endpoint)
}

func (w *worker) transactionsOnEndpointFinished(endpoint string) int {
	return w.mustInt("select count(*) from transactions where endpoint=? and status=?", endpoint, payments.StatusFinished)
}

func (w *worker) statStrings(endpoint string) []string {
	stat := w.getStat(endpoint)
	return []string{
		fmt.Sprintf("Users: %d", stat.UsersCount),
		fmt.Sprintf("Groups: %d", stat.GroupsCount),
		fmt.Sprintf("Active users: %d", stat.ActiveUsersOnEndpointCount),
		fmt.Sprintf("Heavy: %d", stat.HeavyUsersCount),
		fmt.Sprintf("Models: %d", stat.ModelsCount),
		fmt.Sprintf("Models to poll: %d", stat.ModelsToPollOnEndpointCount),
		fmt.Sprintf("Models to poll total: %d", stat.ModelsToPollTotalCount),
		fmt.Sprintf("Models online: %d", stat.OnlineModelsCount),
		fmt.Sprintf("Status changes: %d", stat.StatusChangesCount),
		fmt.Sprintf("Queries duration: %d ms", stat.QueriesDurationMilliseconds),
		fmt.Sprintf("Updates duration: %d ms", stat.UpdatesDurationMilliseconds),
		fmt.Sprintf("Error rate: %d/%d", stat.ErrorRate[0], stat.ErrorRate[1]),
		fmt.Sprintf("Memory usage: %d KiB", stat.Rss),
		fmt.Sprintf("Transactions: %d/%d", stat.TransactionsOnEndpointFinished, stat.TransactionsOnEndpointCount),
		fmt.Sprintf("Reports: %d", stat.ReportsCount),
		fmt.Sprintf("User referrals: %d", stat.UserReferralsCount),
		fmt.Sprintf("Model referrals: %d", stat.ModelReferralsCount),
		fmt.Sprintf("Changes in period: %d", stat.ChangesInPeriod),
		fmt.Sprintf("Confirmed changes in period: %d", stat.ConfirmedChangesInPeriod),
	}
}

func (w *worker) stat(endpoint string) {
	w.sendText(w.highPriorityMsg, endpoint, w.cfg.AdminID, true, true, lib.ParseRaw, strings.Join(w.statStrings(endpoint), "\n"))
}

func (w *worker) performanceStat(endpoint string) {
	durations := w.durations
	var queries []string
	for x := range durations {
		queries = append(queries, x)
	}
	sort.SliceStable(queries, func(i, j int) bool {
		return durations[queries[i]].total() > durations[queries[j]].total()
	})
	for _, x := range queries {
		lines := []string{
			fmt.Sprintf("<b>Desc</b>: %s", html.EscapeString(x)),
			fmt.Sprintf("<b>Total</b>: %d", int(durations[x].avg*float64(durations[x].count)*1000.)),
			fmt.Sprintf("<b>Avg</b>: %d", int(durations[x].avg*1000.)),
			fmt.Sprintf("<b>Count</b>: %d", durations[x].count),
		}
		entry := strings.Join(lines, "\n")
		w.sendText(w.highPriorityMsg, endpoint, w.cfg.AdminID, false, true, lib.ParseHTML, entry)
	}
}

func (w *worker) broadcast(endpoint string, text string) {
	if text == "" {
		return
	}
	if w.cfg.Debug {
		ldbg("broadcasting")
	}
	chats := w.broadcastChats(endpoint)
	for _, chatID := range chats {
		w.sendText(w.lowPriorityMsg, endpoint, chatID, true, false, lib.ParseRaw, text)
	}
	w.sendText(w.lowPriorityMsg, endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "OK")
}

func (w *worker) direct(endpoint string, arguments string) {
	parts := strings.SplitN(arguments, " ", 2)
	if len(parts) < 2 {
		w.sendText(w.highPriorityMsg, endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "usage: /direct chatID text")
		return
	}
	whom, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		w.sendText(w.highPriorityMsg, endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "first argument is invalid")
		return
	}
	text := parts[1]
	if text == "" {
		return
	}
	w.sendText(w.highPriorityMsg, endpoint, whom, true, false, lib.ParseRaw, text)
	w.sendText(w.highPriorityMsg, endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "OK")
}

func (w *worker) blacklist(endpoint string, arguments string) {
	whom, err := strconv.ParseInt(arguments, 10, 64)
	if err != nil {
		w.sendText(w.highPriorityMsg, endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "first argument is invalid")
		return
	}
	w.mustExec("update users set blacklist=1 where chat_id=?", whom)
	w.sendText(w.highPriorityMsg, endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "OK")
}

func (w *worker) addSpecialModel(endpoint string, modelID string) {
	modelID = w.modelIDPreprocessing(modelID)
	if !lib.ModelIDRegexp.MatchString(modelID) {
		w.sendText(w.highPriorityMsg, endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "model ID is invalid")
		return
	}
	w.mustExec(`
		insert into models (model_id, special) values (?,?)
		on conflict(model_id) do update set special=excluded.special`,
		modelID,
		true)
	w.specialModels[modelID] = true
	w.sendText(w.highPriorityMsg, endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "OK")
}

func (w *worker) serveEndpoints() {
	go func() {
		err := http.ListenAndServe(w.cfg.ListenAddress, nil)
		checkErr(err)
	}()
}

func (w *worker) logConfig() {
	cfgString, err := json.MarshalIndent(w.cfg, "", "    ")
	checkErr(err)
	linf("config: " + string(cfgString))
}

func (w *worker) myEmail(endpoint string) {
	email := w.email(endpoint, w.cfg.AdminID)
	w.sendText(w.highPriorityMsg, endpoint, w.cfg.AdminID, true, true, lib.ParseRaw, email)
}

func (w *worker) processAdminMessage(endpoint string, chatID int64, command, arguments string) bool {
	switch command {
	case "stat":
		w.stat(endpoint)
		return true
	case "performance":
		w.performanceStat(endpoint)
		return true
	case "email":
		w.myEmail(endpoint)
		return true
	case "broadcast":
		w.broadcast(endpoint, arguments)
		return true
	case "direct":
		w.direct(endpoint, arguments)
		return true
	case "blacklist":
		w.blacklist(endpoint, arguments)
		return true
	case "special":
		w.addSpecialModel(endpoint, arguments)
		return true
	case "set_max_models":
		parts := strings.Fields(arguments)
		if len(parts) != 2 {
			w.sendText(w.highPriorityMsg, endpoint, chatID, false, true, lib.ParseRaw, "expecting two arguments")
			return true
		}
		who, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			w.sendText(w.highPriorityMsg, endpoint, chatID, false, true, lib.ParseRaw, "first argument is invalid")
			return true
		}
		maxModels, err := strconv.Atoi(parts[1])
		if err != nil {
			w.sendText(w.highPriorityMsg, endpoint, chatID, false, true, lib.ParseRaw, "second argument is invalid")
			return true
		}
		w.setLimit(who, maxModels)
		w.sendText(w.highPriorityMsg, endpoint, chatID, false, true, lib.ParseRaw, "OK")
		return true
	}
	return false
}

func splitAddress(a string) (string, string) {
	a = strings.ToLower(a)
	parts := strings.Split(a, "@")
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func (w *worker) recordForEmail(username string) *email {
	modelsQuery := w.mustQuery(`select chat_id, endpoint from emails where email=?`, username)
	defer func() { checkErr(modelsQuery.Close()) }()
	if modelsQuery.Next() {
		email := email{email: username}
		checkErr(modelsQuery.Scan(&email.chatID, &email.endpoint))
		return &email
	}
	return nil
}

func (w *worker) mailReceived(e *env) {
	emails := make(map[email]bool)
	for _, r := range e.rcpts {
		username, host := splitAddress(r.Email())
		if host != w.cfg.Mail.Host {
			continue
		}
		email := w.recordForEmail(username)
		if email != nil {
			emails[*email] = true
		}
	}

	for email := range emails {
		w.sendTr(w.lowPriorityMsg, email.endpoint, email.chatID, true, w.tr[email.endpoint].MailReceived, tplData{
			"subject": e.mime.GetHeader("Subject"),
			"from":    e.mime.GetHeader("From"),
			"text":    e.mime.Text})
		for _, inline := range e.mime.Inlines {
			b := tg.FileBytes{Name: inline.FileName, Bytes: inline.Content}
			switch {
			case strings.HasPrefix(inline.ContentType, "image/"):
				msg := tg.NewPhotoUpload(email.chatID, b)
				w.enqueueMessage(w.lowPriorityMsg, email.endpoint, &photoConfig{msg})
			default:
				msg := tg.NewDocumentUpload(email.chatID, b)
				w.enqueueMessage(w.lowPriorityMsg, email.endpoint, &documentConfig{msg})
			}
		}
		for _, inline := range e.mime.Attachments {
			b := tg.FileBytes{Name: inline.FileName, Bytes: inline.Content}
			msg := tg.NewDocumentUpload(email.chatID, b)
			w.enqueueMessage(w.lowPriorityMsg, email.endpoint, &documentConfig{msg})
		}
	}
}

func envelopeFactory(ch chan *env) func(smtpd.Connection, smtpd.MailAddress, *int) (smtpd.Envelope, error) {
	return func(c smtpd.Connection, from smtpd.MailAddress, size *int) (smtpd.Envelope, error) {
		return &env{BasicEnvelope: &smtpd.BasicEnvelope{}, from: from, ch: ch}, nil
	}
}

//noinspection SpellCheckingInspection
const letterBytes = "abcdefghijklmnopqrstuvwxyz"

func randString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}

func (w *worker) newRandReferralID() (id string) {
	for {
		id = randString(5)
		if w.mustInt("select count(*) from referrals where referral_id=?", id) == 0 {
			break
		}
	}
	return
}

func (w *worker) refer(followerChatID int64, referrer string) (applied appliedKind) {
	referrerChatID := w.chatForReferralID(referrer)
	if referrerChatID == nil {
		return invalidReferral
	}
	if _, exists := w.user(followerChatID); exists {
		return followerExists
	}
	w.mustExec("insert into users (chat_id, max_models) values (?, ?)", followerChatID, w.cfg.MaxModels+w.cfg.FollowerBonus)
	w.mustExec(`
		insert into users (chat_id, max_models) values (?, ?)
		on conflict(chat_id) do update set max_models=max_models+?`,
		*referrerChatID,
		w.cfg.MaxModels+w.cfg.ReferralBonus,
		w.cfg.ReferralBonus)
	w.mustExec("update referrals set referred_users=referred_users+1 where chat_id=?", referrerChatID)
	return referralApplied
}

func (w *worker) showReferral(endpoint string, chatID int64) {
	referralID := w.referralID(chatID)
	if referralID == nil {
		temp := w.newRandReferralID()
		referralID = &temp
		w.mustExec("insert into referrals (chat_id, referral_id) values (?, ?)", chatID, *referralID)
	}
	referralLink := fmt.Sprintf("https://t.me/%s?start=%s", w.botNames[endpoint], *referralID)
	subscriptionsNumber := w.subscriptionsNumber(endpoint, chatID)
	user := w.mustUser(chatID)
	w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].ReferralLink, tplData{
		"link":                referralLink,
		"referral_bonus":      w.cfg.ReferralBonus,
		"follower_bonus":      w.cfg.FollowerBonus,
		"subscriptions_used":  subscriptionsNumber,
		"total_subscriptions": user.maxModels,
	})
}

func (w *worker) start(endpoint string, chatID int64, referrer string, now int) {
	modelID := ""
	switch {
	case strings.HasPrefix(referrer, "m-"):
		modelID = referrer[2:]
		referrer = ""
	case referrer != "":
		referralID := w.referralID(chatID)
		if referralID != nil && *referralID == referrer {
			w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].OwnReferralLinkHit, nil)
			return
		}
	}
	w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].Help, tplData{
		"website_link": w.cfg.WebsiteLink,
	})
	if chatID > 0 && referrer != "" {
		applied := w.refer(chatID, referrer)
		switch applied {
		case referralApplied:
			w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].ReferralApplied, nil)
		case invalidReferral:
			w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].InvalidReferralLink, nil)
		case followerExists:
			w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].FollowerExists, nil)
		}
	}
	w.addUser(endpoint, chatID)
	if modelID != "" {
		if w.addModel(endpoint, chatID, modelID, now) {
			w.mustExec("insert or ignore into models (model_id) values (?)", modelID)
			w.mustExec("update models set referred_users=referred_users+1 where model_id=?", modelID)
		}
	}
}

func (w *worker) processIncomingCommand(endpoint string, chatID int64, command, arguments string, now int) {
	w.resetBlock(endpoint, chatID)
	command = strings.ToLower(command)
	if command != "start" {
		w.addUser(endpoint, chatID)
	}
	linf("chat: %d, command: %s %s", chatID, command, arguments)

	if chatID == w.cfg.AdminID && w.processAdminMessage(endpoint, chatID, command, arguments) {
		return
	}

	unknown := func() { w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].UnknownCommand, nil) }

	switch command {
	case "add":
		arguments = strings.Replace(arguments, "—", "--", -1)
		_ = w.addModel(endpoint, chatID, arguments, now)
	case "remove":
		arguments = strings.Replace(arguments, "—", "--", -1)
		w.removeModel(endpoint, chatID, arguments)
	case "list":
		w.listModels(endpoint, chatID, now)
	case "pics", "online":
		w.listOnlineModels(endpoint, chatID, now)
	case "start", "help":
		w.start(endpoint, chatID, arguments, now)
	case "faq":
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].FAQ, tplData{
			"dollars":                 w.cfg.CoinPayments.subscriptionPacketPrice,
			"number_of_subscriptions": w.cfg.CoinPayments.subscriptionPacketModelNumber,
			"max_models":              w.cfg.MaxModels,
		})
	case "feedback":
		w.feedback(endpoint, chatID, arguments)
	case "social":
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].Social, nil)
	case "version":
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].Version, tplData{"version": version})
	case "remove_all", "stop":
		w.sendTr(w.highPriorityMsg, endpoint, chatID, false, w.tr[endpoint].RemoveAll, nil)
	case "sure_remove_all":
		w.sureRemoveAll(endpoint, chatID)
	case "want_more":
		w.wantMore(endpoint, chatID)
	case "settings":
		w.settings(endpoint, chatID)
	case "enable_images":
		w.enableImages(endpoint, chatID, true)
	case "disable_images":
		w.enableImages(endpoint, chatID, false)
	case "enable_offline_notifications":
		w.enableOfflineNotifications(endpoint, chatID, true)
	case "disable_offline_notifications":
		w.enableOfflineNotifications(endpoint, chatID, false)
	case "buy":
		if w.cfg.CoinPayments == nil || w.cfg.Mail == nil {
			unknown()
			return
		}
		w.buy(endpoint, chatID)
	case "buy_with":
		if w.cfg.CoinPayments == nil || w.cfg.Mail == nil {
			unknown()
			return
		}
		w.buyWith(endpoint, chatID, arguments)
	case "referral":
		w.showReferral(endpoint, chatID)
	case "week":
		if !w.cfg.EnableWeek {
			unknown()
			return
		}
		w.showWeek(endpoint, chatID, arguments)
	default:
		unknown()
	}
}

func (w *worker) processPeriodic(statusRequests chan lib.StatusRequest) {
	unsuccessfulRequestsCount := w.unsuccessfulRequestsCount()
	now := time.Now()
	if w.nextErrorReport.Before(now) && unsuccessfulRequestsCount > w.cfg.errorThreshold {
		text := fmt.Sprintf("Dangerous error rate reached: %d/%d", unsuccessfulRequestsCount, w.cfg.errorDenominator)
		w.sendText(w.highPriorityMsg, w.cfg.AdminEndpoint, w.cfg.AdminID, true, true, lib.ParseRaw, text)
		w.nextErrorReport = now.Add(time.Minute * time.Duration(w.cfg.ErrorReportingPeriodMinutes))
	}

	select {
	case statusRequests <- lib.StatusRequest{SpecialModels: w.specialModels}:
	default:
		linf("the queue is full")
	}
}

func (w *worker) queryLastStatusChanges() map[string]statusChange {
	query := w.mustQuery(`select model_id, status, timestamp from last_status_changes`)
	defer func() { checkErr(query.Close()) }()
	statusChanges := map[string]statusChange{}
	for query.Next() {
		var statusChange statusChange
		checkErr(query.Scan(&statusChange.modelID, &statusChange.status, &statusChange.timestamp))
		statusChanges[statusChange.modelID] = statusChange
	}
	return statusChanges
}

func (w *worker) queryConfirmedModels() (map[string]bool, map[string]bool) {
	query := w.mustQuery("select model_id, status, special from models")
	defer func() { checkErr(query.Close()) }()
	statuses := map[string]bool{}
	specialModels := map[string]bool{}
	for query.Next() {
		var modelID string
		var status lib.StatusKind
		var special bool
		checkErr(query.Scan(&modelID, &status, &special))
		if status == lib.StatusOnline {
			statuses[modelID] = true
		}
		if special {
			specialModels[modelID] = true
		}
	}
	return statuses, specialModels
}

func hashDiff(before, after map[string]bool) (all, added, removed []string) {
	for k := range after {
		if _, ok := before[k]; !ok {
			all = append(all, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			all = append(all, k)
		}
	}
	return
}

func (w *worker) updateImages(onlineModels []lib.OnlineModel) {
	for _, u := range onlineModels {
		if u.Image != "" {
			w.images[u.ModelID] = u.Image
		} else {
			delete(w.images, u.ModelID)
		}
	}
}

func (w *worker) processStatusUpdates(
	onlineModels []lib.OnlineModel,
	now int,
) (
	changesCount int,
	confirmedChangesCount int,
	notifications []notification,
	elapsed time.Duration,
) {
	start := time.Now()
	w.updateImages(onlineModels)
	usersForModels, endpointsForModels := w.usersForModels()
	tx, err := w.db.Begin()
	checkErr(err)

	insertStatusChangeStmt, err := tx.Prepare(insertStatusChange)
	checkErr(err)
	updateLastStatusChangeStmt, err := tx.Prepare(updateLastStatusChange)
	checkErr(err)
	updateModelStatusStmt, err := tx.Prepare(updateModelStatus)
	checkErr(err)

	next := map[string]bool{}
	hashDone := w.measure("algo: hash diff")
	for _, u := range onlineModels {
		next[u.ModelID] = true
	}
	all, _, _ := hashDiff(w.siteOnline, next)
	hashDone()

	changesCount = len(all)

	statusDone := w.measure("db: status updates")
	for _, u := range all {
		status := lib.StatusOffline
		if _, ok := next[u]; ok {
			status = lib.StatusOnline
		}
		statusChange := statusChange{modelID: u, status: status, timestamp: now}
		w.updateStatus(insertStatusChangeStmt, updateLastStatusChangeStmt, statusChange)
	}
	statusDone()

	confirmationsDone := w.measure("db: confirmations")
	confirmations := w.confirm(updateModelStatusStmt, now)
	confirmationsDone()

	if w.cfg.Debug {
		ldbg("confirmed online models: %d", len(w.ourOnline))
	}

	for _, c := range confirmations {
		users := usersForModels[c]
		endpoints := endpointsForModels[c]
		for i, user := range users {
			status := w.siteStatuses[c].status
			if (w.cfg.OfflineNotifications && user.offlineNotifications) || status != lib.StatusOffline {
				notifications = append(notifications, notification{
					endpoint: endpoints[i],
					chatID:   user.chatID,
					modelID:  c,
					status:   status,
				})
			}
		}
	}

	confirmedChangesCount = len(confirmations)

	defer w.measure("db: status updates commit")()
	checkErr(insertStatusChangeStmt.Close())
	checkErr(updateLastStatusChangeStmt.Close())
	checkErr(updateModelStatusStmt.Close())
	checkErr(tx.Commit())
	elapsed = time.Since(start)
	return
}

func (w *worker) processTGUpdate(p incomingPacket) {
	now := int(time.Now().Unix())
	u := p.message
	if u.Message != nil && u.Message.Chat != nil {
		if newMembers := u.Message.NewChatMembers; newMembers != nil && len(*newMembers) > 0 {
			ourIDs := w.ourIDs()
		addedToChat:
			for _, m := range *newMembers {
				for _, ourID := range ourIDs {
					if int64(m.ID) == ourID {
						w.sendTr(w.highPriorityMsg, p.endpoint, u.Message.Chat.ID, false, w.tr[p.endpoint].Help, tplData{
							"website_link": w.cfg.WebsiteLink,
						})
						break addedToChat
					}
				}
			}
		} else if u.Message.IsCommand() {
			w.processIncomingCommand(p.endpoint, u.Message.Chat.ID, u.Message.Command(), strings.TrimSpace(u.Message.CommandArguments()), now)
		} else {
			if u.Message.Text == "" {
				return
			}
			parts := strings.SplitN(u.Message.Text, " ", 2)
			if parts[0] == "" {
				return
			}
			for len(parts) < 2 {
				parts = append(parts, "")
			}
			w.processIncomingCommand(p.endpoint, u.Message.Chat.ID, parts[0], strings.TrimSpace(parts[1]), now)
		}
	}
	if u.CallbackQuery != nil {
		callback := tg.CallbackConfig{CallbackQueryID: u.CallbackQuery.ID}
		_, err := w.bots[p.endpoint].AnswerCallbackQuery(callback)
		if err != nil {
			lerr("cannot answer callback query, %v", err)
		}
		data := strings.SplitN(u.CallbackQuery.Data, " ", 2)
		chatID := int64(u.CallbackQuery.From.ID)
		if len(data) < 2 {
			data = append(data, "")
		}
		w.processIncomingCommand(p.endpoint, chatID, data[0], data[1], now)
	}
}

func getRss() (int64, error) {
	buf, err := ioutil.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}

	fields := strings.Split(string(buf), " ")
	if len(fields) < 2 {
		return 0, errors.New("cannot parse statm")
	}

	rss, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, err
	}

	return rss * int64(os.Getpagesize()), err
}

func (w *worker) getStat(endpoint string) statistics {
	rss, err := getRss()
	checkErr(err)
	var rusage syscall.Rusage
	checkErr(syscall.Getrusage(syscall.RUSAGE_SELF, &rusage))

	return statistics{
		UsersCount:                     w.usersCount(endpoint),
		GroupsCount:                    w.groupsCount(endpoint),
		ActiveUsersOnEndpointCount:     w.activeUsersOnEndpointCount(endpoint),
		ActiveUsersTotalCount:          w.activeUsersTotalCount(),
		HeavyUsersCount:                w.heavyUsersCount(endpoint),
		ModelsCount:                    w.modelsCount(endpoint),
		ModelsToPollOnEndpointCount:    w.modelsToPollOnEndpointCount(endpoint),
		ModelsToPollTotalCount:         w.modelsToPollTotalCount(),
		OnlineModelsCount:              len(w.ourOnline),
		KnownModelsCount:               len(w.siteStatuses),
		SpecialModelsCount:             len(w.specialModels),
		StatusChangesCount:             w.statusChangesCount(),
		TransactionsOnEndpointCount:    w.transactionsOnEndpoint(endpoint),
		TransactionsOnEndpointFinished: w.transactionsOnEndpointFinished(endpoint),
		QueriesDurationMilliseconds:    int(w.httpQueriesDuration.Milliseconds()),
		UpdatesDurationMilliseconds:    int(w.updatesDuration.Milliseconds()),
		ErrorRate:                      [2]int{w.unsuccessfulRequestsCount(), w.cfg.errorDenominator},
		DownloadErrorRate:              [2]int{w.downloadErrorsCount(), w.cfg.errorDenominator},
		Rss:                            rss / 1024,
		MaxRss:                         rusage.Maxrss,
		UserReferralsCount:             w.userReferralsCount(),
		ModelReferralsCount:            w.modelReferralsCount(),
		ReportsCount:                   w.reports(),
		ChangesInPeriod:                w.changesInPeriod,
		ConfirmedChangesInPeriod:       w.confirmedChangesInPeriod,
		Interactions:                   w.interactions(endpoint),
	}
}

func (w *worker) handleStat(endpoint string, statRequests chan statRequest) func(writer http.ResponseWriter, r *http.Request) {
	return func(writer http.ResponseWriter, r *http.Request) {
		command := statRequest{
			endpoint: endpoint,
			writer:   writer,
			request:  r,
			done:     make(chan bool),
		}
		statRequests <- command
		<-command.done
	}
}

func (w *worker) processStatCommand(endpoint string, writer http.ResponseWriter, r *http.Request, done chan bool) {
	defer func() { done <- true }()
	passwords, ok := r.URL.Query()["password"]
	if !ok || len(passwords) < 1 {
		return
	}
	password := passwords[0]
	if password != w.cfg.StatPassword {
		return
	}
	writer.WriteHeader(http.StatusOK)
	writer.Header().Set("Content-Type", "application/json")
	statJSON, err := json.MarshalIndent(w.getStat(endpoint), "", "    ")
	checkErr(err)
	_, err = writer.Write(statJSON)
	if err != nil {
		lerr("error on processing stat command, %v", err)
	}
}

func (w *worker) handleIPN(ipnRequests chan ipnRequest) func(writer http.ResponseWriter, r *http.Request) {
	return func(writer http.ResponseWriter, r *http.Request) {
		command := ipnRequest{
			writer:  writer,
			request: r,
			done:    make(chan bool),
		}
		ipnRequests <- command
		<-command.done
	}
}

func (w *worker) processIPN(writer http.ResponseWriter, r *http.Request, done chan bool) {
	defer func() { done <- true }()

	linf("got IPN data")

	newStatus, custom, err := payments.ParseIPN(r, w.cfg.CoinPayments.IPNSecret, w.cfg.Debug)
	if err != nil {
		lerr("error on processing IPN, %v", err)
		return
	}

	switch newStatus {
	case payments.StatusFinished:
		oldStatus, chatID, endpoint, found := w.transaction(custom)
		if !found {
			lerr("transaction not found: %s", custom)
			return
		}
		if oldStatus == payments.StatusFinished {
			lerr("transaction is already finished")
			return
		}
		if oldStatus == payments.StatusUnknown {
			lerr("unknown transaction ID")
			return
		}
		w.mustExec("update transactions set status=? where local_id=?", payments.StatusFinished, custom)
		w.mustExec("update users set max_models = max_models + (select coalesce(sum(model_number), 0) from transactions where local_id=?)", custom)
		user := w.mustUser(chatID)
		w.sendTr(w.lowPriorityMsg, endpoint, chatID, false, w.tr[endpoint].PaymentComplete, tplData{"max_models": user.maxModels})
		linf("payment %s is finished", custom)
		text := fmt.Sprintf("payment %s is finished", custom)
		w.sendText(w.lowPriorityMsg, w.cfg.AdminEndpoint, w.cfg.AdminID, false, true, lib.ParseRaw, text)
	case payments.StatusCanceled:
		w.mustExec("update transactions set status=? where local_id=?", payments.StatusCanceled, custom)
		linf("payment %s is canceled", custom)
		text := fmt.Sprintf("payment %s is cancelled", custom)
		w.sendText(w.lowPriorityMsg, w.cfg.AdminEndpoint, w.cfg.AdminID, false, true, lib.ParseRaw, text)
	default:
		linf("payment %s is still pending", custom)
		text := fmt.Sprintf("payment %s is still pending", custom)
		w.sendText(w.lowPriorityMsg, w.cfg.AdminEndpoint, w.cfg.AdminID, false, true, lib.ParseRaw, text)
	}
}

func (w *worker) handleStatEndpoints(statRequests chan statRequest) {
	for n, p := range w.cfg.Endpoints {
		http.HandleFunc(p.WebhookDomain+"/stat", w.handleStat(n, statRequests))
	}
}

func (w *worker) handleIPNEndpoint(ipnRequests chan ipnRequest) {
	http.HandleFunc(w.cfg.CoinPayments.IPNListenURL, w.handleIPN(ipnRequests))
}

func (w *worker) incoming() chan incomingPacket {
	result := make(chan incomingPacket)
	for n, p := range w.cfg.Endpoints {
		linf("listening for a webhook for endpoint %s", n)
		incoming := w.bots[n].ListenForWebhook(p.WebhookDomain + p.ListenPath)
		go func(n string, incoming tg.UpdatesChannel) {
			for i := range incoming {
				result <- incomingPacket{message: i, endpoint: n}
			}
		}(n, incoming)
	}
	return result
}

func (w *worker) ourIDs() []int64 {
	var ids []int64
	for _, e := range w.cfg.Endpoints {
		if idx := strings.Index(e.BotToken, ":"); idx != -1 {
			id, err := strconv.ParseInt(e.BotToken[:idx], 10, 64)
			checkErr(err)
			ids = append(ids, id)
		} else {
			checkErr(errors.New("cannot get our ID"))
		}
	}
	return ids
}

func loadTLS(certFile string, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

func (w *worker) referralID(chatID int64) *string {
	var referralID string
	if !w.maybeRecord("select referral_id from referrals where chat_id=?", queryParams{chatID}, record{&referralID}) {
		return nil
	}
	return &referralID
}

func (w *worker) chatForReferralID(referralID string) *int64 {
	var chatID int64
	if !w.maybeRecord("select chat_id from referrals where referral_id=?", queryParams{referralID}, record{&chatID}) {
		return nil
	}
	return &chatID
}

func (q queryDurationsData) total() float64 {
	return q.avg * float64(q.count)
}

func (w *worker) logQuerySuccess(success bool) {
	w.unsuccessfulRequests[w.successfulRequestsPos] = !success
	w.successfulRequestsPos = (w.successfulRequestsPos + 1) % w.cfg.errorDenominator
}

func main() {
	rand.Seed(time.Now().UnixNano())

	w := newWorker()
	w.logConfig()
	w.setWebhook()
	w.setCommands()
	w.initBotNames()
	w.createDatabase()
	w.initCache()

	incoming := w.incoming()
	statRequests := make(chan statRequest)
	w.handleStatEndpoints(statRequests)

	ipnRequests := make(chan ipnRequest)
	if w.cfg.CoinPayments != nil {
		w.handleIPNEndpoint(ipnRequests)
	}

	w.serveEndpoints()
	mail := make(chan *env)

	if w.cfg.Mail != nil {
		smtp := &smtpd.Server{
			Hostname:  w.cfg.Mail.Host,
			Addr:      w.cfg.Mail.ListenAddress,
			OnNewMail: envelopeFactory(mail),
			TLSConfig: w.mailTLS,
		}
		go func() {
			err := smtp.ListenAndServe()
			checkErr(err)
		}()
	}

	go w.sender(w.highPriorityMsg, 0)
	go w.sender(w.lowPriorityMsg, 1)

	var periodicTimer = time.NewTicker(time.Duration(w.cfg.PeriodSeconds) * time.Second)
	statusRequestsChan, onlineModelsChan, errorsChan, elapsed := lib.StartChecker(
		w.checkModel,
		w.onlineModelsAPI,
		w.cfg.UsersOnlineEndpoint,
		w.clients,
		w.cfg.Headers,
		w.cfg.IntervalMs,
		w.cfg.Debug,
		w.cfg.SpecificConfig)
	statusRequestsChan <- lib.StatusRequest{SpecialModels: w.specialModels}
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGABRT)
	for {
		select {
		case e := <-elapsed:
			w.httpQueriesDuration = e
		case <-periodicTimer.C:
			runtime.GC()
			w.processPeriodic(statusRequestsChan)
		case onlineModels := <-onlineModelsChan:
			now := int(time.Now().Unix())
			changesInPeriod, confirmedChangesInPeriod, notifications, elapsed := w.processStatusUpdates(onlineModels, now)
			w.updatesDuration = elapsed
			w.changesInPeriod = changesInPeriod
			w.confirmedChangesInPeriod = confirmedChangesInPeriod
			w.notifyOfStatuses(w.lowPriorityMsg, notifications)
			if w.cfg.Debug {
				ldbg("status updates processed in %v", elapsed)
			}
			w.logQuerySuccess(true)
		case <-errorsChan:
			w.logQuerySuccess(false)
		case u := <-incoming:
			w.processTGUpdate(u)
		case m := <-mail:
			w.mailReceived(m)
		case s := <-statRequests:
			w.processStatCommand(s.endpoint, s.writer, s.request, s.done)
		case s := <-ipnRequests:
			w.processIPN(s.writer, s.request, s.done)
		case s := <-signals:
			linf("got signal %v", s)
			w.removeWebhook()
			return
		case r := <-w.outgoingMsgResults:
			switch r.result {
			case messageBlocked:
				w.incrementBlock(r.endpoint, r.chatID)
			case messageSent:
				w.resetBlock(r.endpoint, r.chatID)
			}
			w.mustExec("insert into interactions (timestamp, chat_id, result, endpoint, priority, delay) values (?,?,?,?,?,?)",
				r.timestamp,
				r.chatID,
				r.result,
				r.endpoint,
				r.priority,
				r.delay)
		}
	}
}
