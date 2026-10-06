// court-watcher: проверяет свободные слоты на крытых кортах в Altegio
// и шлёт уведомление в Telegram о НОВЫХ слотах.
//
// Переменные окружения:
//   COMPANY_ID    — id филиала (по умолчанию 521176)
//   ALTEGIO_TOKEN — Bearer-токен из запросов виджета (см. README), может быть пустым
//   API_BASE      — по умолчанию https://api.alteg.io/api/v1
//   NAME_FILTER   — слова через запятую для отбора кортов по имени (по умолчанию "крыт,indoor,хард")
//   STAFF_IDS     — явный список id кортов через запятую (перекрывает NAME_FILTER)
//   DAYS_AHEAD    — на сколько дней вперёд смотреть (по умолчанию 7)
//   TG_TOKEN, TG_CHAT_ID — бот и чат для уведомлений
//   STATE_FILE    — файл с уже отправленными слотами (по умолчанию state.json)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type staff struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Bookable bool   `json:"bookable"`
}

type slot struct {
	Time     string `json:"time"`
	Datetime string `json:"datetime"`
}

type datesResp struct {
	BookingDates []string `json:"booking_dates"`
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
		return fmt.Errorf("%s: HTTP %d: %.300s", path, resp.StatusCode, body)
	}
	var wrap struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &wrap) == nil && len(wrap.Data) > 0 {
		body = wrap.Data
	}
	return json.Unmarshal(body, out)
}

func main() {
	cid := env("COMPANY_ID", "521176")
	days := 7
	fmt.Sscanf(env("DAYS_AHEAD", "7"), "%d", &days)

	var all []staff
	if err := get("/book_staff/"+cid, &all); err != nil {
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
	current := map[string]string{} // ключ -> человекочитаемая строка

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
				key := fmt.Sprintf("%d|%s|%s", c.ID, date, s.Time)
				current[key] = fmt.Sprintf("%s — %s %s", c.Name, date, s.Time)
			}
		}
	}

	stateFile := env("STATE_FILE", "state.json")
	seen := map[string]bool{}
	if b, err := os.ReadFile(stateFile); err == nil {
		_ = json.Unmarshal(b, &seen)
	}

	var fresh []string
	newState := map[string]bool{}
	for k, v := range current {
		newState[k] = true
		if !seen[k] {
			fresh = append(fresh, v)
		}
	}
	sort.Strings(fresh)

	log.Printf("кортов: %d, свободных слотов: %d, новых: %d", len(courts), len(current), len(fresh))
	if len(fresh) > 0 {
		msg := "🎾 Появились крытые корты:\n" + strings.Join(fresh, "\n") +
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
		name := strings.ToLower(s.Name)
		for _, w := range words {
			if w = strings.TrimSpace(w); w != "" && strings.Contains(name, w) {
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
