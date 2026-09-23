package main

import (
	"sort"
	"strings"
	"time"
	"unicode"
)

// ---- resource memory ----
//
// start.ai recommends a book, an app or a course; the learner then buys
// something else, or already owns the previous edition, or finds the course too
// expensive. If the plan only remembers what was RECOMMENDED, then months later
// "which chapter should I do today?" is answered against a book they never had.
//
// ResourceSelection keeps both sides: what was suggested, what is actually in
// use, and what was turned down (so it is not suggested again).

type ResourceSelection struct {
	ID string `json:"id"`
	// Need is what this resource is FOR, in the plan's own words. It survives a
	// substitution, which is what makes a replacement checkable: a different
	// product for the same need may be fine, a different need is not.
	Need string `json:"need"`
	Kind string `json:"kind,omitempty"` // book|app|course|gear|materials|...
	// Recommended is start.ai's original suggestion; Selected is what the
	// learner is using. They are equal until the learner says otherwise.
	Recommended string   `json:"recommended"`
	Selected    string   `json:"selected"`
	Rejected    []string `json:"rejected,omitempty"`
	// Source records where Selected came from: "recommended", "user" when the
	// learner named it, or a provider name when a verified listing supplied it.
	Source string `json:"source,omitempty"`
	// Equivalent records whether the substitution was judged to cover the same
	// ground. A "false" here is why a change triggered replanning.
	Equivalent bool      `json:"equivalent"`
	Note       string    `json:"note,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt"`
	// Links are shop search links for what is in use, derived at read time.
	Links []ShopLink `json:"links,omitempty"`
}

func (r ResourceSelection) clone() ResourceSelection {
	c := r
	c.Rejected = append([]string(nil), r.Rejected...)
	c.Links = append([]ShopLink(nil), r.Links...)
	return c
}

// InUse is the resource the learner is actually working from.
func (r ResourceSelection) InUse() string {
	if s := strings.TrimSpace(r.Selected); s != "" {
		return s
	}
	return strings.TrimSpace(r.Recommended)
}

// maxResources bounds the selection list so a long conversation of "actually,
// I got this one instead" cannot grow the plan without limit.
const maxResources = 40

// seedResources derives the initial selection list from a freshly built plan:
// every setup item is something the learner may substitute, and so is every
// distinct resourceRef a todo points at.
func seedResources(plan *Plan) {
	if plan == nil {
		return
	}
	seen := map[string]bool{}
	add := func(need, name, kind string) {
		name = strings.TrimSpace(name)
		if name == "" || len(plan.Resources) >= maxResources {
			return
		}
		key := normalizeResourceKey(name)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		plan.Resources = append(plan.Resources, ResourceSelection{
			ID:          newID("res"),
			Need:        firstNonEmpty(need, name),
			Kind:        kind,
			Recommended: name,
			Selected:    name,
			Source:      "recommended",
			Equivalent:  true,
			UpdatedAt:   time.Now(),
		})
	}
	for _, s := range plan.SetupItems {
		add(s.Rationale, s.Name, s.Category)
	}
	for _, t := range plan.Todos {
		add(t.Title, t.ResourceRef, "")
	}
}

// normalizeResourceKey reduces a product name to something two spellings of the
// same thing agree on: lowercase, letters and digits only, spaces collapsed.
func normalizeResourceKey(s string) string {
	var b strings.Builder
	prevSpace := true
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevSpace = false
		case !prevSpace:
			b.WriteRune(' ')
			prevSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// resourceTokens splits a normalized name into comparable tokens.
func resourceTokens(s string) []string {
	f := strings.Fields(normalizeResourceKey(s))
	sort.Strings(f)
	return f
}

// resourceOverlap scores how much two product names share, 0..1 over the
// shorter one. It is a cheap similarity used only to FIND which recorded
// resource the user means — never to decide whether a substitute is suitable.
func resourceOverlap(a, b string) float64 {
	ta, tb := resourceTokens(a), resourceTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	set := map[string]bool{}
	for _, t := range tb {
		set[t] = true
	}
	hits := 0
	for _, t := range ta {
		if set[t] {
			hits++
		}
	}
	return float64(hits) / float64(minInt(len(ta), len(tb)))
}

// findResource locates the selection a user's words refer to: "the Cambridge
// book", "Cambridge IELTS 19". It matches against both the recommendation and
// the current selection, and returns nil rather than guessing when nothing is
// close enough — a wrong match would silently rewrite the wrong resource.
func findResource(plan *Plan, name string) *ResourceSelection {
	if plan == nil || strings.TrimSpace(name) == "" {
		return nil
	}
	best, bestScore := -1, 0.0
	for i := range plan.Resources {
		r := &plan.Resources[i]
		score := resourceOverlap(name, r.Recommended)
		if s := resourceOverlap(name, r.Selected); s > score {
			score = s
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 || bestScore < 0.5 {
		return nil
	}
	return &plan.Resources[best]
}

// replaceResource records that the learner is using something else, and
// repoints every todo that referenced the old name. It reports how many todos
// changed, which is what tells the caller whether anything downstream moved.
func replaceResource(plan *Plan, sel *ResourceSelection, to string, equivalent bool, note string) int {
	if plan == nil || sel == nil {
		return 0
	}
	to = strings.TrimSpace(to)
	if to == "" {
		return 0
	}
	old := sel.InUse()
	if !strings.EqualFold(old, to) && old != "" {
		if !containsStr(sel.Rejected, old) {
			sel.Rejected = append(sel.Rejected, old)
		}
	}
	sel.Selected = to
	sel.Source = "user"
	sel.Equivalent = equivalent
	sel.Note = note
	sel.UpdatedAt = time.Now()

	touched := 0
	for i := range plan.Todos {
		t := &plan.Todos[i]
		if t.ResourceRef == "" {
			continue
		}
		if strings.EqualFold(t.ResourceRef, old) || resourceOverlap(t.ResourceRef, old) >= 0.8 {
			t.ResourceRef = to
			touched++
		}
	}
	for i := range plan.SetupItems {
		s := &plan.SetupItems[i]
		if strings.EqualFold(s.Name, old) || resourceOverlap(s.Name, old) >= 0.8 {
			s.Name = to
			s.SearchQuery = ""
			touched++
		}
	}
	return touched
}

// addResource records a resource the learner brought that the plan never
// suggested, so later questions can be answered against it.
func addResource(plan *Plan, name, need, kind string) *ResourceSelection {
	if plan == nil || strings.TrimSpace(name) == "" || len(plan.Resources) >= maxResources {
		return nil
	}
	if existing := findResource(plan, name); existing != nil {
		return existing
	}
	plan.Resources = append(plan.Resources, ResourceSelection{
		ID:         newID("res"),
		Need:       firstNonEmpty(need, name),
		Kind:       kind,
		Selected:   name,
		Source:     "user",
		Equivalent: true,
		UpdatedAt:  time.Now(),
	})
	return &plan.Resources[len(plan.Resources)-1]
}

// removeResource drops a resource the learner rejected (too expensive, already
// covered) and clears the todo references that pointed at it.
func removeResource(plan *Plan, sel *ResourceSelection) int {
	if plan == nil || sel == nil {
		return 0
	}
	name := sel.InUse()
	touched := 0
	for i := range plan.Todos {
		if t := &plan.Todos[i]; t.ResourceRef != "" && resourceOverlap(t.ResourceRef, name) >= 0.8 {
			t.ResourceRef = ""
			touched++
		}
	}
	out := plan.Resources[:0]
	for _, r := range plan.Resources {
		if r.ID == sel.ID {
			continue
		}
		out = append(out, r)
	}
	plan.Resources = out
	return touched
}
