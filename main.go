// court-watcher: проверяет свободные слоты на крытых кортах в Altegio
// и шлёт уведомление в Telegram о НОВЫХ слотах.
//
// Переменные окружения:
//
//	COMPANY_ID    — id филиала (по умолчанию 521176)
//	ALTEGIO_TOKEN — Bearer-токен из запросов виджета (см. README);
//	                обязателен: без него API отдаёт 401, и бот пишет об этом в Telegram
//	API_BASE      — по умолчанию https://api.alteg.io/api/v1
//	NAME_FILTER   — слова через запятую для отбора кортов по имени (по умолчанию "крыт,indoor,хард")
//	STAFF_IDS     — явный список id кортов через запятую (перекрывает NAME_FILTER)
//	DAYS_AHEAD    — на сколько дней вперёд смотреть (по умолчанию 7)
//	TG_TOKEN, TG_CHAT_ID — бот и чат для уведомлений
//	STATE_FILE    — файл с уже отправленными слотами (по умолчанию state.json)
//
// Автобронь (по умолчанию выключена — пустой BOOK_HOURS):
//
//	BOOK_HOURS    — часы, которые бронировать автоматически, например "22:00"
//	BOOK_KINDS    — типы кортов через запятую: "крытый", "открытый", "стенка", "пляжный"
//	BOOK_NAME     — имя для брони (без него бронь не делается)
//	BOOK_PHONE    — телефон для брони (без него бронь не делается)
//	BOOK_EMAIL    — email, если клуб его требует
//	BOOK_MAX      — предохранитель: максимум броней за один прогон (по умолчанию 1)
//	BOOK_TOTAL    — предохранитель: сколько броней бот сделает всего (по умолчанию 4)
//	BOOK_MIN_LEAD — не бронировать, если до начала осталось меньше N часов (по умолчанию 3)
//	TZ_OFFSET     — часовой пояс клуба, часы от UTC (по умолчанию 5, Asia/Almaty)
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type staff struct {
	ID             int    `json:"id"`
	Name           string `json:"name"`
	Specialization string `json:"specialization"`
	Bookable       bool   `json:"bookable"`
}

type slot struct {
	Time     string `json:"time"`
	Datetime string `json:"datetime"`
}

type datesResp struct {
	BookingDates []string `json:"booking_dates"`
}

// freeSlot — один свободный час на одном корте.
type freeSlot struct {
	court   string
	staffID int
	sign    string // значок типа корта: 🏠 / ☀️ / 🧱
	label   string // расшифровка значка
	date    string // 2026-10-07
	hour    string // 19:00
}

var (
	client  = &http.Client{Timeout: 20 * time.Second}
	apiBase = env("API_BASE", "https://api.alteg.io/api/v1")
	token   = os.Getenv("ALTEGIO_TOKEN")
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// httpErr несёт код ответа, чтобы отличить сдохший токен (401/403) от прочих сбоев.
type httpErr struct {
	code int
	msg  string
}

func (e *httpErr) Error() string { return e.msg }

// authExpired сообщает, что Altegio отказал именно в авторизации.
func authExpired(err error) bool {
	var he *httpErr
	return errors.As(err, &he) && (he.code == 401 || he.code == 403)
}

// get делает запрос и раскладывает поле data (или весь ответ, если обёртки нет) в out.
func get(path string, out any) error {
	req, _ := http.NewRequest("GET", apiBase+path, nil)
	req.Header.Set("Accept", "application/vnd.api.v2+json")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return &httpErr{
			code: resp.StatusCode,
			msg:  fmt.Sprintf("%s: HTTP %d: %.300s", path, resp.StatusCode, body),
		}
	}
	var wrap struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &wrap) == nil && len(wrap.Data) > 0 {
		body = wrap.Data
	}
	return json.Unmarshal(body, out)
}

// authAlertKey — служебный ключ в state: алерт о мёртвом токене уже отправлен.
// Успешный прогон перезаписывает state одними слотами, так что флаг сам исчезает.
const authAlertKey = "__auth_alert_sent"

// tgLimit — предел Telegram на одно сообщение 4096 символов; берём с запасом
// под заголовок и ссылку. Первый прогон (пустой state) находит сразу сотни слотов.
const tgLimit = 3500

func main() {
	cid := env("COMPANY_ID", "521176")
	days := 7
	fmt.Sscanf(env("DAYS_AHEAD", "7"), "%d", &days)

	stateFile := env("STATE_FILE", "state.json")
	seen := map[string]bool{}
	if b, err := os.ReadFile(stateFile); err == nil {
		_ = json.Unmarshal(b, &seen)
	}

	var all []staff
	if err := get("/book_staff/"+cid, &all); err != nil {
		if authExpired(err) {
			reportDeadToken(stateFile, seen, err)
		}
		log.Fatalf("список кортов: %v", err)
	}

	courts := pickCourts(all)
	if len(courts) == 0 {
		log.Println("Ни один корт не подошёл под фильтр. Все корты:")
		for _, s := range all {
			log.Printf("  id=%d  %q", s.ID, s.Name)
		}
		return
	}

	limit := time.Now().AddDate(0, 0, days).Format("2006-01-02")
	current := map[string]freeSlot{} // ключ -> слот

	for _, c := range courts {
		var d datesResp
		if err := get(fmt.Sprintf("/book_dates/%s?staff_id=%d", cid, c.ID), &d); err != nil {
			log.Printf("даты для %s: %v", c.Name, err)
			continue
		}
		for _, date := range d.BookingDates {
			if date > limit {
				continue
			}
			var slots []slot
			if err := get(fmt.Sprintf("/book_times/%s/%d/%s", cid, c.ID, url.PathEscape(date)), &slots); err != nil {
				log.Printf("слоты %s %s: %v", c.Name, date, err)
				continue
			}
			for _, s := range slots {
				t := padHour(s.Time)
				key := fmt.Sprintf("%d|%s|%s", c.ID, date, t)
				sign, label := kindLabel(c.Specialization)
				current[key] = freeSlot{
					court: c.Name, staffID: c.ID,
					sign: sign, label: label, date: date, hour: t,
				}
			}
		}
	}

	var fresh []freeSlot
	newState := map[string]bool{}
	bookedBefore := 0
	for k := range seen {
		if strings.HasPrefix(k, bookedPrefix) {
			newState[k] = true // лимит броней должен жить между прогонами
			bookedBefore++
		}
	}
	for k, v := range current {
		newState[k] = true
		if !seen[k] {
			fresh = append(fresh, v)
		}
	}
	sort.Slice(fresh, func(i, j int) bool {
		a, b := fresh[i], fresh[j]
		if a.date != b.date {
			return a.date < b.date
		}
		if a.hour != b.hour {
			return a.hour < b.hour
		}
		if a.sign != b.sign {
			return a.sign < b.sign
		}
		return a.court < b.court
	})

	log.Printf("кортов: %d, свободных слотов: %d, новых: %d", len(courts), len(current), len(fresh))
	// бронируем из current, а не из fresh: если прошлая попытка не удалась,
	// слот уже не "новый", но взять его всё ещё надо
	autoBook(cid, current, newState, bookedBefore)

	chunks := chunkLines(groupByTime(fresh))
	for i, ch := range chunks {
		head := "🎾 Появились слоты\n" + legend(fresh) + "\n\n"
		if i > 0 {
			head = fmt.Sprintf("🎾 Слоты, продолжение (%d/%d)\n\n", i+1, len(chunks))
		}
		msg := head + strings.Join(ch, "\n")
		if i == len(chunks)-1 {
			msg += "\n\nhttps://academytennisdaulet.altegio.me/company/" + cid + "/personal/select-master"
		}
		if err := notify(msg); err != nil {
			log.Fatalf("telegram: %v", err) // не сохраняем state, чтобы повторить в следующий раз
		}
	}

	b, _ := json.Marshal(newState)
	if err := os.WriteFile(stateFile, b, 0o644); err != nil {
		log.Printf("state: %v", err)
	}
}

// reportDeadToken шлёт один алерт на всё время, пока токен не принимают:
// иначе каждый часовой прогон писал бы в Telegram одно и то же.
func reportDeadToken(stateFile string, seen map[string]bool, cause error) {
	if seen[authAlertKey] {
		log.Println("токен всё ещё не принимается, алерт уже отправлен ранее")
		return
	}
	msg := "⚠️ court-watcher: Altegio не принимает ALTEGIO_TOKEN.\n" +
		"Проверка слотов стоит, пока токен не обновишь.\n\n" + cause.Error()
	if err := notify(msg); err != nil {
		log.Printf("алерт о токене не ушёл: %v", err) // флаг не ставим — повторим через час
		return
	}
	seen[authAlertKey] = true
	b, _ := json.Marshal(seen)
	if err := os.WriteFile(stateFile, b, 0o644); err != nil {
		log.Printf("state: %v", err)
	}
}

var ruMonths = [...]string{"янв", "фев", "мар", "апр", "май", "июн",
	"июл", "авг", "сен", "окт", "ноя", "дек"}

// bookedPrefix помечает в state слоты, которые бот забронировал. Такие ключи
// переносятся между прогонами, иначе предохранитель BOOK_TOTAL обнулялся бы.
const bookedPrefix = "booked|"

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(strings.ToLower(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == strings.ToLower(v) {
			return true
		}
	}
	return false
}

// post отправляет JSON и раскладывает поле data в out.
func post(path string, body, out any) error {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", apiBase+path, bytes.NewReader(raw))
	req.Header.Set("Accept", "application/vnd.api.v2+json")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &httpErr{
			code: resp.StatusCode,
			msg:  fmt.Sprintf("%s: HTTP %d: %.300s", path, resp.StatusCode, rb),
		}
	}
	if out == nil {
		return nil
	}
	var wrap struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(rb, &wrap) == nil && len(wrap.Data) > 0 {
		rb = wrap.Data
	}
	return json.Unmarshal(rb, out)
}

// pickService ищет услугу, которой бронируется корт. Захардкодить её нельзя:
// у крытых кортов услуг сейчас нет вовсе, они появятся вместе с кортами.
func pickService(cid string, staffID int) (int, int, error) {
	var r struct {
		Services []struct {
			ID       int `json:"id"`
			PriceMin int `json:"price_min"`
		} `json:"services"`
	}
	if err := get(fmt.Sprintf("/book_services/%s?staff_id=%d", cid, staffID), &r); err != nil {
		return 0, 0, err
	}
	if len(r.Services) == 0 {
		return 0, 0, fmt.Errorf("у корта нет услуг — бронировать нечем")
	}
	return r.Services[0].ID, r.Services[0].PriceMin, nil
}

// autoBook бронирует подходящие слоты и пишет об этом в Telegram.
// Выключена, пока не заданы BOOK_HOURS, BOOK_NAME и BOOK_PHONE.
func autoBook(cid string, current map[string]freeSlot, newState map[string]bool, already int) {
	hours := splitCSV(os.Getenv("BOOK_HOURS"))
	if len(hours) == 0 {
		return
	}
	name, phone := os.Getenv("BOOK_NAME"), os.Getenv("BOOK_PHONE")
	if name == "" || phone == "" {
		log.Println("автобронь: BOOK_HOURS задан, но BOOK_NAME/BOOK_PHONE пусты — не бронирую")
		return
	}
	kinds := splitCSV(os.Getenv("BOOK_KINDS"))

	lead := 3.0
	fmt.Sscanf(env("BOOK_MIN_LEAD", "3"), "%f", &lead)
	minLead := time.Duration(lead * float64(time.Hour))

	// Altegio отдаёт время в поясе клуба, а раннер GitHub живёт в UTC: без
	// явного пояса "22:00" уехало бы на 5 часов и фильтр врал бы наоборот.
	tzOff := 5
	fmt.Sscanf(env("TZ_OFFSET", "5"), "%d", &tzOff)
	loc := time.FixedZone("venue", tzOff*3600)

	perRun, total := 1, 4
	fmt.Sscanf(env("BOOK_MAX", "1"), "%d", &perRun)
	fmt.Sscanf(env("BOOK_TOTAL", "4"), "%d", &total)
	// порядок обхода map случаен — сортируем, чтобы брать самый ранний слот
	var want []freeSlot
	matched, tooSoon := 0, 0
	for _, s := range current {
		if !has(hours, s.hour) {
			continue
		}
		if len(kinds) > 0 && !has(kinds, s.label) {
			continue
		}
		matched++
		start, err := time.ParseInLocation("2006-01-02 15:04", s.date+" "+s.hour, loc)
		if err != nil {
			log.Printf("автобронь: не разобрал время %s %s: %v", s.date, s.hour, err)
			continue
		}
		if time.Until(start) < minLead {
			tooSoon++
			continue
		}
		want = append(want, s)
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].date != want[j].date {
			return want[i].date < want[j].date
		}
		return want[i].court < want[j].court
	})
	log.Printf("автобронь: подходящих слотов %d из %d (часы %v, типы %v); отсеяно по запасу <%gч: %d)",
		len(want), matched, hours, kinds, lead, tooSoon)
	if already >= total {
		log.Printf("автобронь: лимит BOOK_TOTAL=%d исчерпан, броней сделано %d", total, already)
		return
	}

	done := 0
	for _, s := range want {
		if done >= perRun || already+done >= total {
			break
		}
		key := fmt.Sprintf("%s%d|%s|%s", bookedPrefix, s.staffID, s.date, s.hour)
		if newState[key] {
			continue
		}
		svc, price, err := pickService(cid, s.staffID)
		if err != nil {
			log.Printf("автобронь %s %s %s: %v", s.court, s.date, s.hour, err)
			continue
		}
		body := map[string]any{
			"phone":    phone,
			"fullname": name,
			"email":    os.Getenv("BOOK_EMAIL"),
			"comment":  "court-watcher",
			"appointments": []map[string]any{{
				"id":       1,
				"services": []int{svc},
				"staff_id": s.staffID,
				"datetime": fmt.Sprintf("%s %s:00", s.date, s.hour),
			}},
		}
		if err := post("/book_record/"+cid, body, nil); err != nil {
			log.Printf("автобронь %s %s %s: %v", s.court, s.date, s.hour, err)
			if nerr := notify(fmt.Sprintf("⚠️ Не смог забронировать %s %s %s:\n%v",
				s.court, ruDate(s.date), s.hour, err)); nerr != nil {
				log.Printf("telegram: %v", nerr)
			}
			continue
		}
		newState[key] = true
		done++
		msg := fmt.Sprintf("✅ Забронировал\n%s %s %s %s\nцена: %d ₸\n\nброней сделано: %d из %d (BOOK_TOTAL)",
			s.sign, s.court, ruDate(s.date), s.hour, price, already+done, total)
		if err := notify(msg); err != nil {
			log.Printf("telegram: %v", err)
		}
	}
	if done > 0 {
		log.Printf("автобронь: забронировано %d", done)
	}
}

// ruDate переводит "2026-10-07" в "7 окт".
func ruDate(iso string) string {
	t, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	return fmt.Sprintf("%d %s", t.Day(), ruMonths[t.Month()-1])
}

// kindLabel даёт значок и короткое название типа корта.
// Порядок проверок важен: "открытый" содержит в себе подстроку "крыт".
func kindLabel(kind string) (string, string) {
	k := strings.ToLower(kind)
	switch {
	case strings.Contains(k, "открыт"):
		return "☀️", "открытый"
	case strings.Contains(k, "крыт"):
		return "🏠", "крытый"
	case strings.Contains(k, "стен"):
		return "🧱", "стенка"
	case strings.Contains(k, "пляж"):
		return "🏖", "пляжный"
	}
	return "•", kind
}

// groupByTime собирает слоты одного часа в одну строку, а внутри неё — корты
// одного типа под общим значком: "7 окт 19:00 — ☀️ Корт №1, Корт №3 · 🏠 Корт 1".
// fresh должен быть отсортирован по дате, часу, типу и названию.
func groupByTime(fresh []freeSlot) []string {
	var lines []string
	for i := 0; i < len(fresh); {
		end := i
		for ; end < len(fresh) && fresh[end].date == fresh[i].date && fresh[end].hour == fresh[i].hour; end++ {
		}
		var groups []string
		for j := i; j < end; {
			k := j
			names := []string{}
			for ; k < end && fresh[k].sign == fresh[j].sign; k++ {
				names = append(names, fresh[k].court)
			}
			groups = append(groups, fresh[j].sign+" "+strings.Join(names, ", "))
			j = k
		}
		lines = append(lines, fmt.Sprintf("%s %s — %s",
			ruDate(fresh[i].date), fresh[i].hour, strings.Join(groups, " · ")))
		i = end
	}
	return lines
}

// legend расшифровывает значки, которые реально встретились в сообщении.
func legend(fresh []freeSlot) string {
	var out []string
	got := map[string]bool{}
	for _, s := range fresh {
		if !got[s.sign] {
			got[s.sign] = true
			out = append(out, s.sign+" "+s.label)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " · ")
}

// chunkLines режет строки на куски, каждый из которых влезает в лимит Telegram.
// Обрезать нельзя: выброшенные слоты всё равно помечаются отправленными,
// то есть о них больше никогда не напомнят. Telegram считает символы, не байты.
func chunkLines(lines []string) [][]string {
	var out [][]string
	cur, total := []string{}, 0
	for _, l := range lines {
		n := utf8.RuneCountInString(l) + 1
		if len(cur) > 0 && total+n > tgLimit {
			out = append(out, cur)
			cur, total = nil, 0
		}
		cur = append(cur, l)
		total += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// padHour приводит "6:00" к "06:00": Altegio отдаёт час без ведущего нуля,
// из-за чего сортировка строк ставила 6:00 после 23:00.
func padHour(t string) string {
	h, rest, ok := strings.Cut(t, ":")
	if !ok || len(h) != 1 {
		return t
	}
	return "0" + h + ":" + rest
}

func pickCourts(all []staff) []staff {
	if ids := os.Getenv("STAFF_IDS"); ids != "" {
		want := map[string]bool{}
		for _, id := range strings.Split(ids, ",") {
			want[strings.TrimSpace(id)] = true
		}
		var out []staff
		for _, s := range all {
			if want[fmt.Sprint(s.ID)] {
				out = append(out, s)
			}
		}
		return out
	}
	filter := env("NAME_FILTER", "крыт,indoor,хард")
	if filter == "*" {
		return all // следим за всеми кортами, включая те, что клуб добавит позже
	}
	words := strings.Split(strings.ToLower(filter), ",")
	var out []staff
	for _, s := range all {
		hay := strings.ToLower(s.Name + " " + s.Specialization)
		for _, w := range words {
			if w = strings.TrimSpace(w); w != "" && startsWord(hay, w) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// startsWord: слово фильтра должно начинать какое-то слово в названии.
// Подстрочный поиск тут не годится — "крыт" входит внутрь "открытый",
// из-за чего фильтр крытых кортов захватывал и открытые.
func startsWord(hay, word string) bool {
	split := func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }
	for _, f := range strings.FieldsFunc(hay, split) {
		if strings.HasPrefix(f, word) {
			return true
		}
	}
	return false
}

func notify(text string) error {
	tg, chat := os.Getenv("TG_TOKEN"), os.Getenv("TG_CHAT_ID")
	if tg == "" || chat == "" {
		fmt.Println(text)
		return nil
	}
	resp, err := client.PostForm("https://api.telegram.org/bot"+tg+"/sendMessage",
		url.Values{"chat_id": {chat}, "text": {text}, "disable_web_page_preview": {"true"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	return nil
}
