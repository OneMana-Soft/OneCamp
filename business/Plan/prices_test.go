package business

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The storefront's public answer, as onemana-backend serves it.
const storefrontPricing = `{"status":"success","data":{
	"currency":"INR","usd_rate":95.7,"usd_rate_at":"2026-11-09T00:00:00Z",
	"lifetime_inr":24999,"lifetime_usd":261,"lifetime_paise":2499900,
	"cloud_inr":9999,"cloud_usd":105,"cloud_paise":999900,"cloud_seats":30,"cloud_configured":true,
	"cloud_yearly_inr":99990,"cloud_yearly_usd":1046,"cloud_yearly_paise":9999000,"cloud_yearly_free_months":2,"cloud_yearly_configured":true,
	"business_inr":24999,"business_usd":261,"business_paise":2499900,"business_seats":100,"business_configured":true,
	"charge_usd":true}}`

func withPriceSeams(t *testing.T, fetch func(context.Context, string) (Prices, error)) *time.Time {
	t.Helper()
	oldFetch, oldNow := fetchPrices, priceNow
	t.Cleanup(func() {
		fetchPrices, priceNow = oldFetch, oldNow
		forgetPrices()
	})
	forgetPrices()
	t.Setenv("ONECAMP_PRICING_URL", "")
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	priceNow = func() time.Time { return clock }
	fetchPrices = fetch
	return &clock
}

func pricesFrom(body string) func(context.Context, string) (Prices, error) {
	return func(context.Context, string) (Prices, error) { return parsePricing(strings.NewReader(body)) }
}

// The server talks to onemana.dev only when someone opens a plan surface:
// nothing at start-up, nothing for a page that doesn't ask.
func TestPricesFetchedOnlyWhenAsked(t *testing.T) {
	var calls atomic.Int32
	withPriceSeams(t, func(ctx context.Context, url string) (Prices, error) {
		calls.Add(1)
		return pricesFrom(storefrontPricing)(ctx, url)
	})
	t.Setenv("ONECAMP_PRICING_URL", "")
	if calls.Load() != 0 {
		t.Fatal("prices were fetched before anyone asked")
	}
	p := CurrentPrices(context.Background())
	if calls.Load() != 1 || !p.Available {
		t.Fatalf("asked once: %d fetches, %+v", calls.Load(), p)
	}
	if p.Licence == nil || p.Licence.USD != 261 || p.Licence.INR != 24999 {
		t.Errorf("the licence, dollars as the storefront derived them: %+v", p.Licence)
	}
	if p.Cloud == nil || p.Cloud.USD != 105 || p.Cloud.INR != 9999 || p.Cloud.Seats != 30 {
		t.Errorf("Cloud with its seats: %+v", p.Cloud)
	}
	if p.CloudYearly == nil || p.CloudYearly.FreeMonths != 2 || p.CloudYearly.INR != 99990 {
		t.Errorf("Cloud yearly: %+v", p.CloudYearly)
	}
	if p.Business == nil || p.Business.Seats != 100 {
		t.Errorf("Business: %+v", p.Business)
	}
	if p.ChargedIn != "INR" || !p.ChargeUSD || p.RateAt == nil || !p.RateAt.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("how it is charged and when the dollars were worked out: %+v", p)
	}
}

// An hour of answers from one fetch, and many admins opening the page at once
// make one request between them.
func TestPricesCachedAnHour(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	clock := withPriceSeams(t, func(ctx context.Context, url string) (Prices, error) {
		calls.Add(1)
		<-release
		return pricesFrom(storefrontPricing)(ctx, url)
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); CurrentPrices(context.Background()) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("eight at once made %d fetches, want 1", calls.Load())
	}
	*clock = clock.Add(59 * time.Minute)
	CurrentPrices(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("within the hour: %d fetches, want 1", calls.Load())
	}
	*clock = clock.Add(2 * time.Minute)
	CurrentPrices(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("after the hour: %d fetches, want 2", calls.Load())
	}
}

// Offline, timed out or switched off: "not available", never an error page,
// and a server that can't reach onemana.dev doesn't try again on every open.
func TestPricesUnavailableOffline(t *testing.T) {
	var calls atomic.Int32
	clock := withPriceSeams(t, func(context.Context, string) (Prices, error) {
		calls.Add(1)
		return Prices{}, errors.New("could not reach backend.onemana.dev")
	})
	if p := CurrentPrices(context.Background()); p.Available || p.Licence != nil {
		t.Fatalf("offline: %+v", p)
	}
	CurrentPrices(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("a failure is remembered for a while: %d fetches", calls.Load())
	}
	*clock = clock.Add(6 * time.Minute)
	CurrentPrices(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("and asked again after it: %d fetches", calls.Load())
	}

	forgetPrices()
	t.Setenv("ONECAMP_PRICING_URL", "off")
	if p := CurrentPrices(context.Background()); p.Available {
		t.Fatalf("switched off: %+v", p)
	}
	if calls.Load() != 2 {
		t.Fatalf("switched off asks nobody: %d fetches", calls.Load())
	}
}

// Only sane positive whole numbers survive; no remote text is ever passed on.
func TestPricesDropNonNumbers(t *testing.T) {
	p, err := parsePricing(strings.NewReader(`{"data":{
		"currency":"<script>alert(1)</script>","usd_rate_at":"yesterday",
		"lifetime_inr":24999,"lifetime_usd":"261",
		"cloud_inr":-5,"cloud_usd":105,"cloud_seats":30,"cloud_configured":true,
		"cloud_yearly_inr":99990.5,"cloud_yearly_usd":1046,"cloud_yearly_free_months":2,"cloud_yearly_configured":true,
		"business_inr":1e12,"business_usd":261,"business_seats":100,"business_configured":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Available || p.Licence == nil || p.Licence.INR != 24999 || p.Licence.USD != 0 {
		t.Errorf("a licence in rupees survives, its quoted dollar doesn't: %+v", p.Licence)
	}
	if p.Cloud != nil || p.CloudYearly != nil || p.Business != nil {
		t.Errorf("a negative, a fraction and an absurd price drop their offer: %+v %+v %+v", p.Cloud, p.CloudYearly, p.Business)
	}
	if p.ChargedIn != "" || p.RateAt != nil {
		t.Errorf("text that isn't a currency code or a time is dropped: %+v", p)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "script") || strings.Contains(string(raw), "yesterday") {
		t.Errorf("remote text was echoed: %s", raw)
	}

	if p, _ := parsePricing(strings.NewReader(`{"data":{"lifetime_inr":0,"cloud_inr":9999,"cloud_seats":30,"cloud_configured":true}}`)); p.Available {
		t.Errorf("no licence price, no prices: %+v", p)
	}
	if p, _ := parsePricing(strings.NewReader(`{"data":{"lifetime_inr":24999,"cloud_inr":9999,"cloud_usd":105,"cloud_seats":30,"cloud_configured":false}}`)); p.Cloud != nil {
		t.Errorf("an offer the storefront hasn't configured isn't shown: %+v", p.Cloud)
	}
	if _, err := parsePricing(strings.NewReader(`not json`)); err == nil {
		t.Error("an unreadable answer is an error")
	}
}

// The real request: a plain GET for JSON, a non-200 is a failure, and a slow
// server is cut off.
func TestPricesFetchAsksPlainly(t *testing.T) {
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		if r.URL.Path == "/down" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(storefrontPricing))
	}))
	defer srv.Close()
	p, err := fetchPublishedPricing(context.Background(), srv.URL+"/onecamp/pricing")
	if err != nil || !p.Available || accept != "application/json" {
		t.Fatalf("fetch: %+v %v (Accept %q)", p, err, accept)
	}
	if _, err := fetchPublishedPricing(context.Background(), srv.URL+"/down"); err == nil {
		t.Error("a 502 is a failure")
	}
	t.Setenv("ONECAMP_PRICING_URL", "")
	if got := pricingURL(); got != defaultPricingURL {
		t.Errorf("default %q", got)
	}
}

// forgetPrices drops the cached answer.
func forgetPrices() {
	priceCache.Lock()
	priceCache.prices = nil
	priceCache.Unlock()
}
