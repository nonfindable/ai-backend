package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---- marketplace selection ----
//
// Every test here uses fake, in-process providers. Nothing reaches the network,
// and nothing depends on a real shop's catalogue — which is also the honest
// state of this build: no real marketplace is implemented, so these tests pin
// the SELECTION RULES that a real one would be plugged into.

type fakeProvider struct {
	name     string
	hosts    []string
	listings []MarketplaceListing
	err      error
	delay    time.Duration
	calls    int
}

func (f *fakeProvider) Name() string           { return f.name }
func (f *fakeProvider) AllowedHosts() []string { return f.hosts }
func (f *fakeProvider) Search(ctx context.Context, _ ProductSearchRequest) ([]MarketplaceListing, error) {
	f.calls++
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.listings, nil
}

// book builds a well-formed listing for the standard need.
func book(provider string, price int64, currency string) MarketplaceListing {
	return MarketplaceListing{
		Provider: provider, ProductID: provider + "-1",
		Title: "Cambridge IELTS 19 Academic", Brand: "Cambridge",
		Edition: "19", Variant: "Academic", Format: "physical",
		Language: "English", Condition: "new", BundleCount: 1,
		PriceMinor: price, Currency: currency, PriceKnown: true,
		InStock: true, StockKnown: true,
		CheckedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
}

func standardNeed() ResourceNeed {
	return ResourceNeed{
		Kind: "book", Query: "Cambridge IELTS 19 Academic",
		Purpose: "full-length IELTS Academic practice",
		Edition: "19", Variant: "Academic", Format: "physical", BundleCount: 1,
	}
}

func serviceWith(t *testing.T, providers ...MarketplaceProvider) *ResourceService {
	t.Helper()
	rs := newResourceService()
	for _, p := range providers {
		rs.registerProvider(p)
	}
	return rs
}

// The shipped default: nothing configured, so nothing is ever recommended.
func TestNoProvidersMeansNoRecommendation(t *testing.T) {
	rs := newResourceService()
	if rs.Enabled() {
		t.Fatal("a fresh service claims to have providers")
	}
	if names := rs.ProviderNames(); len(names) != 0 {
		t.Errorf("provider names = %v, want none: no real marketplace ships with this build", names)
	}
	_, err := rs.FindProduct(context.Background(), standardNeed())
	if !errors.Is(err, errNoProviders) {
		t.Errorf("err = %v, want errNoProviders", err)
	}
}

// TEST 20 — genuinely equivalent products: the cheapest wins.
func TestCheapestEquivalentProductWins(t *testing.T) {
	rs := serviceWith(t,
		&fakeProvider{name: "uzum", listings: []MarketplaceListing{book("uzum", 145000_00, "UZS")}},
		&fakeProvider{name: "yandex", listings: []MarketplaceListing{book("yandex", 132000_00, "UZS")}},
		&fakeProvider{name: "other", listings: []MarketplaceListing{book("other", 149000_00, "UZS")}},
	)
	choice, err := rs.FindProduct(context.Background(), standardNeed())
	if err != nil {
		t.Fatalf("FindProduct: %v", err)
	}
	if choice.Listing.Provider != "yandex" {
		t.Errorf("chose %s at %d, want yandex at 13200000", choice.Listing.Provider, choice.Listing.PriceMinor)
	}
	if choice.Suitable != 3 {
		t.Errorf("suitable = %d, want 3", choice.Suitable)
	}
}

// TEST 21 — a cheaper irrelevant product must not win.
func TestCheaperIrrelevantProductIsRejected(t *testing.T) {
	irrelevant := book("uzum", 10000, "UZS")
	irrelevant.Title = "Pencil case"
	irrelevant.Edition = ""
	irrelevant.Variant = ""

	rs := serviceWith(t,
		&fakeProvider{name: "uzum", listings: []MarketplaceListing{irrelevant}},
		&fakeProvider{name: "yandex", listings: []MarketplaceListing{book("yandex", 132000_00, "UZS")}},
	)
	choice, err := rs.FindProduct(context.Background(), standardNeed())
	if err != nil {
		t.Fatalf("FindProduct: %v", err)
	}
	if choice.Listing.Provider != "yandex" {
		t.Errorf("a cheap unrelated item won: %+v", choice.Listing)
	}
	if choice.Suitable != 1 {
		t.Errorf("suitable = %d, want 1", choice.Suitable)
	}
	if len(choice.Rejections) == 0 {
		t.Error("the rejection was not recorded, so 'why not the cheaper one?' is unanswerable")
	}
}

// TEST 22 — the wrong edition or variant is not equivalent.
func TestWrongEditionOrVariantIsNotEquivalent(t *testing.T) {
	wrongEdition := book("uzum", 90000_00, "UZS")
	wrongEdition.Title = "Cambridge IELTS 18 Academic"
	wrongEdition.Edition = "18"

	wrongVariant := book("other", 95000_00, "UZS")
	wrongVariant.Title = "Cambridge IELTS 19 General Training"
	wrongVariant.Variant = "General Training"

	bundle := book("bundle", 80000_00, "UZS")
	bundle.BundleCount = 4

	digital := book("digital", 50000_00, "UZS")
	digital.Format = "digital"

	rs := serviceWith(t, &fakeProvider{name: "uzum", listings: []MarketplaceListing{
		wrongEdition, wrongVariant, bundle, digital, book("uzum", 132000_00, "UZS"),
	}})
	choice, err := rs.FindProduct(context.Background(), standardNeed())
	if err != nil {
		t.Fatalf("FindProduct: %v", err)
	}
	if choice.Listing.PriceMinor != 132000_00 {
		t.Errorf("chose %+v, want the one genuinely matching the need", choice.Listing)
	}
	if choice.Suitable != 1 {
		t.Errorf("suitable = %d, want 1 (every cheaper option differs materially)", choice.Suitable)
	}
}

// TEST 23 — unknown shipping means no delivered-price claim.
func TestUnknownShippingIsNotClaimedAsDelivered(t *testing.T) {
	withShipping := book("uzum", 130000_00, "UZS")
	withShipping.ShippingKnown = true
	withShipping.ShippingMinor = 20000_00
	unknownShipping := book("yandex", 132000_00, "UZS") // ShippingKnown false

	rs := serviceWith(t,
		&fakeProvider{name: "uzum", listings: []MarketplaceListing{withShipping}},
		&fakeProvider{name: "yandex", listings: []MarketplaceListing{unknownShipping}},
	)
	choice, err := rs.FindProduct(context.Background(), standardNeed())
	if err != nil {
		t.Fatalf("FindProduct: %v", err)
	}
	if choice.Claim == "" {
		t.Fatal("no claim was produced")
	}
	if contains(choice.Claim, "delivered") {
		t.Errorf("claim asserts a delivered price although shipping is unknown for one candidate: %q", choice.Claim)
	}
	if !contains(choice.Claim, "listed") {
		t.Errorf("claim = %q, want it to say the comparison is on listed prices", choice.Claim)
	}
	// With shipping incomparable, the listed price decides.
	if choice.Listing.Provider != "uzum" {
		t.Errorf("chose %s, want the lower LISTED price", choice.Listing.Provider)
	}
	if choice.EffectiveMinor != 130000_00 {
		t.Errorf("effective = %d, want the listed price only", choice.EffectiveMinor)
	}
}

// When shipping IS known for everything, it is added.
func TestKnownShippingIsAddedAndClaimed(t *testing.T) {
	cheapItemPriceyPost := book("uzum", 100000_00, "UZS")
	cheapItemPriceyPost.ShippingKnown, cheapItemPriceyPost.ShippingMinor = true, 50000_00
	dearerItemFreePost := book("yandex", 120000_00, "UZS")
	dearerItemFreePost.ShippingKnown, dearerItemFreePost.ShippingMinor = true, 0

	rs := serviceWith(t,
		&fakeProvider{name: "uzum", listings: []MarketplaceListing{cheapItemPriceyPost}},
		&fakeProvider{name: "yandex", listings: []MarketplaceListing{dearerItemFreePost}},
	)
	choice, err := rs.FindProduct(context.Background(), standardNeed())
	if err != nil {
		t.Fatalf("FindProduct: %v", err)
	}
	if choice.Listing.Provider != "yandex" {
		t.Errorf("chose %s; delivered cost should decide when shipping is known", choice.Listing.Provider)
	}
	if choice.EffectiveMinor != 120000_00 {
		t.Errorf("effective = %d, want 12000000", choice.EffectiveMinor)
	}
	if !contains(choice.Claim, "delivered") {
		t.Errorf("claim = %q, want it to state a delivered comparison", choice.Claim)
	}
}

// TEST 24 — no cross-currency "cheapest" claim without a trusted rate.
func TestMixedCurrenciesProduceNoCheapestClaim(t *testing.T) {
	rs := serviceWith(t,
		&fakeProvider{name: "uzum", listings: []MarketplaceListing{book("uzum", 145000_00, "UZS")}},
		&fakeProvider{name: "amazon", listings: []MarketplaceListing{book("amazon", 25_00, "USD")}},
	)
	_, err := rs.FindProduct(context.Background(), standardNeed())
	if !errors.Is(err, errMixedCurrency) {
		t.Fatalf("err = %v, want errMixedCurrency: 25 USD and 145000 UZS are not comparable without an FX source", err)
	}

	// Naming the currency makes the comparison well-defined again.
	need := standardNeed()
	need.Currency = "UZS"
	choice, err := rs.FindProduct(context.Background(), need)
	if err != nil {
		t.Fatalf("with a stated currency: %v", err)
	}
	if choice.Listing.Currency != "UZS" {
		t.Errorf("chose a %s listing despite UZS being requested", choice.Listing.Currency)
	}
}

// TEST 25 — a cheaper out-of-stock item loses to an available one.
func TestOutOfStockItemDoesNotWin(t *testing.T) {
	cheapGone := book("uzum", 90000_00, "UZS")
	cheapGone.InStock, cheapGone.StockKnown = false, true

	rs := serviceWith(t, &fakeProvider{name: "uzum", listings: []MarketplaceListing{
		cheapGone, book("uzum", 132000_00, "UZS"),
	}})
	choice, err := rs.FindProduct(context.Background(), standardNeed())
	if err != nil {
		t.Fatalf("FindProduct: %v", err)
	}
	if choice.Listing.PriceMinor != 132000_00 {
		t.Errorf("chose the out-of-stock item: %+v", choice.Listing)
	}
	if !anyContains(choice.Rejections, "out of stock") {
		t.Errorf("rejections = %v, want the stock reason recorded", choice.Rejections)
	}
}

// TEST 26 — one provider failing or timing out must not lose the others.
func TestOneProviderFailureStillUsesTheRest(t *testing.T) {
	broken := &fakeProvider{name: "broken", err: errors.New("503 from shop")}
	good := &fakeProvider{name: "yandex", listings: []MarketplaceListing{book("yandex", 132000_00, "UZS")}}

	rs := serviceWith(t, broken, good)
	choice, err := rs.FindProduct(context.Background(), standardNeed())
	if err != nil {
		t.Fatalf("a failing provider broke the whole search: %v", err)
	}
	if choice.Listing.Provider != "yandex" {
		t.Errorf("chose %s, want the working provider's result", choice.Listing.Provider)
	}
}

func TestProviderTimeoutIsBounded(t *testing.T) {
	slow := &fakeProvider{name: "slow", delay: 3 * time.Second, listings: []MarketplaceListing{book("slow", 1, "UZS")}}
	good := &fakeProvider{name: "yandex", listings: []MarketplaceListing{book("yandex", 132000_00, "UZS")}}
	rs := serviceWith(t, slow, good)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	choice, err := rs.FindProduct(ctx, standardNeed())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("FindProduct: %v", err)
	}
	if choice.Listing.Provider != "yandex" {
		t.Errorf("chose %s, want the provider that answered in time", choice.Listing.Provider)
	}
	if elapsed > 2*time.Second {
		t.Errorf("search took %s; a slow shop must not hold up the answer", elapsed)
	}
}

// TEST 27 — nothing verified means nothing recommended. Never an invention.
func TestNoVerifiedProductMeansNoRecommendation(t *testing.T) {
	noPrice := book("uzum", 0, "UZS")
	noPrice.PriceKnown = false

	rs := serviceWith(t, &fakeProvider{name: "uzum", listings: []MarketplaceListing{noPrice}})
	choice, err := rs.FindProduct(context.Background(), standardNeed())
	if !errors.Is(err, errNoSuitable) {
		t.Fatalf("err = %v, want errNoSuitable", err)
	}
	if choice != nil && choice.Listing.Title != "" {
		t.Errorf("a listing was returned despite nothing being verified: %+v", choice.Listing)
	}

	// And an empty provider answer is the same story.
	empty := serviceWith(t, &fakeProvider{name: "uzum"})
	if _, err := empty.FindProduct(context.Background(), standardNeed()); !errors.Is(err, errNoSuitable) {
		t.Errorf("err = %v, want errNoSuitable", err)
	}
}

// A provider that returns a URL on an unexpected host has it dropped: a
// recommendation must never carry a link the provider did not vouch for.
func TestListingURLsAreRestrictedToProviderHosts(t *testing.T) {
	good := book("uzum", 132000_00, "UZS")
	good.URL = "https://uzum.uz/product/123"
	bad := book("uzum", 100000_00, "UZS")
	bad.ProductID = "uzum-2"
	bad.URL = "https://evil.example.com/steal"
	insecure := book("uzum", 110000_00, "UZS")
	insecure.ProductID = "uzum-3"
	insecure.URL = "http://uzum.uz/plain"

	p := &fakeProvider{name: "uzum", hosts: []string{"uzum.uz"},
		listings: []MarketplaceListing{good, bad, insecure}}
	rs := serviceWith(t, p)

	choice, err := rs.FindProduct(context.Background(), standardNeed())
	if err != nil {
		t.Fatalf("FindProduct: %v", err)
	}
	// The cheapest suitable one is `bad`, whose URL must have been stripped.
	if choice.Listing.ProductID == "uzum-2" && choice.Listing.URL != "" {
		t.Errorf("a URL on an unexpected host survived: %q", choice.Listing.URL)
	}
	if !hostAllowed("https://uzum.uz/x", []string{"uzum.uz"}) {
		t.Error("a legitimate provider host was rejected")
	}
	if hostAllowed("https://evil.example.com/x", []string{"uzum.uz"}) {
		t.Error("an unexpected host was allowed")
	}
	if hostAllowed("http://uzum.uz/x", []string{"uzum.uz"}) {
		t.Error("a plaintext URL was allowed")
	}
	if hostAllowed("https://uzum.uz.evil.com/x", []string{"uzum.uz"}) {
		t.Error("a lookalike host was allowed")
	}
}

// Repeat searches are served from cache rather than hitting the shop again.
func TestIdenticalSearchesAreCached(t *testing.T) {
	p := &fakeProvider{name: "uzum", listings: []MarketplaceListing{book("uzum", 132000_00, "UZS")}}
	rs := serviceWith(t, p)
	for i := 0; i < 3; i++ {
		if _, err := rs.FindProduct(context.Background(), standardNeed()); err != nil {
			t.Fatalf("FindProduct: %v", err)
		}
	}
	if p.calls != 1 {
		t.Errorf("provider was called %d times for an identical search, want 1", p.calls)
	}
	// A different query is a different key.
	other := standardNeed()
	other.Query = "Cambridge IELTS 18 Academic"
	other.Edition = "18"
	_, _ = rs.FindProduct(context.Background(), other)
	if p.calls != 2 {
		t.Errorf("provider was called %d times, want a second call for a new query", p.calls)
	}
}

// Expired cache entries are refetched, and CheckedAt reflects the retrieval.
func TestCacheExpires(t *testing.T) {
	p := &fakeProvider{name: "uzum", listings: []MarketplaceListing{book("uzum", 132000_00, "UZS")}}
	rs := serviceWith(t, p)
	now := time.Now()
	rs.now = func() time.Time { return now }

	if _, err := rs.FindProduct(context.Background(), standardNeed()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(productCacheTTL + time.Minute)
	if _, err := rs.FindProduct(context.Background(), standardNeed()); err != nil {
		t.Fatal(err)
	}
	if p.calls != 2 {
		t.Errorf("provider called %d times, want a refetch after the TTL", p.calls)
	}
}

// ---- courses ----

type fakeCourseProvider struct {
	name     string
	hosts    []string
	listings []CourseListing
}

func (f *fakeCourseProvider) Name() string           { return f.name }
func (f *fakeCourseProvider) AllowedHosts() []string { return f.hosts }
func (f *fakeCourseProvider) Search(context.Context, CourseSearchRequest) ([]CourseListing, error) {
	return f.listings, nil
}

// A free but irrelevant course must not beat a paid one that fits.
func TestFreeIrrelevantCourseDoesNotBeatAFittingPaidOne(t *testing.T) {
	rs := newResourceService()
	rs.registerCourseProvider(&fakeCourseProvider{name: "x", listings: []CourseListing{
		{Provider: "x", CourseID: "1", Title: "Intro to Cooking", Skill: "Cooking",
			Level: "beginner", Language: "English", PriceKnown: true, Free: true},
		{Provider: "x", CourseID: "2", Title: "IELTS Writing Band 7", Skill: "IELTS",
			Level: "advanced", Language: "English", PriceMinor: 4900, Currency: "USD", PriceKnown: true},
	}})
	got, err := rs.FindCourse(context.Background(), CourseSearchRequest{
		Query: "IELTS Writing", Skill: "IELTS", Level: "advanced", Language: "English",
	})
	if err != nil {
		t.Fatalf("FindCourse: %v", err)
	}
	if got.CourseID != "2" {
		t.Errorf("chose %q; a free course in the wrong subject must not win", got.Title)
	}
}

// Between two similarly suitable courses, the cheaper one wins.
func TestSimilarCoursesPreferTheCheaper(t *testing.T) {
	rs := newResourceService()
	rs.registerCourseProvider(&fakeCourseProvider{name: "x", listings: []CourseListing{
		{Provider: "x", CourseID: "dear", Title: "IELTS Writing Band 7", Skill: "IELTS",
			Level: "advanced", Language: "English", PriceMinor: 9900, Currency: "USD", PriceKnown: true},
		{Provider: "x", CourseID: "cheap", Title: "IELTS Writing Band 7", Skill: "IELTS",
			Level: "advanced", Language: "English", PriceMinor: 4900, Currency: "USD", PriceKnown: true},
	}})
	got, err := rs.FindCourse(context.Background(), CourseSearchRequest{
		Query: "IELTS Writing", Skill: "IELTS", Level: "advanced", Language: "English",
	})
	if err != nil {
		t.Fatalf("FindCourse: %v", err)
	}
	if got.CourseID != "cheap" {
		t.Errorf("chose %q, want the cheaper of two equally suitable courses", got.CourseID)
	}
}

// A resource lookup failure must never stop a plan being built or changed.
func TestProviderFailureDoesNotBlockPlanChanges(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 1 hour each")
	cfg := Config{DataDir: t.TempDir(), DefaultTimezone: "UTC"}
	gw := newGateway(cfg)
	t.Cleanup(gw.Close)
	p := newPipeline(cfg, f.store, gw, newScheduler(f.store))
	p.res = serviceWith(t, &fakeProvider{name: "broken", err: errors.New("down")})

	sess, _ := f.store.GetSession(f.sess)
	change := planChangeRequest{Type: changeResourceAdd, ResourceTo: "Some Book"}
	p.enrichResourceChange(context.Background(), &change)
	if change.ResourceTo != "Some Book" {
		t.Errorf("a failed lookup altered the change: %+v", change)
	}

	plan := f.planNow(t)
	if _, changed := p.applyPlanChange(sess, plan, change); !changed {
		t.Error("the change did not apply despite the lookup being pure enrichment")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func anyContains(list []string, sub string) bool {
	for _, s := range list {
		if contains(s, sub) {
			return true
		}
	}
	return false
}
