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
package main

import (
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
	court string
	date  string // 2026-10-07
	hour  string // 19:00
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
				current[key] = freeSlot{court: c.Name, date: date, hour: t}
			}
		}
	}

	var fresh []freeSlot
	newState := map[string]bool{}
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
		return a.court < b.court
	})

	log.Printf("кортов: %d, свободных слотов: %d, новых: %d", len(courts), len(current), len(fresh))
	if len(fresh) > 0 {
		body, dropped := fitLines(groupByTime(fresh))
		if dropped > 0 {
			body += fmt.Sprintf("\n…и ещё %d строк", dropped)
		}
		msg := "🎾 Появились слоты (" + kinds(courts) + "):\n" + body +
			"\n\nhttps://academytennisdaulet.altegio.me/company/" + cid + "/personal/select-master"
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

// ruDate переводит "2026-10-07" в "7 окт".
func ruDate(iso string) string {
	t, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	return fmt.Sprintf("%d %s", t.Day(), ruMonths[t.Month()-1])
}

// groupByTime собирает слоты одного часа в строку "7 окт 19:00 — Корт 1, Корт 3".
// fresh должен быть отсортирован по дате, часу и названию корта.
func groupByTime(fresh []freeSlot) []string {
	var lines []string
	for i := 0; i < len(fresh); {
		j, names := i, []string{}
		for ; j < len(fresh) && fresh[j].date == fresh[i].date && fresh[j].hour == fresh[i].hour; j++ {
			names = append(names, fresh[j].court)
		}
		lines = append(lines, fmt.Sprintf("%s %s — %s",
			ruDate(fresh[i].date), fresh[i].hour, strings.Join(names, ", ")))
		i = j
	}
	return lines
}

// fitLines берёт столько строк, сколько влезает в лимит Telegram, и сообщает,
// сколько осталось за бортом. Telegram считает символы, а не байты.
func fitLines(lines []string) (string, int) {
	total := 0
	for i, l := range lines {
		if total += utf8.RuneCountInString(l) + 1; total > tgLimit {
			return strings.Join(lines[:i], "\n"), len(lines) - i
		}
	}
	return strings.Join(lines, "\n"), 0
}

// kinds перечисляет специализации отобранных кортов, чтобы заголовок не врал:
// в STAFF_IDS можно задать любые корты, не только крытые.
func kinds(courts []staff) string {
	var out []string
	got := map[string]bool{}
	for _, c := range courts {
		k := strings.TrimSpace(c.Specialization)
		if k != "" && !got[k] {
			got[k] = true
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return "корты"
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
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
	words := strings.Split(strings.ToLower(env("NAME_FILTER", "крыт,indoor,хард")), ",")
	var out []staff
	for _, s := range all {
		hay := strings.ToLower(s.Name + " " + s.Specialization)
		for _, w := range words {
			if w = strings.TrimSpace(w); w != "" && strings.Contains(hay, w) {
				out = append(out, s)
				break
			}
		}
	}
	return out
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
