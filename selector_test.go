package selector

import (
	"context"
	"io"
	"math"
	"os"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	logrus.SetOutput(io.Discard)
	os.Exit(m.Run())
}

func testIPConfig() IPConfig {
	return IPConfig{
		DNSRefresh:       time.Hour,
		DecayInterval:    time.Hour,
		DNSTimeout:       time.Second,
		Alpha:            0.2,
		FailRatioFactor:  1e6,
		ChanceTime:       time.Nanosecond,
		InitMaxSpeedEWMA: 1e8,
		MinSpeedEWMA:     1e5,
	}
}

func newManualSelector(cfg IPConfig) *IPSelector {
	return &IPSelector{
		config:    cfg,
		domainIPs: make(map[string][]string),
		metrics:   make(map[string]map[string]*ipStats),
	}
}

func TestNewSelector(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewSelector(ctx, nil)
	if s == nil {
		t.Fatal("NewSelector returned nil")
	}
	if s.config.DNSRefresh != 5*time.Minute {
		t.Fatalf("default DNSRefresh: got %v", s.config.DNSRefresh)
	}
	cancel()
	time.Sleep(20 * time.Millisecond)
}

func TestNewSelector_customConfig(t *testing.T) {
	cfg := testIPConfig()
	cfg.DNSRefresh = 42 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewSelector(ctx, &cfg)
	if s.config.DNSRefresh != 42*time.Second {
		t.Fatalf("custom DNSRefresh: got %v", s.config.DNSRefresh)
	}
	cancel()
}

func TestCalculateScore(t *testing.T) {
	cfg := testIPConfig()
	s := newManualSelector(cfg)
	st := &ipStats{
		speedEWMA:    1e7,
		successCount: 9,
		failCount:    1,
	}
	got := s.calculateScore(st)
	failRate := 1.0 / 10.0
	want := 1e7 - cfg.FailRatioFactor*failRate
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("calculateScore: got %v want %v", got, want)
	}
	if s.calculateScore(&ipStats{speedEWMA: 1e6, successCount: 0, failCount: 0}) != 1e6 {
		t.Fatal("zero samples should use failRate denominator 1 with 0 fails")
	}
}

func TestChooseIP_prefersLowerFailureRate(t *testing.T) {
	cfg := testIPConfig()
	s := newManualSelector(cfg)
	domain := "d"
	s.domainIPs[domain] = []string{"10.0.0.1", "10.0.0.2"}
	s.metrics[domain] = map[string]*ipStats{
		"10.0.0.1": {speedEWMA: 1e7, successCount: 10, failCount: 0, updatedAt: time.Now()},
		"10.0.0.2": {speedEWMA: 1e7, successCount: 10, failCount: 9, updatedAt: time.Now()},
	}
	ip, err := s.ChooseIP(domain)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.0.0.1" {
		t.Fatalf("ChooseIP: got %q want 10.0.0.1", ip)
	}
}

func TestChooseIP_prefersHigherSpeed(t *testing.T) {
	cfg := testIPConfig()
	s := newManualSelector(cfg)
	domain := "d"
	s.domainIPs[domain] = []string{"10.0.0.1", "10.0.0.2"}
	s.metrics[domain] = map[string]*ipStats{
		"10.0.0.1": {speedEWMA: 2e7, successCount: 1, failCount: 0, updatedAt: time.Now()},
		"10.0.0.2": {speedEWMA: 1e7, successCount: 1, failCount: 0, updatedAt: time.Now()},
	}
	ip, err := s.ChooseIP(domain)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.0.0.1" {
		t.Fatalf("ChooseIP: got %q want 10.0.0.1", ip)
	}
}

func TestChooseIP_emptyList(t *testing.T) {
	cfg := testIPConfig()
	cfg.DNSTimeout = time.Millisecond
	s := newManualSelector(cfg)
	_, err := s.ChooseIP("nodns.invalid.")
	if err == nil {
		t.Fatal("expected error when no IPs resolved")
	}
}

func TestReportSample_success(t *testing.T) {
	cfg := testIPConfig()
	s := newManualSelector(cfg)
	domain := "d"
	ip := "192.0.2.1"
	s.domainIPs[domain] = []string{ip}
	s.metrics[domain] = map[string]*ipStats{
		ip: {speedEWMA: 1e8, successCount: 0, failCount: 0, updatedAt: time.Now()},
	}
	if err := s.ReportSample(domain, ip, 1e6, true); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	st := s.metrics[domain][ip]
	s.mu.RUnlock()
	if st.successCount != 1 {
		t.Fatalf("successCount: got %d", st.successCount)
	}
	if st.speedEWMA >= 1e8-1 {
		t.Fatalf("speedEWMA should drop toward sample: got %v", st.speedEWMA)
	}
}

func TestReportSample_failure(t *testing.T) {
	cfg := testIPConfig()
	s := newManualSelector(cfg)
	domain := "d"
	ip := "192.0.2.1"
	s.domainIPs[domain] = []string{ip}
	s.metrics[domain] = map[string]*ipStats{
		ip: {speedEWMA: 1e8, successCount: 1, failCount: 0, updatedAt: time.Now()},
	}
	if err := s.ReportSample(domain, ip, 1e9, false); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	st := s.metrics[domain][ip]
	s.mu.RUnlock()
	if st.failCount != 1 {
		t.Fatalf("failCount: got %d", st.failCount)
	}
	if st.speedEWMA == 1e8 {
		t.Fatal("failed sample should pull EWMA down from speed=0")
	}
}

func TestReportSample_errors(t *testing.T) {
	cfg := testIPConfig()
	s := newManualSelector(cfg)
	s.metrics["d"] = map[string]*ipStats{"192.0.2.1": {}}
	if err := s.ReportSample("other", "192.0.2.1", 1, true); err == nil {
		t.Fatal("expected domain not found")
	}
	if err := s.ReportSample("d", "192.0.2.2", 1, true); err == nil {
		t.Fatal("expected IP not found")
	}
}

func TestDecayDomainStats_unknownDomain(t *testing.T) {
	s := newManualSelector(testIPConfig())
	s.decayDomainStats("missing")
}

func TestDecayDomainStats_decayAndBoost(t *testing.T) {
	cfg := testIPConfig()
	s := newManualSelector(cfg)
	domain := "d"
	st := &ipStats{
		speedEWMA:       1e6,
		successCount:    100,
		failCount:       100,
		updatedAt:       time.Now().Add(-2 * time.Hour),
		dnsExpirationAt: time.Time{},
	}
	s.metrics[domain] = map[string]*ipStats{"192.0.2.1": st}
	s.decayDomainStats(domain)
	if st.failCount != 70 || st.successCount != 70 {
		t.Fatalf("decay counters: fail=%d success=%d", st.failCount, st.successCount)
	}
	if st.speedEWMA <= 1e6 {
		t.Fatalf("expected speed boost after idle: got %v", st.speedEWMA)
	}
	if st.speedEWMA > cfg.InitMaxSpeedEWMA {
		t.Fatalf("speed capped: got %v", st.speedEWMA)
	}
}

func TestLogDomainStats(t *testing.T) {
	s := newManualSelector(testIPConfig())
	s.metrics["d"] = map[string]*ipStats{
		"192.0.2.1": {speedEWMA: 1e6, updatedAt: time.Now()},
	}
	s.logDomainStats()
}

func TestIntegration_localhost(t *testing.T) {
	if testing.Short() {
		t.Skip("DNS")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := testIPConfig()
	cfg.DNSTimeout = 5 * time.Second
	s := NewSelector(ctx, &cfg)
	ip, err := s.ChooseIP("localhost")
	if err != nil {
		t.Fatal(err)
	}
	if ip == "" {
		t.Fatal("empty IP")
	}
	if err := s.ReportSample("localhost", ip, 1e5, true); err != nil {
		t.Fatal(err)
	}
	cancel()
}
