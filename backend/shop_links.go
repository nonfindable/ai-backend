package main

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

// ---- shop search links ----
//
// None of the shops a learner in Uzbekistan actually uses offers a public
// search API: Uzum's GraphQL needs a session, Yandex Market only serves HTML,
// Coursera retired its public catalogue search and Udemy's affiliate API needs
// a partner account. Scraping them is fragile and against their terms.
//
// What IS always honest is a link to the shop's own search results for the
// exact thing the plan recommends. The shop does the finding, with its real
// prices and stock, and start.ai claims nothing it has not checked. Every link
// here is marked kind "search" so a client can never present it as a verified
// product page; a registered MarketplaceProvider remains the way to get those.

// ShopLink is one place the learner can look for a recommended item.
type ShopLink struct {
	Provider string `json:"provider"` // uzum | yandex_market | coursera | udemy | stepik
	Label    string `json:"label"`    // display name, e.g. "Uzum Market"
	URL      string `json:"url"`
	// Kind is always "search": the URL opens the shop's own results page for
	// the query, not a specific product whose price or stock we verified.
	Kind string `json:"kind"`
}

// Resource classes that decide which shops are relevant.
const (
	shopClassNone    = ""
	shopClassProduct = "product" // something physical you buy: a book, gear
	shopClassCourse  = "course"
)

// shopClass maps the free-text category the plan uses onto a class of shop.
// Anything unrecognised gets no links: a search on Uzum for an app or a
// YouTube channel would only send the learner somewhere useless.
func shopClass(category string) string {
	c := strings.ToLower(strings.TrimSpace(category))
	switch {
	case c == "":
		return shopClassNone
	case containsAny(c, []string{"course", "курс", "kurs", "class", "mooc"}):
		return shopClassCourse
	case containsAny(c, []string{"book", "книг", "kitob", "material", "gear", "equipment",
		"hardware", "instrument", "kit", "supplies", "stationery", "tool"}):
		return shopClassProduct
	default:
		return shopClassNone
	}
}

// isFreePrice reports whether a price range says the item costs nothing, in
// which case sending the learner to a shop contradicts the recommendation.
func isFreePrice(priceRange string) bool {
	p := strings.ToLower(strings.TrimSpace(priceRange))
	p = strings.NewReplacer(" ", "", "$", "", "usd", "").Replace(p)
	return p == "0" || p == "free" || p == "бесплатно" || p == "bepul"
}

// maxShopQueryLen keeps a search query to something a shop's search box would
// accept; an overlong AI-written name only produces an empty results page.
const maxShopQueryLen = 80

func cleanShopQuery(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if utf8.RuneCountInString(q) > maxShopQueryLen {
		r := []rune(q)
		q = strings.TrimSpace(string(r[:maxShopQueryLen]))
	}
	return q
}

// searchLinks builds the shop search links for one item. country selects the
// product marketplaces (only Uzbekistan's are known); course platforms are
// international. lang picks the shop's interface language where it has one.
func searchLinks(class, query, lang, country string) []ShopLink {
	query = cleanShopQuery(query)
	if query == "" {
		return nil
	}
	q := url.QueryEscape(query)
	switch class {
	case shopClassProduct:
		if !strings.EqualFold(strings.TrimSpace(country), "UZ") {
			return nil
		}
		uzumLang := "ru"
		if normLang(lang) == "uz" {
			uzumLang = "uz"
		}
		return []ShopLink{
			{Provider: "uzum", Label: "Uzum Market", URL: "https://uzum.uz/" + uzumLang + "/search?query=" + q, Kind: "search"},
			{Provider: "yandex_market", Label: "Yandex Market", URL: "https://market.yandex.uz/search?text=" + q, Kind: "search"},
		}
	case shopClassCourse:
		return []ShopLink{
			{Provider: "coursera", Label: "Coursera", URL: "https://www.coursera.org/search?query=" + q, Kind: "search"},
			{Provider: "udemy", Label: "Udemy", URL: "https://www.udemy.com/courses/search/?q=" + q, Kind: "search"},
			{Provider: "stepik", Label: "Stepik", URL: "https://stepik.org/catalog/search?q=" + q, Kind: "search"},
		}
	}
	return nil
}

// withShopLinks fills the derived Links on a plan's setup items and resources.
// Links are computed at read time rather than stored, so they always follow
// the current name after a learner swaps one resource for another. The plan
// passed in must be a copy the caller owns.
func withShopLinks(plan *Plan, country string) *Plan {
	if plan == nil {
		return nil
	}
	items := make([]SetupItem, len(plan.SetupItems))
	for i, s := range plan.SetupItems {
		s.Links = nil
		if !s.Owned && !isFreePrice(s.PriceRange) {
			s.Links = searchLinks(shopClass(s.Category), firstNonEmpty(s.SearchQuery, s.Name), plan.Lang, country)
		}
		items[i] = s
	}
	plan.SetupItems = items

	resources := make([]ResourceSelection, len(plan.Resources))
	for i, r := range plan.Resources {
		r = r.clone()
		r.Links = nil
		if class := shopClass(r.Kind); class != shopClassNone {
			query := r.InUse()
			// A resource seeded from a setup item searches with that item's
			// query while the learner is still using the recommendation.
			if q := setupQueryFor(plan, query); q != "" {
				query = q
			}
			if !resourceIsFree(plan, r.InUse()) {
				r.Links = searchLinks(class, query, plan.Lang, country)
			}
		}
		resources[i] = r
	}
	plan.Resources = resources
	return plan
}

// setupQueryFor returns the search query of the setup item named name, if any.
func setupQueryFor(plan *Plan, name string) string {
	for _, s := range plan.SetupItems {
		if strings.EqualFold(strings.TrimSpace(s.Name), strings.TrimSpace(name)) {
			return strings.TrimSpace(s.SearchQuery)
		}
	}
	return ""
}

// resourceIsFree reports whether the setup item behind a resource is owned or
// free, so the resource list does not offer a shop link the item list withheld.
func resourceIsFree(plan *Plan, name string) bool {
	for _, s := range plan.SetupItems {
		if strings.EqualFold(strings.TrimSpace(s.Name), strings.TrimSpace(name)) {
			return s.Owned || isFreePrice(s.PriceRange)
		}
	}
	return false
}
