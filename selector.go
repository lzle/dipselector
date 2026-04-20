package selector

import (
	"context"
	"fmt"
	"math"
	"net"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// IPConfig holds configuration parameters for the IP selector,
// including DNS TTL, decay timing, EWMA (Exponentially Weighted Moving Average)
// parameters, and factors affecting failure ratio and selection chance.
//
// Fields:
//   - DNSRefresh: Interval for DNS cache and auto-refresh ticker.
//   - DecayInterval: The duration over which the EWMA decays.
//   - Alpha: EWMA smoothing factor (0<Alpha<1).
//   - FailRatioFactor: The factor used to adjust the impact of failure ratios.
//   - ChanceTime: Duration after which an inactive IP (no recent updates) receives
//     a small temporary boost in selection probability during penalty decay.
//   - InitMaxSpeedEWMA: The initial value for the speed EWMA.
//   - MinSpeedEWMA: The minimum value allowed for the speed EWMA.
type IPConfig struct {
	DNSRefresh       time.Duration
	DecayInterval    time.Duration
	DNSTimeout       time.Duration
	Alpha            float64
	FailRatioFactor  float64
	ChanceTime       time.Duration
	InitMaxSpeedEWMA float64
	MinSpeedEWMA     float64
}

// ipStats stores metrics for a given domain-IP pair
type ipStats struct {
	speedEWMA       float64   // bytes per second, smoothed via EWMA
	successCount    int       // number of success requests
	failCount       int       // number of failed requests
	createdAt       time.Time // when this IP was first seen
	updatedAt       time.Time // last metric update time
	dnsExpirationAt time.Time // when IP resolution DNS expiration time
}

// IPSelector provides methods to choose the best IP for requests, update metrics, and auto-refresh DNS
type IPSelector struct {
	config    IPConfig
	mu        sync.RWMutex
	domainIPs map[string][]string            // domain -> IP list
	metrics   map[string]map[string]*ipStats // domain -> IP -> stats
}

// NewSelector creates a new IPSelector instance. If cfg is nil, default settings are used.
func NewSelector(ctx context.Context, cfg *IPConfig) *IPSelector {
	if ctx == nil {
		ctx = context.Background()
	}
	defaultCfg := IPConfig{
		DNSRefresh:       5 * time.Minute,
		DecayInterval:    3 * time.Minute,
		DNSTimeout:       10 * time.Second,
		Alpha:            0.2,
		FailRatioFactor:  1e8, // penalty factor for failure rate (1e8 = 1 failure per 100MB/s)
		ChanceTime:       10 * time.Minute,
		InitMaxSpeedEWMA: 1e8, // initial speed estimate (100MB/s)
		MinSpeedEWMA:     1e5, // minimum speed estimate (100KB/s)
	}
	if cfg != nil {
		defaultCfg = *cfg
	}
	selector := &IPSelector{
		config:    defaultCfg,
		domainIPs: make(map[string][]string),
		metrics:   make(map[string]map[string]*ipStats),
	}

	selector.runRefreshLoop(ctx)
	return selector
}

// runRefreshLoop runs until ctx is canceled: periodic DNS sync for all known domains,
// periodic decay of per-domain stats, and logging after each DNS refresh tick.
func (s *IPSelector) runRefreshLoop(ctx context.Context) {
	dTicker := time.NewTicker(s.config.DNSRefresh)
	eTicker := time.NewTicker(s.config.DecayInterval)
	go func() {
		for {
			select {
			case <-ctx.Done():
				dTicker.Stop()
				eTicker.Stop()
				return
			case <-dTicker.C:
				s.mu.RLock()
				domains := make([]string, 0, len(s.domainIPs))
				for d := range s.domainIPs {
					domains = append(domains, d)
				}
				s.mu.RUnlock()
				for _, d := range domains {
					s.resolveAndSyncIPs(d)
				}
				s.logDomainStats()
			case <-eTicker.C:
				s.mu.RLock()
				domains := make([]string, 0, len(s.domainIPs))
				for d := range s.domainIPs {
					domains = append(domains, d)
				}
				s.mu.RUnlock()
				for _, d := range domains {
					s.decayDomainStats(d)
				}
			}
		}
	}()
}

// ChooseIP returns the IP with the highest selection score for domain.
// Score is speedEWMA penalized by failure rate; higher is better.
// If there is no cached IP list, DNS is refreshed before choosing.
func (s *IPSelector) ChooseIP(domain string) (string, error) {
	s.mu.RLock()
	ipList, exists := s.domainIPs[domain]
	isEmpty := len(ipList) == 0
	s.mu.RUnlock()
	// Refresh immediately if no cache or empty list
	if !exists || isEmpty {
		s.resolveAndSyncIPs(domain)
	}

	// Score each IP
	var bestIP string
	var bestScore float64
	first := true
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, ip := range s.domainIPs[domain] {
		st := s.metrics[domain][ip]
		score := s.calculateScore(st)
		if first || score > bestScore {
			bestIP = ip
			bestScore = score
			first = false
		}
	}

	if bestIP == "" {
		return "", fmt.Errorf("failed to choose IP for domain %s", domain)
	}

	return bestIP, nil
}

func (s *IPSelector) calculateScore(st *ipStats) float64 {
	samples := st.successCount + st.failCount
	failRate := float64(st.failCount) / math.Max(float64(samples), 1)
	return st.speedEWMA - s.config.FailRatioFactor*failRate
}

// ReportSample records the observed speed (bytes/sec) and success/failure result
// of a request into the selector's performance metrics for (domain, ip).
func (s *IPSelector) ReportSample(domain, ip string, speed float64, success bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.metrics[domain]; !ok {
		return fmt.Errorf("domain %s not found", domain)
	}
	st, ok := s.metrics[domain][ip]
	if !ok {
		return fmt.Errorf("IP %s not found for domain %s", ip, domain)
	}

	if success {
		st.successCount++
	} else {
		st.failCount++
		speed = 0
	}

	speedEWMA := st.speedEWMA*(1-s.config.Alpha) + speed*s.config.Alpha
	speedEWMA = math.Max(speedEWMA, s.config.MinSpeedEWMA)
	// need write lock
	st.speedEWMA = math.Min(speedEWMA, s.config.InitMaxSpeedEWMA)
	st.updatedAt = time.Now()

	return nil
}

// decayDomainStats applies exponential decay to success/fail counters
// and boosts rarely-used IPs to improve their chance of selection.
func (s *IPSelector) decayDomainStats(domain string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var m map[string]*ipStats
	var exists bool
	if m, exists = s.metrics[domain]; !exists {
		return
	}

	for _, st := range m {
		// need write lock. 100->0 need 11 times when decay 0.7
		st.failCount = int(float64(st.failCount) * 0.7)
		st.successCount = int(float64(st.successCount) * 0.7)

		if time.Since(st.updatedAt) > s.config.ChanceTime {
			st.speedEWMA *= 1.05 // boost by 5%
			if st.speedEWMA > s.config.InitMaxSpeedEWMA {
				st.speedEWMA = s.config.InitMaxSpeedEWMA
			}
		}
	}
}

// resolveAndSyncIPs resolves the domain via DNS, merges with cached IPs,
// prunes stale entries, and initializes stats for newly seen IPs.
func (s *IPSelector) resolveAndSyncIPs(domain string) {
	ctx, cancel := context.WithTimeout(context.Background(), s.config.DNSTimeout)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupHost(ctx, domain)
	if err != nil {
		logrus.WithError(err).WithField("domain", domain).Error("DNS lookup failed")
		addrs = []string{}
	}

	logrus.WithField("domain", domain).WithField("addrs", addrs).Info("resolved IPs")

	s.mu.Lock()
	defer s.mu.Unlock()

	// some times domain resolved IPs change every time
	newIPSet := make(map[string]bool)
	for _, ip := range addrs {
		newIPSet[ip] = true
	}

	if _, ok := s.metrics[domain]; !ok {
		s.metrics[domain] = make(map[string]*ipStats)
	}

	for ip, ipStats := range s.metrics[domain] {
		if !newIPSet[ip] {
			if ipStats.dnsExpirationAt.IsZero() {
				ipStats.dnsExpirationAt = time.Now()
			}
			if time.Since(ipStats.dnsExpirationAt) > 15*time.Minute {
				logrus.Infof("removing expired IP %s for domain %s", ip, domain)
				delete(s.metrics[domain], ip)
				continue
			}
			// If the IP is not in the new list, check if it has failed or succeeded
			if ipStats.failCount > 0 || ipStats.successCount == 0 {
				logrus.Infof("removing outdated IP %s for domain %s", ip, domain)
				delete(s.metrics[domain], ip)
			} else {
				logrus.Infof("keeping IP %s for domain %s", ip, domain)
				addrs = append(addrs, ip)
			}
		}
	}

	for _, ip := range addrs {
		if _, exists := s.metrics[domain][ip]; !exists {
			logrus.Infof("adding new IP %s for domain %s", ip, domain)
			s.metrics[domain][ip] = &ipStats{
				speedEWMA: s.config.InitMaxSpeedEWMA,
				createdAt: time.Now(),
				updatedAt: time.Now(),
			}
		}
	}

	s.domainIPs[domain] = addrs
}

// logDomainStats logs per-IP metrics for every known domain.
func (s *IPSelector) logDomainStats() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for domain, ipMap := range s.metrics {
		logrus.Infof("=== Selector IP for domain: %s ===", domain)
		for ip, st := range ipMap {
			logrus.Infof(
				"IP: %-15s | Score %10.2f | SpeedEWMA: %10.2f B/s | Success: %4d | Fail: %4d | LastUpdate: %s",
				ip,
				s.calculateScore(st),
				st.speedEWMA,
				st.successCount,
				st.failCount,
				st.updatedAt.Format(time.RFC3339),
			)
		}
	}
}
