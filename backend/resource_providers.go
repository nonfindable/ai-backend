package main

import (
	"context"
	"errors"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---- resource providers ----
//
// WHAT IS AND IS NOT IMPLEMENTED HERE
//
// This file is the ABSTRACTION plus the selection rules. It ships with NO real
// marketplace integration: Uzum, Yandex Market and Amazon all require
// documented, credentialed APIs that this repository does not have, and
// inventing endpoints for them would produce exactly the fabricated prices and
// URLs the design forbids. registerProvider is the seam a real integration
// plugs into; until one is registered, resourceService.FindProduct reports
// "no verified result" and the learning plan is completely unaffected.
//
// The AI decides WHAT is needed (kind, query, purpose, whether alternatives are
// acceptable). Providers supply the FACTS (title, price, currency, stock, URL,
// when it was checked). Neither side does the other's job: the model never
// invents a listing, and a provider never decides whether a book fits a plan.

// ProductSearchRequest is a resource need expressed as a provider query.
type ProductSearchRequest struct {
	Query      string
	Kind       string // book|gear|software|...
	Country    string
	Currency   string
	MaxResults int
}

// MarketplaceListing is one real result from one provider. Every field is
// either supplied by the provider or explicitly marked unknown; nothing here
// may be guessed, and the *Known flags exist so "we were not told" can never be
// silently rendered as "free" or "in stock".
type MarketplaceListing struct {
	Provider  string `json:"provider"`
	ProductID string `json:"productId"`
	Title     string `json:"title"`
	Brand     string `json:"brand,omitempty"`
	// Edition, Variant, Format, Language, Condition and BundleCount are the
	// axes an equivalence check runs on. Empty means the provider did not say,
	// which is treated as "unknown", never as "matches".
	Edition     string `json:"edition,omitempty"`
	Variant     string `json:"variant,omitempty"` // e.g. Academic / General Training
	Format      string `json:"format,omitempty"`  // physical | digital | audio
	Language    string `json:"language,omitempty"`
	Condition   string `json:"condition,omitempty"` // new | used
	BundleCount int    `json:"bundleCount,omitempty"`

	// PriceMinor is in the currency's minor unit (tiyin, cents). Integers
	// because money comparisons must not drift.
	PriceMinor int64  `json:"priceMinor"`
	Currency   string `json:"currency"`
	PriceKnown bool   `json:"priceKnown"`

	ShippingMinor int64 `json:"shippingMinor"`
	ShippingKnown bool  `json:"shippingKnown"`

	InStock    bool `json:"inStock"`
	StockKnown bool `json:"stockKnown"`

	URL       string    `json:"url,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

// MarketplaceProvider is one shop start.ai can ask about a product.
type MarketplaceProvider interface {
	Name() string
	// AllowedHosts bounds the URLs this provider may return. A listing URL from
	// any other host is dropped rather than shown, so a compromised or sloppy
	// provider cannot turn a recommendation into a link to somewhere else — and
	// nothing in the backend ever fetches these URLs.
	AllowedHosts() []string
	Search(ctx context.Context, req ProductSearchRequest) ([]MarketplaceListing, error)
}

// CourseSearchRequest is the same idea for structured courses.
type CourseSearchRequest struct {
	Query      string
	Skill      string
	Level      string
	Language   string
	Country    string
	Currency   string
	MaxResults int
}

// CourseListing is one real course from one provider.
type CourseListing struct {
	Provider   string        `json:"provider"`
	CourseID   string        `json:"courseId"`
	Title      string        `json:"title"`
	Skill      string        `json:"skill,omitempty"`
	Level      string        `json:"level,omitempty"`
	Language   string        `json:"language,omitempty"`
	Hours      int           `json:"hours,omitempty"`
	PriceMinor int64         `json:"priceMinor"`
	Currency   string        `json:"currency"`
	PriceKnown bool          `json:"priceKnown"`
	Free       bool          `json:"free"`
	URL        string        `json:"url,omitempty"`
	CheckedAt  time.Time     `json:"checkedAt"`
	Duration   time.Duration `json:"-"`
}

type CourseProvider interface {
	Name() string
	AllowedHosts() []string
	Search(ctx context.Context, req CourseSearchRequest) ([]CourseListing, error)
}

// ResourceNeed is what the plan actually requires. The AI fills it in; it is
// validated here before any provider sees it.
type ResourceNeed struct {
	Kind        string // book|gear|software|course
	Query       string
	Purpose     string
	Edition     string
	Variant     string
	Format      string
	Language    string
	Condition   string
	BundleCount int // 1 means "a single copy", 0 means "not specified"
	Currency    string
	Country     string
	// MaxMinor is the learner's stated budget in minor units; 0 means none.
	MaxMinor int64
}

// ---- selection ----

// ProductChoice is the outcome of comparing verified listings.
type ProductChoice struct {
	Listing    MarketplaceListing
	Considered int
	Suitable   int
	// EffectiveMinor is price plus shipping when shipping is known for every
	// compared listing; otherwise it equals the listed price and Claim says so.
	EffectiveMinor int64
	// Claim is the exact sentence the assistant is allowed to make about this
	// result. It never says "cheapest delivered" unless shipping really was
	// comparable across the candidates.
	Claim string
	// Rejections records why each discarded listing lost, so "why not the
	// cheaper one?" has a factual answer.
	Rejections []string
}

var (
	// errNoProviders means nothing is configured — the honest default state.
	errNoProviders = errors.New("no marketplace providers configured")
	// errNoSuitable means providers answered but nothing matched the need.
	errNoSuitable = errors.New("no suitable verified listing")
	// errMixedCurrency means the candidates are priced in different currencies
	// and there is no trusted FX source, so no "cheapest" claim is supportable.
	errMixedCurrency = errors.New("candidates priced in different currencies")
)

// variantKeywords are the distinctions that make two otherwise identical
// products non-interchangeable.
var variantKeywords = []string{"academic", "general training", "general", "workbook", "teacher"}

// isSuitable decides whether a listing can serve the need at all. It runs
// BEFORE any price comparison, because the rule is cheapest SUITABLE product,
// not cheapest search result.
func isSuitable(need ResourceNeed, l MarketplaceListing) (bool, string) {
	if !l.PriceKnown {
		return false, l.Title + ": no verified price"
	}
	if l.StockKnown && !l.InStock {
		return false, l.Title + ": out of stock"
	}
	// The listing must actually be the thing asked for: every significant word
	// of the query has to appear. This is what stops a cheap unrelated item
	// winning on price alone.
	if miss := missingQueryTokens(need.Query, l.Title+" "+l.Brand+" "+l.Edition); miss != "" {
		return false, l.Title + ": does not match the requested product (" + miss + ")"
	}
	if need.Edition != "" {
		if l.Edition != "" && !strings.EqualFold(strings.TrimSpace(l.Edition), strings.TrimSpace(need.Edition)) {
			return false, l.Title + ": edition " + l.Edition + ", need " + need.Edition
		}
		if l.Edition == "" && !strings.Contains(normalizeResourceKey(l.Title), normalizeResourceKey(need.Edition)) {
			return false, l.Title + ": edition not confirmed"
		}
	}
	if need.Variant != "" {
		lv := detectVariant(l.Variant + " " + l.Title)
		nv := detectVariant(need.Variant)
		if nv != "" && lv != "" && lv != nv {
			return false, l.Title + ": " + lv + ", need " + nv
		}
		if nv != "" && lv == "" {
			return false, l.Title + ": variant not confirmed"
		}
	}
	if need.Format != "" && l.Format != "" && !strings.EqualFold(need.Format, l.Format) {
		return false, l.Title + ": " + l.Format + ", need " + need.Format
	}
	if need.Language != "" && l.Language != "" && !strings.EqualFold(need.Language, l.Language) {
		return false, l.Title + ": in " + l.Language
	}
	if need.Condition != "" && l.Condition != "" && !strings.EqualFold(need.Condition, l.Condition) {
		return false, l.Title + ": " + l.Condition
	}
	if need.BundleCount > 0 && l.BundleCount > 0 && l.BundleCount != need.BundleCount {
		return false, l.Title + ": bundle of " + itoa(l.BundleCount)
	}
	if need.MaxMinor > 0 && l.PriceMinor > need.MaxMinor {
		return false, l.Title + ": over budget"
	}
	return true, ""
}

// detectVariant normalizes the Academic/General distinction and its kin.
func detectVariant(s string) string {
	low := normalizeResourceKey(s)
	for _, v := range variantKeywords {
		if strings.Contains(low, v) {
			if v == "general" && strings.Contains(low, "general training") {
				return "general training"
			}
			return v
		}
	}
	return ""
}

// stopWords are ignored when checking that a listing matches the query.
var stopWords = map[string]bool{"the": true, "a": true, "an": true, "and": true, "for": true, "of": true}

// missingQueryTokens returns the first significant query word absent from text,
// or "" when the listing covers the whole query.
func missingQueryTokens(query, text string) string {
	hay := " " + normalizeResourceKey(text) + " "
	for _, tok := range strings.Fields(normalizeResourceKey(query)) {
		if stopWords[tok] || len(tok) < 2 {
			continue
		}
		if !strings.Contains(hay, " "+tok+" ") {
			return "missing " + tok
		}
	}
	return ""
}

// chooseCheapestSuitable applies the rule in full: filter to what genuinely
// serves the need, refuse to compare across currencies without a trusted rate,
// and only claim a delivered price when shipping was known for everything
// compared.
func chooseCheapestSuitable(need ResourceNeed, listings []MarketplaceListing) (*ProductChoice, error) {
	choice := &ProductChoice{Considered: len(listings)}
	var suitable []MarketplaceListing
	for _, l := range listings {
		ok, why := isSuitable(need, l)
		if !ok {
			if why != "" {
				choice.Rejections = append(choice.Rejections, why)
			}
			continue
		}
		suitable = append(suitable, l)
	}
	choice.Suitable = len(suitable)
	if len(suitable) == 0 {
		return choice, errNoSuitable
	}

	// Currencies. With a stated currency we filter; without one, a mix has no
	// defensible winner, because converting needs an FX source we do not have.
	currencies := map[string]bool{}
	for _, l := range suitable {
		currencies[strings.ToUpper(l.Currency)] = true
	}
	if want := strings.ToUpper(strings.TrimSpace(need.Currency)); want != "" {
		filtered := suitable[:0]
		for _, l := range suitable {
			if strings.EqualFold(l.Currency, want) {
				filtered = append(filtered, l)
			}
		}
		suitable = filtered
		if len(suitable) == 0 {
			return choice, errNoSuitable
		}
	} else if len(currencies) > 1 {
		return choice, errMixedCurrency
	}

	// Shipping is only additive when every candidate declares it.
	shippingComparable := true
	for _, l := range suitable {
		if !l.ShippingKnown {
			shippingComparable = false
			break
		}
	}
	effective := func(l MarketplaceListing) int64 {
		if shippingComparable {
			return l.PriceMinor + l.ShippingMinor
		}
		return l.PriceMinor
	}
	sort.SliceStable(suitable, func(i, j int) bool {
		if a, b := effective(suitable[i]), effective(suitable[j]); a != b {
			return a < b
		}
		// Ties resolve by provider name so the result is deterministic.
		return suitable[i].Provider < suitable[j].Provider
	})

	best := suitable[0]
	choice.Listing = best
	choice.EffectiveMinor = effective(best)
	if shippingComparable {
		choice.Claim = "Lowest delivered price among the verified results checked."
	} else {
		choice.Claim = "Lowest listed product price among the verified results checked; shipping could not be compared."
	}
	return choice, nil
}

// courseScore ranks a course by fit first and price last, because a free
// beginner course that does not address the learner's gap is worth nothing.
// Lower is better.
func courseScore(req CourseSearchRequest, c CourseListing) (int, bool) {
	score := 0
	if req.Skill != "" && c.Skill != "" && !strings.EqualFold(req.Skill, c.Skill) {
		return 0, false // wrong subject is disqualifying, at any price
	}
	if req.Level != "" && c.Level != "" && !strings.EqualFold(req.Level, c.Level) {
		score += 40 // wrong level is a heavy penalty but not always fatal
	}
	if req.Language != "" && c.Language != "" && !strings.EqualFold(req.Language, c.Language) {
		score += 30
	}
	if miss := missingQueryTokens(req.Query, c.Title+" "+c.Skill); miss != "" {
		score += 20
	}
	if !c.PriceKnown {
		score += 10
	}
	return score, true
}

// chooseCourse picks the most suitable course, preferring the cheaper of two
// similarly suitable ones.
func chooseCourse(req CourseSearchRequest, listings []CourseListing) (*CourseListing, error) {
	type scored struct {
		c CourseListing
		s int
	}
	var out []scored
	for _, c := range listings {
		s, ok := courseScore(req, c)
		if !ok {
			continue
		}
		out = append(out, scored{c, s})
	}
	if len(out) == 0 {
		return nil, errNoSuitable
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].s != out[j].s {
			return out[i].s < out[j].s
		}
		// Similarly suitable: the cheaper one wins, unknown price last.
		ai, aj := out[i].c, out[j].c
		if ai.PriceKnown != aj.PriceKnown {
			return ai.PriceKnown
		}
		if ai.PriceMinor != aj.PriceMinor {
			return ai.PriceMinor < aj.PriceMinor
		}
		return ai.Provider < aj.Provider
	})
	best := out[0].c
	return &best, nil
}

// ---- the service ----

const (
	// providerTimeout bounds one provider call. A slow shop must not hold up a
	// learning plan.
	providerTimeout = 6 * time.Second
	// providerConcurrency bounds how many shops are queried at once.
	providerConcurrency = 4
	// productCacheTTL and courseCacheTTL stop identical searches being repeated.
	// Product prices move; course catalogues barely do.
	productCacheTTL = 30 * time.Minute
	courseCacheTTL  = 12 * time.Hour
	// maxProviderResults bounds what one provider may contribute.
	maxProviderResults = 25
	// maxResourceCacheEntries bounds the cache itself.
	maxResourceCacheEntries = 256
)

type cachedProducts struct {
	listings []MarketplaceListing
	at       time.Time
}

type cachedCourses struct {
	listings []CourseListing
	at       time.Time
}

// ResourceService fans a need out across configured providers, caches what
// comes back, and never lets a provider failure reach the learning plan.
type ResourceService struct {
	mu        sync.RWMutex
	markets   []MarketplaceProvider
	courses   []CourseProvider
	products  map[string]cachedProducts
	courseHit map[string]cachedCourses
	order     []string
	now       func() time.Time
}

func newResourceService() *ResourceService {
	return &ResourceService{
		products:  map[string]cachedProducts{},
		courseHit: map[string]cachedCourses{},
		now:       time.Now,
	}
}

// registerProvider adds a marketplace. This is the seam a real integration
// plugs into; nothing is registered by default.
func (rs *ResourceService) registerProvider(p MarketplaceProvider) {
	if p == nil {
		return
	}
	rs.mu.Lock()
	rs.markets = append(rs.markets, p)
	rs.mu.Unlock()
}

func (rs *ResourceService) registerCourseProvider(p CourseProvider) {
	if p == nil {
		return
	}
	rs.mu.Lock()
	rs.courses = append(rs.courses, p)
	rs.mu.Unlock()
}

// ProviderNames lists what is actually wired up, for the startup banner and
// for tests that assert the honest empty default.
func (rs *ResourceService) ProviderNames() []string {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	out := make([]string, 0, len(rs.markets)+len(rs.courses))
	for _, p := range rs.markets {
		out = append(out, p.Name())
	}
	for _, p := range rs.courses {
		out = append(out, p.Name())
	}
	sort.Strings(out)
	return out
}

// Enabled reports whether any provider exists at all.
func (rs *ResourceService) Enabled() bool {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return len(rs.markets) > 0 || len(rs.courses) > 0
}

func productCacheKey(provider string, req ProductSearchRequest) string {
	return strings.Join([]string{
		provider, strings.ToLower(req.Country), strings.ToUpper(req.Currency),
		strings.ToLower(req.Kind), normalizeResourceKey(req.Query),
	}, "|")
}

func courseCacheKey(provider string, req CourseSearchRequest) string {
	return strings.Join([]string{
		provider, strings.ToLower(req.Country), strings.ToUpper(req.Currency),
		strings.ToLower(req.Level), strings.ToLower(req.Language),
		normalizeResourceKey(req.Skill), normalizeResourceKey(req.Query),
	}, "|")
}

func (rs *ResourceService) cachedProducts(key string) ([]MarketplaceListing, bool) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	c, ok := rs.products[key]
	if !ok || rs.now().Sub(c.at) > productCacheTTL {
		return nil, false
	}
	return c.listings, true
}

func (rs *ResourceService) putProducts(key string, listings []MarketplaceListing) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if _, exists := rs.products[key]; !exists {
		if len(rs.order) >= maxResourceCacheEntries {
			oldest := rs.order[0]
			rs.order = rs.order[1:]
			delete(rs.products, oldest)
			delete(rs.courseHit, oldest)
		}
		rs.order = append(rs.order, key)
	}
	rs.products[key] = cachedProducts{listings: listings, at: rs.now()}
}

// FindProduct asks every configured marketplace and picks the cheapest suitable
// result. It is ENRICHMENT: every failure mode returns an error the caller is
// expected to shrug off, and none of them can block plan creation.
func (rs *ResourceService) FindProduct(ctx context.Context, need ResourceNeed) (*ProductChoice, error) {
	rs.mu.RLock()
	providers := append([]MarketplaceProvider(nil), rs.markets...)
	rs.mu.RUnlock()
	if len(providers) == 0 {
		return nil, errNoProviders
	}
	req := ProductSearchRequest{
		Query: need.Query, Kind: need.Kind, Country: need.Country,
		Currency: need.Currency, MaxResults: maxProviderResults,
	}

	var (
		mu  sync.Mutex
		all []MarketplaceListing
		wg  sync.WaitGroup
	)
	sem := make(chan struct{}, providerConcurrency)
	for _, p := range providers {
		wg.Add(1)
		go func(p MarketplaceProvider) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			listings, err := rs.searchOne(ctx, p, req)
			if err != nil {
				// One shop being down is not a reason to fail the request, and
				// certainly not a reason to make a listing up.
				log.Printf("resources: provider %s failed: %v", p.Name(), err)
				return
			}
			mu.Lock()
			all = append(all, listings...)
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	if len(all) == 0 {
		return nil, errNoSuitable
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Provider != all[j].Provider {
			return all[i].Provider < all[j].Provider
		}
		return all[i].ProductID < all[j].ProductID
	})
	return chooseCheapestSuitable(need, all)
}

// searchOne runs one provider under its own timeout, serves from cache when
// fresh, and sanitizes whatever comes back.
func (rs *ResourceService) searchOne(ctx context.Context, p MarketplaceProvider, req ProductSearchRequest) ([]MarketplaceListing, error) {
	key := productCacheKey(p.Name(), req)
	if hit, ok := rs.cachedProducts(key); ok {
		return hit, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	listings, err := p.Search(callCtx, req)
	if err != nil {
		return nil, err
	}
	clean := sanitizeListings(p, listings)
	rs.putProducts(key, clean)
	return clean, nil
}

// sanitizeListings enforces the provider contract on the way in: the provider
// name is ours to set, results are bounded, a URL from an unexpected host is
// dropped rather than shown, and a missing timestamp is filled so "checkedAt"
// is never zero on something presented as verified.
func sanitizeListings(p MarketplaceProvider, in []MarketplaceListing) []MarketplaceListing {
	allowed := p.AllowedHosts()
	out := make([]MarketplaceListing, 0, minInt(len(in), maxProviderResults))
	for i, l := range in {
		if i >= maxProviderResults {
			break
		}
		l.Provider = p.Name()
		if l.CheckedAt.IsZero() {
			l.CheckedAt = time.Now()
		}
		if l.URL != "" && !hostAllowed(l.URL, allowed) {
			log.Printf("resources: provider %s returned URL on an unexpected host; dropping it", p.Name())
			l.URL = ""
		}
		if l.PriceMinor < 0 {
			l.PriceKnown = false
		}
		out = append(out, l)
	}
	return out
}

// hostAllowed checks a listing URL against the provider's declared hosts. It
// only ever gates what is DISPLAYED — the backend never fetches these URLs, so
// there is no request for an attacker-chosen host to ride on.
func hostAllowed(raw string, allowed []string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// FindCourse is the course-side equivalent of FindProduct.
func (rs *ResourceService) FindCourse(ctx context.Context, req CourseSearchRequest) (*CourseListing, error) {
	rs.mu.RLock()
	providers := append([]CourseProvider(nil), rs.courses...)
	rs.mu.RUnlock()
	if len(providers) == 0 {
		return nil, errNoProviders
	}
	if req.MaxResults <= 0 {
		req.MaxResults = maxProviderResults
	}

	var (
		mu  sync.Mutex
		all []CourseListing
		wg  sync.WaitGroup
	)
	sem := make(chan struct{}, providerConcurrency)
	for _, p := range providers {
		wg.Add(1)
		go func(p CourseProvider) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			key := courseCacheKey(p.Name(), req)
			rs.mu.RLock()
			hit, ok := rs.courseHit[key]
			fresh := ok && rs.now().Sub(hit.at) <= courseCacheTTL
			rs.mu.RUnlock()
			var listings []CourseListing
			if fresh {
				listings = hit.listings
			} else {
				callCtx, cancel := context.WithTimeout(ctx, providerTimeout)
				out, err := p.Search(callCtx, req)
				cancel()
				if err != nil {
					log.Printf("resources: course provider %s failed: %v", p.Name(), err)
					return
				}
				allowedHosts := p.AllowedHosts()
				for i := range out {
					if i >= maxProviderResults {
						out = out[:i]
						break
					}
					out[i].Provider = p.Name()
					if out[i].CheckedAt.IsZero() {
						out[i].CheckedAt = time.Now()
					}
					if out[i].URL != "" && !hostAllowed(out[i].URL, allowedHosts) {
						out[i].URL = ""
					}
				}
				listings = out
				rs.mu.Lock()
				rs.courseHit[key] = cachedCourses{listings: listings, at: rs.now()}
				rs.mu.Unlock()
			}
			mu.Lock()
			all = append(all, listings...)
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	if len(all) == 0 {
		return nil, errNoSuitable
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Provider != all[j].Provider {
			return all[i].Provider < all[j].Provider
		}
		return all[i].CourseID < all[j].CourseID
	})
	return chooseCourse(req, all)
}
