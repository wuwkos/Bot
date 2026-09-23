package app

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"remnabot/internal/assets"
	"remnabot/internal/i18n"
	"remnabot/internal/model"
	"remnabot/internal/remnawave"
)

//go:embed banner_default.jpg
var defaultBanner []byte

var botEmojis = []struct{ E, Use string }{
	{"👋", "приветствие на /start"},
	{"✅", "подтверждение: «Я оплатил», активация подписки, доступ"},
	{"❌", "отказ оплаты, ошибка, кнопка «Закрыть»"},
	{"⏳", "«запускаю обновление…» и другие процессы"},
	{"🕒", "«скриншот получен, ожидайте подтверждения»"},
	{"🔒", "P2P: нужно одобрение администратора"},
	{"📸", "просьба прислать скриншот оплаты"},
	{"💳", "кнопка «Купить», карта в P2P, методы оплаты"},
	{"📦", "выбор тарифа, «моя подписка»"},
	{"📭", "пусто: «нет активных подписок», «тарифы не настроены»"},
	{"🙏", "«способы оплаты пока не настроены»"},
	{"🔥", "подсказка «чаще всего выбирают X мес»"},
	{"⭐", "оплата через Telegram Stars"},
	{"🎁", "триал, кнопка «🎁 Триал», уведомление об активации"},
	{"🏠", "кнопка «На главную»"},
	{"📲", "кнопка «Мои подписки»"},
	{"👥", "кнопка «Группа» на главной у юзера"},
	{"🛟", "кнопка «Поддержка» на главной у юзера"},
	{"📜", "документы сервиса: соглашение и политика конфиденциальности"},
	{"🚪", "кнопка «Не сейчас» на экране согласия"},
}

func (a *App) botLang() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.botCfg != nil && a.botCfg.Language != "" {
		return a.botCfg.Language
	}
	return i18n.Fallback
}

func displayName(first, username string) string {
	if first != "" {
		return escapeName(first)
	}
	if username != "" {
		return "@" + escapeName(username)
	}
	return "друг"
}

func userLabel(u *model.User) string {
	id := strconv.FormatInt(u.TelegramID, 10)
	nick := ""
	switch {
	case u.Username != "":
		nick = "@" + escapeName(u.Username)
	case u.FirstName != "":
		nick = escapeName(u.FirstName)
	}
	if nick == "" {
		return id
	}
	return nick + " (" + id + ")"
}

func (a *App) userLabelByID(ctx context.Context, id int64) string {
	if a.store != nil {
		// Карточка читается ОДИН раз: подпись рисуется в списках пользователей
		// построчно, и второе чтение на строку удваивало бы запросы к базе.
		u, _ := a.store.GetUser(ctx, id)
		// У аккаунта кабинета нет ни имени, ни @username — подписываем почтой.
		// Проверяется не знак идентификатора, а наличие имени: после привязки
		// Telegram аккаунт положительный, и подписывать его почтой вместо имени
		// уже неправильно.
		if u == nil || (u.Username == "" && u.FirstName == "") {
			if wu, _ := a.store.GetWebUserByTgID(ctx, id); wu != nil && wu.Email != "" {
				// Эскейп обязателен: e-mail — свободный ввод при регистрации в
				// кабинете и попадает в сообщения с ParseModeHTML.
				return "📧 " + escapeName(wu.Email)
			}
		}
		if u != nil {
			return userLabel(u)
		}
	}
	return strconv.FormatInt(id, 10)
}

const subCacheTTL = 30

func (a *App) userHasSub(ctx context.Context, chatID int64) bool {

	a.subMu.Lock()
	if a.subCache != nil {
		if e, ok := a.subCache[chatID]; ok && time.Now().Before(e.expireAt) {
			a.subMu.Unlock()
			return e.has
		}
	}
	a.subMu.Unlock()

	a.mu.Lock()
	panel := a.panel
	a.mu.Unlock()
	if panel == nil {
		return false
	}
	_, _, _, has, err := panel.SubscriptionState(ctx, chatID)
	if err != nil {
		// Панель молчит — это НЕ «подписки нет». Отрицательный ответ при
		// аварии кэшировался на полминуты и переживал возвращение панели:
		// платящий клиент полминуты видел «у вас нет подписок, купите».
		// Отдаём последнее известное значение и ничего не запоминаем.
		a.subMu.Lock()
		last := false
		if a.subCache != nil {
			if e, ok := a.subCache[chatID]; ok {
				last = e.has
			}
		}
		a.subMu.Unlock()
		return last
	}

	a.subMu.Lock()
	if a.subCache == nil {
		a.subCache = map[int64]subCacheEntry{}
	}
	a.subCache[chatID] = subCacheEntry{has: has, expireAt: time.Now().Add(subCacheTTL * time.Second)}
	a.subMu.Unlock()
	return has
}

func (a *App) invalidateSubCache(chatID int64) {
	a.subMu.Lock()
	defer a.subMu.Unlock()
	if a.subCache != nil {
		delete(a.subCache, chatID)
	}
}

// userKeyboardLabels — постоянные reply-кнопки снизу: «Подключить VPN»,
// под ней в ряд «Главное меню» и «Помощь». Набор ОДИН на все стадии и экраны
// (исключение — оферта: до согласия клавиатуры нет вовсе), поэтому здесь не
// должно быть ничего, что зависит от состояния пользователя. Каждая подпись
// обязана разбираться в userCommandKey — иначе кнопка немая.
func userKeyboardLabels(lang string) [][]string {
	return [][]string{
		{i18n.T(lang, "rk.vpn")},
		{i18n.T(lang, "rk.menu"), i18n.T(lang, "rk.help")},
	}
}

// userReplyMarkup — постоянные reply-кнопки как Telegram-разметка. Крепится к
// баннеру приветствия: persistent, переживает нажатия, скрывается штатным
// сворачиванием клиента. Кнопки всегда одни и те же, независимо от экрана.
func userReplyMarkup(lang string) models.ReplyKeyboardMarkup {
	var kb [][]models.KeyboardButton
	for _, r := range userKeyboardLabels(lang) {
		var row []models.KeyboardButton
		for _, b := range r {
			row = append(row, models.KeyboardButton{Text: b})
		}
		kb = append(kb, row)
	}
	return models.ReplyKeyboardMarkup{
		Keyboard:       kb,
		ResizeKeyboard: true,
		IsPersistent:   true,
	}
}

// Состояния подписки для экранов.
const (
	subStNone    = 0 // панель не знает такого пользователя
	subStActive  = 1 // живая подписка
	subStUnknown = 2 // панель недоступна
	subStDead    = 3 // истекла / трафик исчерпан / заблокирована
)

// subDeadReason — почему подписку нельзя считать живой: "blocked", "limited",
// "expired" или "" (жива). Статус проверяется первым, но и дата не лишняя:
// панель могла ещё не пересчитать статус, а срок уже в прошлом.
func subDeadReason(status, expireAt string) string {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case remnawave.StatusDisabled:
		return "blocked"
	case remnawave.StatusLimited:
		return "limited"
	case remnawave.StatusExpired:
		return "expired"
	}
	if t, err := time.Parse(time.RFC3339, expireAt); err == nil && !t.After(time.Now()) {
		return "expired"
	}
	return ""
}

// vpnSubState — состояние подписки: найдена ли, жива ли, срок, ссылка и
// причина «мертва». Живой считается только незаблокированная подписка с
// неистёкшим сроком: раньше экран показывал «🟢 Активен» всем, кого панель
// вообще нашла, включая просроченных.
func (a *App) vpnSubState(ctx context.Context, chatID int64) (int, string, string, string) {
	a.mu.Lock()
	panel := a.panel
	a.mu.Unlock()
	if panel == nil {
		return subStNone, "", "", ""
	}
	url, expireAt, status, ok, err := panel.SubscriptionState(ctx, chatID)
	if err != nil {
		return subStUnknown, "", "", ""
	}
	if !ok {
		return subStNone, "", "", ""
	}
	if reason := subDeadReason(status, expireAt); reason != "" {
		// Мёртвой подписке ссылку не показываем: она не работает.
		return subStDead, expireAt, "", reason
	}
	return subStActive, expireAt, url, ""
}

// subDaysLeft — остаток дней по сроку панели, -1 если срок не разобрать.
func subDaysLeft(expireAt string) int {
	if expireAt == "" {
		return -1
	}
	t, err := time.Parse(time.RFC3339, expireAt)
	if err != nil {
		return -1
	}
	return daysUntil(t, time.Now().UTC())
}

// showGreeting — экран /start: приветствие с reply-кнопками.
//
// Reply-клавиатура крепится прямо к баннеру приветствия — отдельного
// сообщения-носителя нет, а баннер НЕ удаляется при навигации (sendBannerKeep):
// некоторые клиенты прячут reply-кнопки вместе с удалённым сообщением.
func (a *App) showGreeting(ctx context.Context, chatID int64, name string) {
	photo, caption, ents := a.welcomeContent(name)
	lang := a.lang(chatID)
	a.sendBannerKeep(ctx, chatID, photo, caption, ents, userReplyMarkup(lang))
}

// showUserMenu — экран /menu: ID, статус подписки, кнопки разделов.
func (a *App) showUserMenu(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	var status string
	switch st, expire, _, reason := a.subStateFor(ctx, chatID); st {
	case subStActive:
		if days := subDaysLeft(expire); days >= 0 {
			status = i18n.T(lang, "ustatus.have", formatExpire(expire, lang), days)
		} else {
			status = i18n.T(lang, "ustatus.have_plain", formatExpire(expire, lang))
		}
	case subStDead:
		switch reason {
		case "blocked":
			status = i18n.T(lang, "ustatus.blocked")
		case "limited":
			status = i18n.T(lang, "ustatus.limited", formatExpire(expire, lang))
		default:
			status = i18n.T(lang, "ustatus.expired", formatExpire(expire, lang))
		}
	case subStUnknown:
		status = i18n.T(lang, "ustatus.unknown")
	default:
		status = i18n.T(lang, "ustatus.none")
	}
	rows := [][]models.InlineKeyboardButton{
		{btn(i18n.T(lang, "btn.my_vpn"), "menu:vpn")},
		{btn(i18n.T(lang, "btn.referral"), "menu:ref")},
	}
	// Канал — над «Помощью», как заказано. Кнопка-ссылка на адрес из админки
	// (Контакты → группа); ссылки нет — кнопка остаётся и отвечает, что канал
	// ещё не настроен, чтобы не выглядеть мёртвой.
	rows = append(rows, a.channelRow(lang))
	rows = append(rows, []models.InlineKeyboardButton{btn(i18n.T(lang, "btn.help"), "menu:info")})
	if row := a.miniAppButtonRow(lang); row != nil {
		rows = append(rows, row)
	}
	if row := a.legalMenuRow(lang); row != nil {
		rows = append(rows, row)
	}
	a.sendKBSection(ctx, chatID, assets.SectionMainMenu, i18n.T(lang, "umenu.title", chatID, status), rows)
}

// channelRow — кнопка «Наш канал»: ссылка из админки, а если канал не задан —
// кнопка-заглушка с уведомлением (ch:none), чтобы она всегда была на месте.
func (a *App) channelRow(lang string) []models.InlineKeyboardButton {
	if group := a.groupURL(); group != "" {
		return []models.InlineKeyboardButton{{Text: i18n.T(lang, "info.channel"), URL: group}}
	}
	return []models.InlineKeyboardButton{btn(i18n.T(lang, "info.channel"), "ch:none")}
}

// daysWord — «1 день» / «3 дня» / «30 дней» для текстов.
func daysWord(lang string, days int) string {
	if lang == model.LangEN {
		if days == 1 {
			return "1 day"
		}
		return strconv.Itoa(days) + " days"
	}
	switch {
	case days%10 == 1 && days%100 != 11:
		return strconv.Itoa(days) + " день"
	case days%10 >= 2 && days%10 <= 4 && (days%100 < 12 || days%100 > 14):
		return strconv.Itoa(days) + " дня"
	default:
		return strconv.Itoa(days) + " дней"
	}
}

// vpnPlanLine — строка «Тариф: …» для активной подписки. Длительность берётся
// из снимка сделки (plan_snapshot): у триала снимка нет — называем его как есть.
func (a *App) vpnPlanLine(ctx context.Context, chatID int64, lang string) string {
	if a.store == nil {
		return ""
	}
	u, _ := a.store.GetUser(ctx, chatID)
	if u == nil {
		return ""
	}
	if u.NotifyKind == "trial" {
		return i18n.T(lang, "vpn.plan_trial")
	}
	if u.Snapshot == nil {
		return ""
	}
	days := u.Snapshot.Days
	if days == 0 && u.Snapshot.Months > 0 {
		days = u.Snapshot.Months * 30
	}
	if days <= 0 {
		return ""
	}
	return i18n.T(lang, "vpn.plan", daysWord(lang, days))
}

// localSubTrace — есть ли у пользователя локальный след подписки или триала
// (платил/пробовал в этом боте). Когда панель недоступна, этот след —
// единственный способ отличить «точно без подписки» от «не знаем»: новичку
// без следа можно смело показывать обычный вход, платившему — нет.
func (a *App) localSubTrace(ctx context.Context, chatID int64) bool {
	if a.store == nil {
		return false
	}
	u, _ := a.store.GetUser(ctx, chatID)
	if u == nil {
		return false
	}
	return u.SubExpireAt != "" || u.TrialUsedAt != "" || u.NotifyKind != ""
}

// subStateFor — состояние подписки для ЭКРАНОВ с поправкой на аварию панели:
// панель молчит, а следа подписки/триала у человека нет — он точно ничего не
// покупал, и статус «не удалось проверить» его только пугает. Считаем его
// обычным «без подписки». Платившим остаётся честный статус-неизвестен.
func (a *App) subStateFor(ctx context.Context, chatID int64) (int, string, string, string) {
	st, expire, url, reason := a.vpnSubState(ctx, chatID)
	if st == subStUnknown && !a.localSubTrace(ctx, chatID) {
		return subStNone, "", "", ""
	}
	return st, expire, url, reason
}

// showVPN — экран «Мой VPN»: статус подписки и действия по нему.
//
// Три состояния: активная подписка (подключение, белые списки, продление),
// подписки нет (бесплатный триал, если он ещё доступен, и покупка) и панель
// недоступна (ничего не продаём: предложение купить при аварии подтолкнуло бы
// платящего клиента оплатить второй раз).
func (a *App) showVPN(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	// Возвратный гейт: после «Принимаю» человек должен увидеть хаб, а не
	// витрину — он ничего не покупал, он подключался.
	if a.legalGateBack(ctx, chatID, "vpn") {
		return
	}
	title := i18n.T(lang, "vpn.title_named", a.brandName(lang))
	var head string
	var rows [][]models.InlineKeyboardButton
	switch st, expire, url, reason := a.subStateFor(ctx, chatID); st {
	case subStActive:
		head = title + "\n" + i18n.T(lang, "vpn.status_on")
		if line := a.vpnPlanLine(ctx, chatID, lang); line != "" {
			head += "\n" + line
		}
		if days := subDaysLeft(expire); days >= 0 {
			head += "\n" + i18n.T(lang, "vpn.until", daysWord(lang, days))
		} else if expire != "" {
			head += "\n" + i18n.T(lang, "vpn.until", formatExpire(expire, lang))
		}
		if url = a.rewriteSub(url); url != "" {
			head += "\n" + i18n.T(lang, "vpn.sub_link", "<code>"+html_(url)+"</code>")
		}
		rows = [][]models.InlineKeyboardButton{
			{btn(i18n.T(lang, "vpn.btn_connect"), "menu:mysubs")},
		}
		// Кнопка режима CSQTT — только если выдача включена и настроена в
		// админке. Иначе кнопки нет вообще (не заглушка).
		if a.csqttEnabled() {
			rows = append(rows, []models.InlineKeyboardButton{btn(i18n.T(lang, "vpn.btn_whitelist"), "menu:csqtt")})
		}
		rows = append(rows, []models.InlineKeyboardButton{btn(i18n.T(lang, "btn.renew"), "menu:renew")})
	case subStDead:
		// Просроченная, исчерпанная или заблокированная подписка — НЕ активна.
		// Ссылку не показываем (не работает), первое действие — продление;
		// при блокировке администратором продлевать нечего — ведём в поддержку.
		head = title + "\n" + i18n.T(lang, "vpn.status_"+reason)
		if line := a.vpnPlanLine(ctx, chatID, lang); line != "" {
			head += "\n" + line
		}
		if reason == "blocked" {
			rows = [][]models.InlineKeyboardButton{
				{btn(i18n.T(lang, "btn.help"), "menu:info")},
			}
		} else {
			head += "\n\n" + i18n.T(lang, "vpn.dead_hint")
			rows = [][]models.InlineKeyboardButton{
				{btn(i18n.T(lang, "btn.renew"), "menu:renew")},
				{btn(i18n.T(lang, "vpn.btn_connect"), "menu:mysubs")},
			}
			if a.csqttEnabled() {
				rows = append(rows, []models.InlineKeyboardButton{btn(i18n.T(lang, "vpn.btn_whitelist"), "menu:csqtt")})
			}
		}
	case subStUnknown:
		head = title + "\n" + i18n.T(lang, "vpn.status_unknown")
		rows = [][]models.InlineKeyboardButton{
			{btn(i18n.T(lang, "vpn.btn_connect"), "menu:mysubs")},
		}
		if a.csqttEnabled() {
			rows = append(rows, []models.InlineKeyboardButton{btn(i18n.T(lang, "vpn.btn_whitelist"), "menu:csqtt")})
		}
	default:
		if a.trialAvailable(ctx, chatID) {
			head = title + "\n" + i18n.T(lang, "vpn.status_new") + "\n" + i18n.T(lang, "vpn.try_free")
			rows = [][]models.InlineKeyboardButton{
				{btn(i18n.T(lang, "vpn.btn_try"), "menu:trial")},
				{btn(i18n.T(lang, "vpn.btn_buy_sub"), "menu:buy")},
			}
		} else {
			head = title + "\n" + i18n.T(lang, "vpn.status_off")
			rows = [][]models.InlineKeyboardButton{
				{btn(i18n.T(lang, "vpn.btn_buy"), "menu:buy")},
			}
		}
	}
	rows = append(rows, homeRow(lang))
	a.sendKBSection(ctx, chatID, assets.SectionMySubscription, head, rows)
}

// showInfo — экран «Помощь»: контакт поддержки и оферта ссылкой.
func (a *App) showInfo(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	a.mu.Lock()
	support := ""
	if a.botCfg != nil {
		support = a.botCfg.Contact.SupportURL
	}
	a.mu.Unlock()
	supportLine := i18n.T(lang, "info.no_support")
	if strings.TrimSpace(support) != "" {
		supportLine = i18n.T(lang, "info.support", support)
	}
	text := i18n.T(lang, "info.title") + "\n\n" + supportLine
	var rows [][]models.InlineKeyboardButton
	// Оба документа из конфига: текстовые открываются в боте, со ссылкой —
	// ведут на страницу. Что не задано, того нет.
	rows = append(rows, legalDocRows(lang, a.legalCfg().Docs())...)
	rows = append(rows, a.contactRows()...)
	rows = append(rows, homeRow(lang))
	a.sendKBSection(ctx, chatID, assets.SectionMainMenu, text, rows)
}

func (a *App) renewEligible(ctx context.Context, chatID int64) bool {
	if a.store == nil {
		return false
	}
	u, _ := a.store.GetUser(ctx, chatID)
	if u == nil {
		return false
	}
	if u.NotifyKind == "trial" {
		return true
	}
	if u.SubExpireAt == "" {
		return false
	}
	exp, err := time.Parse(time.RFC3339, u.SubExpireAt)
	if err != nil {
		return false
	}
	return daysUntil(exp, time.Now().UTC()) <= 7
}

func (a *App) contactRows() [][]models.InlineKeyboardButton {
	a.mu.Lock()
	g, sup := "", ""
	if a.botCfg != nil {
		g, sup = a.botCfg.Contact.GroupURL, a.botCfg.Contact.SupportURL
	}
	lang := i18n.Fallback
	if a.botCfg != nil && a.botCfg.Language != "" {
		lang = a.botCfg.Language
	}
	a.mu.Unlock()
	// Второй рубеж: адрес из конфига мог попасть туда до проверки при вводе
	// (старая установка) или из импорта. Битая кнопка отвергает всё сообщение
	// целиком, поэтому лучше показать меню без неё, чем не показать вовсе.
	var row []models.InlineKeyboardButton
	if validButtonURL(g) {
		row = append(row, models.InlineKeyboardButton{Text: i18n.T(lang, "btn.group"), URL: g})
	}
	if validButtonURL(sup) {
		row = append(row, models.InlineKeyboardButton{Text: i18n.T(lang, "btn.support"), URL: sup})
	}
	if len(row) == 0 {
		return nil
	}
	return [][]models.InlineKeyboardButton{row}
}

// sendPayKB renders a Sales sub-screen on the parent "Продажи" banner so that
// navigation within the category edits the caption in place (no delete+resend).
func (a *App) sendPayKB(ctx context.Context, chatID int64, text string, rows [][]models.InlineKeyboardButton) {
	a.sendKBSection(ctx, chatID, assets.SectionBuySubscription, text, rows)
}

func (a *App) sendSysKB(ctx context.Context, chatID int64, text string, rows [][]models.InlineKeyboardButton) {
	a.sendKBSection(ctx, chatID, assets.SectionAdminStats, text, rows)
}

func (a *App) sendIfaceKB(ctx context.Context, chatID int64, text string, rows [][]models.InlineKeyboardButton) {
	a.sendKBSection(ctx, chatID, assets.SectionMainMenu, text, rows)
}

func (a *App) sendMktKB(ctx context.Context, chatID int64, text string, rows [][]models.InlineKeyboardButton) {
	a.sendKBSection(ctx, chatID, assets.SectionPromoCode, text, rows)
}

// sendUsrKB renders an admin Users screen on the shared Users/referral banner
// so navigation within the section edits the caption in place.
func (a *App) sendUsrKB(ctx context.Context, chatID int64, text string, rows [][]models.InlineKeyboardButton) {
	a.sendKBSection(ctx, chatID, assets.SectionReferral, text, rows)
}

func homeRow(lang string) []models.InlineKeyboardButton {
	return []models.InlineKeyboardButton{btn(i18n.T(lang, "btn.home"), "menu:home")}
}

// toggleBtn renders an on/off button whose label reflects the current state,
// so the admin sees the state without reading the title.
func toggleBtn(lang string, on bool, cb string) models.InlineKeyboardButton {
	key := "btn.toggle_off"
	if on {
		key = "btn.toggle_on"
	}
	return btn(i18n.T(lang, key), cb)
}

func navBack(lang, backCB string) []models.InlineKeyboardButton {
	return []models.InlineKeyboardButton{
		btn(i18n.T(lang, "btn.back"), backCB),
		btn(i18n.T(lang, "btn.home"), "menu:home"),
	}
}

// paginationRow builds a prev/next navigation row. prefix is the callback prefix
// up to and including the trailing separator (e.g. "usr:page:"); the target page
// index is appended. Returns nil when there is only a single page.
func paginationRow(prefix string, page, pages int, prevLabel, nextLabel string) []models.InlineKeyboardButton {
	var nav []models.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, btn(prevLabel, prefix+strconv.Itoa(page-1)))
	}
	if page+1 < pages {
		nav = append(nav, btn(nextLabel, prefix+strconv.Itoa(page+1)))
	}
	return nav
}

func (a *App) adminMenuRows(lang string) [][]models.InlineKeyboardButton {
	return [][]models.InlineKeyboardButton{
		{btn(i18n.T(lang, "menu.cat_pay"), "menu:pay"), btn(i18n.T(lang, "menu.cat_marketing"), "menu:marketing")},
		{btn(i18n.T(lang, "menu.cat_iface"), "menu:iface"), btn(i18n.T(lang, "btn.users"), "menu:users")},
		{btn(i18n.T(lang, "menu.cat_system"), "menu:system"), btn(i18n.T(lang, "btn.storefront"), "menu:buy")},
	}
}

func (a *App) showIface(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	a.sendKBSection(ctx, chatID, assets.SectionMainMenu, i18n.T(lang, "menu.iface_title"), [][]models.InlineKeyboardButton{
		{btn(i18n.T(lang, "btn.banner"), "menu:welcome"), btn(i18n.T(lang, "btn.emoji"), "menu:emoji")},
		{btn(i18n.T(lang, "btn.section_banners"), "menu:welcome_sections")},
		{btn(i18n.T(lang, "btn.contacts"), "menu:contacts")},
		{btn(i18n.T(lang, "btn.devices_admin"), "menu:devices")},
		{btn(i18n.T(lang, "btn.service_name") + ": " + a.serviceNameDisplay(lang), "menu:svcname")},
		{btn(i18n.T(lang, "btn.bot_lang")+": "+i18n.T(lang, "lang.name_"+lang), "menu:botlang")},
		homeRow(lang),
	})
}

// serviceName — название сервиса из админки ("" — не задано).
func (a *App) serviceName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.botCfg == nil {
		return ""
	}
	return strings.TrimSpace(a.botCfg.ServiceName)
}

// serviceNameDisplay — название как есть для подписей кнопок и экранов
// (без экранирования: текст кнопок Telegram разметкой не разбирает).
func (a *App) serviceNameDisplay(lang string) string {
	if n := a.serviceName(); n != "" {
		return n
	}
	return i18n.T(lang, "brand.default")
}

// brandName — название сервиса для HTML-текстов: заданное админом или
// стандартное «VPN». Экранируется — подставляется внутрь разметки.
func (a *App) brandName(lang string) string {
	if n := a.serviceName(); n != "" {
		return html.EscapeString(n)
	}
	return i18n.T(lang, "brand.default")
}

// serviceNameRe — что вообще может быть названием сервиса: буквы, цифры,
// пробел и обычная пунктуация брендов. Разметка, переносы строк и эмодзи
// отсекаются здесь, а не в момент подстановки.
var serviceNameRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._+\-]{0,31}$`)

// sanitizeServiceName проверяет ввод админа: ok=false — название не подходит.
// Пустое и «-» означают «вернуть стандартное».
func sanitizeServiceName(raw string) (name string, ok bool) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "-" || s == "—" {
		return "", true
	}
	if !serviceNameRe.MatchString(s) {
		return "", false
	}
	return s, true
}

// showServiceName — админский экран названия сервиса.
func (a *App) showServiceName(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	rows := [][]models.InlineKeyboardButton{
		{btn(i18n.T(lang, "svc.btn_edit"), "svc:edit"), btn(i18n.T(lang, "svc.btn_reset"), "svc:reset")},
		navBack(lang, "menu:iface"),
	}
	body := i18n.T(lang, "svc.title") +
		i18n.T(lang, "svc.current", html.EscapeString(a.serviceNameDisplay(lang))) +
		i18n.T(lang, "svc.hint", a.serviceNameDisplay(lang))
	a.sendIfaceKB(ctx, chatID, body, rows)
}

// applyServiceName сохраняет введённое название. Мусор не принимаем: экран
// остаётся в режиме ввода и повторяет вопрос.
func (a *App) applyServiceName(ctx context.Context, chatID int64, text string) {
	lang := a.lang(chatID)
	name, ok := sanitizeServiceName(text)
	if !ok {
		a.askInput(ctx, chatID, i18n.T(lang, "svc.bad"), "menu:svcname")
		return
	}
	a.getUI(chatID).adminInput = ""
	a.mu.Lock()
	if a.botCfg != nil {
		a.botCfg.ServiceName = name
	}
	a.mu.Unlock()
	_ = a.saveBotConfig(ctx)
	a.showServiceName(ctx, chatID)
}

// onServiceNameAdmin — кнопки админского экрана названия сервиса.
func (a *App) onServiceNameAdmin(ctx context.Context, chatID int64, val string) {
	lang := a.lang(chatID)
	switch val {
	case "edit":
		a.getUI(chatID).adminInput = "svc_name"
		a.askInput(ctx, chatID, i18n.T(lang, "svc.ask"), "menu:svcname")
	case "reset":
		a.getUI(chatID).adminInput = ""
		a.mu.Lock()
		if a.botCfg != nil {
			a.botCfg.ServiceName = ""
		}
		a.mu.Unlock()
		_ = a.saveBotConfig(ctx)
		a.showServiceName(ctx, chatID)
	}
}

// showBotLang — выбор языка бота.
//
// До этого язык задавался только на первом шаге первичного мастера, и сменить
// его после установки было нельзя вообще ничем: переустановка стартует сразу с
// выбора базы, а обработчик языка живёт только при активном мастере.
func (a *App) showBotLang(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	a.sendKBSection(ctx, chatID, assets.SectionMainMenu, i18n.T(lang, "lang.title"), [][]models.InlineKeyboardButton{
		{btn("🇷🇺 "+i18n.T(lang, "lang.name_ru"), "botlang:ru"), btn("🇬🇧 "+i18n.T(lang, "lang.name_en"), "botlang:en")},
		{btn(i18n.T(lang, "btn.back"), "menu:iface"), btn(i18n.T(lang, "btn.home"), "menu:home")},
	})
}

// setBotLang меняет язык бота и сразу перерисовывает экран уже на новом.
func (a *App) setBotLang(ctx context.Context, chatID int64, code string) {
	if code != "ru" && code != "en" {
		return
	}
	a.mu.Lock()
	if a.botCfg != nil {
		a.botCfg.Language = code
	}
	a.mu.Unlock()
	_ = a.saveBotConfig(ctx)
	a.showBotLang(ctx, chatID)
}

func (a *App) showPay(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	a.mu.Lock()
	p2pOn, starsOn, ykOn, cbOn, plOn, hlOn, trbOn := false, false, false, false, false, false, false
	strat := "MONTH"
	addsubOn, addsubGB, addsubInt := false, 0, 0
	if a.botCfg != nil {
		p2pOn = a.botCfg.P2P.Enabled
		starsOn = a.botCfg.Stars.Enabled
		ykOn = a.botCfg.YooKassa.Enabled
		cbOn = a.botCfg.CryptoBot.Enabled
		plOn = a.botCfg.Platega.Enabled
		hlOn = a.botCfg.Heleket.Enabled
		trbOn = a.botCfg.Tribute.Enabled
		strat = a.botCfg.Pricing.ResetStrategy()
		addsubOn = a.botCfg.AddSub.Enabled
		addsubGB = a.botCfg.AddSub.TrafficGB
		addsubInt = len(a.botCfg.AddSub.InternalSquads)
	}
	a.mu.Unlock()
	mark := func(on bool) string {
		if on {
			return "✅"
		}
		return "❌"
	}
	internalCSV, externalName := a.squadDisplay(ctx)
	title := i18n.T(lang, "subsetup.title",
		mark(p2pOn), mark(starsOn), mark(ykOn), mark(cbOn), mark(plOn), mark(hlOn), mark(trbOn),
		a.formatTrafficLimits(), a.formatDeviceLimits(lang), strat,
		internalCSV, externalName,
	)
	if addsubOn {
		traffic := i18n.T(lang, "addsub.unlimited")
		if addsubGB > 0 {
			traffic = strconv.Itoa(addsubGB) + " GB"
		}
		title += i18n.T(lang, "subsetup.addsub_block", traffic, addsubInt)
	}
	a.sendKBSection(ctx, chatID, assets.SectionBuySubscription, title, [][]models.InlineKeyboardButton{
		{btn(i18n.T(lang, "subsetup.btn_quick"), "prc:quick"), btn(i18n.T(lang, "subsetup.btn_manual"), "menu:pricing")},
		{btn(i18n.T(lang, "btn.plans"), "menu:plans")},
		{btn(i18n.T(lang, "btn.trial_admin"), "menu:trial"), btn(i18n.T(lang, "btn.squads"), "menu:squads")},
		{btn(i18n.T(lang, "btn.addsub"), "menu:addsub")},
		{btn(i18n.T(lang, "btn.p2p"), "menu:p2p"), btn(i18n.T(lang, "btn.stars"), "menu:stars")},
		{btn(i18n.T(lang, "btn.yookassa"), "menu:yookassa"), btn(i18n.T(lang, "btn.cryptobot"), "menu:cryptobot")},
		{btn(i18n.T(lang, "btn.platega"), "menu:platega"), btn(i18n.T(lang, "btn.heleket"), "menu:heleket")},
		{btn(i18n.T(lang, "btn.tribute"), "menu:tribute")},
		{btn(i18n.T(lang, "btn.wallet"), "menu:wallet")},
		{btn(i18n.T(lang, "btn.payments"), "menu:payments"), btn(i18n.T(lang, "btn.analytics"), "menu:analytics")},
		{btn(i18n.T(lang, "btn.moynalog"), "menu:moynalog")},
		homeRow(lang),
	})
}

func (a *App) showMarketing(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	a.sendKBSection(ctx, chatID, assets.SectionPromoCode, i18n.T(lang, "menu.marketing_title"), [][]models.InlineKeyboardButton{
		{btn(i18n.T(lang, "btn.promo_admin"), "menu:promoadmin"), btn(i18n.T(lang, "btn.referral_admin"), "menu:refadmin")},
		{btn(i18n.T(lang, "btn.broadcast"), "menu:broadcast"), btn(i18n.T(lang, "btn.notify"), "menu:notify")},
		homeRow(lang),
	})
}

func (a *App) showSystem(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	a.mu.Lock()
	updOn := a.botCfg != nil && a.botCfg.UpdateCheck.Enabled
	a.mu.Unlock()
	updLabel := i18n.T(lang, "btn.upd_notify_off")
	if updOn {
		updLabel = i18n.T(lang, "btn.upd_notify_on")
	}
	rows := [][]models.InlineKeyboardButton{
		{btn(i18n.T(lang, "btn.update"), "menu:update"), btn(i18n.T(lang, "btn.check_update"), "upd:check")},
		{btn(updLabel, "upd:toggle"), btn(i18n.T(lang, "btn.channel")+": "+a.channelName(lang), "upd:chan")},
		{btn(i18n.T(lang, "btn.status"), "menu:status"), btn(i18n.T(lang, "btn.apilog"), "menu:apilog")},
		{btn(i18n.T(lang, "btn.webhooks"), "menu:webhooks"), btn(i18n.T(lang, "btn.subdomain"), "menu:subdomain")},
		{btn(i18n.T(lang, "btn.miniapp"), "menu:miniapp"), btn(i18n.T(lang, "btn.cabinet"), "menu:cabinet")},
		{btn(i18n.T(lang, "btn.mail"), "menu:mail")},
		{btn(i18n.T(lang, "btn.rsimport"), "menu:rsimp")},
		{btn(i18n.T(lang, "csqtt.btn_adm"), "menu:csqttadm")},
	}
	// Ключ/кука доступа к панели — только там, где панель вообще чем-то закрыта.
	if a.panelAuthRelevant() {
		rows = append(rows, []models.InlineKeyboardButton{
			btn(i18n.T(lang, "btn.panelauth"), "menu:panelauth"),
		})
	}
	rows = append(rows,
		[]models.InlineKeyboardButton{btn(i18n.T(lang, "btn.reconfig"), "menu:reconf")},
		homeRow(lang))
	a.sendKBSection(ctx, chatID, assets.SectionAdminStats, i18n.T(lang, "menu.system_title"), rows)
}

func (a *App) squadDisplay(ctx context.Context) (string, string) {
	a.mu.Lock()
	var activeInt []string
	extUUID := ""
	if a.botCfg != nil {
		activeInt = append([]string(nil), a.botCfg.Plan.ActiveInternalSquads...)
		extUUID = a.botCfg.Plan.ExternalSquadUUID
	}
	a.mu.Unlock()
	return a.squadNames(ctx, activeInt, extUUID)
}

func (a *App) squadNames(ctx context.Context, activeInt []string, extUUID string) (string, string) {
	a.mu.Lock()
	panel := a.panel
	lang := i18n.Fallback
	if a.botCfg != nil && a.botCfg.Language != "" {
		lang = a.botCfg.Language
	}
	a.mu.Unlock()

	names := map[string]string{}
	if panel != nil {
		if ints, err := panel.ListSquads(ctx); err == nil {
			for _, s := range ints {
				names[s.UUID] = s.Name
			}
		}
		if exts, err := panel.ListExternalSquads(ctx); err == nil {
			for _, s := range exts {
				names[s.UUID] = s.Name
			}
		}
	}
	disp := func(uuid string) string {
		if n, ok := names[uuid]; ok && n != "" {
			return n
		}
		return uuid
	}
	var ints []string
	for _, u := range activeInt {
		ints = append(ints, disp(u))
	}
	internalCSV := strings.Join(ints, ", ")
	if internalCSV == "" {
		internalCSV = i18n.T(lang, "admin.none")
	}
	externalName := i18n.T(lang, "admin.none")
	if extUUID != "" {
		externalName = disp(extUUID)
	}
	return internalCSV, externalName
}

func (a *App) startReconfigure(ctx context.Context, chatID int64) {
	a.mu.Lock()
	var base model.BotConfig
	// Именно копия: обычное присваивание оставляет карты и слайсы общими с живым
	// конфигом, и мастер переустановки правил бы работающего бота ещё до
	// сохранения — да ещё и без замка. Ошибку копии глотать нельзя молча, но и
	// падать здесь незачем: пустой конфиг мастер просто спросит заново.
	if cp, err := a.botCfg.Clone(); err != nil {
		a.log.Warn("копия конфига для мастера переустановки не снята", "err", err)
	} else if cp != nil {
		base = *cp
	}
	w := &wizard{step: stepDB, cfg: base, reconfig: true}
	a.wiz[chatID] = w
	a.mu.Unlock()
	// Незавершённое ожидание ввода из другого раздела снимаем: админ ушёл
	// сюда, значит тот ввод брошен. Иначе первый же текст в мастере будет
	// принят за ответ тому разделу — мастер погаснет на середине, а текст
	// уедет не туда.
	a.getUI(chatID).adminInput = ""
	a.gotoDB(ctx, chatID, w)
}

// cancelReconfigure aborts an in-progress reconfigure wizard and returns to the
// System menu (rendered in place on the same banner).
func (a *App) cancelReconfigure(ctx context.Context, chatID int64) {
	a.mu.Lock()
	delete(a.wiz, chatID)
	a.mu.Unlock()
	a.showSystem(ctx, chatID)
}

func bannerInputFor(section string) models.InputFile {
	if b := assets.Bytes(section); len(b) > 0 {
		return &models.InputFileUpload{Filename: section + ".jpg", Data: bytes.NewReader(b)}
	}
	return &models.InputFileUpload{Filename: "welcome.jpg", Data: bytes.NewReader(defaultBanner)}
}

func (a *App) welcomeContent(name string) (models.InputFile, string, []models.MessageEntity) {
	a.mu.Lock()
	var w model.WelcomeConfig
	lang := i18n.Fallback
	if a.botCfg != nil {
		w = a.botCfg.Welcome
		if a.botCfg.Language != "" {
			lang = a.botCfg.Language
		}
	}
	a.mu.Unlock()

	var photo models.InputFile
	switch {
	case w.ImageFileID != "":
		photo = &models.InputFileString{Data: w.ImageFileID}
	case w.ImageURL != "":
		photo = &models.InputFileString{Data: w.ImageURL}
	default:
		photo = &models.InputFileUpload{Filename: "welcome.jpg", Data: bytes.NewReader(defaultBanner)}
	}

	caption := w.Text
	var ents []models.MessageEntity
	if caption == "" {
		// Текст приветствия зависит от выдачи CSQTT: иначе строка про неё
		// вводит в заблуждение.
		key := "menu.welcome_no_csqtt"
		if a.csqttEnabled() {
			key = "menu.welcome"
		}
		caption = i18n.T(lang, key, name, a.brandName(lang))
	} else if len(w.Entities) > 0 {
		_ = json.Unmarshal(w.Entities, &ents)
	}
	return photo, caption, ents
}

func (a *App) showMenu(ctx context.Context, chatID int64, isAdmin bool, name string) {
	// Гейт согласия на входе стоит здесь, а не только в enterHome: в меню
	// ведёт и кнопка «🏠 На главную» с любого экрана, включая сами документы.
	if !isAdmin && a.legalStartRequired(ctx, chatID) {
		a.getUI(chatID).pendingLegalHome = true
		a.askLegal(ctx, chatID)
		return
	}
	lang := a.botLang()
	photo, caption, ents := a.welcomeContent(name)
	var rows [][]models.InlineKeyboardButton
	if isAdmin {
		caption = i18n.T(lang, "menu.admin_title")
		ents = nil
		rows = a.adminMenuRows(lang)
		photo = bannerInputFor(assets.SectionAdminStats)
	} else {
		a.showUserMenu(ctx, chatID)
		return
	}
	if len(ents) == 0 {
		caption = a.applyPremium(caption)
	}
	a.sendBanner(ctx, chatID, photo, caption, ents, models.InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (a *App) registerUser(ctx context.Context, chatID int64, firstName, username string) {
	if a.store != nil {
		_ = a.store.UpsertUser(ctx, chatID)
		_ = a.store.SetUserInfo(ctx, chatID, username, firstName)
		a.reconcileWhitelist(ctx, chatID)
	}
	if a.guardNewUser(ctx, chatID, firstName, username) {
		return
	}
	if a.syncPanelAccount(ctx, chatID) {
		if u, _ := a.store.GetUser(ctx, chatID); u != nil && u.SubExpireAt != "" {
			lang := a.lang(chatID)
			a.notify(ctx, chatID, i18n.T(lang, "sync.linked", formatExpire(u.SubExpireAt, lang)))
		}
	}
	// Согласие при первом входе: новичок — как раз тот, кому документы и
	// показывают, поэтому гейт стоит и здесь, а не только в enterHome.
	if a.legalStartRequired(ctx, chatID) {
		a.getUI(chatID).pendingLegalHome = true
		a.askLegal(ctx, chatID)
		return
	}
	a.showGreeting(ctx, chatID, displayName(firstName, username))
}

func (a *App) onMenu(ctx context.Context, chatID int64, val string, isAdmin bool, firstName, username string) {
	name := displayName(firstName, username)
	// Уход в меню отменяет ожидание секрета доступа к панели: см. clearPanelInput.
	a.clearPanelInput(chatID)
	switch val {
	case "buy":
		a.showPlans(ctx, chatID)
	case "vpn":
		a.showVPN(ctx, chatID)
	case "info":
		a.showInfo(ctx, chatID)
	case "renew":
		// Продление ведёт на СВОЙ тариф: подписчику тарифа по ссылке витрина
		// «Базового» продала бы чужие условия (или отказала бы вовсе).
		a.showRenew(ctx, chatID)
	case "topup":
		if a.legalGateOrAsk(ctx, chatID) {
			return
		}
		a.showTopUp(ctx, chatID)
	case "wallet":
		if isAdmin {
			a.showWalletAdmin(ctx, chatID)
		}
	case "balance":
		a.showBalance(ctx, chatID)
	case "ref":
		a.showReferral(ctx, chatID)
	case "refadmin":
		if isAdmin {
			a.showReferralAdmin(ctx, chatID)
		}
	case "broadcast":
		if isAdmin {
			a.showBroadcast(ctx, chatID)
		}
	case "promo":
		if a.legalGateOrAsk(ctx, chatID) {
			return
		}
		a.showPromoUser(ctx, chatID)
	case "promoadmin":
		if isAdmin {
			a.showPromoAdmin(ctx, chatID)
		}
	case "moynalog":
		if isAdmin {
			a.showMoyNalogAdmin(ctx, chatID)
		}
	case "platega":
		if isAdmin {
			a.showPlategaAdmin(ctx, chatID)
		}
	case "heleket":
		if isAdmin {
			a.showHeleketAdmin(ctx, chatID)
		}
	case "tribute":
		if isAdmin {
			a.showTributeAdmin(ctx, chatID)
		}
	case "analytics":
		if isAdmin {
			a.showAnalytics(ctx, chatID)
		}
	case "mysubs":
		a.showMySubs(ctx, chatID)
	case "csqtt":
		a.showCsqttUser(ctx, chatID)
	case "csqttadm":
		if isAdmin {
			a.showCsqttAdmin(ctx, chatID)
		}
	case "autopay":
		a.showAutoPay(ctx, chatID)
	case "access":
		if isAdmin {
			a.showAccess(ctx, chatID)
		}
	case "home":
		a.showMenu(ctx, chatID, isAdmin, name)
	case "register":
		a.registerUser(ctx, chatID, firstName, username)
	case "status":
		if isAdmin {
			a.handleStatus(ctx, chatID)
		}
	case "p2p":
		if isAdmin {
			a.showP2PAdmin(ctx, chatID)
		}
	case "emoji":
		if isAdmin {
			a.showEmojiGrid(ctx, chatID)
		}
	case "welcome":
		if isAdmin {
			a.showWelcomeAdmin(ctx, chatID)
		}
	case "welcome_sections":
		if isAdmin {
			a.showSectionBanners(ctx, chatID)
		}
	case "subdomain":
		if isAdmin {
			a.showSubdomain(ctx, chatID)
		}
	case "panelauth":
		if isAdmin {
			a.showPanelAuth(ctx, chatID, "")
		}
	case "apilog":
		if isAdmin {
			a.showAPILog(ctx, chatID, 0)
		}
	case "webhooks":
		if isAdmin {
			a.showWebhooksAdmin(ctx, chatID)
		}
	case "notify":
		if isAdmin {
			a.showNotifyAdmin(ctx, chatID)
		}
	case "cryptobot":
		if isAdmin {
			a.showCryptoBotAdmin(ctx, chatID)
		}
	case "squads":
		if isAdmin {
			a.showSquads(ctx, chatID)
		}
	case "trial":
		if isAdmin {
			a.showTrialAdmin(ctx, chatID)
		} else {
			// Возвратный гейт: после «Принимаю» триал активируется, а не
			// теряется на витрине.
			if a.legalGateBack(ctx, chatID, "trial") {
				return
			}
			a.activateTrial(ctx, chatID)
		}
	case "contacts":
		if isAdmin {
			a.showContacts(ctx, chatID)
		}
	case "svcname":
		if isAdmin {
			a.showServiceName(ctx, chatID)
		}
	case "update":
		if isAdmin {
			a.handleUpdate(ctx, chatID)
		}
	case "iface":
		if isAdmin {
			a.showIface(ctx, chatID)
		}
	case "botlang":
		if isAdmin {
			a.showBotLang(ctx, chatID)
		}
	case "pay":
		if isAdmin {
			a.showPay(ctx, chatID)
		}
	case "manage":
		if isAdmin {
			a.showMenu(ctx, chatID, true, name)
		}
	case "addsub":
		if isAdmin {
			a.showAddSubAdmin(ctx, chatID)
		}
	case "marketing":
		if isAdmin {
			a.showMarketing(ctx, chatID)
		}
	case "system":
		if isAdmin {
			a.showSystem(ctx, chatID)
		}
	case "devices":
		if isAdmin {
			a.showDevicesAdmin(ctx, chatID)
		}
	case "miniapp":
		if isAdmin {
			a.showMiniAppAdmin(ctx, chatID)
		}
	case "miniapptoggle":
		if isAdmin {
			a.toggleMiniApp(ctx, chatID)
		}
	case "cabinet":
		if isAdmin {
			a.showCabinetAdmin(ctx, chatID)
		}
	case "rsimp":
		if isAdmin {
			a.showRSImport(ctx, chatID)
		}
	case "cabtoggle":
		if isAdmin {
			a.toggleCabinet(ctx, chatID)
		}
	case "cabpath":
		if isAdmin {
			a.getUI(chatID).adminInput = "cab_path"
			a.askInput(ctx, chatID, i18n.T(a.lang(chatID), "cabinet.ask_path"), "menu:cabinet")
		}
	case "cabapprove":
		if isAdmin {
			a.cycleCabinetApproval(ctx, chatID)
		}
	case "cabtitle":
		if isAdmin {
			a.getUI(chatID).adminInput = "cab_title"
			a.askInput(ctx, chatID, i18n.T(a.lang(chatID), "cabinet.ask_title"), "menu:cabinet")
		}
	case "cabdesc":
		if isAdmin {
			a.getUI(chatID).adminInput = "cab_desc"
			a.askInput(ctx, chatID, i18n.T(a.lang(chatID), "cabinet.ask_desc"), "menu:cabinet")
		}
	case "cabfav":
		if isAdmin {
			a.getUI(chatID).adminInput = "cab_favicon"
			a.askInput(ctx, chatID, i18n.T(a.lang(chatID), "cabinet.ask_favicon"), "menu:cabinet")
		}
	case "cabfp":
		if isAdmin {
			a.toggleCabinetAntiFP(ctx, chatID)
		}
	case "cablogout":
		if isAdmin {
			a.logoutAllSessions(ctx, chatID)
		}
	case "mail":
		if isAdmin {
			a.showMailAdmin(ctx, chatID)
		}
	case "mailtoggle":
		if isAdmin {
			a.toggleMail(ctx, chatID)
		}
	case "mailmode":
		if isAdmin {
			a.cycleMailMode(ctx, chatID)
		}
	case "mailtls":
		if isAdmin {
			a.cycleMailTLS(ctx, chatID)
		}
	case "mailtest":
		if isAdmin {
			a.sendTestMail(ctx, chatID)
		}
	case "mailfrom", "mailfromname", "mailhost", "mailuser", "mailpass", "mailapiurl", "mailapikey":
		if isAdmin {
			a.getUI(chatID).adminInput = "mail_" + strings.TrimPrefix(val, "mail")
			a.askInput(ctx, chatID, i18n.T(a.lang(chatID), "mail.ask_"+strings.TrimPrefix(val, "mail")), "menu:mail")
		}
	case "reconf":
		if isAdmin {
			a.startReconfigure(ctx, chatID)
		}
	case "users":
		if isAdmin {
			a.showUsers(ctx, chatID, 0)
		}
	case "stars":
		if isAdmin {
			a.showStarsAdmin(ctx, chatID)
		}
	case "yookassa":
		if isAdmin {
			a.showYooKassaAdmin(ctx, chatID)
		}
	case "plans":
		if isAdmin {
			a.showPlansAdmin(ctx, chatID, 0)
		}
	case "pricing":
		if isAdmin {
			// Старый экран цен убран: кнопки из старых переписок ведут в
			// редактор цен «Базового» — то же содержимое, один источник истины.
			a.showPlanPricing(ctx, chatID, model.PlanCodeBase)
		}
	case "payments":
		if isAdmin {
			a.showPayments(ctx, chatID, 0)
		}
	}
}

func (a *App) showWelcomeAdmin(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	a.sendIfaceKB(ctx, chatID, i18n.T(lang, "welcome.title"), [][]models.InlineKeyboardButton{
		{btn(i18n.T(lang, "welcome.btn_image"), "wel:img"), btn(i18n.T(lang, "welcome.btn_text"), "wel:txt")},
		{btn(i18n.T(lang, "btn.back"), "menu:iface"), btn(i18n.T(lang, "btn.home"), "menu:home")},
	})
}

func (a *App) onWelcome(ctx context.Context, chatID int64, val string) {
	lang := a.lang(chatID)
	ui := a.getUI(chatID)
	cancel := [][]models.InlineKeyboardButton{{btn(i18n.T(lang, "btn.cancel"), "wel:cancel")}}
	switch val {
	case "img":
		ui.welcomeAwait = "img"
		a.sendKB(ctx, chatID, i18n.T(lang, "welcome.ask_image"), cancel)
	case "txt":
		ui.welcomeAwait = "txt"
		a.sendKB(ctx, chatID, i18n.T(lang, "welcome.ask_text"), cancel)
	case "cancel":
		ui.welcomeAwait = ""
		a.showWelcomeAdmin(ctx, chatID)
	}
}

// setWelcomeImageURL сохраняет ссылку на баннер главной.
//
// Текст, который не является ссылкой, Telegram трактует как file_id и
// отвечает «wrong remote file identifier» — баннер не уходит, а вместе с ним
// не уходит и главное меню: со стороны это выглядит как «бот не запустился».
// Поэтому мусор сюда не пропускаем, а пустое значение сбрасывает картинку к
// встроенной.
func (a *App) setWelcomeImageURL(ctx context.Context, chatID int64, url string) {
	lang := a.lang(chatID)
	raw := strings.TrimSpace(url)
	if raw != "" && raw != "-" && raw != "—" {
		norm, ok := normalizeDocURL(raw)
		if !ok {
			// Ввод не сбрасываем: человек дошлёт правильную ссылку или фото.
			a.sendKB(ctx, chatID, i18n.T(lang, "welcome.bad_image"), [][]models.InlineKeyboardButton{
				{btn(i18n.T(lang, "btn.cancel"), "wel:cancel")},
			})
			return
		}
		raw = norm
	} else {
		raw = ""
	}
	a.getUI(chatID).welcomeAwait = ""
	a.mu.Lock()
	if a.botCfg != nil {
		a.botCfg.Welcome.ImageURL = raw
		a.botCfg.Welcome.ImageFileID = ""
	}
	a.mu.Unlock()
	_ = a.saveBotConfig(ctx)
	a.showWelcomeAdmin(ctx, chatID)
}

func (a *App) setWelcomeImageFile(ctx context.Context, chatID int64, fileID string) {
	a.getUI(chatID).welcomeAwait = ""
	a.mu.Lock()
	if a.botCfg != nil {
		a.botCfg.Welcome.ImageFileID = fileID
		a.botCfg.Welcome.ImageURL = ""
	}
	a.mu.Unlock()
	_ = a.saveBotConfig(ctx)
	a.showWelcomeAdmin(ctx, chatID)
}

func (a *App) setWelcomeText(ctx context.Context, chatID int64, m *models.Message) {
	a.getUI(chatID).welcomeAwait = ""
	ents, _ := json.Marshal(m.Entities)
	a.mu.Lock()
	if a.botCfg != nil {
		a.botCfg.Welcome.Text = m.Text
		a.botCfg.Welcome.Entities = ents
	}
	a.mu.Unlock()
	_ = a.saveBotConfig(ctx)
	a.showWelcomeAdmin(ctx, chatID)
}

func (a *App) showEmojiGrid(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	m := a.premiumMap()
	var sb strings.Builder
	sb.WriteString(i18n.T(lang, "emoji.title"))
	sb.WriteString("\n")
	for _, e := range botEmojis {
		mark := ""
		if _, ok := m[e.E]; ok {
			mark = " ✅"
		}
		sb.WriteString("\n" + e.E + mark + " — " + e.Use)
	}

	var rows [][]models.InlineKeyboardButton
	var row []models.InlineKeyboardButton
	for _, e := range botEmojis {
		label := e.E
		if _, ok := m[e.E]; ok {
			label = e.E + "✅"
		}
		row = append(row, btn(label, "emo:set:"+e.E))
		if len(row) == 4 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []models.InlineKeyboardButton{btn(i18n.T(lang, "btn.back"), "menu:iface"), btn(i18n.T(lang, "btn.home"), "menu:home")})
	a.sendIfaceKB(ctx, chatID, sb.String(), rows)
}

func (a *App) onEmoji(ctx context.Context, chatID int64, val string) {
	lang := a.lang(chatID)
	action, arg, _ := strings.Cut(val, ":")
	switch action {
	case "set":
		a.getUI(chatID).awaitEmojiFor = arg
		a.sendKB(ctx, chatID, i18n.T(lang, "emoji.ask_one", arg),
			[][]models.InlineKeyboardButton{{btn(i18n.T(lang, "btn.cancel"), "emo:done")}})
	case "done":
		a.getUI(chatID).awaitEmojiFor = ""
		a.showEmojiGrid(ctx, chatID)
	}
}

func (a *App) setEmojiFor(ctx context.Context, chatID int64, m *models.Message) {
	ui := a.getUI(chatID)
	target := ui.awaitEmojiFor
	ui.awaitEmojiFor = ""
	var id string
	for _, e := range m.Entities {
		if e.Type == models.MessageEntityTypeCustomEmoji && e.CustomEmojiID != "" {
			id = e.CustomEmojiID
			break
		}
	}
	if id == "" {
		a.showEmojiGrid(ctx, chatID)
		return
	}
	a.mu.Lock()
	if a.botCfg != nil {
		if a.botCfg.PremiumEmoji == nil {
			a.botCfg.PremiumEmoji = map[string]string{}
		}
		a.botCfg.PremiumEmoji[target] = id
	}
	a.mu.Unlock()
	_ = a.saveBotConfig(ctx)
	a.showEmojiGrid(ctx, chatID)
}
