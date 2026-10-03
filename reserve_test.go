package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func TestReserveValidationAndKeySelection(t *testing.T) {
	a := &application{cfg: config{jwtSecret: "secret"}}
	a.ready.Store(true)
	user := identity{UserID: "buyer", Role: "user"}
	for _, tc := range []struct {
		name, body, header string
	}{
		{"empty seats", `{"seats":[],"idempotency_key":"k"}`, ""},
		{"duplicate normalized seats", `{"seats":["a1"," A1 "],"idempotency_key":"k"}`, ""},
		{"missing key", `{"seats":["A1"]}`, ""},
		{"mismatching keys", `{"seats":["A1"],"idempotency_key":"body"}`, "header"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/shows/00000000-0000-4000-8000-000000000000/reserve", strings.NewReader(tc.body))
			req.SetPathValue("id", "00000000-0000-4000-8000-000000000000")
			req = req.WithContext(context.WithValue(req.Context(), identityKey{}, user))
			if tc.header != "" {
				req.Header.Set("Idempotency-Key", tc.header)
			}
			rr := httptest.NewRecorder()
			a.reserve(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestReserveHotSeatOneWinner500Users(t *testing.T) {
	h := newReserveHarness(t, 20, []string{"A1"})
	const n = 500
	results := parallelRequests(n, func(i int) reserveHTTPResult {
		return h.reserve("user-"+strconv.Itoa(i), "key-"+strconv.Itoa(i), []string{"a1"}, "")
	})
	created, conflicts, serverErrors := 0, 0, 0
	for _, res := range results {
		if res.err != nil {
			t.Fatal(res.err)
		}
		switch res.status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			if res.status >= 500 {
				serverErrors++
			}
			t.Errorf("unexpected status %d: %s", res.status, res.body)
		}
	}
	if created != 1 || conflicts != n-1 || serverErrors != 0 {
		t.Fatalf("201=%d 409=%d 5xx=%d", created, conflicts, serverErrors)
	}
	assertShowInvariant(t, h.db, h.showID)
	var reservations int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM reservations WHERE show_id=?`, h.showID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 1 {
		t.Fatalf("got %d reservations, want 1", reservations)
	}
	var idemRows int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM idempotency_keys WHERE show_id=?`, h.showID).Scan(&idemRows); err != nil {
		t.Fatal(err)
	}
	if idemRows != 1 {
		t.Fatalf("declines persisted idempotency rows: got %d, want 1", idemRows)
	}
	assertReservationMetrics(t, h.app.metrics, 1, map[string]uint64{"seat_taken": n - 1})
}

func TestReservePerUserLimitUnderConcurrency(t *testing.T) {
	h := newReserveHarness(t, 4, []string{"A1", "A2", "A3", "A4", "A5", "A6", "A7", "A8", "A9", "A10"})
	results := parallelRequests(10, func(i int) reserveHTTPResult {
		return h.reserve("one-user", "limit-"+strconv.Itoa(i), []string{fmt.Sprintf("a%d", i+1)}, "")
	})
	created, conflicts := 0, 0
	for _, res := range results {
		if res.err != nil {
			t.Fatal(res.err)
		}
		if res.status == http.StatusCreated {
			created++
		} else if res.status == http.StatusConflict {
			conflicts++
		} else {
			t.Fatalf("unexpected status %d: %s", res.status, res.body)
		}
	}
	if created > 4 || conflicts != 10-created {
		t.Fatalf("201=%d 409=%d, per-user limit is 4", created, conflicts)
	}
	var confirmed int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM seats WHERE show_id=? AND user_id='one-user' AND status='confirmed'`, h.showID).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed > 4 {
		t.Fatalf("user confirmed %d seats with limit 4", confirmed)
	}
	assertReservationMetrics(t, h.app.metrics, uint64(created), map[string]uint64{"per_user_limit": uint64(conflicts)})
	var idemRows int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM idempotency_keys WHERE show_id=?`, h.showID).Scan(&idemRows); err != nil {
		t.Fatal(err)
	}
	if idemRows != created {
		t.Fatalf("declines persisted idempotency rows: got %d, successful reservations=%d", idemRows, created)
	}
	assertShowInvariant(t, h.db, h.showID)
}

func TestReserveSameKeyParallelReplaysIdentical201(t *testing.T) {
	h := newReserveHarness(t, 4, []string{"A1", "A2"})
	results := parallelRequests(50, func(int) reserveHTTPResult { return h.reserve("same-user", "same-key", []string{"a1"}, "") })
	var body []byte
	replays, created := 0, 0
	for _, res := range results {
		if res.err != nil {
			t.Fatal(res.err)
		}
		if res.status != http.StatusCreated {
			t.Fatalf("got %d: %s", res.status, res.body)
		}
		if body == nil {
			body = res.body
		} else if !bytes.Equal(body, res.body) {
			t.Fatalf("replay body differs: %s vs %s", body, res.body)
		}
		if res.replayed {
			replays++
		} else {
			created++
		}
	}
	if created != 1 || replays != 49 {
		t.Fatalf("initial=%d replays=%d", created, replays)
	}
	var reservationCount int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM reservations WHERE show_id=? AND user_id='same-user'`, h.showID).Scan(&reservationCount); err != nil {
		t.Fatal(err)
	}
	if reservationCount != 1 {
		t.Fatalf("got %d reservations, want 1", reservationCount)
	}
	assertReservationMetrics(t, h.app.metrics, 1, map[string]uint64{"idempotent_replay": 49})
	assertShowInvariant(t, h.db, h.showID)
}

func TestReserveSameKeyDifferentSeatsConflicts(t *testing.T) {
	h := newReserveHarness(t, 4, []string{"A1", "A2"})
	start := make(chan struct{})
	results := make([]reserveHTTPResult, 2)
	var wg sync.WaitGroup
	for i, seats := range [][]string{{"A1"}, {"A2"}} {
		wg.Add(1)
		go func(i int, seats []string) {
			defer wg.Done()
			<-start
			results[i] = h.reserve("same-user", "one-key", seats, "")
		}(i, seats)
	}
	close(start)
	wg.Wait()
	created, keyConflicts := 0, 0
	for _, res := range results {
		if res.err != nil {
			t.Fatal(res.err)
		}
		if res.status == http.StatusCreated {
			created++
		} else if res.status == http.StatusConflict && strings.Contains(string(res.body), "idempotency_key_conflict") {
			keyConflicts++
		} else {
			t.Fatalf("unexpected result: %d %s", res.status, res.body)
		}
	}
	if created != 1 || keyConflicts != 1 {
		t.Fatalf("201=%d idempotency conflicts=%d", created, keyConflicts)
	}
	assertShowInvariant(t, h.db, h.showID)
}

func TestReserveOppositeSeatOrderHasNoDeadlocksOr5xx(t *testing.T) {
	h := newReserveHarness(t, 1000, []string{"A1", "A2"})
	const pairs = 50
	results := parallelRequests(pairs*2, func(i int) reserveHTTPResult {
		seats := []string{"A1", "A2"}
		if i%2 == 1 {
			seats = []string{"A2", "A1"}
		}
		return h.reserve("opposite-"+strconv.Itoa(i), "opposite-key-"+strconv.Itoa(i), seats, "")
	})
	for _, res := range results {
		if res.err != nil {
			t.Fatal(res.err)
		}
		if res.status != http.StatusCreated && res.status != http.StatusConflict {
			t.Fatalf("unexpected status %d: %s", res.status, res.body)
		}
	}
	assertShowInvariant(t, h.db, h.showID)
}

func TestReserveMultiSeatIsAllOrNothing(t *testing.T) {
	h := newReserveHarness(t, 4, []string{"A12", "A13"})
	if _, err := h.db.Exec(`UPDATE seats SET status='confirmed', user_id='already-booked' WHERE show_id=? AND seat_id='A13'`, h.showID); err != nil {
		t.Fatal(err)
	}
	res := h.reserve("new-user", "partial-key", []string{"A12", "A13"}, "")
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.status != http.StatusConflict || !strings.Contains(string(res.body), "seat_taken") {
		t.Fatalf("got %d: %s", res.status, res.body)
	}
	var status string
	if err := h.db.QueryRow(`SELECT status FROM seats WHERE show_id=? AND seat_id='A12'`, h.showID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "available" {
		t.Fatalf("A12 status is %s after rejected all-or-nothing request", status)
	}
	var idemRows int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM idempotency_keys WHERE user_id='new-user' AND idem_key=?`, h.showID+":partial-key").Scan(&idemRows); err != nil {
		t.Fatal(err)
	}
	if idemRows != 0 {
		t.Fatal("all-or-nothing decline persisted the idempotency row")
	}
	assertShowInvariant(t, h.db, h.showID)
}

func TestReserveIdentityComesFromJWT(t *testing.T) {
	h := newReserveHarness(t, 4, []string{"A1"})
	res := h.reserve("jwt-user", "identity-key", []string{"A1"}, "spoofed-body-user")
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.status != http.StatusCreated {
		t.Fatalf("got %d: %s", res.status, res.body)
	}
	var out reserveResponse
	if err := json.Unmarshal(res.body, &out); err != nil {
		t.Fatal(err)
	}
	if out.UserID != "jwt-user" {
		t.Fatalf("response user is %q", out.UserID)
	}
	var stored string
	if err := h.db.QueryRow(`SELECT user_id FROM seats WHERE show_id=? AND seat_id='A1'`, h.showID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "jwt-user" {
		t.Fatalf("seat belongs to %q", stored)
	}
	assertShowInvariant(t, h.db, h.showID)
}

func TestReserveIdempotencyHeaderAndMissingSeat(t *testing.T) {
	h := newReserveHarness(t, 4, []string{"A1"})
	res := h.reserveHeader("header-user", "header-key", []string{"a1"})
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.status != http.StatusCreated {
		t.Fatalf("header idempotency key returned %d: %s", res.status, res.body)
	}
	missing := h.reserve("header-user", "missing-seat", []string{"A2"}, "")
	if missing.err != nil {
		t.Fatal(missing.err)
	}
	if missing.status != http.StatusNotFound {
		t.Fatalf("missing seat returned %d: %s", missing.status, missing.body)
	}
	assertShowInvariant(t, h.db, h.showID)
}

type reserveHarness struct {
	db     *sql.DB
	app    *application
	server *httptest.Server
	client *http.Client
	showID string
}
type reserveHTTPResult struct {
	status   int
	body     []byte
	replayed bool
	err      error
}

func newReserveHarness(t *testing.T, limit int, seats []string) *reserveHarness {
	t.Helper()
	db, err := sql.Open("mysql", testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(200)
	db.SetMaxIdleConns(100)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("MariaDB integration test unavailable: %v", err)
	}
	if err = migrate(ctx, db); err != nil {
		db.Close()
		t.Fatal(err)
	}
	showID := newID()
	if _, err = db.ExecContext(ctx, `INSERT INTO shows (id,name,price_paise,per_user_limit,total_seats) VALUES (?,?,?, ?, ?)`, showID, "reserve-test-"+showID, 1234, limit, len(seats)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	for start := 0; start < len(seats); start += 1000 {
		end := min(start+1000, len(seats))
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*2)
		for _, seat := range seats[start:end] {
			values = append(values, "(?,?)")
			args = append(args, showID, seat)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO seats (show_id,seat_id) VALUES `+strings.Join(values, ","), args...); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	app := &application{db: db, cfg: config{jwtSecret: "reserve-test-secret", requestTimeout: 60 * time.Second, dbAcquireTimeout: 30 * time.Second, statementTimeout: 10 * time.Second}, metrics: newMetrics(), logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	app.ready.Store(true)
	server := httptest.NewServer(app.middleware(app.routes()))
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = db.Close() })
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxConnsPerHost: 600, MaxIdleConns: 200}}
	t.Cleanup(func() { client.CloseIdleConnections() })
	return &reserveHarness{db: db, app: app, server: server, client: client, showID: showID}
}

func (h *reserveHarness) reserve(user, key string, seats []string, spoof string) reserveHTTPResult {
	payload := map[string]any{"seats": seats, "idempotency_key": h.showID + ":" + key}
	if spoof != "" {
		payload["user_id"] = spoof
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/shows/"+h.showID+"/reserve", bytes.NewReader(body))
	if err != nil {
		return reserveHTTPResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken("reserve-test-secret", user, "user"))
	res, err := h.client.Do(req)
	if err != nil {
		return reserveHTTPResult{err: err}
	}
	defer res.Body.Close()
	responseBody, err := io.ReadAll(res.Body)
	if err != nil {
		return reserveHTTPResult{err: err}
	}
	return reserveHTTPResult{status: res.StatusCode, body: responseBody, replayed: res.Header.Get("Idempotent-Replayed") == "true"}
}

func (h *reserveHarness) reserveHeader(user, key string, seats []string) reserveHTTPResult {
	body, _ := json.Marshal(map[string]any{"seats": seats})
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/shows/"+h.showID+"/reserve", bytes.NewReader(body))
	if err != nil {
		return reserveHTTPResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken("reserve-test-secret", user, "user"))
	req.Header.Set("Idempotency-Key", h.showID+":"+key)
	res, err := h.client.Do(req)
	if err != nil {
		return reserveHTTPResult{err: err}
	}
	defer res.Body.Close()
	responseBody, err := io.ReadAll(res.Body)
	if err != nil {
		return reserveHTTPResult{err: err}
	}
	return reserveHTTPResult{status: res.StatusCode, body: responseBody, replayed: res.Header.Get("Idempotent-Replayed") == "true"}
}

func parallelRequests(n int, run func(int) reserveHTTPResult) []reserveHTTPResult {
	start := make(chan struct{})
	results := make([]reserveHTTPResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; results[i] = run(i) }(i)
	}
	close(start)
	wg.Wait()
	return results
}

func assertShowInvariant(t *testing.T, db *sql.DB, showID string) {
	t.Helper()
	var total, rowCount, available, held, confirmed int
	err := db.QueryRow(`SELECT sh.total_seats, COUNT(se.seat_id), COALESCE(SUM(se.status='available'),0), COALESCE(SUM(se.status='held'),0), COALESCE(SUM(se.status='confirmed'),0) FROM shows sh LEFT JOIN seats se ON se.show_id=sh.id WHERE sh.id=? GROUP BY sh.id, sh.total_seats`, showID).Scan(&total, &rowCount, &available, &held, &confirmed)
	if err != nil {
		t.Fatal(err)
	}
	if rowCount != total || available+held+confirmed != total {
		t.Fatalf("seat invariant failed: rows=%d available=%d held=%d confirmed=%d total=%d", rowCount, available, held, confirmed, total)
	}
}

func assertReservationMetrics(t *testing.T, m *metrics, confirmed uint64, declined map[string]uint64) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reservationsConfirmed != confirmed {
		t.Fatalf("confirmed metric=%d, want %d", m.reservationsConfirmed, confirmed)
	}
	for reason, want := range declined {
		if got := m.reservationsDeclined[reason]; got != want {
			t.Fatalf("declined metric %s=%d, want %d", reason, got, want)
		}
	}
}
