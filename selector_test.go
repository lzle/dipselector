package selector

import (
	"context"
	"io"
	"math"
	"os"
	"runtime"
	"sync"
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

func TestNewSelector_nilContext(t *testing.T) {
	s := NewSelector(nil, nil)
	if s == nil {
		t.Fatal("NewSelector with nil context returned nil")
	}
	if s.config.DNSRefresh != 5*time.Minute {
		t.Fatalf("default DNSRefresh: got %v", s.config.DNSRefresh)
	}
}

func TestDecayDomainStats_boostCapAtInitMax(t *testing.T) {
	cfg := testIPConfig()
	s := newManualSelector(cfg)
	domain := "d"
	nearMax := cfg.InitMaxSpeedEWMA * 0.97
	st := &ipStats{
		speedEWMA: nearMax,
		updatedAt: time.Now().Add(-2 * time.Hour),
	}
	s.metrics[domain] = map[string]*ipStats{"192.0.2.1": st}
	s.decayDomainStats(domain)
	if st.speedEWMA != cfg.InitMaxSpeedEWMA {
		t.Fatalf("speedEWMA should be capped at InitMaxSpeedEWMA: got %v want %v", st.speedEWMA, cfg.InitMaxSpeedEWMA)
	}
}

func TestReportSample_minSpeedClamp(t *testing.T) {
	cfg := testIPConfig()
	s := newManualSelector(cfg)
	domain := "d"
	ip := "192.0.2.1"
	s.domainIPs[domain] = []string{ip}
	s.metrics[domain] = map[string]*ipStats{
		ip: {speedEWMA: cfg.MinSpeedEWMA, updatedAt: time.Now()},
	}
	for i := 0; i < 10; i++ {
		if err := s.ReportSample(domain, ip, 0, false); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.RLock()
	st := s.metrics[domain][ip]
	s.mu.RUnlock()
	if st.speedEWMA != cfg.MinSpeedEWMA {
		t.Fatalf("speedEWMA should be clamped at MinSpeedEWMA: got %v want %v", st.speedEWMA, cfg.MinSpeedEWMA)
	}
}

func TestReportSample_maxSpeedClamp(t *testing.T) {
	cfg := testIPConfig()
	s := newManualSelector(cfg)
	domain := "d"
	ip := "192.0.2.1"
	s.domainIPs[domain] = []string{ip}
	s.metrics[domain] = map[string]*ipStats{
		ip: {speedEWMA: cfg.InitMaxSpeedEWMA, updatedAt: time.Now()},
	}
	if err := s.ReportSample(domain, ip, cfg.InitMaxSpeedEWMA*10, true); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	st := s.metrics[domain][ip]
	s.mu.RUnlock()
	if st.speedEWMA != cfg.InitMaxSpeedEWMA {
		t.Fatalf("speedEWMA should be clamped at InitMaxSpeedEWMA: got %v want %v", st.speedEWMA, cfg.InitMaxSpeedEWMA)
	}
}

func TestCalculateScore_allFailures(t *testing.T) {
	cfg := testIPConfig()
	s := newManualSelector(cfg)
	st := &ipStats{
		speedEWMA:    1e7,
		failCount:    5,
		successCount: 0,
	}
	got := s.calculateScore(st)
	want := 1e7 - cfg.FailRatioFactor
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("calculateScore all-failure: got %v want %v", got, want)
	}
}

func TestCalculateScore_mixedVsAllFailures(t *testing.T) {
	cfg := testIPConfig()
	s := newManualSelector(cfg)
	allFail := &ipStats{speedEWMA: 1e7, failCount: 5, successCount: 0}
	partialFail := &ipStats{speedEWMA: 1e7, failCount: 5, successCount: 5}
	if s.calculateScore(allFail) >= s.calculateScore(partialFail) {
		t.Fatal("all-failure should score lower than partial failure at same speed")
	}
}

func testDNSAvailable(t *testing.T) *IPSelector {
	t.Helper()
	cfg := testIPConfig()
	cfg.DNSTimeout = 3 * time.Second
	s := newManualSelector(cfg)
	s.resolveAndSyncIPs("localhost")
	s.mu.RLock()
	_, ok := s.metrics["localhost"]
	ips := s.domainIPs["localhost"]
	s.mu.RUnlock()
	if !ok || len(ips) == 0 {
		t.Skip("cannot resolve localhost for DNS churn test")
	}
	return s
}

const staleTestIP = "192.0.2.99"

func TestResolveAndSyncIPs_staleFailCountRemoved(t *testing.T) {
	if testing.Short() {
		t.Skip("DNS")
	}
	s := testDNSAvailable(t)
	s.mu.Lock()
	s.metrics["localhost"][staleTestIP] = &ipStats{
		failCount:    1,
		successCount: 5,
		updatedAt:    time.Now(),
	}
	s.domainIPs["localhost"] = append(s.domainIPs["localhost"], staleTestIP)
	s.mu.Unlock()

	s.resolveAndSyncIPs("localhost")

	s.mu.RLock()
	_, exists := s.metrics["localhost"][staleTestIP]
	s.mu.RUnlock()
	if exists {
		t.Fatal("stale IP with failCount > 0 should be removed")
	}
}

func TestResolveAndSyncIPs_staleNoSuccessRemoved(t *testing.T) {
	if testing.Short() {
		t.Skip("DNS")
	}
	s := testDNSAvailable(t)
	s.mu.Lock()
	s.metrics["localhost"][staleTestIP] = &ipStats{
		failCount:    0,
		successCount: 0,
		updatedAt:    time.Now(),
	}
	s.domainIPs["localhost"] = append(s.domainIPs["localhost"], staleTestIP)
	s.mu.Unlock()

	s.resolveAndSyncIPs("localhost")

	s.mu.RLock()
	_, exists := s.metrics["localhost"][staleTestIP]
	s.mu.RUnlock()
	if exists {
		t.Fatal("stale IP with successCount == 0 should be removed")
	}
}

func TestResolveAndSyncIPs_staleValidRetained(t *testing.T) {
	if testing.Short() {
		t.Skip("DNS")
	}
	s := testDNSAvailable(t)
	s.mu.Lock()
	s.metrics["localhost"][staleTestIP] = &ipStats{
		failCount:    0,
		successCount: 1,
		updatedAt:    time.Now(),
	}
	s.domainIPs["localhost"] = append(s.domainIPs["localhost"], staleTestIP)
	s.mu.Unlock()

	s.resolveAndSyncIPs("localhost")

	s.mu.RLock()
	st, exists := s.metrics["localhost"][staleTestIP]
	foundInIPs := false
	for _, ip := range s.domainIPs["localhost"] {
		if ip == staleTestIP {
			foundInIPs = true
			break
		}
	}
	s.mu.RUnlock()
	if !exists {
		t.Fatal("valid stale IP should be retained in metrics")
	}
	if !foundInIPs {
		t.Fatal("valid stale IP should be re-appended to IP list")
	}
	if st == nil {
		t.Fatal("retained IP stats should not be nil")
	}
}

func TestResolveAndSyncIPs_staleExpiredRemoved(t *testing.T) {
	if testing.Short() {
		t.Skip("DNS")
	}
	s := testDNSAvailable(t)
	s.mu.Lock()
	s.metrics["localhost"][staleTestIP] = &ipStats{
		failCount:        0,
		successCount:     1,
		dnsExpirationAt:  time.Now().Add(-16 * time.Minute),
		updatedAt:        time.Now(),
	}
	s.domainIPs["localhost"] = append(s.domainIPs["localhost"], staleTestIP)
	s.mu.Unlock()

	s.resolveAndSyncIPs("localhost")

	s.mu.RLock()
	_, exists := s.metrics["localhost"][staleTestIP]
	s.mu.RUnlock()
	if exists {
		t.Fatal("expired stale IP should be removed")
	}
}

func TestResolveAndSyncIPs_staleZeroExpirationGetsTimestamp(t *testing.T) {
	if testing.Short() {
		t.Skip("DNS")
	}
	s := testDNSAvailable(t)
	s.mu.Lock()
	s.metrics["localhost"][staleTestIP] = &ipStats{
		failCount:        0,
		successCount:     1,
		dnsExpirationAt:  time.Time{},
		updatedAt:        time.Now(),
	}
	s.domainIPs["localhost"] = append(s.domainIPs["localhost"], staleTestIP)
	s.mu.Unlock()

	s.resolveAndSyncIPs("localhost")

	s.mu.RLock()
	st, exists := s.metrics["localhost"][staleTestIP]
	s.mu.RUnlock()
	if !exists {
		t.Fatal("stale IP with zero expiration should not be removed")
	}
	if st.dnsExpirationAt.IsZero() {
		t.Fatal("dnsExpirationAt should be set to current time")
	}
}

func TestResolveAndSyncIPs_newIPAdded(t *testing.T) {
	if testing.Short() {
		t.Skip("DNS")
	}
	s := testDNSAvailable(t)
	s.mu.RLock()
	resolvedIPs := s.domainIPs["localhost"]
	s.mu.RUnlock()

	for _, ip := range resolvedIPs {
		s.mu.RLock()
		st, exists := s.metrics["localhost"][ip]
		s.mu.RUnlock()
		if !exists {
			t.Fatalf("new IP %s should exist in metrics", ip)
		}
		if st.speedEWMA != s.config.InitMaxSpeedEWMA {
			t.Fatalf("new IP %s speedEWMA should be InitMaxSpeedEWMA: got %v", ip, st.speedEWMA)
		}
		if st.createdAt.IsZero() {
			t.Fatalf("new IP %s createdAt should be set", ip)
		}
	}
}

func TestResolveAndSyncIPs_existingIPPreserved(t *testing.T) {
	if testing.Short() {
		t.Skip("DNS")
	}
	s := testDNSAvailable(t)
	s.mu.RLock()
	resolvedIPs := s.domainIPs["localhost"]
	s.mu.RUnlock()
	if len(resolvedIPs) == 0 {
		t.Fatal("no localhost IPs resolved")
	}
	existingIP := resolvedIPs[0]
	s.mu.Lock()
	s.metrics["localhost"][existingIP].speedEWMA = 5e7
	s.mu.Unlock()

	s.resolveAndSyncIPs("localhost")

	s.mu.RLock()
	st := s.metrics["localhost"][existingIP]
	s.mu.RUnlock()
	if st.speedEWMA != 5e7 {
		t.Fatalf("existing IP speedEWMA should be preserved: got %v want 5e7", st.speedEWMA)
	}
}

func TestRunRefreshLoop_decayTickerFires(t *testing.T) {
	cfg := testIPConfig()
	cfg.DecayInterval = 20 * time.Millisecond
	cfg.DNSRefresh = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewSelector(ctx, &cfg)

	s.mu.Lock()
	s.domainIPs["d"] = []string{"192.0.2.1"}
	s.metrics["d"] = map[string]*ipStats{
		"192.0.2.1": {speedEWMA: 1e6, successCount: 100, failCount: 100, updatedAt: time.Now()},
	}
	s.mu.Unlock()

	time.Sleep(100 * time.Millisecond)

	s.mu.RLock()
	successCount := s.metrics["d"]["192.0.2.1"].successCount
	failCount := s.metrics["d"]["192.0.2.1"].failCount
	s.mu.RUnlock()
	if successCount >= 100 && failCount >= 100 {
		t.Fatal("decay ticker should have reduced counters")
	}
	if successCount > 70 || failCount > 70 {
		t.Fatalf("counters should decay by 0.7 factor: success=%d fail=%d", successCount, failCount)
	}
}

func TestRunRefreshLoop_contextCancelStops(t *testing.T) {
	cfg := testIPConfig()
	cfg.DecayInterval = 10 * time.Millisecond
	cfg.DNSRefresh = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	s := NewSelector(ctx, &cfg)

	s.mu.Lock()
	s.domainIPs["d"] = []string{"192.0.2.1"}
	s.metrics["d"] = map[string]*ipStats{
		"192.0.2.1": {speedEWMA: 1e6, updatedAt: time.Now()},
	}
	s.mu.Unlock()

	before := runtime.NumGoroutine()
	cancel()
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before {
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}

func TestRunRefreshLoop_emptyStateNoPanic(t *testing.T) {
	cfg := testIPConfig()
	cfg.DecayInterval = 10 * time.Millisecond
	cfg.DNSRefresh = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	NewSelector(ctx, &cfg)
	time.Sleep(50 * time.Millisecond)
	cancel()
}

func TestConcurrentChooseIPAndReportSample(t *testing.T) {
	cfg := testIPConfig()
	s := newManualSelector(cfg)
	domain := "d"
	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	s.mu.Lock()
	s.domainIPs[domain] = ips
	s.metrics[domain] = make(map[string]*ipStats)
	for _, ip := range ips {
		s.metrics[domain][ip] = &ipStats{
			speedEWMA: 1e7,
			updatedAt: time.Now(),
		}
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	const goroutines = 20
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = s.ChooseIP(domain)
			}
		}()
	}
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ip := ips[idx%len(ips)]
			for j := 0; j < 50; j++ {
				_ = s.ReportSample(domain, ip, 1e6, j%2 == 0)
			}
		}(i)
	}
	wg.Wait()

	s.mu.RLock()
	total := 0
	for _, st := range s.metrics[domain] {
		total += st.successCount + st.failCount
	}
	s.mu.RUnlock()
	if total != goroutines*50 {
		t.Fatalf("concurrent ReportSample lost updates: total=%d want %d", total, goroutines*50)
	}
}
