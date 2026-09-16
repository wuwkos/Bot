package app

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot/models"

	"remnabot/internal/assets"
	"remnabot/internal/csqtt"
	"remnabot/internal/i18n"
	"remnabot/internal/model"
)

// csqttIssueMu сериализует выдачу доступов: проверка «уже есть tg_<id>» и
// создание идут парой List→Create, и двойной тап без замка давал бы два
// клиента на одного человека (а в панели всего 20 слотов).
var csqttIssueMu sync.Mutex

func (a *App) csqttCfg() model.CSQTTConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.botCfg == nil {
		return model.CSQTTConfig{}
	}
	return a.botCfg.CSQTT
}

// csqttEnabled сообщает, показывать ли кнопку выдачи обычным пользователям.
func (a *App) csqttEnabled() bool {
	cfg := a.csqttCfg()
	return cfg.Enabled && strings.TrimSpace(cfg.BaseURL) != "" &&
		strings.TrimSpace(cfg.User) != "" && cfg.Pass != ""
}

func csqttClientName(tgID int64) string {
	return fmt.Sprintf("tg_%d", tgID)
}

// csqttHost — хост для ссылки csqtt://: явный PublicHost из настроек, иначе
// имя хоста из BaseURL панели (без схемы и порта).
func csqttHost(cfg model.CSQTTConfig) string {
	if h := strings.TrimSpace(cfg.PublicHost); h != "" {
		h = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://"))
		if i := strings.Index(h, "/"); i >= 0 {
			h = h[:i]
		}
		if i := strings.Index(h, ":"); i >= 0 {
			h = h[:i]
		}
		if h != "" {
			return h
		}
	}
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(cfg.BaseURL)
	}
	return u.Hostname()
}

func csqttExpireText(expires int64, lang string) string {
	if expires <= 0 {
		return i18n.T(lang, "sub.no_expire")
	}
	return time.Unix(expires, 0).In(displayTZ).Format("02.01.2006 15:04") + " " + i18n.T(lang, "sub.tz_msk")
}

// showCsqttUser — пользовательский экран «Доступ к белым спискам» с отдельной
// кнопкой «Получить данные». «Назад» ведёт в хаб /vpn, откуда сюда пришли.
func (a *App) showCsqttUser(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	back := []models.InlineKeyboardButton{btn(i18n.T(lang, "btn.back"), "menu:vpn")}
	if !a.csqttEnabled() {
		a.sendKBSection(ctx, chatID, assets.SectionMySubscription,
			i18n.T(lang, "csqtt.off_user"),
			[][]models.InlineKeyboardButton{back, homeRow(lang)})
		return
	}
	a.sendKBSection(ctx, chatID, assets.SectionMySubscription,
		i18n.T(lang, "csqtt.user_title"),
		[][]models.InlineKeyboardButton{
			{btn(i18n.T(lang, "csqtt.btn_get"), "csq:get")},
			back,
			homeRow(lang),
		})
}

// issueCsqttAccess — сердце автовыдачи: имя tg_<id> → переиспользовать живой
// доступ → иначе скопировать порты из клиента-шаблона → создать с ПУСТЫМИ
// VK-хешами (их пользователь вводит сам в клиентском приложении) → собрать
// ссылку без хешей → отдать пароль + ссылку в чат.
func (a *App) issueCsqttAccess(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	cfg := a.csqttCfg()
	if !cfg.Enabled || strings.TrimSpace(cfg.BaseURL) == "" ||
		strings.TrimSpace(cfg.User) == "" || cfg.Pass == "" {
		a.sendKBSection(ctx, chatID, assets.SectionMySubscription,
			i18n.T(lang, "csqtt.off_user"),
			[][]models.InlineKeyboardButton{homeRow(lang)})
		return
	}

	a.sendKB(ctx, chatID, i18n.T(lang, "csqtt.working"),
		[][]models.InlineKeyboardButton{homeRow(lang)})

	csqttIssueMu.Lock()
	defer csqttIssueMu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	cli := csqtt.New(cfg.BaseURL, cfg.User, cfg.Pass)
	if err := cli.Login(cctx); err != nil {
		a.log.Warn("csqtt: login failed", "err", err)
		a.sendKBSection(cctx, chatID, assets.SectionMySubscription,
			i18n.T(lang, "csqtt.fail", i18n.T(lang, "csqtt.err_login")),
			[][]models.InlineKeyboardButton{
				{btn(i18n.T(lang, "csqtt.btn_retry"), "csq:get")},
				homeRow(lang),
			})
		return
	}
	list, err := cli.ListClients(cctx)
	if err != nil {
		a.log.Warn("csqtt: list failed", "err", err)
		a.sendKBSection(cctx, chatID, assets.SectionMySubscription,
			i18n.T(lang, "csqtt.fail", i18n.T(lang, "csqtt.err_list")),
			[][]models.InlineKeyboardButton{
				{btn(i18n.T(lang, "csqtt.btn_retry"), "csq:get")},
				homeRow(lang),
			})
		return
	}

	name := csqttClientName(chatID)
	nowUnix := time.Now().Unix()
	// 1. Свой живой доступ уже есть — отдаём его повторно, новый не создаём
	// (слоты панели ограничены 20, просроченные панель чистит сама).
	var own *csqtt.ClientInfo
	for i := range list {
		c := &list[i]
		if c.Name != name {
			continue
		}
		if !c.Active || (c.Expires != 0 && c.Expires <= nowUnix) {
			continue
		}
		if own == nil || c.Expires > own.Expires {
			cp := *c
			own = &cp
		}
	}
	host := csqttHost(cfg)
	if own != nil && own.Password != "" {
		peer := own.DTLSPort
		if peer == 0 {
			peer = 46000
		}
		// Ссылка всегда без хешей: их пользователь вводит сам в приложении.
		link := csqtt.BuildLink(host, own.Password, peer, "")
		a.sendCsqttReady(cctx, chatID, own.Password, link, own.Expires, true)
		return
	}

	// 2. Клиент-шаблон: копируем только порты. VK-хеши НЕ копируем и НЕ
	// подставляем: пользователь вводит их сам в клиентском приложении,
	// доступы создаются с пустым полем.
	var tpl *csqtt.ClientInfo
	tplName := strings.TrimSpace(cfg.TemplateName)
	if tplName != "" {
		for i := range list {
			if list[i].Name == tplName {
				cp := list[i]
				tpl = &cp
				break
			}
		}
		if tpl == nil {
			a.log.Warn("csqtt: template not found", "template", tplName)
			a.sendKBSection(cctx, chatID, assets.SectionMySubscription,
				i18n.T(lang, "csqtt.fail", i18n.T(lang, "csqtt.err_notemplate", html.EscapeString(tplName))),
				[][]models.InlineKeyboardButton{homeRow(lang)})
			return
		}
	} else {
		// Шаблон не задан — берём первый клиент как образец портов.
		if len(list) > 0 {
			cp := list[0]
			tpl = &cp
		}
	}

	hash, dtls, wg, local := "", 46000, 46001, 0
	if tpl != nil {
		if tpl.DTLSPort != 0 {
			dtls = tpl.DTLSPort
		}
		if tpl.WGPort != 0 {
			wg = tpl.WGPort
		}
		local = tpl.LocalPort
	}

	// 3. Создание доступа: пароль генерирует сама панель.
	res, err := cli.CreateClient(cctx, csqtt.CreateRequest{
		Name: name, Days: cfg.Days, Hash: hash,
		DTLSPort: dtls, WGPort: wg, LocalPort: local,
	})
	if err != nil {
		a.log.Warn("csqtt: create failed", "err", err, "user", chatID)
		msg := i18n.T(lang, "csqtt.err_create")
		if strings.Contains(err.Error(), "limit reached") {
			msg = i18n.T(lang, "csqtt.err_limit")
		}
		a.sendKBSection(cctx, chatID, assets.SectionMySubscription,
			i18n.T(lang, "csqtt.fail", msg),
			[][]models.InlineKeyboardButton{
				{btn(i18n.T(lang, "csqtt.btn_retry"), "csq:get")},
				homeRow(lang),
			})
		return
	}
	peer := res.DTLSPort
	if peer == 0 {
		peer = dtls
	}
	link := csqtt.BuildLink(host, res.Password, peer, "")
	a.log.Info("csqtt: access issued (no vk hashes)", "user", chatID, "name", name)
	a.sendCsqttReady(cctx, chatID, res.Password, link, res.Expires, false)
}

// sendCsqttReady отдаёт пароль + ссылку. Длинная ссылка уходит отдельным
// сообщением частями: в одну подпись Telegram её может не принять.
func (a *App) sendCsqttReady(ctx context.Context, chatID int64, password, link string, expires int64, reused bool) {
	lang := a.lang(chatID)
	key := "csqtt.done"
	if reused {
		key = "csqtt.done_reuse"
	}
	a.sendKBParts(ctx, chatID,
		[]string{i18n.T(lang, key, password, link, csqttExpireText(expires, lang))},
		[][]models.InlineKeyboardButton{homeRow(lang)})
}

// --- Админка: Система → CSQTT ---

func (a *App) showCsqttAdmin(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	cfg := a.csqttCfg()
	mark := "❌"
	if cfg.Enabled {
		mark = "✅"
	}
	masked := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return i18n.T(lang, "mn.not_set")
		}
		return "•••••"
	}
	days := cfg.Days
	daysText := i18n.T(lang, "csqtt.days_inf")
	if days > 0 {
		daysText = i18n.T(lang, "csqtt.days_n", days)
	}
	text := i18n.T(lang, "csqtt.adm_title", mark,
		displayOr(lang, cfg.BaseURL), displayOr(lang, cfg.User), masked(cfg.Pass),
		displayOr(lang, cfg.TemplateName), daysText, displayOr(lang, cfg.PublicHost))
	a.sendKBSection(ctx, chatID, assets.SectionAdminStats, text, [][]models.InlineKeyboardButton{
		{toggleBtn(lang, cfg.Enabled, "csq:toggle")},
		{btn(i18n.T(lang, "csqtt.btn_url"), "csq:url"), btn(i18n.T(lang, "csqtt.btn_login"), "csq:login")},
		{btn(i18n.T(lang, "csqtt.btn_pass"), "csq:pass"), btn(i18n.T(lang, "csqtt.btn_tpl"), "csq:tpl")},
		{btn(i18n.T(lang, "csqtt.btn_days"), "csq:days"), btn(i18n.T(lang, "csqtt.btn_host"), "csq:host")},
		{btn(i18n.T(lang, "csqtt.btn_check"), "csq:check")},
		navBack(lang, "menu:system"),
	})
}

func displayOr(lang, s string) string {
	if strings.TrimSpace(s) == "" {
		return i18n.T(lang, "admin.none")
	}
	return "<code>" + html.EscapeString(strings.TrimSpace(s)) + "</code>"
}

func (a *App) onCsqttAdmin(ctx context.Context, chatID int64, val string) {
	lang := a.lang(chatID)
	switch val {
	case "toggle":
		a.mu.Lock()
		if a.botCfg != nil {
			a.botCfg.CSQTT.Enabled = !a.botCfg.CSQTT.Enabled
		}
		a.mu.Unlock()
		_ = a.saveBotConfig(ctx)
		a.showCsqttAdmin(ctx, chatID)
	case "url":
		a.getUI(chatID).adminInput = "csqtt_url"
		a.askInput(ctx, chatID, i18n.T(lang, "csqtt.ask_url"), "menu:csqttadm")
	case "login":
		a.getUI(chatID).adminInput = "csqtt_login"
		a.askInput(ctx, chatID, i18n.T(lang, "csqtt.ask_login"), "menu:csqttadm")
	case "pass":
		a.getUI(chatID).adminInput = "csqtt_pass"
		a.askInput(ctx, chatID, i18n.T(lang, "csqtt.ask_pass"), "menu:csqttadm")
	case "tpl":
		a.getUI(chatID).adminInput = "csqtt_tpl"
		a.askInput(ctx, chatID, i18n.T(lang, "csqtt.ask_tpl"), "menu:csqttadm")
	case "days":
		a.getUI(chatID).adminInput = "csqtt_days"
		a.askInput(ctx, chatID, i18n.T(lang, "csqtt.ask_days"), "menu:csqttadm")
	case "host":
		a.getUI(chatID).adminInput = "csqtt_host"
		a.askInput(ctx, chatID, i18n.T(lang, "csqtt.ask_host"), "menu:csqttadm")
	case "check":
		a.checkCsqtt(ctx, chatID)
	case "get":
		a.issueCsqttAccess(ctx, chatID)
	}
}

// setCsqttField принимает ввод админа из handleAdminText.
func (a *App) setCsqttField(ctx context.Context, chatID int64, field, text string) {
	text = strings.TrimSpace(text)
	if text == "-" || text == "—" {
		text = ""
	}
	switch field {
	case "csqtt_url":
		if text != "" {
			text = normalizeBaseURL(text)
		}
		a.mu.Lock()
		if a.botCfg != nil {
			a.botCfg.CSQTT.BaseURL = text
		}
		a.mu.Unlock()
	case "csqtt_login":
		a.mu.Lock()
		if a.botCfg != nil {
			a.botCfg.CSQTT.User = text
		}
		a.mu.Unlock()
	case "csqtt_pass":
		a.mu.Lock()
		if a.botCfg != nil {
			a.botCfg.CSQTT.Pass = text
		}
		a.mu.Unlock()
	case "csqtt_tpl":
		a.mu.Lock()
		if a.botCfg != nil {
			a.botCfg.CSQTT.TemplateName = text
		}
		a.mu.Unlock()
	case "csqtt_days":
		n, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || n < 0 || n > 3650 {
			lang := a.lang(chatID)
			a.sendKB(ctx, chatID, i18n.T(lang, "csqtt.bad_days"),
				[][]models.InlineKeyboardButton{{btn(i18n.T(lang, "btn.cancel"), "inp:cancel")}})
			return
		}
		a.mu.Lock()
		if a.botCfg != nil {
			a.botCfg.CSQTT.Days = n
			a.botCfg.NormalizeCSQTT()
		}
		a.mu.Unlock()
	case "csqtt_host":
		a.mu.Lock()
		if a.botCfg != nil {
			a.botCfg.CSQTT.PublicHost = text
		}
		a.mu.Unlock()
	default:
		return
	}
	a.getUI(chatID).adminInput = ""
	_ = a.saveBotConfig(ctx)
	a.showCsqttAdmin(ctx, chatID)
}

// checkCsqtt — проверка связи: логин + подсчёт клиентов + поиск шаблона.
func (a *App) checkCsqtt(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	cfg := a.csqttCfg()
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.User) == "" || cfg.Pass == "" {
		a.sendKBSection(ctx, chatID, assets.SectionAdminStats,
			i18n.T(lang, "csqtt.check_nocfg"),
			[][]models.InlineKeyboardButton{navBack(lang, "menu:system")})
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cli := csqtt.New(cfg.BaseURL, cfg.User, cfg.Pass)
	if err := cli.Login(cctx); err != nil {
		a.sendKBSection(ctx, chatID, assets.SectionAdminStats,
			i18n.T(lang, "csqtt.check_fail", html.EscapeString(err.Error())),
			[][]models.InlineKeyboardButton{navBack(lang, "menu:system")})
		return
	}
	list, err := cli.ListClients(cctx)
	if err != nil {
		a.sendKBSection(ctx, chatID, assets.SectionAdminStats,
			i18n.T(lang, "csqtt.check_fail", html.EscapeString(err.Error())),
			[][]models.InlineKeyboardButton{navBack(lang, "menu:system")})
		return
	}
	tplMark := i18n.T(lang, "csqtt.check_tpl_skip")
	if tpl := strings.TrimSpace(cfg.TemplateName); tpl != "" {
		found := false
		for i := range list {
			if list[i].Name == tpl {
				found = true
				break
			}
		}
		if found {
			tplMark = i18n.T(lang, "csqtt.check_tpl_ok", html.EscapeString(tpl))
		} else {
			tplMark = i18n.T(lang, "csqtt.check_tpl_bad", html.EscapeString(tpl))
		}
	}
	a.sendKBSection(ctx, chatID, assets.SectionAdminStats,
		i18n.T(lang, "csqtt.check_ok", len(list), tplMark, html.EscapeString(csqttHost(cfg))),
		[][]models.InlineKeyboardButton{navBack(lang, "menu:system")})
}
