package main

import (
	"regexp"
	"strings"
	"time"
)

// This file is the mock brain: deterministic stand-ins for each AI stage so the
// whole product demos end-to-end with no OPENAI_API_KEY. With a key set, the
// pipeline uses the real ChatGPT API instead and none of this runs.

var (
	reInt     = regexp.MustCompile(`\d+`)
	reISODate = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
)

type skillDef struct {
	name    string
	pivotal string
	options []string
}

// known concrete skills → canonical name (+ optional pivotal fork).
// Keys are matched as word-start stems, so "код" matches "кодинг" but "code"
// does not match "decode" and "fit" does not match "profit".
var knownSkills = []struct {
	keys []string
	def  skillDef
}{
	{[]string{"ielts"}, skillDef{"IELTS", "Academic vs General", []string{"Academic", "General"}}},
	{[]string{"toefl"}, skillDef{"TOEFL", "", nil}},
	{[]string{"english", "английск", "ingliz"}, skillDef{"English", "", nil}},
	{[]string{"guitar", "гитар", "gitara"}, skillDef{"Guitar", "Acoustic vs Electric", []string{"Acoustic", "Electric"}}},
	{[]string{"piano", "пианино", "pianino"}, skillDef{"Piano", "", nil}},
	{[]string{"python", "programming", "coding", "code", "программир", "dasturlash", "kod"}, skillDef{"Programming", "", nil}},
	{[]string{"data analytics", "data", "аналитик", "data tahlil", "tahlil"}, skillDef{"Data analytics", "", nil}},
	{[]string{"valorant"}, skillDef{"Valorant", "", nil}},
	{[]string{"chess", "шахмат", "shaxmat"}, skillDef{"Chess", "", nil}},
	{[]string{"driving", "driver", "вожден", "haydovchi"}, skillDef{"Driving test", "", nil}},
	{[]string{"photography", "photo", "фотограф", "fotograf"}, skillDef{"Photography", "", nil}},
	{[]string{"drawing", "draw", "рисова", "chizish"}, skillDef{"Drawing", "", nil}},
	{[]string{"public speaking", "speaking", "ораторск", "notiqlik"}, skillDef{"Public speaking", "", nil}},
}

// vague goals that must pass through the disambiguation gate first.
var vagueGoals = []struct {
	id   string
	keys []string
}{
	{"gaming", []string{"gamer", "gaming", "games", "геймер", "гейм", "видеоигр", "geymer"}},
	{"fitness", []string{"get fit", "fitness", "fit", "healthy", "in shape", "фитнес", "спорт", "fitnes", "sport"}},
	{"business", []string{"business", "бизнес", "biznes"}},
}

// vagueQ returns the localized narrowing question + options for a vague goal.
func vagueQ(id, lang string) (string, []string) {
	switch id {
	case "gaming":
		return tr(lang,
				"“Gamer” can mean a few different journeys — which one fits you?",
				"«Геймер» — это несколько разных путей. Какой вам ближе?",
				"«Geymer» bir necha xil yo'lni anglatadi — qaysi biri sizga mos?"),
			[]string{
				tr(lang, "Get good at a game", "Прокачаться в игре", "O'yinda zo'r bo'lish"),
				tr(lang, "Go competitive", "Киберспорт", "Kibersportga kirish"),
				tr(lang, "Stream / build an audience", "Стриминг / аудитория", "Striming / auditoriya"),
				tr(lang, "Make games", "Разработка игр", "O'yin yaratish"),
			}
	case "fitness":
		return tr(lang,
				"Fitness covers a lot — what's your main aim?",
				"Фитнес — это широко. Какая у вас главная цель?",
				"Fitnes keng tushuncha — asosiy maqsadingiz nima?"),
			[]string{
				tr(lang, "Lose weight", "Похудеть", "Vazn tashlash"),
				tr(lang, "Build muscle", "Набрать мышцы", "Mushak yig'ish"),
				tr(lang, "Run / endurance", "Бег / выносливость", "Yugurish / chidamlilik"),
				tr(lang, "General health", "Общее здоровье", "Umumiy salomatlik"),
			}
	case "business":
		return tr(lang,
				"Business is broad — where do you want to start?",
				"Бизнес — это широко. С чего хотите начать?",
				"Biznes keng — nimadan boshlamoqchisiz?"),
			[]string{
				tr(lang, "Start a small business", "Открыть малый бизнес", "Kichik biznes ochish"),
				tr(lang, "Learn marketing", "Маркетинг", "Marketingni o'rganish"),
				tr(lang, "Learn finance", "Финансы", "Moliyani o'rganish"),
				tr(lang, "Freelance / side income", "Фриланс / подработка", "Frilans / qo'shimcha daromad"),
			}
	}
	return "", nil
}

var learningIntent = []string{"learn", "study", "prepare", "improve", "master", "practice", "how to", "become", "be a", "get good", "get better", "start"}
var offTopicStarts = []string{"what is", "what's the", "who is", "who's", "when is", "where is", "why is", "weather", "news", "tell me a joke", "translate", "write me", "capital of"}

// maxSkillLabel bounds how much raw user text can become a skill name, since
// that name ends up in plan titles and in the .ics filename.
const maxSkillLabel = 40

func mockUnderstand(message, lang string) understandResult {
	lower := strings.ToLower(message)

	// vague first — the disambiguation gate
	for _, v := range vagueGoals {
		if containsAny(lower, v.keys) {
			q, opts := vagueQ(v.id, lang)
			return understandResult{
				InScope:                true,
				Skill:                  capitalize(firstMatch(lower, v.keys)),
				NeedsDisambiguation:    true,
				DisambiguationQuestion: q,
				Options:                opts,
			}
		}
	}

	// known concrete skill
	for _, ks := range knownSkills {
		if containsAny(lower, ks.keys) {
			return understandResult{
				InScope:       true,
				Skill:         ks.def.name,
				PivotalChoice: ks.def.pivotal,
				Options:       ks.def.options,
				Overview:      overviewFor(ks.def.name, lang),
			}
		}
	}

	hasIntent := containsAny(lower, learningIntent)
	looksOffTopic := containsAny(lower, offTopicStarts)
	if looksOffTopic && !hasIntent {
		return understandResult{
			InScope: false,
			Decline: tr(lang,
				"I'm your learning coach, so I can't help with that — but tell me a skill you'd like to learn and we'll build a plan.",
				"Я ваш коуч по обучению и с этим помочь не смогу — но назовите навык, который хотите освоить, и мы составим план.",
				"Men sizning o'quv murabbiyingizman, bunga yordam bera olmayman — lekin o'rganmoqchi bo'lgan ko'nikmani ayting, biz reja tuzamiz."),
		}
	}

	// otherwise treat it as a (novel) skill the framework can still handle
	skill := sanitizeSkillLabel(message)
	if skill == "" {
		skill = tr(lang, "your goal", "вашей цели", "maqsadingiz")
	}
	return understandResult{
		InScope: true,
		Skill:   skill,
		Overview: tr(lang,
			"We'll treat this as a skill and build a personalized plan around it.",
			"Будем считать это навыком и построим вокруг него персональный план.",
			"Buni ko'nikma sifatida olib, atrofida shaxsiy reja tuzamiz."),
	}
}

// sanitizeSkillLabel keeps free-text skill names printable and bounded. Control
// characters are stripped because this value is echoed into plan text and, via
// the export, into an HTTP header.
func sanitizeSkillLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > maxSkillLabel {
		return ""
	}
	return s
}

func overviewFor(skill, lang string) string {
	switch skill {
	case "IELTS":
		return tr(lang,
			"IELTS has four sections — Listening, Reading, Writing, Speaking — scored 0–9. Academic and General are different tests.",
			"IELTS состоит из четырёх частей — Listening, Reading, Writing, Speaking — по шкале 0–9. Academic и General — это разные тесты.",
			"IELTS to'rt bo'limdan iborat — Listening, Reading, Writing, Speaking — 0–9 ball bilan. Academic va General — bu ikki xil imtihon.")
	case "Guitar":
		return tr(lang,
			"Guitar splits into acoustic and electric, and early progress is mostly chords, rhythm, and finger strength.",
			"Гитара делится на акустическую и электро, и на старте главное — аккорды, ритм и сила пальцев.",
			"Gitara akustik va elektrga bo'linadi, boshida asosiysi — akkordlar, ritm va barmoq kuchi.")
	case "Programming":
		return tr(lang,
			"Programming is built in layers — syntax, problem-solving, then real projects — and daily reps beat weekend cramming.",
			"Программирование строится слоями — синтаксис, решение задач, затем реальные проекты — и ежедневная практика лучше рывков по выходным.",
			"Dasturlash qatlamma-qatlam quriladi — sintaksis, masala yechish, so'ng real loyihalar — kunlik mashq dam olish kunidagi tiqishtirishdan afzal.")
	default:
		return tr(lang,
			"Here's roughly what this skill involves; a few questions will let me tailor the plan to you.",
			"Вот примерно, что включает этот навык; пара вопросов — и я подстрою план под вас.",
			"Bu ko'nikma taxminan nimani o'z ichiga oladi; bir necha savol bilan rejani sizga moslayman.")
	}
}

// mockIntake returns the next question given how many have already been asked,
// merging the newest user answer into the framework answers.
func mockIntake(sess *IntakeSession, latest string) intakeResult {
	res := intakeResult{Answers: map[string]string{}}
	ingestAnswer(sess, latest)

	type q struct {
		key     string
		text    string
		options []string
	}
	lang := sess.Lang
	queue := []q{}
	if sess.PivotalChoice != "" && sess.Answers.PivotalChoice == "" {
		queue = append(queue, q{"pivotal",
			tr(lang, "Quick fork: "+sess.PivotalChoice+"?", "Быстрый выбор: "+sess.PivotalChoice+"?", "Tezkor tanlov: "+sess.PivotalChoice+"?"),
			pivotalOptions(sess)})
	}
	queue = append(queue,
		q{"currentLevel", tr(lang,
			"What's your current level — where are you starting from?",
			"Какой у вас сейчас уровень — с чего начинаете?",
			"Hozirgi darajangiz qanday — qayerdan boshlayapsiz?"), nil},
		q{"target", tr(lang,
			"What exactly do you want to achieve (your target)?",
			"Чего именно вы хотите достичь (ваша цель)?",
			"Aynan nimaga erishmoqchisiz (maqsadingiz)?"), nil},
		q{"deadline", tr(lang,
			"Is there a deadline or test date? (a date, or “no deadline”)",
			"Есть ли срок или дата экзамена? (дата или «без срока»)",
			"Muddat yoki imtihon sanasi bormi? (sana yoki «muddatsiz»)"),
			[]string{tr(lang, "No deadline", "Без срока", "Muddatsiz")}},
		q{"time", mockTimeQuestion(sess), nil},
		q{"budget", tr(lang,
			"What's your budget for materials or gear?",
			"Какой у вас бюджет на материалы или оборудование?",
			"Materiallar yoki jihozlar uchun byudjetingiz qancha?"),
			[]string{
				tr(lang, "Free / minimal", "Бесплатно / минимум", "Bepul / minimal"),
				tr(lang, "Some budget", "Есть бюджет", "Byudjet bor"),
				tr(lang, "Flexible", "Гибко", "Moslashuvchan"),
			}},
	)

	for _, item := range queue {
		if !answered(sess, item.key) {
			res.NextQuestion = item.text
			res.Options = item.options
			return res
		}
	}
	res.Done = true
	return res
}

// mockTimeAskLimit bounds how often the no-key interview may come back for the
// availability it still needs, so an unparseable answer cannot loop.
const mockTimeAskLimit = 2

// mockTimeQuestion asks only for the half of the availability that is missing.
// Asking someone who just said "Monday and Saturday" which days they can study
// is the redundancy the interview is supposed to avoid.
func mockTimeQuestion(sess *IntakeSession) string {
	lang := sess.Lang
	switch missingAvailability(currentAvailability(sess)) {
	case "days":
		return tr(lang,
			"Which days of the week can you study?",
			"В какие дни недели вы можете заниматься?",
			"Haftaning qaysi kunlari shug'ullana olasiz?")
	case "time":
		return tr(lang,
			"How much time can you give it on those days — per day, or per week?",
			"Сколько времени вы можете уделять в эти дни — в день или в неделю?",
			"O'sha kunlari qancha vaqt ajrata olasiz — kuniga yoki haftasiga?")
	default:
		return tr(lang,
			"Which days can you study, and how much time on each?",
			"В какие дни вы можете заниматься и сколько времени в каждый?",
			"Qaysi kunlari shug'ullana olasiz va har birida qancha vaqt?")
	}
}

func mockTimeAsks(sess *IntakeSession) int { return atoi(sess.AnswerBag["time_asks"]) }

func pivotalOptions(sess *IntakeSession) []string {
	for _, ks := range knownSkills {
		if ks.def.name == sess.Skill {
			return ks.def.options
		}
	}
	return nil
}

func answered(sess *IntakeSession, key string) bool {
	a := sess.Answers
	switch key {
	case "pivotal":
		return a.PivotalChoice != ""
	case "currentLevel":
		return a.CurrentLevel != ""
	case "target":
		return a.Target != ""
	case "deadline":
		return a.Deadline != "" || sess.AnswerBag["deadline_asked"] == "yes"
	case "time":
		// Both halves, or the allowance is spent. A weekly figure with no days
		// (or days with no hours) cannot be scheduled and must not be treated
		// as an answer — that is how an unstated availability used to become a
		// silent six-hour default.
		return currentAvailability(sess).Complete() || mockTimeAsks(sess) >= mockTimeAskLimit
	case "budget":
		return a.Budget != ""
	}
	return false
}

// ingestAnswer maps the newest free-text reply onto whichever category is next.
func ingestAnswer(sess *IntakeSession, latest string) {
	latest = strings.TrimSpace(latest)
	if latest == "" {
		return
	}
	if sess.AnswerBag == nil {
		sess.AnswerBag = map[string]string{}
	}
	a := &sess.Answers
	lower := strings.ToLower(latest)

	// pivotal
	if sess.PivotalChoice != "" && a.PivotalChoice == "" {
		for _, opt := range pivotalOptions(sess) {
			if strings.Contains(lower, strings.ToLower(opt)) {
				a.PivotalChoice = opt
				return
			}
		}
	}
	if a.CurrentLevel == "" {
		a.CurrentLevel = latest
		return
	}
	if a.Target == "" {
		a.Target = latest
		return
	}
	if a.Deadline == "" && sess.AnswerBag["deadline_asked"] != "yes" {
		sess.AnswerBag["deadline_asked"] = "yes"
		if d := reISODate.FindString(latest); d != "" {
			if validDate(d) {
				a.Deadline = d
			}
		}
		return
	}
	if !currentAvailability(sess).Complete() && mockTimeAsks(sess) < mockTimeAskLimit {
		sess.AnswerBag["time_asks"] = itoa(mockTimeAsks(sess) + 1)
		// Deterministic parsing, same as the live path: the backend does this
		// arithmetic. Nothing is invented when the answer says nothing — an
		// unparseable reply leaves availability incomplete and earns one more
		// question rather than a default the learner never chose.
		if parsed := parseAvailabilityAnswer(latest); parsed.HasDays() || parsed.HasTime() {
			syncAnswersAvailability(sess, mergeAvailability(currentAvailability(sess), parsed))
		}
		return
	}
	if a.Budget == "" {
		a.Budget = latest
		return
	}
}

// parseHoursPerWeek pulls a plausible weekly hour count out of free text. Date
// fragments are removed first, otherwise an answer like "2026-12-01" yields the
// year and clamps to an absurd 60 hours a week.
func parseHoursPerWeek(text string) (int, bool) {
	cleaned := reISODate.ReplaceAllString(text, " ")
	for _, m := range reInt.FindAllString(cleaned, -1) {
		n, ok := parseInt(m)
		if !ok {
			continue
		}
		if n >= 1 && n <= 80 {
			return clamp(n, 1, 60), true
		}
	}
	return 0, false
}

// mockPlan builds a believable, schedulable plan without any model call.
//
// loc is the *user's* timezone. Measuring the gap to their deadline in the
// server's zone instead shifts the plan length by a week for anyone far enough
// east or west of the server.
func mockPlan(sess *IntakeSession, loc *time.Location) planAI {
	lang := sess.Lang
	weeks := 12
	if d, ok := parseDateIn(sess.Answers.Deadline, loc); ok {
		w := daysBetween(todayIn(loc), d)/7 + 1
		weeks = clamp(w, 2, 24)
	}
	skill := sess.Skill
	target := firstNonEmpty(sess.Answers.Target, tr(lang, "your goal", "вашей цели", "maqsadingiz"))
	level := firstNonEmpty(sess.Answers.CurrentLevel, tr(lang, "your current level", "вашего уровня", "hozirgi darajangiz"))

	phases := mockPhases(weeks, lang)
	mid := clamp(weeks/2, 1, weeks)
	diagWeek := clamp(phases[0].WeekEnd, 1, weeks)

	p := planAI{
		Assessment: tr(lang,
			"Going from "+level+" to "+target+" in about "+itoa(weeks)+" weeks is realistic with steady, focused practice. We'll front-load fundamentals, then drill your weak spots, then rehearse under real conditions.",
			"Переход от «"+level+"» к «"+target+"» примерно за "+itoa(weeks)+" недель реалистичен при регулярной, сфокусированной практике. Сначала укрепим основы, затем проработаем слабые места, затем — репетиции в реальных условиях.",
			"«"+level+"» dan «"+target+"» ga taxminan "+itoa(weeks)+" hafta ichida yetish — muntazam, izchil mashq bilan real. Avval asoslarni mustahkamlaymiz, so'ng zaif tomonlarni ishlaymiz, keyin real sharoitda mashq qilamiz."),
		Feasibility: tr(lang,
			"Feasible at your pace if you protect the weekly hours. If you fall behind, the schedule rolls forward automatically and the finish date shifts.",
			"Достижимо в вашем темпе, если беречь недельные часы. Если отстанете, расписание автоматически сдвигается вперёд, и дата финиша меняется.",
			"Haftalik soatlarni saqlasangiz, sur'atingizda erishsa bo'ladi. Orqada qolsangiz, jadval avtomatik oldinga suriladi va tugash sanasi o'zgaradi."),
		WeeksTotal: weeks,
		Phases:     phases,
		Milestones: []milestoneAI{
			{tr(lang, "Complete a diagnostic and know your weak areas", "Пройти диагностику и узнать слабые места", "Diagnostikadan o'tib, zaif tomonlarni bilish"), phases[0].Key, diagWeek},
			{tr(lang, "Hit a solid mid-point checkpoint on "+skill, "Достичь уверенной контрольной точки по «"+skill+"»", "«"+skill+"» bo'yicha ishonchli oraliq nuqtaga yetish"), phases[len(phases)/2].Key, mid},
			{tr(lang, "Pass a full practice run at target level", "Пройти полный пробный прогон на целевом уровне", "Maqsad darajasida to'liq sinov mashqidan o'tish"), phases[len(phases)-1].Key, weeks},
		},
		Todos:      mockTodos(skill, lang, phases),
		SetupItems: mockSetup(skill, lang),
	}
	return p
}

// mockPhases lays out fundamentals → drills → rehearsal with week ranges that
// stay valid for short plans. A fixed layout produced windows like [3, -1] as
// soon as the deadline was under six weeks away.
func mockPhases(weeks int, lang string) []phaseAI {
	fundEnd := clamp(weeks/6, 1, weeks)
	if weeks >= 3 && fundEnd > weeks-2 {
		fundEnd = weeks - 2
	}
	rehStart := weeks - clamp(weeks/6, 1, weeks) + 1
	if rehStart <= fundEnd {
		rehStart = fundEnd + 1
	}
	if rehStart > weeks {
		rehStart = weeks
	}

	out := []phaseAI{{
		Key:       "fundamentals",
		Title:     tr(lang, "Fundamentals & diagnostic", "Основы и диагностика", "Asoslar va diagnostika"),
		Summary:   tr(lang, "Establish a baseline and cover the basics.", "Определить стартовый уровень и закрыть основы.", "Boshlang'ich darajani aniqlab, asoslarni yopish."),
		WeekStart: 1,
		WeekEnd:   fundEnd,
	}}
	if drillsEnd := rehStart - 1; drillsEnd >= fundEnd+1 {
		out = append(out, phaseAI{
			Key:       "drills",
			Title:     tr(lang, "Focused drills", "Целевые тренировки", "Yo'naltirilgan mashqlar"),
			Summary:   tr(lang, "Target the highest-impact weaknesses.", "Проработать самые важные слабые места.", "Eng ta'sirli zaif tomonlarni ishlash."),
			WeekStart: fundEnd + 1,
			WeekEnd:   drillsEnd,
		})
	}
	if rehStart > fundEnd {
		out = append(out, phaseAI{
			Key:       "rehearsal",
			Title:     tr(lang, "Full rehearsal", "Полная репетиция", "To'liq mashq"),
			Summary:   tr(lang, "Simulate the real thing under time pressure.", "Смоделировать реальные условия на время.", "Real sharoitni vaqt bosimida modellashtirish."),
			WeekStart: rehStart,
			WeekEnd:   weeks,
		})
	}
	return out
}

// phaseKeyOr returns want if the plan has that phase, else the nearest one, so
// short plans that drop the drills phase still produce schedulable todos.
func phaseKeyOr(phases []phaseAI, want string) string {
	for _, p := range phases {
		if p.Key == want {
			return want
		}
	}
	if len(phases) == 0 {
		return ""
	}
	return phases[len(phases)-1].Key
}

func mockTodos(skill, lang string, phases []phaseAI) []todoAI {
	fund := phaseKeyOr(phases, "fundamentals")
	drills := phaseKeyOr(phases, "drills")
	reh := phaseKeyOr(phases, "rehearsal")

	if strings.EqualFold(skill, "IELTS") {
		return []todoAI{
			{tr(lang, "Diagnostic full practice test + score yourself", "Диагностический пробный тест + самооценка", "Diagnostik to'liq sinov testi + o'zingizni baholang"), 120, "once", "high", fund, nil, "cambridge-ielts"},
			{tr(lang, "Reading: one Cambridge test + review mistakes", "Reading: один тест Cambridge + разбор ошибок", "Reading: bitta Cambridge testi + xatolarni tahlil"), 60, "twice_weekly", "high", drills, nil, "cambridge-ielts"},
			{tr(lang, "Writing Task 2 essay + self-check against band descriptors", "Writing Task 2 эссе + самопроверка по критериям", "Writing Task 2 esse + band mezonlari bo'yicha o'z-o'zini tekshirish"), 60, "twice_weekly", "high", drills, nil, "band-descriptors"},
			{tr(lang, "Speaking practice out loud (record & review)", "Speaking вслух (запись и разбор)", "Speaking ovoz chiqarib (yozib olib, tahlil qilish)"), 30, "thrice_weekly", "medium", drills, nil, ""},
			{tr(lang, "Listening section under timed conditions", "Listening на время", "Listening bo'limi vaqt bilan"), 45, "weekly", "medium", drills, nil, "cambridge-ielts"},
			{tr(lang, "Full timed mock test", "Полный пробный экзамен на время", "To'liq vaqtli sinov imtihoni"), 165, "weekly", "high", reh, nil, "cambridge-ielts"},
		}
	}
	return []todoAI{
		{tr(lang, "Diagnostic: assess where you stand in "+skill, "Диагностика: оценить ваш уровень в «"+skill+"»", "Diagnostika: «"+skill+"» bo'yicha darajangizni baholash"), 60, "once", "high", fund, nil, ""},
		{tr(lang, "Core practice session on fundamentals", "Базовая тренировка по основам", "Asoslar bo'yicha asosiy mashg'ulot"), 45, "thrice_weekly", "high", drills, nil, ""},
		{tr(lang, "Targeted drill on your weakest area", "Целевая тренировка слабого места", "Eng zaif tomon bo'yicha yo'naltirilgan mashq"), 45, "twice_weekly", "high", drills, nil, ""},
		{tr(lang, "Review progress & reflect on what's working", "Обзор прогресса и что работает", "Taraqqiyotni ko'rib chiqish va nima ishlayotganini baholash"), 30, "weekly", "medium", drills, nil, ""},
		{tr(lang, "Full rehearsal at target difficulty", "Полная репетиция на целевой сложности", "Maqsad darajasidagi to'liq mashq"), 90, "weekly", "high", reh, nil, ""},
	}
}

func mockSetup(skill, lang string) []setupAI {
	if strings.EqualFold(skill, "IELTS") {
		return []setupAI{
			{tr(lang, "Official Cambridge IELTS practice books", "Официальные сборники Cambridge IELTS", "Rasmiy Cambridge IELTS mashq kitoblari"), "materials", "high", "$0-25", false,
				tr(lang, "The single best-value resource; older editions and library copies are near-free.", "Лучший по соотношению цена/польза; старые издания и библиотека — почти бесплатно.", "Eng foydali resurs; eski nashrlar va kutubxona nusxalari deyarli bepul."), "Cambridge IELTS"},
			{tr(lang, "Decent headphones for Listening practice", "Нормальные наушники для Listening", "Listening uchun yaxshi quloqchin"), "gear", "medium", "$15-30", false,
				tr(lang, "Clear audio matters for the Listening section; you may already own a pair.", "Чистый звук важен для Listening; возможно, у вас уже есть.", "Listening uchun toza ovoz muhim; ehtimol sizda bor."), "наушники"},
			{tr(lang, "Notebook + timer (phone works)", "Блокнот + таймер (подойдёт телефон)", "Daftar + taymer (telefon ham bo'ladi)"), "gear", "low", "$0", false,
				tr(lang, "Timed practice is free — use what you have.", "Практика на время бесплатна — используйте то, что есть.", "Vaqtli mashq bepul — bor narsangizdan foydalaning."), ""},
		}
	}
	return []setupAI{
		{tr(lang, "Free/low-cost starter materials for "+skill, "Бесплатные/недорогие стартовые материалы по «"+skill+"»", "«"+skill+"» uchun bepul/arzon boshlang'ich materiallar"), "materials", "high", "$0-20", false,
			tr(lang, "Start with free resources; upgrade only once you hit a real limit.", "Начните с бесплатных ресурсов; улучшайте только при реальном упоре в потолок.", "Bepul resurslardan boshlang; faqat haqiqiy chegaraga yetganda yangilang."), skill},
		{tr(lang, "Basic practice tools you likely already own", "Базовые инструменты, которые у вас, вероятно, уже есть", "Sizda allaqachon bor bo'lishi mumkin bo'lgan asosiy vositalar"), "gear", "low", "$0", false,
			tr(lang, "Don't buy anything yet — settings and free tools go a long way.", "Пока ничего не покупайте — настроек и бесплатных инструментов достаточно.", "Hozircha hech narsa sotib olmang — sozlamalar va bepul vositalar yetarli."), ""},
	}
}

// ---- tiny helpers ----

// containsAny reports whether any key appears in s at a word-start boundary.
func containsAny(s string, keys []string) bool {
	for _, k := range keys {
		if containsStem(s, k) {
			return true
		}
	}
	return false
}

func firstMatch(s string, keys []string) string {
	for _, k := range keys {
		if containsStem(s, k) {
			return k
		}
	}
	return ""
}

// affirmatives are the ways a user says "go ahead" in the three supported
// languages, matched as word-start stems so "да" does not fire inside "дальше".
var affirmatives = []string{
	"yes", "yeah", "yep", "ok", "okay", "sure", "go ahead", "build", "confirm", "proceed",
	"да", "давай", "конечно", "стро", "состав", "подтвер",
	"ha", "mayli", "albatta", "tuz", "boshla", "roziman",
}

// negatives veto an affirmative, because the stems above happily match inside a
// refusal: "not sure" contains "sure" and was read as a green light, building a
// plan for someone who had just said they were undecided. Approval has to be
// unambiguous — anything else returns to the recap, which costs a turn, while
// guessing wrong costs a plan nobody agreed to.
var negatives = []string{
	"no", "not", "nope", "wait", "hold", "change", "instead", "rather",
	"нет", "не", "подожд", "измен", "друг",
	"yo'q", "emas", "kut", "o'zgart", "boshqa",
}

// approvesPlan reports a clear, unambiguous go-ahead.
func approvesPlan(latest string) bool {
	lower := strings.ToLower(latest)
	if containsAny(lower, negatives) {
		return false
	}
	return containsAny(lower, affirmatives)
}

// mockConfirm is the no-key stand-in for the confirmation gate. Without it the
// mock pipeline skipped straight from the interview to a plan, so the stage the
// user has to approve — the one a frontend most needs to build against — could
// only be exercised by spending real tokens.
//
// It does not estimate how long a skill takes; that is the one thing this file
// cannot fake honestly. It reports the capacity the user actually has — from
// their stated availability, never from the span to the deadline — and asks.
func mockConfirm(sess *IntakeSession, h horizon, latest string) feasibilityResult {
	lang := sess.Lang
	a := sess.Answers
	availableHours := h.studyHours()

	line := func(label, value string) string {
		if strings.TrimSpace(value) == "" {
			return ""
		}
		return "- " + label + ": " + value + "\n"
	}
	var sb strings.Builder
	sb.WriteString(line(tr(lang, "Starting point", "Сейчас", "Hozirgi daraja"), a.CurrentLevel))
	sb.WriteString(line(tr(lang, "Target", "Цель", "Maqsad"), a.Target))
	sb.WriteString(line(tr(lang, "Deadline", "Срок", "Muddat"), a.Deadline))
	av := currentAvailability(sess)
	if av.HasTime() {
		sb.WriteString(line(tr(lang, "Hours per week", "Часов в неделю", "Haftasiga soat"), itoa(av.HoursPerWeek())))
	}
	sb.WriteString(line(tr(lang, "Days", "Дни", "Kunlar"), strings.Join(av.Days, ", ")))
	for _, pd := range av.PerDay {
		sb.WriteString(line(pd.Weekday, itoa(pd.Minutes)+tr(lang, " min", " мин", " daqiqa")))
	}
	sb.WriteString(line(tr(lang, "Budget", "Бюджет", "Byudjet"), a.Budget))
	for _, n := range sess.PlanNotes {
		sb.WriteString(line(tr(lang, "Note", "Заметка", "Eslatma"), n))
	}

	// The verdict states what is KNOWN and nothing more. This file has no basis
	// for estimating how many hours a skill takes, so it never implies one —
	// and where availability is still missing it says so instead of quietly
	// reporting a capacity derived from a default.
	verdict := ""
	status := unknownStatus
	switch {
	case !h.HasDeadline && av.Complete():
		// No deadline is not a problem to be solved, and certainly not one to
		// be solved by inventing a year. There is simply nothing to be late
		// for; the projection line says when this would finish.
		status = feasibleStatus
		verdict = tr(lang,
			"At "+itoa(av.HoursPerWeek())+"h a week on "+strings.Join(av.Days, "/")+", with no deadline set, we can pace this properly.",
			"При "+itoa(av.HoursPerWeek())+" ч в неделю ("+strings.Join(av.Days, "/")+") и без установленного срока темп можно выстроить спокойно.",
			"Haftasiga "+itoa(av.HoursPerWeek())+" soat ("+strings.Join(av.Days, "/")+") va muddat belgilanmagan — sur'atni bemalol tanlaymiz.")
	case !h.CapacityKnown:
		verdict = tr(lang,
			"I don't have your study availability yet, so I can't tell you how much practice time this timeline actually gives you.",
			"У меня пока нет вашей доступности для занятий, поэтому я не могу сказать, сколько практики реально даёт этот срок.",
			"Menda hali o'quv vaqtingiz yo'q, shuning uchun bu muddat qancha mashq vaqti berishini ayta olmayman.")
	case a.Deadline != "":
		status = feasibleStatus
		verdict = tr(lang,
			"At "+itoa(av.HoursPerWeek())+"h a week on "+strings.Join(av.Days, "/")+", you have about "+itoa(availableHours)+" scheduled study hours before "+a.Deadline+". I'll build the plan to fit that.",
			"При "+itoa(av.HoursPerWeek())+" ч в неделю ("+strings.Join(av.Days, "/")+") до "+a.Deadline+" у вас около "+itoa(availableHours)+" запланированных учебных часов. Я составлю план под это.",
			"Haftasiga "+itoa(av.HoursPerWeek())+" soat ("+strings.Join(av.Days, "/")+") bilan "+a.Deadline+" gacha sizda taxminan "+itoa(availableHours)+" rejalashtirilgan o'quv soati bor. Rejani shunga moslayman.")
	default:
		status = feasibleStatus
		verdict = tr(lang,
			"At "+itoa(av.HoursPerWeek())+"h a week on "+strings.Join(av.Days, "/")+", with no fixed deadline, we can pace this properly.",
			"При "+itoa(av.HoursPerWeek())+" ч в неделю ("+strings.Join(av.Days, "/")+") и без жёсткого срока темп можно выстроить спокойно.",
			"Haftasiga "+itoa(av.HoursPerWeek())+" soat ("+strings.Join(av.Days, "/")+") va qat'iy muddatsiz — sur'atni bemalol belgilaymiz.")
	}
	question := tr(lang,
		"Shall I build your plan from this?",
		"Составить план на этой основе?",
		"Shu asosda rejangizni tuzaymi?")

	r := feasibilityResult{
		AvailableHours: availableHours,
		Status:         status,
		Summary:        strings.TrimRight(sb.String(), "\n"),
		Verdict:        verdict,
		Question:       question,
	}
	if strings.TrimSpace(latest) == "" {
		return r
	}
	// A reply: anything affirmative goes ahead, anything else comes back here.
	r.Decision.Resolved = true
	r.Decision.KeepOriginal = true
	r.Decision.Approved = approvesPlan(latest)
	if !r.Decision.Approved {
		r.Question = tr(lang,
			"Tell me what to change, or say yes and I'll build it.",
			"Скажите, что изменить, или ответьте «да», и я составлю план.",
			"Nimani o'zgartirishni ayting yoki «ha» deng — men rejani tuzaman.")
	}
	return r
}
