package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
)

func main() {
	cfg, err := loadConfig()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err != nil {
		logger.Error("invalid configuration", "request_id", "startup", "error", err)
		os.Exit(1)
	}
	db, err := sql.Open("mysql", cfg.dbDSN)
	if err != nil {
		logger.Error("open database", "request_id", "startup", "error", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(cfg.poolMaxOpen)
	db.SetMaxIdleConns(cfg.poolMaxIdle)
	db.SetConnMaxLifetime(cfg.connLifetime)
	app := &application{db: db, cfg: cfg, metrics: newMetrics(), logger: logger}
	srv := &http.Server{Addr: ":" + cfg.port, Handler: app.middleware(app.routes()), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: cfg.requestTimeout, WriteTimeout: cfg.requestTimeout, IdleTimeout: 60 * time.Second}
	go func() {
		app.logger.Info("server listening", "request_id", "startup", "port", cfg.port)
		if e := srv.ListenAndServe(); e != nil && !errors.Is(e, http.ErrServerClosed) {
			app.logger.Error("server failed", "request_id", "startup", "error", e)
			os.Exit(1)
		}
	}()
	startupCtx, cancelStartup := context.WithCancel(context.Background())
	go app.initialize(startupCtx)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	cancelStartup()
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	_ = srv.Shutdown(shutdown)
	_ = db.Close()
}

type config struct {
	dbDSN, jwtSecret, adminUser, adminPassword, port                                               string
	poolMaxOpen, poolMaxIdle, dbConnectionLimit                                                    int
	connLifetime, statementTimeout, requestTimeout, startupTimeout, dbAcquireTimeout, readyTimeout time.Duration
}

func loadConfig() (config, error) {
	c := config{dbDSN: os.Getenv("DB_DSN"), jwtSecret: os.Getenv("JWT_SECRET"), adminUser: os.Getenv("ADMIN_USERNAME"), adminPassword: os.Getenv("ADMIN_PASSWORD"), port: env("PORT", "8080"), poolMaxOpen: envInt("DB_POOL_MAX_OPEN", 100), poolMaxIdle: envInt("DB_POOL_MAX_IDLE", 50), dbConnectionLimit: envInt("DB_CONNECTION_LIMIT", 400), connLifetime: envDuration("DB_CONN_MAX_LIFETIME", "5m"), statementTimeout: envDuration("DB_STATEMENT_TIMEOUT", "5s"), requestTimeout: envDuration("REQUEST_TIMEOUT", "60s"), startupTimeout: envDuration("DB_STARTUP_TIMEOUT", "5s"), dbAcquireTimeout: envDuration("DB_ACQUIRE_TIMEOUT", "30s"), readyTimeout: envDuration("READY_TIMEOUT", "2s")}
	if c.dbDSN == "" || c.jwtSecret == "" || c.adminUser == "" || c.adminPassword == "" {
		return c, errors.New("DB_DSN, JWT_SECRET, ADMIN_USERNAME, and ADMIN_PASSWORD are required")
	}
	if c.poolMaxOpen < 1 || c.poolMaxIdle < 0 || c.poolMaxIdle > c.poolMaxOpen || c.dbConnectionLimit < 2 || c.poolMaxOpen >= c.dbConnectionLimit {
		return c, errors.New("invalid DB pool sizes")
	}
	for name, d := range map[string]time.Duration{"DB_CONN_MAX_LIFETIME": c.connLifetime, "DB_STATEMENT_TIMEOUT": c.statementTimeout, "REQUEST_TIMEOUT": c.requestTimeout, "DB_STARTUP_TIMEOUT": c.startupTimeout, "DB_ACQUIRE_TIMEOUT": c.dbAcquireTimeout, "READY_TIMEOUT": c.readyTimeout} {
		if d <= 0 {
			return c, fmt.Errorf("%s must be positive", name)
		}
	}
	if c.requestTimeout <= c.dbAcquireTimeout {
		return c, errors.New("REQUEST_TIMEOUT must be greater than DB_ACQUIRE_TIMEOUT")
	}
	return c, nil
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func envInt(k string, d int) int {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	n, e := strconv.Atoi(v)
	if e != nil {
		return -1
	}
	return n
}
func envDuration(k, d string) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		v = d
	}
	n, e := time.ParseDuration(v)
	if e != nil {
		return -1
	}
	return n
}

type application struct {
	db      *sql.DB
	cfg     config
	metrics *metrics
	logger  *slog.Logger
	ready   atomic.Bool
}

func (a *application) initialize(ctx context.Context) {
	delay := 250 * time.Millisecond
	for {
		attempt, cancel := context.WithTimeout(ctx, a.cfg.startupTimeout)
		err := a.db.PingContext(attempt)
		if err == nil {
			err = migrate(attempt, a.db)
		}
		cancel()
		if err == nil {
			a.ready.Store(true)
			a.logger.Info("service ready", "request_id", "startup")
			return
		}
		a.logger.Warn("database startup attempt failed", "request_id", "startup", "error", err, "retry_in", delay.String())
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if delay < 5*time.Second {
			delay *= 2
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
		}
	}
}

func (a *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", a.livez)
	mux.HandleFunc("GET /readyz", a.readyz)
	mux.HandleFunc("GET /metrics", a.prometheus)
	mux.HandleFunc("GET /shows/{id}", a.getShow)
	mux.Handle("POST /shows", a.authenticate(http.HandlerFunc(a.createShow)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "Route not found")
	})
	return mux
}
func (a *application) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if len(id) > 128 || strings.ContainsAny(id, "\r\n") || id == "" {
			id = newID()
		}
		w.Header().Set("X-Request-ID", id)
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), requestIDKey{}, id), a.cfg.requestTimeout)
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				a.logger.Error("request panic", "request_id", id, "error", fmt.Sprint(recovered))
				if !rw.wroteHeader {
					writeError(rw, http.StatusInternalServerError, "internal_error", "The request could not be completed")
				}
			}
			route := "unmatched"
			if r.Method == http.MethodGet && r.URL.Path == "/livez" {
				route = "/livez"
			}
			if r.Method == http.MethodGet && r.URL.Path == "/readyz" {
				route = "/readyz"
			}
			if r.Method == http.MethodGet && r.URL.Path == "/metrics" {
				route = "/metrics"
			}
			if r.Method == http.MethodPost && r.URL.Path == "/shows" {
				route = "/shows"
			}
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/shows/") {
				route = "/shows/{id}"
			}
			a.metrics.observe(route, rw.status, time.Since(start))
			a.logger.Info("http request", "request_id", id, "method", r.Method, "route", route, "status", rw.status, "duration_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rw, r.WithContext(ctx))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(s int) {
	if w.wroteHeader {
		return
	}
	w.status, w.wroteHeader = s, true
	w.ResponseWriter.WriteHeader(s)
}
func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

type requestIDKey struct{}
type identity struct {
	UserID string
	Role   string
}
type identityKey struct{}

func (a *application) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			writeError(w, 401, "unauthorized", "A valid bearer token is required")
			return
		}
		ident, err := verifyJWT(parts[1], a.cfg.jwtSecret)
		if err != nil || ident.UserID == "" {
			writeError(w, 401, "unauthorized", "A valid bearer token is required")
			return
		}
		if ident.Role != "admin" {
			writeError(w, 403, "forbidden", "Admin role is required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, ident)))
	})
}
func verifyJWT(token, secret string) (identity, error) {
	p := strings.Split(token, ".")
	if len(p) != 3 {
		return identity{}, errors.New("invalid token")
	}
	mac := hmacSHA256([]byte(secret), []byte(p[0]+"."+p[1]))
	got, e := decodeURL(p[2])
	if e != nil || !constantEqual(mac, got) {
		return identity{}, errors.New("invalid signature")
	}
	var head struct {
		Alg string `json:"alg"`
	}
	var claims struct {
		Sub  string `json:"sub"`
		Role string `json:"role"`
		Exp  int64  `json:"exp"`
	}
	hb, e := decodeURL(p[0])
	if e != nil {
		return identity{}, e
	}
	if json.Unmarshal(hb, &head) != nil || head.Alg != "HS256" {
		return identity{}, errors.New("unsupported algorithm")
	}
	cb, e := decodeURL(p[1])
	if e != nil {
		return identity{}, e
	}
	if json.Unmarshal(cb, &claims) != nil {
		return identity{}, errors.New("invalid claims")
	}
	if claims.Exp != 0 && time.Now().Unix() >= claims.Exp {
		return identity{}, errors.New("expired")
	}
	return identity{UserID: claims.Sub, Role: claims.Role}, nil
}
func (a *application) livez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
func (a *application) readyz(w http.ResponseWriter, r *http.Request) {
	if !a.ready.Load() {
		writeError(w, 503, "not_ready", "Service is not ready")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.readyTimeout)
	defer cancel()
	if err := a.db.PingContext(ctx); err != nil {
		writeError(w, 503, "not_ready", "Database is unavailable")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

type showRequest struct {
	Name  string          `json:"name"`
	Seats []string        `json:"seats"`
	Price json.RawMessage `json:"price_paise"`
}
type showResponse struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	PricePaise   int64          `json:"price_paise"`
	PerUserLimit int            `json:"per_user_limit"`
	TotalSeats   int            `json:"total_seats"`
	Seats        []seatResponse `json:"seats"`
}
type seatResponse struct {
	SeatID string `json:"seat_id"`
	Status string `json:"status"`
}

func (a *application) createShow(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var in showRequest
	if err := dec.Decode(&in); err != nil {
		writeError(w, 400, "invalid_request", "Request body must be valid JSON")
		return
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, 400, "invalid_request", "Request body must contain one JSON object")
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		writeError(w, 400, "invalid_name", "name must be non-empty")
		return
	}
	var price int64
	if len(in.Price) == 0 || json.Unmarshal(in.Price, &price) != nil || price <= 0 {
		writeError(w, 400, "invalid_price", "price_paise must be a JSON integer greater than zero")
		return
	}
	if len(in.Seats) == 0 {
		writeError(w, 400, "invalid_seats", "seats must be non-empty")
		return
	}
	if len(in.Seats) > 50000 {
		writeError(w, 400, "too_many_seats", "seats may contain at most 50000 entries")
		return
	}
	norm := make([]string, len(in.Seats))
	seen := make(map[string]struct{}, len(in.Seats))
	for i, s := range in.Seats {
		v := NormalizeSeatID(s)
		if v == "" || utf8.RuneCountInString(v) > 16 {
			writeError(w, 400, "invalid_seat", "each seat must be non-empty and at most 16 characters after normalization")
			return
		}
		if _, ok := seen[v]; ok {
			writeError(w, 400, "duplicate_seat", "seat IDs must be unique after normalization")
			return
		}
		seen[v] = struct{}{}
		norm[i] = v
	}
	name := strings.TrimSpace(in.Name)
	if !a.ready.Load() {
		writeError(w, 503, "not_ready", "Service is not ready")
		return
	}
	id := newID()
	ctx := r.Context()
	acquireCtx, cancelAcquire := context.WithTimeout(ctx, a.cfg.dbAcquireTimeout)
	conn, err := a.db.Conn(acquireCtx)
	cancelAcquire()
	if err != nil {
		a.dbError(w, r, err)
		return
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		a.dbError(w, r, err)
		return
	}
	defer tx.Rollback()
	stmtCtx, sc := context.WithTimeout(ctx, a.cfg.statementTimeout)
	_, err = tx.ExecContext(stmtCtx, "INSERT INTO shows (id,name,price_paise,total_seats) VALUES (?,?,?,?)", id, name, price, len(norm))
	sc()
	if err != nil {
		a.dbError(w, r, err)
		return
	}
	const batch = 1000
	for start := 0; start < len(norm); start += batch {
		end := start + batch
		if end > len(norm) {
			end = len(norm)
		}
		var q strings.Builder
		q.WriteString("INSERT INTO seats (show_id,seat_id) VALUES ")
		args := make([]any, 0, (end-start)*2)
		for i := start; i < end; i++ {
			if i > start {
				q.WriteByte(',')
			}
			q.WriteString("(?,?)")
			args = append(args, id, norm[i])
		}
		stmtCtx, sc := context.WithTimeout(ctx, a.cfg.statementTimeout)
		_, err = tx.ExecContext(stmtCtx, q.String(), args...)
		sc()
		if err != nil {
			a.dbError(w, r, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		a.dbError(w, r, err)
		return
	}
	outSeats := make([]seatResponse, len(norm))
	for i, s := range norm {
		outSeats[i] = seatResponse{SeatID: s, Status: "available"}
	}
	writeJSON(w, 201, showResponse{ID: id, Name: name, PricePaise: price, PerUserLimit: 4, TotalSeats: len(norm), Seats: outSeats})
}
func (a *application) dbError(w http.ResponseWriter, r *http.Request, err error) {
	requestID, _ := r.Context().Value(requestIDKey{}).(string)
	if requestID == "" {
		requestID = "unknown"
	}
	a.logger.Error("database operation failed", "request_id", requestID, "error", err)
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusTooManyRequests, "temporarily_busy", "The service is busy; retry the request")
		return
	}
	var dbErr *mysql.MySQLError
	if errors.As(err, &dbErr) && (dbErr.Number == 1040 || dbErr.Number == 1205 || dbErr.Number == 1213) {
		writeError(w, http.StatusTooManyRequests, "temporarily_busy", "The service is busy; retry the request")
		return
	}
	writeError(w, 503, "database_unavailable", "The request could not be completed")
}
func NormalizeSeatID(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

type showSummary struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	PricePaise   int64  `json:"price_paise"`
	PerUserLimit int    `json:"per_user_limit"`
	Available    int    `json:"available"`
	Held         int    `json:"held"`
	Confirmed    int    `json:"confirmed"`
	TotalSeats   int    `json:"total_seats"`
}

func (a *application) getShow(w http.ResponseWriter, r *http.Request) {
	if !a.ready.Load() {
		writeError(w, 503, "not_ready", "Service is not ready")
		return
	}
	ctx := r.Context()
	acquireCtx, cancelAcquire := context.WithTimeout(ctx, a.cfg.dbAcquireTimeout)
	conn, err := a.db.Conn(acquireCtx)
	cancelAcquire()
	if err != nil {
		a.dbError(w, r, err)
		return
	}
	defer conn.Close()
	stmtCtx, cancel := context.WithTimeout(ctx, a.cfg.statementTimeout)
	defer cancel()
	var out showSummary
	err = conn.QueryRowContext(stmtCtx, `SELECT sh.id, sh.name, sh.price_paise, sh.per_user_limit, sh.total_seats,
		COALESCE(SUM(se.status='available'),0), COALESCE(SUM(se.status='held'),0), COALESCE(SUM(se.status='confirmed'),0)
		FROM shows sh LEFT JOIN seats se ON se.show_id=sh.id WHERE sh.id=?
		GROUP BY sh.id, sh.name, sh.price_paise, sh.per_user_limit, sh.total_seats`, r.PathValue("id")).
		Scan(&out.ID, &out.Name, &out.PricePaise, &out.PerUserLimit, &out.TotalSeats, &out.Available, &out.Held, &out.Confirmed)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "show_not_found", "Show not found")
		return
	}
	if err != nil {
		a.dbError(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}

func (a *application) prometheus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	acquireCtx, cancelAcquire := context.WithTimeout(ctx, a.cfg.dbAcquireTimeout)
	conn, err := a.db.Conn(acquireCtx)
	cancelAcquire()
	if err != nil {
		a.dbError(w, r, err)
		return
	}
	defer conn.Close()
	stmtCtx, cancel := context.WithTimeout(ctx, a.cfg.statementTimeout)
	defer cancel()
	rows, err := conn.QueryContext(stmtCtx, "SELECT status, COUNT(*) FROM seats GROUP BY status")
	if err != nil {
		a.dbError(w, r, err)
		return
	}
	seatCounts := map[string]uint64{"available": 0, "held": 0, "confirmed": 0}
	for rows.Next() {
		var status string
		var count uint64
		if err = rows.Scan(&status, &count); err != nil {
			_ = rows.Close()
			a.dbError(w, r, err)
			return
		}
		seatCounts[status] = count
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		a.dbError(w, r, err)
		return
	}
	if err = rows.Close(); err != nil {
		a.dbError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprint(w, a.metrics.render(seatCounts))
	st := a.db.Stats()
	fmt.Fprintf(w, "db_open_connections %d\ndb_in_use_connections %d\ndb_idle_connections %d\ndb_wait_count %d\ndb_wait_duration_seconds %.6f\n", st.OpenConnections, st.InUse, st.Idle, st.WaitCount, st.WaitDuration.Seconds())
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
