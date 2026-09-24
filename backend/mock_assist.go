package main

import (
	"strings"
)

// The no-key stand-in for the post-plan assistant.
//
// It exists for the same reason the rest of mock.go does: every behaviour the
// product depends on has to be exercisable, and testable, without spending a
// token or reaching the network. It is deterministic by construction — the same
// message against the same plan always produces the same change — which is what
// lets the rescheduling and resource tests assert exact outcomes.
//
// It reads intent with keyword matching in all three languages. That is a
// deliberately crude stand-in for the live model's comprehension, not a second
// implementation of it: the CHANGE it proposes goes through exactly the same
// validation and application path as the live model's, so what these tests pin
// down is the backend behaviour, not the mock's understanding.

var (
	// Words that mean "I can no longer study on these days" / "use these days".
	dropDayWords = []string{
		"can't", "cannot", "cant", "no longer", "not anymore", "drop", "remove", "stop",
		"не могу", "больше не", "убер", "отмен",
		"qila olmayman", "endi", "olib tashla", "bekor",
	}
	useDayWords = []string{
		"use", "switch to", "move to", "instead", "only", "now i can", "change to",
		"использ", "перейд", "вместо", "только", "теперь",
		"foydalan", "o'rniga", "faqat", "endi",
	}
	questionWords = []string{
		"why", "what", "when", "which", "how", "do i need", "should i", "can i",
		"почему", "зачем", "что", "когда", "какой", "как", "нужно ли", "можно",
		"nega", "nima", "qachon", "qaysi", "qanday", "kerakmi", "mumkinmi",
	}
	boughtWords = []string{
		"bought", "got", "have", "using", "already own", "purchased", "switch",
		"купил", "взял", "есть", "использую", "приобрел",
		"sotib oldim", "bor", "foydalanaman", "oldim",
	}
	insteadWords = []string{"instead", "rather than", "in place of", "вместо", "o'rniga", "orniga"}
	shorterWords = []string{
		"shorter", "too long", "smaller sessions", "cut the sessions",
		"короче", "слишком длин", "покороче",
		"qisqaroq", "juda uzun",
	}
	expensiveWords = []string{
		"too expensive", "can't afford", "cheaper", "no money",
		"дорого", "дешевле", "не по карману",
		"qimmat", "arzonroq",
	}
)

// mockAssist turns one post-plan message into a reply and at most one change.
func mockAssist(sess *IntakeSession, plan *Plan, msg string) assistResult {
	lang := sess.Lang
	lower := strings.ToLower(strings.TrimSpace(msg))
	res := assistResult{}

	// A question is answered, never applied. "Can I do this on Saturday
	// instead?" asks whether that would work; only a statement of fact about
	// their life ("I can only study Saturdays now") changes the plan. Getting
	// this backwards would rewrite someone's calendar because they wondered
	// aloud.
	if isPlainQuestion(lower) && !isPoliteRequest(lower) {
		res.Change = planChangeRequest{Type: changePlanQuestion}
		res.Reply = mockPlanAnswer(sess, plan, lower)
		return res
	}

	// 1. An availability statement. Parsed by the same deterministic parser the
	//    interview uses, so "Friday and Saturday, 2 hours each" means exactly
	//    240 minutes here too.
	parsed := parseAvailabilityText(msg)
	mentionsDays := parsed.HasDays()
	mentionsTime := parsed.HasTime() || len(parsed.PerDay) > 0
	if mentionsDays || mentionsTime {
		current := planAvailabilityOf(plan)
		next := current
		if mentionsDays {
			// One message often does both: "I can't study Tuesdays anymore. Use
			// Friday and Saturday." names a day to DROP and a set to KEEP, and
			// reading it as one flat day list would leave Tuesday in. Each
			// sentence is classified on its own.
			removeDays, setDays := mockDayIntent(msg)
			days := current.Days
			if len(setDays) > 0 {
				days = setDays
			}
			kept := []string{}
			for _, d := range days {
				if !containsStr(removeDays, d) {
					kept = append(kept, d)
				}
			}
			if len(kept) > 0 {
				next = StudyAvailability{Days: kept, WeeklyMinutes: current.WeeklyMinutes}
				next.normalize()
			}
		}
		if mentionsTime {
			timeOnly := StudyAvailability{WeeklyMinutes: parsed.WeeklyMinutes, PerDay: parsed.PerDay}
			if mentionsDays {
				timeOnly.Days = next.Days
			}
			next = mergeAvailability(next, timeOnly)
		}

		changeType := changeAvailability
		if !mentionsDays {
			changeType = changeWeeklyTime
		}
		res.Change = planChangeRequest{
			Type:          changeType,
			Days:          next.Days,
			WeeklyMinutes: next.WeeklyMinutes,
			PerDay:        next.PerDay,
			Reason:        msg,
		}
		res.Reply = tr(lang,
			"Got it — updating your schedule.",
			"Понял — обновляю ваше расписание.",
			"Tushundim — jadvalingizni yangilayapman.")
		return res
	}

	// 2. A resource substitution: "I bought Cambridge IELTS 18 instead".
	if from, to, ok := mockResourceSwap(plan, msg, lower); ok {
		res.Change = planChangeRequest{
			Type:         changeResourceReplace,
			ResourceFrom: from,
			ResourceTo:   to,
			// The mock cannot judge coverage, so it takes the conservative
			// option: treat it as equivalent and change nothing but the label.
			// A relabelling is always safe; regenerating work that did not need
			// regenerating is not.
			Equivalent:      true,
			WorkloadChanged: false,
			Reason:          msg,
		}
		res.Reply = tr(lang,
			"Noted — you're using "+to+" now. I've pointed the relevant work at it.",
			"Принято — теперь вы используете «"+to+"». Я переключил соответствующие задания на него.",
			"Qabul qilindi — endi «"+to+"» dan foydalanasiz. Tegishli ishlarni unga yo'naltirdim.")
		return res
	}

	// 3. Dropping something they cannot afford.
	if containsAny(lower, expensiveWords) {
		if sel := mockCostliestResource(plan, msg); sel != nil {
			res.Change = planChangeRequest{
				Type: changeResourceRemove, ResourceFrom: sel.InUse(), Reason: msg,
			}
			res.Reply = tr(lang,
				"Understood — I've dropped "+sel.InUse()+" and left the work that doesn't need it.",
				"Понял — я убрал «"+sel.InUse()+"», а задания, которым он не нужен, остались.",
				"Tushundim — «"+sel.InUse()+"» ni olib tashladim, unga bog'liq bo'lmagan ishlar qoldi.")
			return res
		}
	}

	// 4. Session length preference.
	if containsAny(lower, shorterWords) {
		if mins, ok := firstDurationMinutes(lower); ok {
			res.Change = planChangeRequest{Type: changeSessionDuration, SessionMaxMinutes: mins, Reason: msg}
		} else {
			res.Change = planChangeRequest{Type: changeSessionDuration, SessionMaxMinutes: 30, Reason: msg}
		}
		res.Reply = tr(lang,
			"I'll keep the sessions shorter.",
			"Сделаю занятия короче.",
			"Mashg'ulotlarni qisqaroq qilaman.")
		return res
	}

	// 5. Everything else is a question about the plan.
	res.Change = planChangeRequest{Type: changePlanQuestion}
	res.Reply = mockPlanAnswer(sess, plan, lower)
	return res
}

// mockCostliestResource picks which resource "this course is too expensive"
// refers to: the one they named, or failing that the first paid-looking one.
func mockCostliestResource(plan *Plan, msg string) *ResourceSelection {
	if sel := findResource(plan, longestProductPhrase(msg)); sel != nil {
		return sel
	}
	for i := range plan.Resources {
		if strings.EqualFold(plan.Resources[i].Kind, "course") {
			return &plan.Resources[i]
		}
	}
	return nil
}

// isPlainQuestion keeps "can I do this on Saturday instead?" from being applied
// as a change. A question is answered; only a statement changes the plan.
func isPlainQuestion(lower string) bool {
	if strings.HasSuffix(strings.TrimSpace(lower), "?") {
		return true
	}
	// Plenty of questions arrive without a question mark.
	return containsAny(lower, questionWords)
}

// isPoliteRequest distinguishes "can you make the sessions shorter?" — which is
// an instruction wearing a question mark — from "can I move this to Saturday?",
// which really is a question.
func isPoliteRequest(lower string) bool {
	for _, p := range []string{
		"can you", "could you", "would you", "please", "make the", "make it",
		"можете", "можешь", "пожалуйста", "сделай",
		"iltimos", "qila olasizmi", "qilib ber",
	} {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// mockDayIntent classifies each sentence: which days are being given up, and
// which are being named as the new set.
func mockDayIntent(msg string) (remove, set []string) {
	for _, sentence := range strings.FieldsFunc(msg, func(r rune) bool {
		return r == '.' || r == ';' || r == '!' || r == '\n'
	}) {
		lower := strings.ToLower(sentence)
		days := detectDays(lower)
		if len(days) == 0 {
			continue
		}
		switch {
		case containsAny(lower, dropDayWords):
			remove = append(remove, days...)
		case containsAny(lower, useDayWords):
			set = append(set, days...)
		default:
			// A bare day list with no verb reads as the new set, which is how
			// "Friday and Saturday" on its own is meant.
			set = append(set, days...)
		}
	}
	sortDayCodes(remove)
	sortDayCodes(set)
	return remove, set
}

// mockResourceSwap spots "I bought X instead [of Y]" and resolves both ends
// against the plan's recorded resources.
func mockResourceSwap(plan *Plan, msg, lower string) (from, to string, ok bool) {
	if !containsAny(lower, boughtWords) && !containsAny(lower, insteadWords) {
		return "", "", false
	}
	// The replacement is the longest capitalized/alphanumeric run that is not
	// already the recommended name. Crude, deterministic, and enough for the
	// mock: the live model does this properly.
	candidate := longestProductPhrase(msg)
	if candidate == "" {
		return "", "", false
	}
	if sel := findResource(plan, candidate); sel != nil {
		if strings.EqualFold(sel.InUse(), candidate) {
			return "", "", false // already using it
		}
		return sel.InUse(), candidate, true
	}
	// Nothing matched by name; fall back to the first recommended resource when
	// the sentence clearly signals a swap.
	if containsAny(lower, insteadWords) && len(plan.Resources) > 0 {
		return plan.Resources[0].InUse(), candidate, true
	}
	return "", "", false
}

// stopPhraseWords are conversational filler that must not end up inside a
// product name.
var stopPhraseWords = map[string]bool{
	"i": true, "bought": true, "got": true, "have": true, "already": true,
	"instead": true, "of": true, "the": true, "a": true, "an": true, "use": true,
	"using": true, "can": true, "my": true, "and": true, "but": true, "am": true,
	"is": true, "it": true, "to": true, "for": true, "purchased": true, "own": true,
}

// longestProductPhrase extracts the longest run of consecutive non-filler words,
// which for "I bought Cambridge IELTS 18 instead" is "Cambridge IELTS 18".
func longestProductPhrase(msg string) string {
	fields := strings.Fields(strings.Trim(msg, ".?!"))
	best, cur := []string{}, []string{}
	flush := func() {
		if len(cur) > len(best) {
			best = append([]string(nil), cur...)
		}
		cur = cur[:0]
	}
	for _, f := range fields {
		clean := strings.Trim(f, ".,;:!?\"'")
		if clean == "" || stopPhraseWords[strings.ToLower(clean)] {
			flush()
			continue
		}
		cur = append(cur, clean)
	}
	flush()
	if len(best) == 0 {
		return ""
	}
	return strings.Join(best, " ")
}

// mockPlanAnswer gives a factual, deterministic answer to a plan question. It
// answers from the plan's own state — never from invention.
func mockPlanAnswer(sess *IntakeSession, plan *Plan, lower string) string {
	lang := sess.Lang

	// "Which book am I using?"
	if containsAny(lower, []string{"book", "resource", "material", "книг", "материал", "ресурс", "kitob", "material"}) {
		if len(plan.Resources) > 0 {
			names := make([]string, 0, len(plan.Resources))
			for _, r := range plan.Resources {
				if n := r.InUse(); n != "" {
					names = append(names, n)
				}
			}
			if len(names) > 0 {
				return tr(lang,
					"You're working from: "+strings.Join(names, "; ")+".",
					"Вы занимаетесь по: "+strings.Join(names, "; ")+".",
					"Siz quyidagilar bo'yicha ishlayapsiz: "+strings.Join(names, "; ")+".")
			}
		}
	}

	// "What should I do today?"
	if containsAny(lower, []string{"today", "сегодня", "bugun"}) {
		return tr(lang,
			"Check today's sessions on your calendar — "+planSummaryLine("en", plan),
			"Посмотрите сегодняшние занятия в календаре — "+planSummaryLine("ru", plan),
			"Bugungi mashg'ulotlarni kalendardan ko'ring — "+planSummaryLine("uz", plan))
	}

	// "Why did my finish date move?"
	if containsAny(lower, []string{"finish", "date move", "moved", "дата", "сдвин", "tugash", "ko'chdi"}) && len(plan.ChangeLog) > 0 {
		last := plan.ChangeLog[len(plan.ChangeLog)-1]
		return tr(lang,
			"Because of the last change: "+last.Summary,
			"Из-за последнего изменения: "+last.Summary,
			"Oxirgi o'zgarish tufayli: "+last.Summary)
	}

	return planSummaryLine(lang, plan)
}
