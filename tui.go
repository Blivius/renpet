package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The view borrows termheat's grammar: a weekday × time grid of 2-character cells, one colour
// ramp from less to more, a fire flicker where something burned hot (here: 3+ agents at once),
// a bold stats block, one line of voice, and bracketed keys. Motion is time-based, not
// frame-based, so it looks the same at any frame rate: a short ease-out reveal on every load,
// cells that breathe near full brightness, and fire that flickers fast.
const (
	fps     = 30
	reveal  = 450 * time.Millisecond
	breathe = 3 * time.Second
	gutter  = 7 // "Wed ›  ": every grid row and lane starts after this
)

// palette is every colour the app uses. Dark is the default; a light terminal gets light.
type palette struct {
	ramp, fire, whom      []string
	accent, voice, unknwn string
}

var (
	darkPalette = palette{
		ramp:   []string{"#30363d", "#0e4429", "#006d32", "#26a641", "#39d353"}, // spring green: renewal
		fire:   []string{"#ff5f1f", "#ff8c42", "#ffb627", "#ffd23f"},
		whom:   []string{"#58a6ff", "#ffa657", "#d2a8ff", "#7ee787", "#ff7b72", "#f2cc60", "#79c0ff"},
		accent: "#39d353", voice: "#ffb627", unknwn: "#8b949e",
	}
	lightPalette = palette{
		ramp:   []string{"#ebedf0", "#9be9a8", "#40c463", "#30a14e", "#216e39"},
		fire:   []string{"#cf222e", "#e16f24", "#bc4c00", "#9a6700"},
		whom:   []string{"#0969da", "#bc4c00", "#8250df", "#1a7f37", "#cf222e", "#9a6700", "#0550ae"},
		accent: "#1a7f37", voice: "#9a6700", unknwn: "#6e7781",
	}
	ramp, fireRamp, whomHex      = darkPalette.ramp, darkPalette.fire, darkPalette.whom
	accent, voiceHex, unknownHex = darkPalette.accent, darkPalette.voice, darkPalette.unknwn
	dim                          = lipgloss.NewStyle().Faint(true)
	bold                         = lipgloss.NewStyle().Bold(true)
)

func usePalette(light bool) {
	p := darkPalette
	if light {
		p = lightPalette
	}
	ramp, fireRamp, whomHex, accent, voiceHex, unknownHex = p.ramp, p.fire, p.whom, p.accent, p.voice, p.unknwn
}

// scaleHex darkens a #rrggbb colour toward black: 1 = unchanged, 0 = black. Breathing and
// flicker vary this factor instead of swapping palette entries.
func scaleHex(hex string, f float64) string {
	f = math.Max(0, math.Min(1, f))
	out := "#"
	for i := 1; i < 7; i += 2 {
		v, err := strconv.ParseUint(hex[i:i+2], 16, 8)
		if err != nil {
			return hex
		}
		out += fmt.Sprintf("%02x", int(math.Round(float64(v)*f)))
	}
	return out
}

func paint(hex, s string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Render(s)
}

// level maps time inside a cell to a ramp step 0–4, by quarters of the cell.
func level(d, cell time.Duration) int {
	if d <= 0 {
		return 0
	}
	return min(4, int(math.Ceil(4*float64(d)/float64(cell))))
}

// A period is what the arrows step through: a day, a week, a month, or a custom stretch of days.
type period int

const (
	dayP period = iota
	weekP
	monthP
	customP
)

var periodNames = [...]string{"day", "week", "month", "range"}

func startOfDay(t time.Time) time.Time {
	y, mo, d := t.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, time.Local)
}

// weekday is t's row in the week grid: 0 for the configured first day of the week … 6.
func weekday(t time.Time) int { return (int(t.Weekday()) - int(weekStart) + 7) % 7 }

// weekOf returns the first day of t's week.
func weekOf(t time.Time) time.Time { return startOfDay(t).AddDate(0, 0, -weekday(t)) }

// periodSpan is the day, week or month holding t.
func periodSpan(p period, t time.Time) (from, to time.Time) {
	switch p {
	case dayP:
		d := startOfDay(t)
		return d, d.AddDate(0, 0, 1)
	case monthP:
		y, mo, _ := t.Date()
		f := time.Date(y, mo, 1, 0, 0, 0, 0, time.Local)
		return f, f.AddDate(0, 1, 0)
	}
	w := weekOf(t)
	return w, w.AddDate(0, 0, 7)
}

// nDays counts calendar days in [from, to), DST-safe.
func nDays(from, to time.Time) (n int) {
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		n++
	}
	return
}

// prevSpan is the stretch of the period before to compare with. While the period is still
// running it is cut to the same length, so a Wednesday isn't compared with all of last week.
func prevSpan(p period, from, to, now time.Time) (time.Time, time.Time) {
	pf := from.AddDate(0, 0, -nDays(from, to))
	switch p {
	case weekP:
		pf = from.AddDate(0, 0, -7)
	case monthP:
		pf = from.AddDate(0, -1, 0)
	}
	end := from
	if now.Before(to) && now.After(from) {
		end = minT(from, pf.Add(now.Sub(from)))
	}
	return pf, end
}

// rangeData is everything the screens draw for one range, read once per range.
type rangeData struct {
	from, to time.Time
	cells    [][]Cell // per day: 24 hourly cells
	days     []Report // per day
	total    Report
	prev     Report // the same stretch of the period before: "▲ 12% vs last week"
	ats      []Attributed
	gaps     []time.Duration // your prompt gaps: what the gaps screen draws
}

func loadRange(p period, from, to time.Time, rules []Rule, idle time.Duration) rangeData {
	pf, _ := prevSpan(p, from, to, time.Now())
	return rangeFrom(load(pf, to), p, from, to, rules, idle, time.Now())
}

// rangeFrom is loadRange without the reading: sessions in, everything the screens draw out.
func rangeFrom(sessions []Session, p period, from, to time.Time, rules []Rule, idle time.Duration, now time.Time) rangeData {
	pf, pt := prevSpan(p, from, to, now)
	ats := attribute(sessions, rules, idle, from, to)
	d := rangeData{from: from, to: to, ats: ats, total: report(ats, idle, from, to), gaps: promptGaps(sessions, from, to)}
	// Compare only with a stretch the logs still cover: agents delete old transcripts (Claude Code
	// after 30 days by default), and "▲ 381% vs last month" would just be measuring the deletion.
	oldest := now
	for _, s := range sessions {
		for _, e := range s.Events {
			oldest = minT(oldest, e.T)
		}
	}
	if !oldest.After(pf.Add(24 * time.Hour)) {
		d.prev = build(sessions, rules, idle, pf, pt)
	}
	for day := from; day.Before(to); day = day.AddDate(0, 0, 1) {
		next := day.AddDate(0, 0, 1)
		in := clipAts(ats, day, next)
		d.cells = append(d.cells, laneCells(in, day, time.Hour))
		d.days = append(d.days, report(in, idle, day, next))
	}
	return d
}

// total is a report's grand-total row; whomRow one whom's.
func total(r Report) Row { return whomRow(r, "TOTAL") }

func whomRow(r Report, w string) Row {
	for _, row := range r.Rows {
		if row.Project == "*" && row.Whom == w {
			return row
		}
	}
	return Row{Project: "*", Whom: w}
}

type tickMsg time.Time
type loadedMsg struct {
	d   rangeData
	gen int
}

type model struct {
	rules      []Rule
	idle       time.Duration
	per        period
	from, to   time.Time // the range on screen, to exclusive
	sel        int       // selected day, 0 = from
	d          *rangeData
	loaded     bool                        // something has been drawn: later loads keep the shell up
	past       map[[2]time.Time]*rangeData // finished ranges can't change, so each is read once
	now, shown time.Time                   // shown = when the current range appeared; the reveal replays from there
	gen        int                         // bumped by every settings change, so loads started before it are dropped

	scr, ret      screen // ret: where esc goes from gaps (overview or settings)
	width, height int
	whole         bool       // projects and sessions cover the whole range, not just the selected day
	all           bool       // also list tiny projects, automation folders and subagents one by one
	cur, order    [16]int    // cursor and list order, per screen
	filter        string     // the current list shows only rows containing this
	typing        bool       // the filter line has focus
	proj          Row        // the project being looked at: "" = every project, "*" = every folder of proj.Whom
	at            Attributed // the session being looked at
	detail        *Session   // that session re-read with your prompts; nil while reading
	derr          error
	cfg           Config
	cfgPath       string
	assigning     bool   // typing who an unassigned folder is for
	input         string // what has been typed so far
	gapIdle       time.Duration
	note          string // one line of feedback, e.g. "saved", shown in place of the keys for a while
	noteAt        time.Time
	help          bool // the key sheet is open
	pal           paletteState
	light         bool
	rec           *recordsData // nil until the year of transcripts behind Records is read
	recBusy       bool
	gs            guessState // the perception check
	live          *liveData  // what Live shows; read every 2s while it is open
	liveBusy      bool
	card          bool // the share card is open
	cardHide      bool // and blanks every name
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second/fps, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) load() tea.Cmd {
	p, from, to, rules, idle, gen := m.per, m.from, m.to, m.rules, m.idle, m.gen
	return func() tea.Msg { return loadedMsg{loadRange(p, from, to, rules, idle), gen} }
}

func (m model) Init() tea.Cmd { return tea.Batch(tickCmd(), m.load(), tea.RequestBackgroundColor) }

func (m model) days() int { return nDays(m.from, m.to) }

func (m model) selDay() time.Time { return m.from.AddDate(0, 0, m.sel) }

func (m model) flash(s string) model {
	m.note, m.noteAt = s, m.now
	return m
}

// setRange shows [from, to) with day selected if it is inside, reading it unless remembered.
func (m model) setRange(from, to, day time.Time) (model, tea.Cmd) {
	m.from, m.to, m.sel = from, to, 0
	if !day.Before(from) && day.Before(to) {
		m.sel = nDays(from, startOfDay(day))
	}
	m.d, m.shown = m.past[[2]time.Time{from, to}], m.now
	if m.d == nil {
		return m, m.load()
	}
	return m, nil
}

// shift moves the range n periods back or forward; never past the period holding today.
func (m model) shift(n int) (model, tea.Cmd) {
	day, f, t := m.selDay(), m.from, m.to
	switch m.per {
	case dayP:
		f = m.from.AddDate(0, 0, n)
		t, day = f.AddDate(0, 0, 1), f
	case monthP:
		f = m.from.AddDate(0, n, 0)
		t, day = f.AddDate(0, 1, 0), f
	default:
		k := nDays(m.from, m.to)
		f, t, day = m.from.AddDate(0, 0, n*k), m.to.AddDate(0, 0, n*k), day.AddDate(0, 0, n*k)
	}
	if !f.Before(time.Now()) {
		return m, nil
	}
	return m.setRange(f, t, day)
}

func (m model) setPeriod(p period) (model, tea.Cmd) {
	m.per = p
	f, t := periodSpan(p, m.selDay())
	return m.setRange(f, t, m.selDay())
}

func (m model) today() (model, tea.Cmd) {
	if m.per == customP {
		m.per = weekP
	}
	now := time.Now()
	f, t := periodSpan(m.per, now)
	return m.setRange(f, t, now)
}

// rangeLabel names the range: "Tue Nov 24", "week of Nov 23", "November 2026", "Nov 1 – Nov 25".
func (m model) rangeLabel() string {
	switch m.per {
	case dayP:
		return m.from.Format("Mon Jan 2")
	case weekP:
		return "week of " + m.from.Format("Jan 2")
	case monthP:
		return m.from.Format("January 2006")
	}
	return m.from.Format("Jan 2") + " – " + m.to.AddDate(0, 0, -1).Format("Jan 2")
}

// delta compares the range with the same stretch of the period before: "▲ 23% vs last week".
func (m model) delta() string {
	cur, prev := total(m.d.total).ActiveHours, total(m.d.prev).ActiveHours
	if prev < 0.05 || cur < 0.05 {
		return ""
	}
	what := map[period]string{dayP: "the day before", weekP: "last week", monthP: "last month", customP: "the range before"}[m.per]
	if time.Now().Before(m.to) {
		what = "this point " + strings.TrimPrefix(what, "the ")
	}
	pct := 100 * (cur - prev) / prev
	if pct >= 0 {
		return paint(accent, fmt.Sprintf("▲ %.0f%%", pct)) + dim.Render(" vs "+what)
	}
	return dim.Render(fmt.Sprintf("▼ %.0f%% vs %s", -pct, what))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		m.now = time.Time(msg)
		if m.scr == liveScr && !m.liveBusy && (m.live == nil || m.now.Sub(m.live.at) >= livePoll) {
			m.liveBusy = true
			return m, tea.Batch(tickCmd(), m.loadLive())
		}
		return m, tickCmd()
	case liveMsg:
		m.liveBusy = false
		if msg.gen == m.gen {
			m.live = &msg.d
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		m.light = !msg.IsDark()
		usePalette(m.light)
	case loadedMsg:
		if msg.gen != m.gen { // read with settings that have since changed
			return m, nil
		}
		d := msg.d
		if !d.to.After(startOfDay(time.Now())) {
			m.past[[2]time.Time{d.from, d.to}] = &d
		}
		if d.from.Equal(m.from) && d.to.Equal(m.to) { // drop a slow load for a range we already left
			m.d, m.shown, m.loaded = &d, m.now, true
			m.sel = min(m.sel, len(d.days)-1)
		}
		if m.rec == nil && !m.recBusy { // after the first screen, so it never delays it
			m.recBusy = true
			return m, m.loadRecords()
		}
	case recordsMsg:
		if msg.gen == m.gen {
			m.rec, m.recBusy = &msg.r, false
		}
	case detailMsg:
		if msg.s.File == m.at.File {
			m.detail, m.derr = &msg.s, msg.err
		}
	case tea.MouseWheelMsg:
		if msg.Button == tea.MouseWheelUp {
			return m.key("up")
		}
		return m.key("down")
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			return m.click(msg.X, msg.Y)
		}
	case tea.KeyPressMsg:
		k := msg.String()
		switch {
		case k == "ctrl+c":
			return m, tea.Quit
		case m.pal.open:
			return m.paletteKey(msg)
		case m.gs.open:
			return m.guessKey(msg)
		case m.card:
			if k == "n" {
				m.cardHide = !m.cardHide
			} else {
				m.card = false
			}
			return m, nil
		case m.help:
			if k == "esc" || k == "?" || k == "q" {
				m.help = false
			}
			return m, nil
		case m.typing:
			return m.typeFilter(msg), nil
		case m.assigning:
			return m.typeWhom(msg)
		}
		return m.key(k)
	}
	return m, nil
}

// key handles one key: global keys first, then the current screen's.
func (m model) key(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "q":
		return m, tea.Quit
	case "?":
		m.help = true
		return m, nil
	case ":", "ctrl+k":
		m.pal = paletteState{open: true}
		return m, nil
	case "[":
		return m.shift(-1)
	case "]":
		return m.shift(1)
	case "r":
		return m.setPeriod((m.per + 1) % customP) // day → week → month; a custom range is in the palette
	case "t":
		return m.today()
	case "p":
		if m.d == nil {
			return m.flash("still reading: try again in a moment"), nil
		}
		m.gs = guessState{open: true}
		return m, nil
	case "c":
		if m.d == nil {
			return m.flash("still reading: try again in a moment"), nil
		}
		m.card = true
		return m, nil
	}
	if i, err := strconv.Atoi(k); err == nil && i >= 1 && i <= len(nav) {
		return m.goTo(nav[i-1].scr), nil
	}
	if m.d == nil {
		return m, nil
	}
	switch m.scr {
	case overviewScr:
		return m.overviewKey(k)
	case gapsScr:
		return m.gapsKey(k)
	case settingsScr:
		return m.settingsKey(k)
	case timelineScr:
		return m.timelineKey(k)
	case recordsScr:
		return m.recordsKey(k)
	default:
		return m.listKey(k)
	}
}

func (m model) overviewKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k":
		m.sel = max(0, m.sel-1)
	case "down", "j":
		m.sel = min(m.days()-1, m.sel+1)
	case "left", "h":
		return m.shift(-1)
	case "right", "l":
		return m.shift(1)
	case "enter":
		return m.goTo(projectsScr), nil
	}
	return m, nil
}

func (m model) viewOverview() string {
	d := m.d
	var b strings.Builder
	t := m.now.Sub(time.Time{}).Seconds()
	glow := 0.9 + 0.1*(0.5+0.5*math.Sin(2*math.Pi*t/breathe.Seconds())) // 90–100%: alive, not muddy
	p := math.Min(1, math.Max(0, m.now.Sub(m.shown).Seconds()/reveal.Seconds()))
	wipe := 1 - math.Pow(1-p, 3) // ease-out: fast start, soft landing
	b.WriteString(m.head(m.rangeLabel(), "", m.delta()))
	m.hero(&b)
	b.WriteString("\n")
	if len(d.days) <= 7 {
		m.hourGrid(&b, wipe, glow, t)
	} else {
		m.calendar(&b, wipe, glow)
	}
	// the selected day: agents and you, hour by hour, aligned under the grid
	b.WriteString("\n")
	sel := d.cells[m.sel]
	b.WriteString(bold.Render(fmt.Sprintf("%-*s", gutter, "agents")))
	for j, c := range sel {
		b.WriteString(m.cell(j, 1000+j, c.Run, c.Peak, wipe, glow, t))
	}
	b.WriteString("\n" + bold.Render(fmt.Sprintf("%-*s", gutter, "you")))
	for j, c := range sel {
		b.WriteString(m.cell(j, 2000+j, c.Think, 0, wipe, glow, t))
	}
	day := total(d.days[m.sel])
	fmt.Fprintf(&b, "\n%s  %s with agents · agents %s · you %s · peak %d\n", dim.Render(m.selDay().Format("Mon Jan 2")),
		bold.Render(hdur(day.ActiveHours)), bold.Render(hdur(day.AgentHours)), bold.Render(hdur(day.YouHours)), day.Peak)
	b.WriteString(m.whomLine(d.total) + "\n")
	b.WriteString(paint(voiceHex, voice(*d)) + "\n")
	return b.String() + m.keys("[↑/↓] day", "[←/→] "+periodNames[m.per], "[⏎] projects", "[r] day·week·month")
}

// hero is the three numbers people quote: your hours, your agents' hours and the multiple, and
// how long the agents sat waiting for you.
func (m model) hero(b *strings.Builder) {
	tot := total(m.d.total)
	most := math.Max(tot.YouHours, tot.AgentHours)
	bw := max(8, min(28, m.wide()-36))
	line := func(label string, x float64, hex string) string {
		f := 0.0
		if most > 0 {
			f = x / most
		}
		return fmt.Sprintf("%s %s  %s", bold.Render(fmt.Sprintf("%-7s", label)), bold.Render(rjust(hdur(x), 8)), paint(hex, fit(bar(f, bw), bw)))
	}
	b.WriteString(line("you", tot.YouHours, voiceHex) + "\n")
	mult := ""
	if tot.YouHours > 0.01 {
		mult = "  " + bold.Render(paint(accent, fmt.Sprintf("%.1f×", tot.AgentHours/tot.YouHours))) + dim.Render(" you")
	}
	b.WriteString(line("agents", tot.AgentHours, accent) + mult + "\n")
	peak := ""
	if tot.Peak > 0 {
		peak = fmt.Sprintf(" · peak %s at once, %s", bold.Render(strconv.Itoa(tot.Peak)), tot.PeakAt.Local().Format("Mon 15:04"))
	}
	b.WriteString(dim.Render("agents waited on you ") + bold.Render(hdur(tot.ThinkHours)) + dim.Render(peak) + "\n")
}

// whomLine is who the range's time was for, in each whom's colour.
func (m model) whomLine(r Report) string {
	var parts []string
	for _, row := range r.Rows {
		if row.Project == "*" && row.Whom != "TOTAL" && row.ActiveHours >= 0.05 {
			parts = append(parts, paint(whomColor(m.rules, row.Whom), title(row.Whom))+" "+bold.Render(hdur(row.ActiveHours)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return dim.Render(fmt.Sprintf("%-*s", gutter, "for")) + strings.Join(parts, dim.Render(" · "))
}

// hourGrid is the weekday × hour grid, for ranges up to a week.
func (m model) hourGrid(b *strings.Builder, wipe, glow, t float64) {
	b.WriteString(strings.Repeat(" ", gutter))
	for hr := range 24 { // every hour labelled; 00 06 12 18 stand out so quarters of the day are easy to find
		label := fmt.Sprintf("%02d", hr)
		if hr%6 == 0 {
			b.WriteString(label + " ")
		} else {
			b.WriteString(dim.Render(label) + " ")
		}
	}
	b.WriteString("\n")
	today := startOfDay(time.Now())
	for i, cells := range m.d.cells {
		day := m.from.AddDate(0, 0, i)
		label, marker := day.Format("Mon"), " "
		if i == m.sel {
			marker = paint(accent, "›")
		}
		switch {
		case day.Equal(today):
			label = bold.Render(paint(accent, label))
		case i != m.sel:
			label = dim.Render(label)
		}
		b.WriteString(label + " " + marker + "  ")
		for j, c := range cells {
			b.WriteString(m.cell(j, i*24+j, c.Active, c.Peak, wipe, glow, t))
		}
		b.WriteString("\n")
	}
}

// calendar is a month as weeks × weekdays, one cell per day, for ranges longer than a week.
// Brightness is the day's hours against the busiest day in the range.
func (m model) calendar(b *strings.Builder, wipe, glow float64) {
	first := weekOf(m.from)
	weeks := (nDays(first, m.to) + 6) / 7
	var most time.Duration
	for _, r := range m.d.days {
		most = max(most, time.Duration(total(r).ActiveHours*float64(time.Hour)))
	}
	b.WriteString(strings.Repeat(" ", gutter))
	for w := range weeks {
		b.WriteString(dim.Render(fit(first.AddDate(0, 0, 7*w).Format("Jan 2"), 7)))
	}
	b.WriteString("\n")
	today := startOfDay(time.Now())
	for wd := range 7 {
		label := first.AddDate(0, 0, wd).Format("Mon")
		if wd == weekday(m.selDay()) {
			label += " " + paint(accent, "›") + "  "
		} else {
			label = dim.Render(label) + "    "
		}
		b.WriteString(label)
		for w := range weeks {
			day := first.AddDate(0, 0, 7*w+wd)
			i := nDays(m.from, day)
			switch {
			case day.Before(m.from) || !day.Before(m.to):
				b.WriteString("       ")
				continue
			case float64(w)/float64(weeks) >= wipe && wipe < 1:
				b.WriteString("       ")
				continue
			}
			a := time.Duration(total(m.d.days[i]).ActiveHours * float64(time.Hour))
			cell := paint(ramp[0], "··")
			if l := level(a, most); l > 0 && most > 0 {
				cell = paint(scaleHex(ramp[l], glow), "██")
			}
			tail := "     "
			switch {
			case i == m.sel:
				tail = paint(accent, "◂") + "    "
			case day.Equal(today):
				tail = paint(accent, "•") + "    "
			}
			b.WriteString(cell + tail)
		}
		b.WriteString("\n")
	}
}

// cell draws one hour: a 2-character square and a 1-column gap, so it sits under its label.
// Hours where 3+ agents ran at once burn; each burning cell flickers on its own phase so a hot
// evening shimmers instead of blinking in unison.
func (m model) cell(col, phase int, d time.Duration, peakAgents int, wipe, glow, t float64) string {
	if float64(col)/24 >= wipe && wipe < 1 { // chronological wipe, oldest hour first
		return "   "
	}
	if peakAgents >= 3 && d > 0 {
		hex := fireRamp[min(peakAgents-3, len(fireRamp)-1)]
		return bold.Render(paint(scaleHex(hex, 0.8+0.2*math.Sin(2*math.Pi*1.4*t+float64(phase)*1.1)), "██")) + " "
	}
	if l := level(d, time.Hour); l > 0 {
		return paint(scaleHex(ramp[l], glow), "██") + " "
	}
	return paint(ramp[0], "··") + " "
}

// summary writes a report's numbers in one line, then who they were for.
func (m model) summary(b *strings.Builder, r Row, whoms Report) {
	fmt.Fprintf(b, "%s with agents · agents %s · you %s %s\n", bold.Render(hdur(r.ActiveHours)), bold.Render(hdur(r.AgentHours)),
		bold.Render(hdur(r.YouHours)), dim.Render(fmt.Sprintf("· peak %d", r.Peak)))
	if l := m.whomLine(whoms); l != "" && r.Whom == "TOTAL" {
		b.WriteString(l + "\n")
	}
}

// voice picks the single most telling true fact about the range, like termheat's "The grid is
// cooling…". Every line comes from the numbers on screen; nothing is invented.
func voice(d rangeData) string {
	tot := total(d.total)
	var top Row
	var agentSum float64
	for _, row := range d.total.Rows {
		if row.Project == "*" && row.Whom != "TOTAL" {
			agentSum += row.AgentHours
			if row.AgentHours > top.AgentHours {
				top = row
			}
		}
	}
	if tot.ActiveHours == 0 {
		if demo {
			return "🌱 Nothing logged here yet."
		}
		return "🌱 Nothing logged here yet. `renpet --demo` shows a made-up week."
	}
	if u := whomRow(d.total, "unknown"); agentSum > 0 && u.AgentHours/agentSum >= 0.5 {
		return fmt.Sprintf("❓ %.0f%% of your agent time has no owner yet. Settings asks who it's for.", 100*u.AgentHours/agentSum)
	}
	burst, burstDay, burstHour, nights := 0, 0, 0, 0
	for i, cells := range d.cells {
		late := false
		for j, c := range cells {
			if c.Peak > burst {
				burst, burstDay, burstHour = c.Peak, i, j
			}
			late = late || (j < 5 && c.Active > 0)
		}
		if late {
			nights++
		}
	}
	switch share := top.AgentHours / agentSum; {
	case share >= 0.6:
		return fmt.Sprintf("⚖  %s took %.0f%% of your agent time.", title(top.Whom), 100*share)
	case burst >= 5:
		return fmt.Sprintf("🔥 %d agents ran at once on %s at %02d:00.", burst, d.from.AddDate(0, 0, burstDay).Format("Mon"), burstHour)
	case nights >= 2:
		return fmt.Sprintf("🌙 You or your agents were up past midnight on %d days.", nights)
	default:
		return fmt.Sprintf("🧠 You thought for %.0f%% of your hours with agents; the agents ran the rest.", 100*tot.YouHours/tot.ActiveHours)
	}
}

func runTUI(cfg Config, cfgPath string, rules []Rule, idle time.Duration, day time.Time) error {
	now := time.Now()
	from, to := periodSpan(weekP, day)
	m := model{rules: rules, idle: idle, per: weekP, from: from, to: to, sel: nDays(from, startOfDay(day)),
		past: map[[2]time.Time]*rangeData{}, now: now, shown: now, cfg: cfg, cfgPath: cfgPath}
	_, err := tea.NewProgram(m).Run()
	return err
}
