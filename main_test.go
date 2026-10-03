package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestNormalizeSeatID(t *testing.T) {
	if got := NormalizeSeatID("  a1  "); got != "A1" {
		t.Fatalf("got %q", got)
	}
}

func TestLivenessAndReadinessSplit(t *testing.T) {
	a := &application{cfg: config{readyTimeout: time.Millisecond}}
	live := httptest.NewRecorder()
	a.livez(live, httptest.NewRequest("GET", "/livez", nil))
	if live.Code != http.StatusOK {
		t.Fatalf("livez returned %d", live.Code)
	}
	ready := httptest.NewRecorder()
	a.readyz(ready, httptest.NewRequest("GET", "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz returned %d", ready.Code)
	}
}

func TestRequestIDAlwaysReturned(t *testing.T) {
	a := &application{cfg: config{requestTimeout: time.Second}, metrics: newMetrics(), logger: slog.Default()}
	h := a.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"ok": "yes"}) }))
	for _, supplied := range []string{"client-id-123", ""} {
		req := httptest.NewRequest("GET", "/test", nil)
		if supplied != "" {
			req.Header.Set("X-Request-ID", supplied)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		id := rr.Header().Get("X-Request-ID")
		if id == "" {
			t.Fatal("response omitted X-Request-ID")
		}
		if supplied != "" && id != supplied {
			t.Fatalf("got request ID %q, want %q", id, supplied)
		}
	}
}

func TestRequestPanicBecomesStructuredError(t *testing.T) {
	a := &application{cfg: config{requestTimeout: time.Second}, metrics: newMetrics(), logger: slog.Default()}
	h := a.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("test failure") }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/broken", nil))
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), `"code":"internal_error"`) || rr.Header().Get("X-Request-ID") == "" {
		t.Fatalf("unexpected panic response: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestMetricsRegisterReservationAndSeatSeries(t *testing.T) {
	m := newMetrics()
	m.incReservationConfirmed()
	m.incReservationDeclined("seat_taken")
	text := m.render(map[string]uint64{"available": 7, "held": 2, "confirmed": 1})
	for _, want := range []string{"reservations_confirmed_total 1", `reservations_declined_total{reason="seat_taken"} 1`, `reservations_declined_total{reason="per_user_limit"} 0`, `seats{status="available"} 7`, `seats{status="held"} 2`, `seats{status="confirmed"} 1`} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestDatabaseCapacityErrorsAreDeclines(t *testing.T) {
	a := &application{logger: slog.Default()}
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{{"pool deadline", context.DeadlineExceeded, http.StatusTooManyRequests}, {"connection limit", &mysql.MySQLError{Number: 1040}, http.StatusTooManyRequests}, {"lock timeout", &mysql.MySQLError{Number: 1205}, http.StatusTooManyRequests}, {"deadlock", &mysql.MySQLError{Number: 1213}, http.StatusTooManyRequests}, {"database unavailable", fmt.Errorf("connection refused"), http.StatusServiceUnavailable}} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/shows", nil).WithContext(context.WithValue(context.Background(), requestIDKey{}, "test-request"))
			a.dbError(rr, req, tc.err)
			if rr.Code != tc.want {
				t.Fatalf("got %d, want %d", rr.Code, tc.want)
			}
		})
	}
}
func TestJWTAuthStatuses(t *testing.T) {
	a := &application{cfg: config{jwtSecret: "secret"}}
	handler := a.authenticate(a.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := r.Context().Value(identityKey{}).(identity)
		if who.UserID != "u1" {
			t.Errorf("unexpected identity: %+v", who)
		}
		w.WriteHeader(204)
	})))
	for _, tc := range []struct {
		name, token string
		want        int
	}{{"missing", "", 401}, {"user", testToken("secret", "u1", "user"), 403}, {"admin", testToken("secret", "u1", "admin"), 204}} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/shows", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("got %d, want %d", rr.Code, tc.want)
			}
		})
	}
}
func testToken(secret, sub, role string) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	c, _ := json.Marshal(map[string]any{"sub": sub, "role": role, "exp": time.Now().Add(time.Hour).Unix()})
	p := h + "." + base64.RawURLEncoding.EncodeToString(c)
	m := hmac.New(sha256.New, []byte(secret))
	_, _ = m.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func TestCreateShowValidation(t *testing.T) {
	a := &application{cfg: config{jwtSecret: "secret"}}
	h := a.authenticate(a.requireAdmin(http.HandlerFunc(a.createShow)))
	tests := []struct{ name, body string }{{"duplicate", "{\"name\":\"x\",\"seats\":[\"a1\",\"A1\"],\"price_paise\":1}"}, {"float", "{\"name\":\"x\",\"seats\":[\"A1\"],\"price_paise\":250.5}"}, {"string price", "{\"name\":\"x\",\"seats\":[\"A1\"],\"price_paise\":\"250\"}"}, {"empty seats", "{\"name\":\"x\",\"seats\":[],\"price_paise\":1}"}, {"too many", "{\"name\":\"x\",\"seats\":[" + strings.TrimSuffix(strings.Repeat(`"A",`, 50001), ",") + "],\"price_paise\":1}"}}
	tests = append(tests,
		struct{ name, body string }{"empty name", `{"name":"  ","seats":["A1"],"price_paise":1}`},
		struct{ name, body string }{"empty seat", `{"name":"x","seats":["  "],"price_paise":1}`},
		struct{ name, body string }{"seat too long", `{"name":"x","seats":["ABCDEFGHIJKLMNOPQ"],"price_paise":1}`},
	)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/shows", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+testToken("secret", "admin", "admin"))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != 400 {
				t.Fatalf("got %d, body %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestCreateShowTenThousandSeats(t *testing.T) {
	dsn := testDSN(t)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		t.Skipf("MariaDB integration test unavailable: %v", err)
	}
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("test-%d", time.Now().UnixNano())
	seats := make([]string, 10000)
	for i := range seats {
		seats[i] = fmt.Sprintf("S%05d", i)
	}
	body, _ := json.Marshal(map[string]any{"name": name, "seats": seats, "price_paise": 250})
	a := &application{db: db, cfg: config{jwtSecret: "secret", requestTimeout: 30 * time.Second, statementTimeout: 10 * time.Second, dbAcquireTimeout: 10 * time.Second, readyTimeout: time.Second}, logger: slog.Default(), metrics: newMetrics()}
	a.ready.Store(true)
	req := httptest.NewRequest("POST", "/shows", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken("secret", "admin", "admin"))
	rr := httptest.NewRecorder()
	started := time.Now()
	a.authenticate(a.requireAdmin(http.HandlerFunc(a.createShow))).ServeHTTP(rr, req)
	if rr.Code != 201 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	if time.Since(started) > 10*time.Second {
		t.Fatalf("10k-seat creation took %s", time.Since(started))
	}
	var out showResponse
	if err = json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var count int
	var available int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(status='available'),0) FROM seats WHERE show_id=?", out.ID).Scan(&count, &available); err != nil {
		t.Fatal(err)
	}
	if count != 10000 || available != 10000 {
		t.Fatalf("count=%d available=%d", count, available)
	}
	showReq := httptest.NewRequest("GET", "/shows/"+out.ID, nil)
	showReq.SetPathValue("id", out.ID)
	showRes := httptest.NewRecorder()
	showStarted := time.Now()
	a.getShow(showRes, showReq)
	showDuration := time.Since(showStarted)
	var summary showSummary
	if err = json.Unmarshal(showRes.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if showRes.Code != 200 || summary.Available != 10000 || summary.Held != 0 || summary.Confirmed != 0 || summary.TotalSeats != 10000 {
		t.Fatalf("unexpected reconciliation response: code=%d body=%s", showRes.Code, showRes.Body.String())
	}
	if len(summary.Seats) != 10000 || showDuration >= time.Second {
		t.Fatalf("10k-seat reconciliation has %d seats and took %s", len(summary.Seats), showDuration)
	}
	if summary.Seats[0].SeatID != "S00000" || summary.Seats[0].Status != "available" || summary.Seats[9999].SeatID != "S09999" {
		t.Fatalf("seat list is not ordered or has unexpected status: first=%+v last=%+v", summary.Seats[0], summary.Seats[9999])
	}
	summaryReq := httptest.NewRequest("GET", "/shows/"+out.ID+"?summary=true", nil)
	summaryReq.SetPathValue("id", out.ID)
	summaryRR := httptest.NewRecorder()
	a.getShow(summaryRR, summaryReq)
	var summaryBody map[string]json.RawMessage
	if err = json.Unmarshal(summaryRR.Body.Bytes(), &summaryBody); err != nil {
		t.Fatal(err)
	}
	if summaryRR.Code != 200 || summaryBody["seats"] != nil {
		t.Fatalf("summary response should omit seats: code=%d body=%s", summaryRR.Code, summaryRR.Body.String())
	}
	unknownReq := httptest.NewRequest("GET", "/shows/00000000-0000-4000-8000-000000000000", nil)
	unknownReq.SetPathValue("id", "00000000-0000-4000-8000-000000000000")
	unknownRR := httptest.NewRecorder()
	a.getShow(unknownRR, unknownReq)
	if unknownRR.Code != http.StatusNotFound || !strings.Contains(unknownRR.Body.String(), `"code":"show_not_found"`) {
		t.Fatalf("unknown show response: code=%d body=%s", unknownRR.Code, unknownRR.Body.String())
	}
	readyRes := httptest.NewRecorder()
	a.readyz(readyRes, httptest.NewRequest("GET", "/readyz", nil))
	if readyRes.Code != http.StatusOK {
		t.Fatalf("readyz returned %d: %s", readyRes.Code, readyRes.Body.String())
	}
	var expectedAvailable, expectedHeld, expectedConfirmed uint64
	if err = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(status='available'),0), COALESCE(SUM(status='held'),0), COALESCE(SUM(status='confirmed'),0) FROM seats`).Scan(&expectedAvailable, &expectedHeld, &expectedConfirmed); err != nil {
		t.Fatal(err)
	}
	metricsRes := httptest.NewRecorder()
	a.prometheus(metricsRes, httptest.NewRequest("GET", "/metrics", nil))
	if metricsRes.Code != http.StatusOK {
		t.Fatalf("metrics returned %d: %s", metricsRes.Code, metricsRes.Body.String())
	}
	for _, want := range []string{fmt.Sprintf(`seats{status="available"} %d`, expectedAvailable), fmt.Sprintf(`seats{status="held"} %d`, expectedHeld), fmt.Sprintf(`seats{status="confirmed"} %d`, expectedConfirmed)} {
		if !strings.Contains(metricsRes.Body.String(), want) {
			t.Errorf("metrics missing DB-derived count %q", want)
		}
	}
}

func TestPoolPressureWaitsThenDeclinesWithout5xx(t *testing.T) {
	db, err := sql.Open("mysql", testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		t.Skipf("MariaDB integration test unavailable: %v", err)
	}
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	held, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	a := &application{db: db, cfg: config{requestTimeout: time.Second, dbAcquireTimeout: 120 * time.Millisecond, statementTimeout: time.Second}, logger: slog.Default()}
	a.ready.Store(true)
	req := httptest.NewRequest("POST", "/shows", strings.NewReader(`{"name":"pool-test","seats":["A1"],"price_paise":1}`))
	started := time.Now()
	res := httptest.NewRecorder()
	a.createShow(res, req)
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429: %s", res.Code, res.Body.String())
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Fatalf("pool acquisition did not wait: %s", elapsed)
	}
}

func TestCreateShowRollbackOnSeatInsertError(t *testing.T) {
	dsn := testDSN(t)
	db, e := sql.Open("mysql", dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if e = db.PingContext(ctx); e != nil {
		t.Skipf("MariaDB integration test unavailable: %v", e)
	}
	if e = migrate(ctx, db); e != nil {
		t.Fatal(e)
	}
	_, e = db.ExecContext(ctx, `CREATE TRIGGER fail_seat_insert BEFORE INSERT ON seats FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected failure'`)
	if e != nil {
		t.Skipf("cannot create failure trigger: %v", e)
	}
	defer db.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS fail_seat_insert`)
	name := fmt.Sprintf("rollback-%d", time.Now().UnixNano())
	a := &application{db: db, cfg: config{jwtSecret: "secret", requestTimeout: 10 * time.Second, statementTimeout: 5 * time.Second, dbAcquireTimeout: 10 * time.Second}, logger: slog.Default()}
	a.ready.Store(true)
	req := httptest.NewRequest("POST", "/shows", strings.NewReader(`{"name":"`+name+`","seats":["A1"],"price_paise":1}`))
	req.Header.Set("Authorization", "Bearer "+testToken("secret", "admin", "admin"))
	rr := httptest.NewRecorder()
	a.authenticate(a.requireAdmin(http.HandlerFunc(a.createShow))).ServeHTTP(rr, req)
	if rr.Code < 400 {
		t.Fatalf("expected failure, got %d", rr.Code)
	}
	var shows, seats int
	if e = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM shows WHERE name=?", name).Scan(&shows); e != nil {
		t.Fatal(e)
	}
	if shows != 0 {
		t.Fatalf("show persisted despite rollback: %d", shows)
	}
	if e = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM seats s JOIN shows sh ON sh.id=s.show_id WHERE sh.name=?", name).Scan(&seats); e != nil {
		t.Fatal(e)
	}
	if seats != 0 {
		t.Fatalf("seats persisted despite rollback: %d", seats)
	}
}

func testDSN(t *testing.T) string {
	t.Helper()
	if d := strings.TrimSpace(os.Getenv("TEST_DB_DSN")); d != "" {
		return d
	}
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4", env("DB_USER", "ticket"), env("DB_PASSWORD", "ticket"), env("DB_HOST", "127.0.0.1:3306"), env("DB_NAME", "tickets"))
}
