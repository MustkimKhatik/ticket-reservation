package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"sort"
	"sync"
	"time"
)

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(data)
	return h.Sum(nil)
}
func constantEqual(a, b []byte) bool     { return hmac.Equal(a, b) }
func decodeURL(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
func newID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type sample struct {
	value  float64
	labels string
}
type metrics struct {
	mu                    sync.Mutex
	counts                map[string]uint64
	buckets               map[string][]uint64
	sums                  map[string]float64
	bounds                []float64
	reservationsConfirmed uint64
	reservationsDeclined  map[string]uint64
}

func newMetrics() *metrics {
	return &metrics{counts: map[string]uint64{}, buckets: map[string][]uint64{}, sums: map[string]float64{}, bounds: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}, reservationsDeclined: map[string]uint64{"seat_taken": 0, "per_user_limit": 0, "idempotent_replay": 0}}
}
func (m *metrics) incReservationConfirmed() { m.mu.Lock(); m.reservationsConfirmed++; m.mu.Unlock() }
func (m *metrics) incReservationDeclined(reason string) {
	m.mu.Lock()
	if _, ok := m.reservationsDeclined[reason]; ok {
		m.reservationsDeclined[reason]++
	}
	m.mu.Unlock()
}
func (m *metrics) observe(route string, status int, d time.Duration) {
	class := fmt.Sprintf("%dxx", status/100)
	key := route + "\x00" + class
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[key]++
	v := d.Seconds()
	m.sums[key] += v
	if m.buckets[key] == nil {
		m.buckets[key] = make([]uint64, len(m.bounds)+1)
	}
	for i, b := range m.bounds {
		if v <= b {
			m.buckets[key][i]++
		}
	}
	m.buckets[key][len(m.bounds)]++
}
func (m *metrics) render(seats map[string]uint64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s string
	s += "# HELP http_requests_total Total HTTP requests.\n# TYPE http_requests_total counter\n"
	keys := make([]string, 0, len(m.counts))
	for k := range m.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		route, class := splitKey(k)
		s += fmt.Sprintf("http_requests_total{route=%q,status_class=%q} %d\n", route, class, m.counts[k])
	}
	s += "# HELP http_request_duration_seconds HTTP request latency.\n# TYPE http_request_duration_seconds histogram\n"
	for _, k := range keys {
		route, class := splitKey(k)
		for i, b := range m.bounds {
			s += fmt.Sprintf("http_request_duration_seconds_bucket{route=%q,status_class=%q,le=%q} %d\n", route, class, fmt.Sprint(b), m.buckets[k][i])
		}
		s += fmt.Sprintf("http_request_duration_seconds_bucket{route=%q,status_class=%q,le=\"+Inf\"} %d\n", route, class, m.buckets[k][len(m.bounds)])
		s += fmt.Sprintf("http_request_duration_seconds_sum{route=%q,status_class=%q} %g\nhttp_request_duration_seconds_count{route=%q,status_class=%q} %d\n", route, class, m.sums[k], route, class, m.counts[k])
	}
	s += "# HELP reservations_confirmed_total Reservations successfully confirmed.\n# TYPE reservations_confirmed_total counter\n"
	s += fmt.Sprintf("reservations_confirmed_total %d\n", m.reservationsConfirmed)
	s += "# HELP reservations_declined_total Reservation declines by reason.\n# TYPE reservations_declined_total counter\n"
	for _, reason := range []string{"seat_taken", "per_user_limit", "idempotent_replay"} {
		s += fmt.Sprintf("reservations_declined_total{reason=%q} %d\n", reason, m.reservationsDeclined[reason])
	}
	s += "# HELP seats Current seat count by status, queried directly from MariaDB at scrape time.\n# TYPE seats gauge\n"
	for _, status := range []string{"available", "held", "confirmed"} {
		s += fmt.Sprintf("seats{status=%q} %d\n", status, seats[status])
	}
	return s
}
func splitKey(k string) (string, string) {
	for i := range k {
		if k[i] == 0 {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}
