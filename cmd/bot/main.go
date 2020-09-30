package main

import (
	"bytes"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/ioutil"
	"math/rand"
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
	chatID    int64
	maxModels int
	reports   int
	blacklist bool
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
	tr                       map[string]*lib.Translations
	tpl                      map[string]*template.Template
	checkModel               func(client *lib.Client, modelID string, headers [][2]string, dbg bool, config map[string]string) lib.StatusKind
	startChecker             func(
		usersOnlineEndpoint []string,
		clients []*lib.Client,
		headers [][2]string,
		intervalMs int,
		debug bool,
		config map[string]string,
	) (
		requests chan lib.StatusRequest,
		output chan []lib.StatusUpdate,
		elapsed chan time.Duration)

	senders               map[string]func(msg tg.Chattable) (tg.Message, error)
	unsuccessfulRequests  []bool
	successfulRequestsPos int
	downloadResults       []bool
	downloadResultsPos    int
	nextErrorReport       time.Time
	coinPaymentsAPI       *payments.CoinPaymentsAPI
	ipnServeMux           *http.ServeMux
	mailTLS               *tls.Config
	lastStatusChanges     map[string]statusChange
	confirmedStatuses     map[string]lib.StatusKind
	images                map[string]string
	sqlQueryDurations     map[string]queryDurationsData
}

type packet struct {
	message  tg.Update
	endpoint string
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

//go:generate jsonenums -type=checkerKind
type checkerKind int

const (
	checkerAPI checkerKind = iota
	checkerPolling
)

func (c checkerKind) String() string {
	switch c {
	case checkerAPI:
		return "api"
	case checkerPolling:
		return "polling"
	default:
		return "unknown"
	}
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

	bots := make(map[string]*tg.BotAPI)
	senders := make(map[string]func(msg tg.Chattable) (tg.Message, error))
	for n, p := range cfg.Endpoints {
		//noinspection GoNilness
		var bot *tg.BotAPI
		bot, err = tg.NewBotAPIWithClient(p.BotToken, tg.APIEndpoint, clients[0].Client)
		checkErr(err)
		bots[n] = bot
		senders[n] = bot.Send
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
		senders:              senders,
		unsuccessfulRequests: make([]bool, cfg.errorDenominator),
		downloadResults:      make([]bool, cfg.errorDenominator),
		ipnServeMux:          http.NewServeMux(),
		mailTLS:              mailTLS,
		images:               map[string]string{},
		sqlQueryDurations:    map[string]queryDurationsData{},
	}

	if cp := cfg.CoinPayments; cp != nil {
		w.coinPaymentsAPI = payments.NewCoinPaymentsAPI(cp.PublicKey, cp.PrivateKey, "https://"+cp.IPNListenURL, cfg.TimeoutSeconds, cfg.Debug)
	}

	switch cfg.Website {
	case "bongacams":
		w.checkModel = lib.CheckModelBongaCams
		switch w.cfg.Checker {
		case checkerAPI:
			w.startChecker = lib.StartBongaCamsAPIChecker
		case checkerPolling:
			w.startChecker = lib.StartBongaCamsPollingChecker
		default:
			panic("specify checker")
		}
	case "chaturbate":
		w.checkModel = lib.CheckModelChaturbate
		w.startChecker = lib.StartChaturbateAPIChecker
	case "stripchat":
		w.checkModel = lib.CheckModelStripchat
		w.startChecker = lib.StartStripchatAPIChecker
	case "livejasmin":
		w.checkModel = lib.CheckModelLiveJasmin
		w.startChecker = lib.StartLiveJasminAPIChecker
	default:
		panic("wrong website")
	}

	return w
}

func (w *worker) cleanStatuses() {
	if w.cfg.Checker != checkerPolling {
		return
	}

	now := int(time.Now().Unix())
	query := w.mustQuery(`
		select model_id from models
		where status != ? and not exists(select * from signals where signals.model_id = models.model_id)`,
		lib.StatusUnknown)
	var models []string
	for query.Next() {
		var modelID string
		checkErr(query.Scan(&modelID))
		models = append(models, modelID)
	}
	tx, err := w.db.Begin()
	checkErr(err)
	insertStatusChangeStmt, err := tx.Prepare(insertStatusChange)
	checkErr(err)
	updateLastStatusChangeStmt, err := tx.Prepare(updateLastStatusChange)
	checkErr(err)
	for _, modelID := range models {
		w.mustExecInTx(tx, "update models set status=? where model_id=?", lib.StatusUnknown, modelID)
		w.confirmedStatuses[modelID] = lib.StatusUnknown
		w.mustExecPrepared(insertStatusChange, insertStatusChangeStmt, modelID, lib.StatusUnknown, now)
		w.mustExecPrepared(updateLastStatusChange, updateLastStatusChangeStmt, modelID, lib.StatusUnknown, now)
		w.lastStatusChanges[modelID] = statusChange{
			modelID:   modelID,
			status:    lib.StatusUnknown,
			timestamp: now,
		}
	}
	defer w.measure("commit -- clean statuses")()
	checkErr(insertStatusChangeStmt.Close())
	checkErr(updateLastStatusChangeStmt.Close())
	checkErr(tx.Commit())
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

func (w *worker) sendText(endpoint string, chatID int64, notify bool, disablePreview bool, parse lib.ParseKind, text string) {
	msg := tg.NewMessage(chatID, text)
	msg.DisableNotification = !notify
	msg.DisableWebPagePreview = disablePreview
	switch parse {
	case lib.ParseHTML, lib.ParseMarkdown:
		msg.ParseMode = parse.String()
	}
	w.sendMessage(endpoint, &messageConfig{msg})
}

func (w *worker) sendImage(endpoint string, chatID int64, notify bool, parse lib.ParseKind, text string, image []byte) {
	fileBytes := tg.FileBytes{Name: "preview", Bytes: image}
	msg := tg.NewPhotoUpload(chatID, fileBytes)
	msg.Caption = text
	msg.DisableNotification = !notify
	switch parse {
	case lib.ParseHTML, lib.ParseMarkdown:
		msg.ParseMode = parse.String()
	}
	w.sendMessage(endpoint, &photoConfig{msg})
}

func (w *worker) sendMessage(endpoint string, msg baseChattable) {
	chatID := msg.baseChat().ChatID
	if _, err := w.bots[endpoint].Send(msg); err != nil {
		switch err := err.(type) {
		case tg.Error:
			if err.Code == 403 {
				linf("bot is blocked by the user %d, %v", chatID, err)
				w.incrementBlock(endpoint, chatID)
			} else {
				lerr("cannot send a message to %d, code %d, %v", chatID, err.Code, err)
			}
		default:
			lerr("unexpected error type while sending a message to %d, %v", msg.baseChat().ChatID, err)
		}
		return
	}
	if w.cfg.Debug {
		ldbg("message sent to %d", chatID)
	}
	w.resetBlock(endpoint, chatID)
}

func templateToString(t *template.Template, key string, data map[string]interface{}) string {
	buf := &bytes.Buffer{}
	err := t.ExecuteTemplate(buf, key, data)
	checkErr(err)
	return buf.String()
}

func (w *worker) sendTr(endpoint string, chatID int64, notify bool, translation *lib.Translation, data map[string]interface{}) {
	tpl := w.tpl[endpoint]
	text := templateToString(tpl, translation.Key, data)
	w.sendText(endpoint, chatID, notify, translation.DisablePreview, translation.Parse, text)
}

func (w *worker) sendTrImage(endpoint string, chatID int64, notify bool, translation *lib.Translation, data map[string]interface{}, image []byte) {
	tpl := w.tpl[endpoint]
	text := templateToString(tpl, translation.Key, data)
	w.sendImage(endpoint, chatID, notify, translation.Parse, text, image)
}

func (w *worker) createDatabase() {
	linf("creating database if needed...")
	if w.cfg.SQLPrelude != "" {
		w.mustExec(w.cfg.SQLPrelude)
	}
	w.mustExec(`create table if not exists schema_version (version integer);`)
	w.applyMigrations()
}

func (w *worker) initCache() {
	start := time.Now()
	w.lastStatusChanges = w.queryLastStatusChanges()
	w.confirmedStatuses = w.queryConfirmedStatuses()
	elapsed := time.Since(start)
	linf("cache initialized in %d ms", elapsed.Milliseconds())
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

func (w *worker) updateStatus(
	insertStatusChangeStmt, updateLastStatusChangeStmt, updateModelStatusStmt *sql.Stmt,
	statusChange statusChange,
) (
	changeConfirmed bool,
) {
	lastStatusChange := w.lastStatusChanges[statusChange.modelID]
	if statusChange.status != lastStatusChange.status {
		w.mustExecPrepared(insertStatusChange, insertStatusChangeStmt, statusChange.modelID, statusChange.status, statusChange.timestamp)
		w.mustExecPrepared(updateLastStatusChange, updateLastStatusChangeStmt, statusChange.modelID, statusChange.status, statusChange.timestamp)
		w.lastStatusChanges[statusChange.modelID] = statusChange
	}
	confirmationSeconds := w.confirmationSeconds(statusChange.status)
	durationConfirmed := false ||
		confirmationSeconds == 0 ||
		(statusChange.status == lastStatusChange.status && statusChange.timestamp-lastStatusChange.timestamp >= confirmationSeconds)
	if w.confirmedStatuses[statusChange.modelID] != statusChange.status && durationConfirmed {
		w.mustExecPrepared(updateModelStatus, updateModelStatusStmt, statusChange.modelID, statusChange.status)
		w.confirmedStatuses[statusChange.modelID] = statusChange.status
		return true
	}

	return false
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

func (w *worker) chatsForModels() (chats map[string][]int64, endpoints map[string][]string) {
	chats = make(map[string][]int64)
	endpoints = make(map[string][]string)
	chatsQuery := w.mustQuery(`select model_id, chat_id, endpoint from signals`)
	defer func() { checkErr(chatsQuery.Close()) }()
	for chatsQuery.Next() {
		var modelID string
		var chatID int64
		var endpoint string
		checkErr(chatsQuery.Scan(&modelID, &chatID, &endpoint))
		chats[modelID] = append(chats[modelID], chatID)
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

func (w *worker) model(modelID string) (status lib.StatusKind, referredUsers int) {
	_ = w.maybeRecord("select status, referred_users from models where model_id=?",
		queryParams{modelID},
		record{&status, &referredUsers})
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

func (w *worker) statusesForChat(endpoint string, chatID int64) []lib.StatusUpdate {
	statusesQuery := w.mustQuery(`
		select models.model_id, models.status
		from models
		join signals on signals.model_id=models.model_id
		where signals.chat_id=? and signals.endpoint=?
		order by models.model_id`,
		chatID,
		endpoint)
	defer func() { checkErr(statusesQuery.Close()) }()
	var statuses []lib.StatusUpdate
	for statusesQuery.Next() {
		var modelID string
		var status lib.StatusKind
		checkErr(statusesQuery.Scan(&modelID, &status))
		statuses = append(statuses, lib.StatusUpdate{ModelID: modelID, Status: status})
	}
	return statuses
}

func (w *worker) notifyOfStatuses(notifications []notification) {
	models := map[string]bool{}
	for _, n := range notifications {
		models[n.modelID] = true
	}
	images := map[string][]byte{}
	for m := range models {
		if url := w.images[m]; url != "" {
			images[m] = w.download(url)
		}
	}
	for _, n := range notifications {
		w.notifyOfStatus(n, images[n.modelID])
	}
}

func (w *worker) notifyOfStatus(n notification, image []byte) {
	if w.cfg.Debug {
		ldbg("notifying of status of the model %s", n.modelID)
	}
	data := tplData{"model": n.modelID, "time_diff": n.timeDiff}
	switch n.status {
	case lib.StatusOnline:
		if image == nil {
			w.sendTr(n.endpoint, n.chatID, true, w.tr[n.endpoint].Online, data)
		} else {
			w.sendTrImage(n.endpoint, n.chatID, true, w.tr[n.endpoint].Online, data, image)
		}
	case lib.StatusOffline:
		w.sendTr(n.endpoint, n.chatID, false, w.tr[n.endpoint].Offline, data)
	case lib.StatusDenied:
		w.sendTr(n.endpoint, n.chatID, false, w.tr[n.endpoint].Denied, data)
	}
	w.addUser(n.endpoint, n.chatID)
	w.mustExec("update users set reports=reports+1 where chat_id=?", n.chatID)
}

func (w *worker) subscriptionExists(endpoint string, chatID int64, modelID string) bool {
	count := w.mustInt("select count(*) from signals where chat_id=? and model_id=? and endpoint=?", chatID, modelID, endpoint)
	return count != 0
}

func (w *worker) userExists(chatID int64) bool {
	count := w.mustInt("select count(*) from users where chat_id=?", chatID)
	return count != 0
}

func (w *worker) subscriptionsNumber(endpoint string, chatID int64) int {
	return w.mustInt("select count(*) from signals where chat_id=? and endpoint=?", chatID, endpoint)
}

func (w *worker) maxModels(chatID int64) int {
	user, found := w.user(chatID)
	if !found {
		return w.cfg.MaxModels
	}
	return user.maxModels
}

func (w *worker) user(chatID int64) (user user, found bool) {
	found = w.maybeRecord("select chat_id, max_models, reports, blacklist from users where chat_id=?",
		queryParams{chatID},
		record{&user.chatID, &user.maxModels, &user.reports, &user.blacklist})
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
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].ZeroSubscriptions, nil)
	}

}

func (w *worker) showWeekForModel(endpoint string, chatID int64, modelID string) {
	modelID = strings.ToLower(modelID)
	if !lib.ModelIDRegexp.MatchString(modelID) {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].InvalidSymbols, tplData{"model": modelID})
		return
	}
	hours, start := w.week(modelID)
	w.sendTr(endpoint, chatID, false, w.tr[endpoint].Week, tplData{
		"hours":   hours,
		"weekday": int(start.UTC().Weekday()),
		"model":   modelID,
	})
}

func (w *worker) addModel(endpoint string, chatID int64, modelID string, now int) bool {
	if modelID == "" {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].SyntaxAdd, nil)
		return false
	}
	modelID = strings.ToLower(modelID)
	if !lib.ModelIDRegexp.MatchString(modelID) {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].InvalidSymbols, tplData{"model": modelID})
		return false
	}

	w.addUser(endpoint, chatID)

	if w.subscriptionExists(endpoint, chatID, modelID) {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].AlreadyAdded, tplData{"model": modelID})
		return false
	}
	subscriptionsNumber := w.subscriptionsNumber(endpoint, chatID)
	maxModels := w.maxModels(chatID)
	if subscriptionsNumber >= maxModels {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].NotEnoughSubscriptions, nil)
		w.subscriptionUsage(endpoint, chatID, true)
		return false
	}
	confirmedStatus := w.confirmedStatuses[modelID]
	newStatus := confirmedStatus
	if newStatus == lib.StatusUnknown {
		checkedStatus := w.checkModel(w.clients[0], modelID, w.cfg.Headers, w.cfg.Debug, w.cfg.SpecificConfig)
		if checkedStatus == lib.StatusUnknown || checkedStatus == lib.StatusNotFound {
			w.sendTr(endpoint, chatID, false, w.tr[endpoint].AddError, tplData{"model": modelID})
			return false
		}
		if w.cfg.Checker == checkerAPI {
			newStatus = lib.StatusOffline
		}
	}
	w.mustExec("insert into signals (chat_id, model_id, endpoint) values (?,?,?)", chatID, modelID, endpoint)
	w.mustExec("insert or ignore into models (model_id, status) values (?,?)", modelID, newStatus)
	subscriptionsNumber++
	if newStatus != lib.StatusDenied {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].ModelAdded, tplData{"model": modelID})
	}
	w.notifyOfStatuses([]notification{{
		endpoint: endpoint,
		chatID:   chatID,
		modelID:  modelID,
		status:   newStatus,
		timeDiff: w.modelTimeDiff(modelID, now)}})
	if subscriptionsNumber >= maxModels-w.cfg.HeavyUserRemainder {
		w.subscriptionUsage(endpoint, chatID, true)
	}
	return true
}

func (w *worker) subscriptionUsage(endpoint string, chatID int64, ad bool) {
	subscriptionsNumber := w.subscriptionsNumber(endpoint, chatID)
	maxModels := w.maxModels(chatID)
	tr := w.tr[endpoint].SubscriptionUsage
	if ad {
		tr = w.tr[endpoint].SubscriptionUsageAd
	}
	w.sendTr(endpoint, chatID, false, tr, tplData{
		"subscriptions_used":  subscriptionsNumber,
		"total_subscriptions": maxModels})
}

func (w *worker) wantMore(endpoint string, chatID int64) {
	w.subscriptionUsage(endpoint, chatID, false)
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
	w.sendMessage(endpoint, &messageConfig{msg})
}

func (w *worker) removeModel(endpoint string, chatID int64, modelID string) {
	if modelID == "" {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].SyntaxRemove, nil)
		return
	}
	modelID = strings.ToLower(modelID)
	if !lib.ModelIDRegexp.MatchString(modelID) {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].InvalidSymbols, tplData{"model": modelID})
		return
	}
	if !w.subscriptionExists(endpoint, chatID, modelID) {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].ModelNotInList, tplData{"model": modelID})
		return
	}
	w.mustExec("delete from signals where chat_id=? and model_id=? and endpoint=?", chatID, modelID, endpoint)
	w.cleanStatuses()
	w.sendTr(endpoint, chatID, false, w.tr[endpoint].ModelRemoved, tplData{"model": modelID})
}

func (w *worker) sureRemoveAll(endpoint string, chatID int64) {
	w.mustExec("delete from signals where chat_id=? and endpoint=?", chatID, endpoint)
	w.cleanStatuses()
	w.sendTr(endpoint, chatID, false, w.tr[endpoint].AllModelsRemoved, nil)
}

func (w *worker) buy(endpoint string, chatID int64) {
	var buttons [][]tg.InlineKeyboardButton
	for _, c := range w.cfg.CoinPayments.Currencies {
		buttons = append(buttons, []tg.InlineKeyboardButton{tg.NewInlineKeyboardButtonData(c, "buy_with "+c)})
	}

	keyboard := tg.NewInlineKeyboardMarkup(buttons...)
	tpl := w.tpl[endpoint]
	text := templateToString(tpl, w.tr[endpoint].SelectCurrency.Key, tplData{
		"dollars":                 w.cfg.CoinPayments.subscriptionPacketPrice,
		"number_of_subscriptions": w.cfg.CoinPayments.subscriptionPacketModelNumber,
		"total_subscriptions":     w.maxModels(chatID) + w.cfg.CoinPayments.subscriptionPacketModelNumber,
	})

	msg := tg.NewMessage(chatID, text)
	msg.ReplyMarkup = keyboard
	w.sendMessage(endpoint, &messageConfig{msg})
}

func (w *worker) email(endpoint string, chatID int64) string {
	username := w.mustString("select email from emails where endpoint=? and chat_id=?", endpoint, chatID)
	return username + "@" + w.cfg.Mail.Host
}

func (w *worker) transaction(uuid string) (status payments.StatusKind, chatID int64, endpoint string) {
	_ = w.maybeRecord("select status, chat_id, endpoint from transactions where local_id=?",
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
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].UnknownCurrency, nil)
		return
	}

	w.addUser(endpoint, chatID)
	email := w.email(endpoint, chatID)
	localID := uuid.New()
	transaction, err := w.coinPaymentsAPI.CreateTransaction(w.cfg.CoinPayments.subscriptionPacketPrice, currency, email, localID.String())
	if err != nil {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].TryToBuyLater, nil)
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

	w.sendTr(endpoint, chatID, false, w.tr[endpoint].PayThis, tplData{
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
			Model:    s.ModelID,
			TimeDiff: w.modelTimeDiff(s.ModelID, now),
		}
		switch s.Status {
		case lib.StatusOnline:
			online = append(online, data)
		case lib.StatusDenied:
			denied = append(denied, data)
		default:
			offline = append(offline, data)
		}
	}
	w.sendTr(endpoint, chatID, false, w.tr[endpoint].List, tplData{"online": online, "offline": offline, "denied": denied})
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
	w.downloadResults[w.downloadResultsPos] = success
	w.downloadResultsPos = (w.downloadResultsPos + 1) % w.cfg.errorDenominator
}

func (w *worker) download(url string) []byte {
	resp, err := w.clients[0].Client.Get(url)
	if err != nil {
		w.downloadSuccess(false)
		return nil
	}
	defer func() { checkErr(resp.Body.Close()) }()
	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(resp.Body)
	if err != nil {
		w.downloadSuccess(false)
		return nil
	}
	w.downloadSuccess(true)
	return buf.Bytes()
}

func (w *worker) listOnlineModels(endpoint string, chatID int64, now int) {
	statuses := w.statusesForChat(endpoint, chatID)
	online := 0
	for _, s := range statuses {
		if s.Status == lib.StatusOnline {
			imageURL := w.images[s.ModelID]
			var image []byte
			if imageURL != "" {
				image = w.download(imageURL)
			}
			if image == nil {
				w.sendTr(
					endpoint,
					chatID,
					false,
					w.tr[endpoint].Online,
					tplData{"model": s.ModelID, "time_diff": w.modelTimeDiff(s.ModelID, now)})
			} else {
				w.sendTrImage(
					endpoint,
					chatID,
					false,
					w.tr[endpoint].Online,
					tplData{"model": s.ModelID, "time_diff": w.modelTimeDiff(s.ModelID, now)},
					image)
			}
			online++
		}
	}
	if online == 0 {
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].NoOnlineModels, nil)
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
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].SyntaxFeedback, nil)
		return
	}
	w.mustExec("insert into feedback (endpoint, chat_id, text) values (?, ?, ?)", endpoint, chatID, text)
	w.sendTr(endpoint, chatID, false, w.tr[endpoint].Feedback, nil)
	user, _ := w.user(chatID)
	if !user.blacklist {
		w.sendText(endpoint, w.cfg.AdminID, true, true, lib.ParseRaw, fmt.Sprintf("Feedback from %d: %s", chatID, text))
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
	for _, s := range w.downloadResults {
		if !s {
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

func (w *worker) onlineModelsCount() int {
	return w.mustInt("select count(*) from models where models.status=?", lib.StatusOnline)
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
	w.sendText(endpoint, w.cfg.AdminID, true, true, lib.ParseRaw, strings.Join(w.statStrings(endpoint), "\n"))
}

func (w *worker) sqlStat(endpoint string) {
	durations := w.sqlQueryDurations
	var queries []string
	for x := range durations {
		queries = append(queries, x)
	}
	sort.SliceStable(queries, func(i, j int) bool {
		return durations[queries[i]].total() > durations[queries[j]].total()
	})
	for _, x := range queries {
		lines := []string{
			fmt.Sprintf("<b>Query</b>: %s", html.EscapeString(x)),
			fmt.Sprintf("<b>Total</b>: %f", durations[x].avg*float64(durations[x].count)),
			fmt.Sprintf("<b>Avg</b>: %f", durations[x].avg),
			fmt.Sprintf("<b>Count</b>: %d", durations[x].count),
		}
		entry := strings.Join(lines, "\n")
		w.sendText(endpoint, w.cfg.AdminID, false, true, lib.ParseHTML, entry)
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
		w.sendText(endpoint, chatID, true, false, lib.ParseRaw, text)
	}
	w.sendText(endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "OK")
}

func (w *worker) direct(endpoint string, arguments string) {
	parts := strings.SplitN(arguments, " ", 2)
	if len(parts) < 2 {
		w.sendText(endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "usage: /direct chatID text")
		return
	}
	whom, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		w.sendText(endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "first argument is invalid")
		return
	}
	text := parts[1]
	if text == "" {
		return
	}
	w.sendText(endpoint, whom, true, false, lib.ParseRaw, text)
	w.sendText(endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "OK")
}

func (w *worker) blacklist(endpoint string, arguments string) {
	whom, err := strconv.ParseInt(arguments, 10, 64)
	if err != nil {
		w.sendText(endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "first argument is invalid")
		return
	}
	w.mustExec("update users set blacklist=1 where chat_id=?", whom)
	w.sendText(endpoint, w.cfg.AdminID, false, true, lib.ParseRaw, "OK")
}

func (w *worker) serveEndpoint(n string, p endpoint) {
	linf("serving endpoint %s...", n)
	var err error
	if p.CertificatePath != "" && p.CertificateKeyPath != "" {
		err = http.ListenAndServeTLS(p.ListenAddress, p.CertificatePath, p.CertificateKeyPath, nil)
	} else {
		err = http.ListenAndServe(p.ListenAddress, nil)
	}
	checkErr(err)
}

func (w *worker) serveEndpoints() {
	for n, p := range w.cfg.Endpoints {
		go w.serveEndpoint(n, p)
	}
}

func (w *worker) serveIPN() {
	go func() {
		err := http.ListenAndServe(w.cfg.CoinPayments.IPNListenAddress, w.ipnServeMux)
		checkErr(err)
	}()
}

func (w *worker) logConfig() {
	cfgString, err := json.MarshalIndent(w.cfg, "", "    ")
	checkErr(err)
	linf("config: " + string(cfgString))
}

func (w *worker) myEmail(endpoint string) {
	w.addUser(endpoint, w.cfg.AdminID)
	email := w.email(endpoint, w.cfg.AdminID)
	w.sendText(endpoint, w.cfg.AdminID, true, true, lib.ParseRaw, email)
}

func (w *worker) processAdminMessage(endpoint string, chatID int64, command, arguments string) bool {
	switch command {
	case "stat":
		w.stat(endpoint)
		return true
	case "sql_stat":
		w.sqlStat(endpoint)
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
	case "set_max_models":
		parts := strings.Fields(arguments)
		if len(parts) != 2 {
			w.sendText(endpoint, chatID, false, true, lib.ParseRaw, "expecting two arguments")
			return true
		}
		who, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			w.sendText(endpoint, chatID, false, true, lib.ParseRaw, "first argument is invalid")
			return true
		}
		maxModels, err := strconv.Atoi(parts[1])
		if err != nil {
			w.sendText(endpoint, chatID, false, true, lib.ParseRaw, "second argument is invalid")
			return true
		}
		w.setLimit(who, maxModels)
		w.sendText(endpoint, chatID, false, true, lib.ParseRaw, "OK")
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
		w.sendTr(email.endpoint, email.chatID, true, w.tr[email.endpoint].MailReceived, tplData{
			"subject": e.mime.GetHeader("Subject"),
			"from":    e.mime.GetHeader("From"),
			"text":    e.mime.Text})
		for _, inline := range e.mime.Inlines {
			b := tg.FileBytes{Name: inline.FileName, Bytes: inline.Content}
			switch {
			case strings.HasPrefix(inline.ContentType, "image/"):
				msg := tg.NewPhotoUpload(email.chatID, b)
				w.sendMessage(email.endpoint, &photoConfig{msg})
			default:
				msg := tg.NewDocumentUpload(email.chatID, b)
				w.sendMessage(email.endpoint, &documentConfig{msg})
			}
		}
		for _, inline := range e.mime.Attachments {
			b := tg.FileBytes{Name: inline.FileName, Bytes: inline.Content}
			msg := tg.NewDocumentUpload(email.chatID, b)
			w.sendMessage(email.endpoint, &documentConfig{msg})
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
	if w.userExists(followerChatID) {
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
	referralLink := fmt.Sprintf("https://t.me/%s?start=%s", w.cfg.Endpoints[endpoint].BotName, *referralID)
	w.sendTr(endpoint, chatID, false, w.tr[endpoint].ReferralLink, tplData{
		"link":           referralLink,
		"referral_bonus": w.cfg.ReferralBonus,
		"follower_bonus": w.cfg.FollowerBonus,
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
			w.sendTr(endpoint, chatID, false, w.tr[endpoint].OwnReferralLinkHit, nil)
			return
		}
	}
	w.sendTr(endpoint, chatID, false, w.tr[endpoint].Help, nil)
	if chatID > 0 && referrer != "" {
		applied := w.refer(chatID, referrer)
		switch applied {
		case referralApplied:
			w.sendTr(endpoint, chatID, false, w.tr[endpoint].ReferralApplied, nil)
		case invalidReferral:
			w.sendTr(endpoint, chatID, false, w.tr[endpoint].InvalidReferralLink, nil)
		case followerExists:
			w.sendTr(endpoint, chatID, false, w.tr[endpoint].FollowerExists, nil)
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
	linf("chat: %d, command: %s %s", chatID, command, arguments)

	if chatID == w.cfg.AdminID && w.processAdminMessage(endpoint, chatID, command, arguments) {
		return
	}

	unknown := func() { w.sendTr(endpoint, chatID, false, w.tr[endpoint].UnknownCommand, nil) }

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
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].FAQ, tplData{
			"dollars":                 w.cfg.CoinPayments.subscriptionPacketPrice,
			"number_of_subscriptions": w.cfg.CoinPayments.subscriptionPacketModelNumber,
			"max_models":              w.cfg.MaxModels,
		})

	case "feedback":
		w.feedback(endpoint, chatID, arguments)
	case "social":
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].Social, nil)
	case "version":
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].Version, tplData{"version": version})
	case "remove_all", "stop":
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].RemoveAll, nil)
	case "sure_remove_all":
		w.sureRemoveAll(endpoint, chatID)
	case "want_more":
		w.wantMore(endpoint, chatID)
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
	case "max_models":
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].YourMaxModels, tplData{"max_models": w.maxModels(chatID)})
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
		w.sendText(w.cfg.AdminEndpoint, w.cfg.AdminID, true, true, lib.ParseRaw, text)
		w.nextErrorReport = now.Add(time.Minute * time.Duration(w.cfg.ErrorReportingPeriodMinutes))
	}

	select {
	case statusRequests <- lib.StatusRequest{KnownModels: w.confirmedStatuses, ModelsToPoll: w.modelsToPoll()}:
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

func (w *worker) queryConfirmedStatuses() map[string]lib.StatusKind {
	query := w.mustQuery("select model_id, status from models")
	defer func() { checkErr(query.Close()) }()
	statuses := map[string]lib.StatusKind{}
	for query.Next() {
		var modelID string
		var status lib.StatusKind
		checkErr(query.Scan(&modelID, &status))
		statuses[modelID] = status
	}
	return statuses
}

func (w *worker) processStatusUpdates(
	statusUpdates []lib.StatusUpdate,
	now int,
) (
	changesCount int,
	confirmedChangesCount int,
	notifications []notification,
	elapsed time.Duration,
) {
	start := time.Now()
	chatsForModels, endpointsForModels := w.chatsForModels()
	tx, err := w.db.Begin()
	checkErr(err)

	insertStatusChangeStmt, err := tx.Prepare(insertStatusChange)
	checkErr(err)
	updateLastStatusChangeStmt, err := tx.Prepare(updateLastStatusChange)
	checkErr(err)
	updateModelStatusStmt, err := tx.Prepare(updateModelStatus)
	checkErr(err)

	for _, u := range statusUpdates {
		if u.Status != lib.StatusUnknown {
			statusChange := statusChange{modelID: u.ModelID, status: u.Status, timestamp: now}
			if w.lastStatusChanges[u.ModelID].status != statusChange.status {
				changesCount++
			}
			changeConfirmed := w.updateStatus(insertStatusChangeStmt, updateLastStatusChangeStmt, updateModelStatusStmt, statusChange)
			if changeConfirmed {
				confirmedChangesCount++
			}
			if u.Image != "" {
				w.images[u.ModelID] = u.Image
			} else {
				delete(w.images, u.ModelID)
			}
			if changeConfirmed && (w.cfg.OfflineNotifications || u.Status != lib.StatusOffline) {
				chats := chatsForModels[u.ModelID]
				endpoints := endpointsForModels[u.ModelID]
				for i, chatID := range chats {
					notifications = append(notifications, notification{
						endpoint: endpoints[i],
						chatID:   chatID,
						modelID:  u.ModelID,
						status:   u.Status,
					})
				}
			}
		}
	}

	defer w.measure("commit -- update statuses")()
	checkErr(insertStatusChangeStmt.Close())
	checkErr(updateLastStatusChangeStmt.Close())
	checkErr(updateModelStatusStmt.Close())
	checkErr(tx.Commit())
	elapsed = time.Since(start)
	return
}

func (w *worker) processTGUpdate(p packet) {
	now := int(time.Now().Unix())
	u := p.message
	if u.Message != nil && u.Message.Chat != nil {
		if newMembers := u.Message.NewChatMembers; newMembers != nil && len(*newMembers) > 0 {
			ourIDs := w.ourIDs()
		addedToChat:
			for _, m := range *newMembers {
				for _, ourID := range ourIDs {
					if int64(m.ID) == ourID {
						w.sendTr(p.endpoint, u.Message.Chat.ID, false, w.tr[p.endpoint].Help, nil)
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
		OnlineModelsCount:              w.onlineModelsCount(),
		KnownModelsCount:               len(w.confirmedStatuses),
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
	if !ok || len(passwords[0]) < 1 {
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
		oldStatus, chatID, endpoint := w.transaction(custom)
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
		w.sendTr(endpoint, chatID, false, w.tr[endpoint].PaymentComplete, tplData{"max_models": w.maxModels(chatID)})
		linf("payment %s is finished", custom)
		text := fmt.Sprintf("payment %s is finished", custom)
		w.sendText(w.cfg.AdminEndpoint, w.cfg.AdminID, false, true, lib.ParseRaw, text)
	case payments.StatusCanceled:
		w.mustExec("update transactions set status=? where local_id=?", payments.StatusCanceled, custom)
		linf("payment %s is canceled", custom)
		text := fmt.Sprintf("payment %s is cancelled", custom)
		w.sendText(w.cfg.AdminEndpoint, w.cfg.AdminID, false, true, lib.ParseRaw, text)
	default:
		linf("payment %s is still pending", custom)
		text := fmt.Sprintf("payment %s is still pending", custom)
		w.sendText(w.cfg.AdminEndpoint, w.cfg.AdminID, false, true, lib.ParseRaw, text)
	}
}

func (w *worker) handleStatEndpoints(statRequests chan statRequest) {
	for n, p := range w.cfg.Endpoints {
		http.HandleFunc(p.WebhookDomain+"/stat", w.handleStat(n, statRequests))
	}
}

func (w *worker) handleIPNEndpoint(ipnRequests chan ipnRequest) {
	w.ipnServeMux.HandleFunc(w.cfg.CoinPayments.IPNListenURL, w.handleIPN(ipnRequests))
}

func (w *worker) incoming() chan packet {
	result := make(chan packet)
	for n, p := range w.cfg.Endpoints {
		linf("listening for a webhook for endpoint %s", n)
		incoming := w.bots[n].ListenForWebhook(p.WebhookDomain + p.ListenPath)
		go func(n string, incoming tg.UpdatesChannel) {
			for i := range incoming {
				result <- packet{message: i, endpoint: n}
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

func main() {
	rand.Seed(time.Now().UnixNano())

	w := newWorker()
	w.logConfig()
	w.setWebhook()
	w.setCommands()
	w.createDatabase()
	w.initCache()

	incoming := w.incoming()
	statRequests := make(chan statRequest)
	w.handleStatEndpoints(statRequests)
	w.serveEndpoints()

	ipnRequests := make(chan ipnRequest)
	if w.cfg.CoinPayments != nil {
		w.handleIPNEndpoint(ipnRequests)
		w.serveIPN()
	}

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

	var periodicTimer = time.NewTicker(time.Duration(w.cfg.PeriodSeconds) * time.Second)
	statusRequestsChan, statusUpdatesChan, elapsed := w.startChecker(
		w.cfg.UsersOnlineEndpoint,
		w.clients,
		w.cfg.Headers,
		w.cfg.IntervalMs,
		w.cfg.Debug,
		w.cfg.SpecificConfig)
	statusRequestsChan <- lib.StatusRequest{KnownModels: w.confirmedStatuses, ModelsToPoll: w.modelsToPoll()}
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGABRT)
	for {
		select {
		case e := <-elapsed:
			w.httpQueriesDuration = e
		case <-periodicTimer.C:
			runtime.GC()
			w.processPeriodic(statusRequestsChan)
		case statusUpdates := <-statusUpdatesChan:
			w.unsuccessfulRequests[w.successfulRequestsPos] = statusUpdates == nil
			w.successfulRequestsPos = (w.successfulRequestsPos + 1) % w.cfg.errorDenominator
			now := int(time.Now().Unix())
			changesInPeriod, confirmedChangesInPeriod, notifications, elapsed := w.processStatusUpdates(statusUpdates, now)
			w.updatesDuration = elapsed
			w.changesInPeriod = changesInPeriod
			w.confirmedChangesInPeriod = confirmedChangesInPeriod
			w.notifyOfStatuses(notifications)
			if w.cfg.Debug {
				ldbg("status updates processed in %v", elapsed)
			}
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
		}
	}
}
