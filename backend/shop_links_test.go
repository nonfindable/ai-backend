package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func linkProviders(links []ShopLink) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, l.Provider)
	}
	return out
}

func TestShopClass(t *testing.T) {
	cases := map[string]string{
		"book": shopClassProduct, "Books": shopClassProduct, "materials": shopClassProduct,
		"gear": shopClassProduct, "equipment": shopClassProduct,
		"course": shopClassCourse, "Online courses": shopClassCourse, "курс": shopClassCourse,
		"software": shopClassNone, "membership": shopClassNone, "app": shopClassNone,
		"notebook": shopClassNone, "": shopClassNone,
	}
	for in, want := range cases {
		if got := shopClass(in); got != want {
			t.Errorf("shopClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchLinksProducts(t *testing.T) {
	links := searchLinks(shopClassProduct, "  Cambridge IELTS 18  Academic ", "uz", "UZ")
	if got := strings.Join(linkProviders(links), ","); got != "uzum,yandex_market" {
		t.Fatalf("providers = %s", got)
	}
	for _, l := range links {
		if l.Kind != "search" {
			t.Errorf("%s: kind %q, want search", l.Provider, l.Kind)
		}
		u, err := url.Parse(l.URL)
		if err != nil || u.Scheme != "https" {
			t.Fatalf("%s: bad URL %q", l.Provider, l.URL)
		}
		q := u.Query().Get("query") + u.Query().Get("text")
		if q != "Cambridge IELTS 18 Academic" {
			t.Errorf("%s: query %q, want the cleaned name", l.Provider, q)
		}
	}
	if !strings.HasPrefix(links[0].URL, "https://uzum.uz/uz/") {
		t.Errorf("uzum should follow the plan language: %s", links[0].URL)
	}
	if ru := searchLinks(shopClassProduct, "метроном", "en", "UZ"); !strings.HasPrefix(ru[0].URL, "https://uzum.uz/ru/") {
		t.Errorf("uzum should default to Russian: %s", ru[0].URL)
	}
}

func TestSearchLinksCoursesAndGuards(t *testing.T) {
	courses := searchLinks(shopClassCourse, "Learning How to Learn", "en", "")
	if got := strings.Join(linkProviders(courses), ","); got != "coursera,udemy,stepik" {
		t.Fatalf("course providers = %s", got)
	}
	// Only Uzbek marketplaces are known, so another country gets no product links.
	if l := searchLinks(shopClassProduct, "guitar", "en", "DE"); l != nil {
		t.Errorf("non-UZ country produced product links: %v", l)
	}
	if l := searchLinks(shopClassNone, "Anki", "en", "UZ"); l != nil {
		t.Errorf("unclassified item produced links: %v", l)
	}
	if l := searchLinks(shopClassCourse, "   ", "en", "UZ"); l != nil {
		t.Errorf("empty query produced links: %v", l)
	}
	long := strings.Repeat("я", 200)
	l := searchLinks(shopClassCourse, long, "en", "UZ")
	u, _ := url.Parse(l[0].URL)
	if n := len([]rune(u.Query().Get("query"))); n != maxShopQueryLen {
		t.Errorf("query length %d, want capped at %d", n, maxShopQueryLen)
	}
}

func TestWithShopLinksSkipsOwnedAndFreeItems(t *testing.T) {
	plan := &Plan{
		Lang: "en",
		SetupItems: []SetupItem{
			{Name: "Decent headphones", Category: "gear", PriceRange: "$15-30", SearchQuery: "наушники"},
			{Name: "Guitar", Category: "gear", PriceRange: "$100-200", Owned: true},
			{Name: "Notebook + timer", Category: "gear", PriceRange: "$0"},
			{Name: "Anki", Category: "software", PriceRange: "$0-25"},
		},
	}
	seedResources(plan)
	got := withShopLinks(plan.clone(), "UZ")

	head := got.SetupItems[0].Links
	if len(head) != 2 || !strings.Contains(head[0].URL, url.QueryEscape("наушники")) {
		t.Fatalf("headphones should search by their query, got %+v", head)
	}
	for _, s := range got.SetupItems[1:] {
		if len(s.Links) != 0 {
			t.Errorf("%s: owned, free or unlinkable item got links %+v", s.Name, s.Links)
		}
	}
	for _, r := range got.Resources {
		want := r.InUse() == "Decent headphones"
		if (len(r.Links) > 0) != want {
			t.Errorf("resource %q links = %+v", r.InUse(), r.Links)
		}
	}
	if len(plan.SetupItems[0].Links) != 0 {
		t.Error("withShopLinks mutated the caller's plan")
	}
}

// A swapped resource must link to what the learner now uses, not the original.
func TestShopLinksFollowResourceReplacement(t *testing.T) {
	plan := &Plan{Lang: "en", SetupItems: []SetupItem{
		{Name: "Cambridge IELTS 17", Category: "book", PriceRange: "$10-20", SearchQuery: "Cambridge IELTS 17"},
	}}
	seedResources(plan)
	replaceResource(plan, findResource(plan, "Cambridge IELTS 17"), "Cambridge IELTS 18", true, "")

	got := withShopLinks(plan.clone(), "UZ")
	for _, links := range [][]ShopLink{got.SetupItems[0].Links, got.Resources[0].Links} {
		if len(links) == 0 || !strings.Contains(links[0].URL, "IELTS+18") {
			t.Errorf("links still point at the old book: %+v", links)
		}
	}
}

func TestPlanEndpointReturnsShopLinks(t *testing.T) {
	ts := newTestServerCfg(t, func(c *Config) { c.MarketplaceCountry = "UZ" })
	c := ts.newClient(t)
	_, planID := c.buildPlan()

	var plan Plan
	if code := c.do("GET", "/api/plan/"+planID, nil, &plan); code != http.StatusOK {
		t.Fatalf("GET plan: %d", code)
	}
	linked := 0
	for _, s := range plan.SetupItems {
		for _, l := range s.Links {
			linked++
			if l.Kind != "search" || !strings.HasPrefix(l.URL, "https://") {
				t.Errorf("%s: bad link %+v", s.Name, l)
			}
		}
		if isFreePrice(s.PriceRange) && len(s.Links) > 0 {
			t.Errorf("%s: free item got shop links", s.Name)
		}
	}
	if linked == 0 {
		t.Fatalf("no setup item carried a shop link: %+v", plan.SetupItems)
	}

	// Links are derived, never persisted.
	stored, _ := ts.store.GetPlan(planID)
	for _, s := range stored.SetupItems {
		if len(s.Links) > 0 {
			t.Fatalf("links were stored on the plan: %+v", s)
		}
	}
}
