package business

// What the ways past the free plan cost, for the Plan page and the plan card.
//
// The prices live on onemana.dev (onemana-backend's GET /onecamp/pricing). A
// self-hosted browser can't read them there (onemana.dev answers only its own
// origins), so this server asks for it. It does so only when an admin, or the
// demo's visitor, opens a plan surface: never at start-up, as the update
// check never does. The answer is kept for an hour; a failure for a few
// minutes, so an offline server doesn't make every open wait for a timeout.
//
// Only sane positive whole numbers are passed on. The dollar figures are the
// storefront's own, worked out from the rupee charge at its daily rate; none
// is typed here. No remote text is ever echoed.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// Prices is GET /plan/prices' answer. Available false: show the pricing link.
type Prices struct {
	Available   bool       `json:"available"`
	Licence     *Money     `json:"licence,omitempty"`
	Cloud       *Offer     `json:"cloud,omitempty"`
	CloudYearly *Yearly    `json:"cloud_yearly,omitempty"`
	Business    *Offer     `json:"business,omitempty"`
	RateAt      *time.Time `json:"rate_at,omitempty"`
	// ChargedIn is the currency every charge is made in ("INR").
	ChargedIn string `json:"charged_in,omitempty"`
	// ChargeUSD: buyers outside India are charged the dollar price for the licence.
	ChargeUSD bool `json:"charge_usd,omitempty"`
}

// Money is a price in rupees, with the storefront's dollar figure when it has one.
type Money struct {
	USD int `json:"usd,omitempty"`
	INR int `json:"inr"`
}

// Offer is a Cloud size: its monthly price and how many people it is for.
type Offer struct {
	Money
	Seats int `json:"seats"`
}

// Yearly is Cloud paid by the year.
type Yearly struct {
	Money
	FreeMonths int `json:"free_months,omitempty"`
}

const (
	defaultPricingURL = "https://backend.onemana.dev/onecamp/pricing"
	pricesTTL         = time.Hour
	pricesFailedTTL   = 5 * time.Minute
	pricesTimeout     = 8 * time.Second
	// The largest figure believed: a price above it is a mistake upstream.
	maxPrice = 10_000_000
	maxSeats = 100_000
)

// Seams.
var (
	fetchPrices = fetchPublishedPricing
	priceNow    = time.Now
)

var priceCache struct {
	sync.Mutex
	at     time.Time
	ttl    time.Duration
	prices *Prices
}

// CurrentPrices is the storefront's prices, from the cache when fresh. Callers
// asking at once wait on one fetch. Never an error: unavailable is an answer.
func CurrentPrices(ctx context.Context) Prices {
	from := pricingURL()
	if strings.EqualFold(from, "off") {
		return Prices{}
	}
	priceCache.Lock()
	defer priceCache.Unlock()
	if priceCache.prices != nil && priceNow().Sub(priceCache.at) < priceCache.ttl {
		return *priceCache.prices
	}
	// Not the caller's context: one admin closing the page must not cut the
	// fetch short for everyone waiting on it.
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pricesTimeout)
	defer cancel()
	p, err := fetchPrices(fetchCtx, from)
	ttl := pricesTTL
	if err != nil || !p.Available {
		p, ttl = Prices{}, pricesFailedTTL
	}
	priceCache.prices, priceCache.at, priceCache.ttl = &p, priceNow(), ttl
	return p
}

// pricingURL is ONECAMP_PRICING_URL, or the storefront's own; "off" turns the
// fetch off.
func pricingURL() string { return helpers.StorefrontURL("ONECAMP_PRICING_URL", defaultPricingURL) }

// fetchPublishedPricing GETs the storefront's prices.
func fetchPublishedPricing(ctx context.Context, from string) (Prices, error) {
	return helpers.GetStorefront(ctx, from, parsePricing)
}

// parsePricing reads the storefront's {"data":{...}}, keeping only sane
// positive whole numbers, an offer only when the storefront has configured
// it, a currency only from a known list and a time only as a time. Pure.
func parsePricing(r io.Reader) (Prices, error) {
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return Prices{}, fmt.Errorf("unreadable prices: %w", err)
	}
	d := body.Data
	money := func(prefix string) (Money, bool) {
		inr := whole(d[prefix+"_inr"], maxPrice)
		return Money{INR: inr, USD: whole(d[prefix+"_usd"], maxPrice)}, inr > 0
	}
	configured := func(prefix string) bool { on, _ := d[prefix+"_configured"].(bool); return on }

	licence, ok := money("lifetime")
	if !ok {
		return Prices{}, nil
	}
	p := Prices{Available: true, Licence: &licence}
	if m, ok := money("cloud"); ok && configured("cloud") {
		if seats := whole(d["cloud_seats"], maxSeats); seats > 0 {
			p.Cloud = &Offer{Money: m, Seats: seats}
		}
	}
	if m, ok := money("cloud_yearly"); ok && configured("cloud_yearly") {
		p.CloudYearly = &Yearly{Money: m, FreeMonths: whole(d["cloud_yearly_free_months"], 12)}
	}
	if m, ok := money("business"); ok && configured("business") {
		if seats := whole(d["business_seats"], maxSeats); seats > 0 {
			p.Business = &Offer{Money: m, Seats: seats}
		}
	}
	if s, _ := d["usd_rate_at"].(string); s != "" {
		if at, err := time.Parse(time.RFC3339, s); err == nil {
			at = at.UTC()
			p.RateAt = &at
		}
	}
	if c, _ := d["currency"].(string); c == "INR" || c == "USD" {
		p.ChargedIn = c
	}
	p.ChargeUSD, _ = d["charge_usd"].(bool)
	return p, nil
}

// whole is v as a positive whole number no larger than most, else 0. Pure.
func whole(v any, most int) int {
	f, ok := v.(float64)
	if !ok || f <= 0 || f > float64(most) || f != math.Trunc(f) {
		return 0
	}
	return int(f)
}
