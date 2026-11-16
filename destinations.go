package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type destination struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Type             string `json:"type"`
	FloorID          int64  `json:"floor_id"`
	FloorName        string `json:"floor_name"`
	NavigationNodeID int64  `json:"navigation_node_id"`
	Kind             string `json:"kind"`
}

func destinationsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			sendJSON(w, http.StatusMethodNotAllowed,
				map[string]string{"error": "Use GET"})
			return
		}

		locationID, err := strconv.ParseInt(
			r.URL.Query().Get("location_id"), 10, 64,
		)
		if err != nil || locationID <= 0 {
			sendJSON(w, http.StatusBadRequest,
				map[string]string{"error": "A valid location_id is required"})
			return
		}

		query := strings.TrimSpace(r.URL.Query().Get("q"))

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		rows, err := db.QueryContext(ctx, `
			SELECT f.facility_id AS id, f.facility_name AS name, f.facility_type AS type,
			       f.floor_id AS floor_id, COALESCE(fl.floor_name, '') AS floor_name,
			       f.navigation_node_id AS node_id, 'facility' AS kind
			FROM indoor_nav_locations.facilities AS f
			JOIN indoor_nav_locations.floors AS fl
				ON fl.floor_id = f.floor_id AND fl.location_id = f.location_id
			JOIN indoor_nav_locations.navigation_nodes AS n
				ON n.node_id = f.navigation_node_id AND n.location_id = f.location_id AND n.floor_id = f.floor_id
			JOIN indoor_nav_common.locations AS l
				ON l.location_id = f.location_id
			WHERE f.location_id = ? AND f.is_active = 1 AND n.is_active = 1 AND l.is_active = 1
				AND (? = '' OR LOCATE(?, f.facility_name) > 0)

			UNION ALL

			SELECT s.store_id, s.store_name, c.category_name,
			       s.floor_id, COALESCE(fl.floor_name, ''),
			       s.navigation_node_id, 'store'
			FROM indoor_nav_locations.stores AS s
			JOIN indoor_nav_locations.store_categories AS c
				ON c.category_id = s.category_id
			JOIN indoor_nav_locations.floors AS fl
				ON fl.floor_id = s.floor_id AND fl.location_id = s.location_id
			JOIN indoor_nav_locations.navigation_nodes AS n
				ON n.node_id = s.navigation_node_id AND n.location_id = s.location_id AND n.floor_id = s.floor_id
			JOIN indoor_nav_common.locations AS l
				ON l.location_id = s.location_id
			WHERE s.location_id = ? AND s.is_active = 1 AND n.is_active = 1 AND l.is_active = 1
				AND (? = '' OR LOCATE(?, s.store_name) > 0)

			ORDER BY name, kind, id
			LIMIT 100
		`, locationID, query, query, locationID, query, query)

		if err != nil {
			log.Printf("Destinations query failed: %v", err)
			sendJSON(w, http.StatusInternalServerError,
				map[string]string{"error": "Could not load destinations"})
			return
		}
		defer rows.Close()

		destinations := make([]destination, 0)

		for rows.Next() {
			var item destination
			if err := rows.Scan(
				&item.ID,
				&item.Name,
				&item.Type,
				&item.FloorID,
				&item.FloorName,
				&item.NavigationNodeID,
				&item.Kind,
			); err != nil {
				log.Printf("Destination scan failed: %v", err)
				sendJSON(w, http.StatusInternalServerError,
					map[string]string{"error": "Could not load destinations"})
				return
			}
			destinations = append(destinations, item)
		}

		if err := rows.Err(); err != nil {
			log.Printf("Destination rows failed: %v", err)
			sendJSON(w, http.StatusInternalServerError,
				map[string]string{"error": "Could not load destinations"})
			return
		}

		sendJSON(w, http.StatusOK, map[string]any{
			"location_id":  locationID,
			"destinations": destinations,
		})
	}
}
