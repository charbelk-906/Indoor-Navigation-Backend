package main

// GET /places/details?kind=store&id=1   (kind = store | facility)
//
// Returns the extra information of one place: description, phone, website,
// opening hours and menu. Places without details return empty fields (not 404),
// so the app can call this for every place.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

type placeHoursJSON struct {
	Day   int    `json:"day"`   // 0 = Monday … 6 = Sunday
	Open  string `json:"open"`  // "05:00"
	Close string `json:"close"` // "23:00" (earlier than open = after midnight, "24:00" = midnight)
}

type menuItemJSON struct {
	ID          int64   `json:"id"`
	Section     string  `json:"section"`
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Price       float64 `json:"price"`
	Currency    string  `json:"currency"`
	Vegetarian  bool    `json:"vegetarian"`
	Spicy       bool    `json:"spicy"`
	Available   bool    `json:"available"`
}

type placeDetailsJSON struct {
	Kind        string           `json:"kind"`
	ID          int64            `json:"id"`
	Description string           `json:"description"`
	Phone       string           `json:"phone"`
	Website     string           `json:"website"`
	Email       string           `json:"email"`
	Instagram   string           `json:"instagram"`
	PriceLevel  int              `json:"price_level"`
	Tags        []string         `json:"tags"`
	Hours       []placeHoursJSON `json:"hours"`
	Menu        []menuItemJSON   `json:"menu"`
}

// locDB = the database connection from main.go (tables are named with their database, so any connection works)
func placeDetailsHandler(locDB *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		kind := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("kind")))
		if kind != "STORE" && kind != "FACILITY" {
			http.Error(w, "kind must be store or facility", http.StatusBadRequest)
			return
		}
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		ctx := r.Context()
		out := placeDetailsJSON{Kind: strings.ToLower(kind), ID: id, Tags: []string{}, Hours: []placeHoursJSON{}, Menu: []menuItemJSON{}}

		// Info
		var desc, phone, website, email, insta, tags sql.NullString
		var price sql.NullInt64
		err = locDB.QueryRowContext(ctx, `
			SELECT description, phone, website, email, instagram, price_level, tags
			  FROM indoor_nav_locations.place_info WHERE place_kind = ? AND place_id = ?`, kind, id).
			Scan(&desc, &phone, &website, &email, &insta, &price, &tags)
		if err != nil && err != sql.ErrNoRows {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		out.Description, out.Phone, out.Website = desc.String, phone.String, website.String
		out.Email, out.Instagram, out.PriceLevel = email.String, insta.String, int(price.Int64)
		for _, t := range strings.Split(tags.String, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out.Tags = append(out.Tags, t)
			}
		}

		// Opening hours
		rows, err := locDB.QueryContext(ctx, `
			SELECT day_of_week, TIME_FORMAT(open_time, '%H:%i'), TIME_FORMAT(close_time, '%H:%i')
			  FROM indoor_nav_locations.place_hours WHERE place_kind = ? AND place_id = ?
			 ORDER BY day_of_week, open_time`, kind, id)
		if err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var h placeHoursJSON
			if err := rows.Scan(&h.Day, &h.Open, &h.Close); err != nil {
				rows.Close()
				http.Error(w, "database error", http.StatusInternalServerError)
				return
			}
			out.Hours = append(out.Hours, h)
		}
		rows.Close()

		// Menu
		rows, err = locDB.QueryContext(ctx, `
			SELECT item_id, section, item_name, COALESCE(description, ''), price, currency,
			       is_vegetarian, is_spicy, is_available
			  FROM indoor_nav_locations.menu_items WHERE place_kind = ? AND place_id = ?
			 ORDER BY MIN(sort_order) OVER (PARTITION BY section), section, sort_order, item_name`, kind, id)
		if err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var m menuItemJSON
			if err := rows.Scan(&m.ID, &m.Section, &m.Name, &m.Description, &m.Price, &m.Currency,
				&m.Vegetarian, &m.Spicy, &m.Available); err != nil {
				http.Error(w, "database error", http.StatusInternalServerError)
				return
			}
			out.Menu = append(out.Menu, m)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}
