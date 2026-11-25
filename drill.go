package main

import (
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The drill-down: week → projects → sessions → one session's prompts. Enter (or →) goes one
// level deeper, esc (or ←) one level back, so every number on screen leads to its evidence.

type screen int

const ( // drill-down screens first and in order: esc from one goes to the one before it
	weekScr screen = iota
	projectsScr
	sessionsScr
	detailScr
	gapsScr
	settingsScr
)

var (
	projectOrders = []string{"hours", "name", "whom"}
	sessionOrders = []string{"time", "hours"}
	whomHex       = []string{"#58a6ff", "#ffa657", "#d2a8ff", "#7ee787", "#ff7b72", "#f2cc60", "#79c0ff"}
	unknownHex    = "#8b949e"
)

// whomColor gives each "for whom" one colour on every screen: in the order your rules first
// name them, so the palette stays put as hours change. Whoms no rule names get a hashed colour.
func whomColor(rules []Rule, w string) string {
	if w == "unknown" || w == "" {
		return unknownHex
	}
	var seen []string
	for _, r := range rules {
		if !slices.Contains(seen, r.Whom) {
			seen = append(seen, r.Whom)
		}
		if r.Whom == w {
			return whomHex[(len(seen)-1)%len(whomHex)]
		}
	}
	h := fnv.New32a()
	h.Write([]byte(w))
	return whomHex[h.Sum32()%uint32(len(whomHex))]
}

func title(w string) string {
	if w == "" {
		return w
	}
	return strings.ToUpper(w[:1]) + w[1:]
}

// fit truncates or pads s to exactly n terminal cells.
func fit(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = ansi.Truncate(s, n, "…")
	return s + strings.Repeat(" ", max(0, n-ansi.StringWidth(s)))
}

// fitLeft is fit for paths: it keeps the end, where the repo's name is.
func fitLeft(s string, n int) string {
	if w := ansi.StringWidth(s); w > n && n > 1 {
		s = "…" + ansi.TruncateLeft(s, w-n+1, "")
	}
	return fit(s, n)
}

// short is fitLeft without the padding.
func short(s string, n int) string { return strings.TrimRight(fitLeft(s, n), " ") }

func rjust(s string, n int) string { return strings.Repeat(" ", max(0, n-ansi.StringWidth(s))) + s }

// bar is a horizontal bar w cells wide at full scale, in eighths of a cell.
func bar(f float64, w int) string {
	n := int(math.Round(math.Max(0, math.Min(1, f)) * float64(w*8)))
	s := strings.Repeat("█", n/8)
	if n%8 > 0 {
		s += string([]rune("▏▎▍▌▋▊▉")[n%8-1])
	}
	return s
}

// spark draws how busy each of n equal slices of [from, to) was, from blank to █.
func spark(sp []Span, from, to time.Time, n int) string {
	if n <= 0 || !to.After(from) {
		return strings.Repeat(" ", max(0, n))
	}
	step := max(1, to.Sub(from)/time.Duration(n))
	levels := []rune(" ▁▂▃▄▅▆▇█")
	var b strings.Builder
	for i := range n {
		s := from.Add(time.Duration(i) * step)
		f := float64(unionLen(clip(sp, s, s.Add(step)))) / float64(step)
		b.WriteRune(levels[min(8, int(math.Ceil(f*8)))])
	}
	return b.String()
}

func clip(sp []Span, from, to time.Time) (out []Span) {
	for _, s := range sp {
		if c := (Span{maxT(s.Start, from), minT(s.End, to)}); c.Len() > 0 {
			out = append(out, c)
		}
	}
	return
}

func matches(filter string, fields ...string) bool {
	f := strings.ToLower(filter)
	for _, s := range fields {
		if strings.Contains(strings.ToLower(s), f) {
			return true
		}
	}
	return f == ""
}

// visible returns the rows [lo, hi) of n that fit in room lines with cur in view.
func visible(n, cur, room int) (int, int) {
	room = max(1, room)
	lo := min(max(0, cur-room/2), max(0, n-room))
	return lo, min(n, lo+room)
}

func (m model) wide() int {
	if m.width == 0 {
		return 80
	}
	return m.width
}

// room is how many list rows fit below what b already holds, leaving below lines free.
func (m model) room(b *strings.Builder, below int) int {
	h := m.height
	if h == 0 {
		h = 24
	}
	return h - strings.Count(b.String(), "\n") - below
}

func (m model) scope() (from, to time.Time) {
	if m.whole {
		return m.start, m.start.AddDate(0, 0, 7)
	}
	d := m.start.AddDate(0, 0, m.sel)
	return d, d.AddDate(0, 0, 1)
}

func (m model) scopeLabel() string {
	if m.whole {
		return "week of " + m.start.Format("Jan 2")
	}
	return m.start.AddDate(0, 0, m.sel).Format("Mon Jan 2")
}

func (m model) report() Report {
	if m.whole {
		return m.w.week
	}
	return m.w.days[m.sel]
}

func (m model) open(s screen) model {
	m.scr, m.filter, m.typing = s, "", false
	m.cur[s] = 0
	return m
}

// projects are the scope's projects. Ones under 6 minutes are hidden until you ask ([a]) or
// filter: they are still in every total.
func (m model) projects() (out []Row, hidden int) {
	for _, r := range m.report().Rows {
		switch {
		case r.Project == "*" || !matches(m.filter, r.Project, r.Whom):
		case !m.all && m.filter == "" && r.ActiveHours < 0.1:
			hidden++
		default:
			out = append(out, r) // already by hours, most first
		}
	}
	switch projectOrders[m.order[projectsScr]] {
	case "name":
		sort.SliceStable(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	case "whom":
		sort.SliceStable(out, func(i, j int) bool { return out[i].Whom < out[j].Whom })
	}
	return out, hidden
}

// projRow is the selected project's numbers in the current scope (they change with tab).
func (m model) projRow() Row {
	for _, r := range m.report().Rows {
		if r.Project == m.proj.Project && r.Whom == m.proj.Whom {
			return r
		}
	}
	return Row{Project: m.proj.Project, Whom: m.proj.Whom}
}

func busy(at Attributed) []Span { return append(append([]Span(nil), at.Running...), at.Thinking...) }

func bounds(at Attributed) (first, last time.Time) {
	for i, s := range busy(at) {
		if i == 0 || s.Start.Before(first) {
			first = s.Start
		}
		if s.End.After(last) {
			last = s.End
		}
	}
	return
}

// sessions are the selected project's sessions, cut to the current scope. Subagents are hidden
// until you ask ([a]) or filter: their parent is the session you remember.
func (m model) sessions() (out []Attributed, hidden int) {
	from, to := m.scope()
	for _, at := range m.w.ats {
		if at.Project != m.proj.Project || at.Whom != m.proj.Whom {
			continue
		}
		at.Running, at.Thinking = clip(at.Running, from, to), clip(at.Thinking, from, to)
		switch {
		case len(at.Running)+len(at.Thinking) == 0 || !matches(m.filter, at.Name(), at.Agent):
		case !m.all && m.filter == "" && at.Agent == "claude-sub":
			hidden++
		default:
			out = append(out, at)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if sessionOrders[m.order[sessionsScr]] == "hours" {
			return unionLen(busy(out[i])) > unionLen(busy(out[j]))
		}
		a, _ := bounds(out[i])
		b, _ := bounds(out[j])
		return a.Before(b)
	})
	return out, hidden
}

// promptRow is one prompt you sent: how long you thought before it, how long the agents then
// ran before your next prompt.
type promptRow struct {
	T          time.Time
	Think, Run time.Duration
	Text       string
}

func promptRows(s Session, idle time.Duration) []promptRow {
	ev := s.Events
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].T.Before(ev[j].T) })
	var rows []promptRow
	for i, e := range ev {
		var gap time.Duration
		if i > 0 {
			if gap = e.T.Sub(ev[i-1].T); gap > idle {
				gap = 0
			}
		}
		switch {
		case e.Human:
			rows = append(rows, promptRow{T: e.T, Think: gap, Text: e.text})
		case len(rows) > 0:
			rows[len(rows)-1].Run += gap
		}
	}
	return rows
}

type detailMsg struct {
	s   Session
	err error
}

// readSession re-reads one transcript in full, keeping your prompts' text: the cache only
// keeps the session's name, so the text is read when you open it and never stored.
func readSession(s Session) tea.Cmd {
	return func() tea.Msg {
		full := Session{Agent: s.Agent, File: s.File, Cwd: s.Cwd}
		_, err := parseFrom(s.Agent, s.File, 0, &full)
		return detailMsg{full, err}
	}
}

func (m model) count() int {
	switch m.scr {
	case projectsScr:
		rows, _ := m.projects()
		return len(rows)
	case sessionsScr:
		ss, _ := m.sessions()
		return len(ss)
	case detailScr:
		if m.detail != nil {
			return len(promptRows(*m.detail, m.idle))
		}
	}
	return 0
}

func (m model) listKey(k string) (tea.Model, tea.Cmd) {
	if m.w == nil {
		if k == "esc" {
			m.scr = weekScr
		}
		return m, nil
	}
	cur := &m.cur[m.scr]
	switch k {
	case "esc", "left", "h":
		if m.filter != "" && k == "esc" {
			m.filter, *cur = "", 0
			return m, nil
		}
		m.scr--
		m.filter = ""
		return m, nil
	case "up", "k":
		*cur--
	case "down", "j":
		*cur++
	case "pgup":
		*cur -= 10
	case "pgdown":
		*cur += 10
	case "home":
		*cur = 0
	case "end":
		*cur = m.count() - 1
	case "tab":
		if m.scr != detailScr {
			m.whole, *cur = !m.whole, 0
		}
	case "o":
		switch m.scr {
		case projectsScr:
			m.order[m.scr] = (m.order[m.scr] + 1) % len(projectOrders)
		case sessionsScr:
			m.order[m.scr] = (m.order[m.scr] + 1) % len(sessionOrders)
		}
	case "/":
		if m.scr != detailScr {
			m.typing = true
		}
	case "a":
		m.all, *cur = !m.all, 0
	case "enter", "right", "l":
		rows, _ := m.projects()
		ss, _ := m.sessions()
		switch {
		case m.scr == projectsScr && len(rows) > 0:
			m.proj = rows[min(*cur, len(rows)-1)]
			return m.open(sessionsScr), nil
		case m.scr == sessionsScr && len(ss) > 0:
			m.at, m.detail, m.derr = ss[min(*cur, len(ss)-1)], nil, nil
			return m.open(detailScr), readSession(m.at.Session)
		}
	}
	*cur = max(0, min(*cur, m.count()-1))
	return m, nil
}

func (m model) typeFilter(msg tea.KeyPressMsg) model {
	switch msg.String() {
	case "enter":
		m.typing = false
	case "esc":
		m.typing, m.filter = false, ""
	case "backspace":
		if r := []rune(m.filter); len(r) > 0 {
			m.filter = string(r[:len(r)-1])
		}
	default:
		m.filter += msg.Text
	}
	m.cur[m.scr] = 0
	return m
}

// hiddenLine says what [a] would show: hidden rows still count in every number on screen.
func (m model) hiddenLine(b *strings.Builder, n int, what string) {
	if n > 0 {
		fmt.Fprintf(b, "%s\n", dim.Render(fmt.Sprintf("   + %d %s, in the totals · [a] shows them", n, what)))
	}
}

func (m model) filterLine(b *strings.Builder) {
	switch {
	case m.typing:
		fmt.Fprintf(b, "\n   %s %s%s\n", paint(accent, "/"), m.filter, paint(accent, "▏"))
	case m.filter != "":
		fmt.Fprintf(b, "\n   %s %s %s\n", paint(accent, "/"), m.filter, dim.Render("(esc clears)"))
	}
}

func (m model) viewProjects() string {
	rows, hidden := m.projects()
	var b strings.Builder
	fmt.Fprintf(&b, "\n🌿 %s  %s  %s\n\n", logo(), bold.Render(m.scopeLabel()), dim.Render(fmt.Sprintf("%d projects", len(rows))))
	statsTitle := "This week"
	if !m.whole {
		statsTitle = m.start.AddDate(0, 0, m.sel).Format("Mon Jan 2")
	}
	stats(&b, statsTitle, m.report())
	nameW := max(10, m.wide()-52)
	fmt.Fprintf(&b, "\n   %s %s %s %s %s %s %s\n", dim.Render(fit("for whom", 10)), dim.Render(fit("project", nameW)), fit("", 10),
		dim.Render(rjust("with", 6)), dim.Render(rjust("ran", 6)), dim.Render(rjust("you", 6)), dim.Render(rjust("∥", 3)))
	most := 0.0
	for _, r := range rows {
		most = math.Max(most, r.ActiveHours)
	}
	cur := m.cur[projectsScr]
	lo, hi := visible(len(rows), cur, m.room(&b, 5))
	for i := lo; i < hi; i++ {
		r := rows[i]
		c := whomColor(m.rules, r.Whom)
		mark, name := "   ", fitLeft(r.Project, nameW)
		if i == cur {
			mark, name = paint(accent, " › "), bold.Render(name)
		}
		fmt.Fprintf(&b, "%s%s %s %s %s %s %s %s\n", mark, paint(c, fit(title(r.Whom), 10)), name, paint(c, fit(bar(r.ActiveHours/most, 10), 10)),
			rjust(h(r.ActiveHours)+"h", 6), rjust(h(r.AgentHours)+"h", 6), rjust(h(r.ThinkHours)+"h", 6), rjust(strconv.Itoa(r.Peak), 3))
	}
	if len(rows) == 0 {
		b.WriteString(dim.Render("   nothing here") + "\n")
	}
	m.hiddenLine(&b, hidden, "under 6 min")
	m.filterLine(&b)
	return b.String() + keys("[⏎] open", "[tab] day/week", "[/] filter", "[o] order: "+projectOrders[m.order[projectsScr]], "[a] all", "[esc] back")
}

func (m model) viewSessions() string {
	ss, hidden := m.sessions()
	pr := m.projRow()
	c := whomColor(m.rules, pr.Whom)
	var b strings.Builder
	fmt.Fprintf(&b, "\n🌿 %s  %s\n   %s %s\n\n", logo(), bold.Render(fitLeft(pr.Project, m.wide()-14)), paint(c, title(pr.Whom)),
		dim.Render("· "+m.scopeLabel()+fmt.Sprintf(" · %d sessions", len(ss))))
	fmt.Fprintf(&b, "   %s with agents = %s agents ran + %s you %s\n", bold.Render(h(pr.ActiveHours)+"h"), bold.Render(h(pr.AgentHours)+"h"),
		bold.Render(h(pr.ThinkHours)+"h"), dim.Render(fmt.Sprintf("(overlap counted once) · peak %d", pr.Peak)))
	whatW := max(10, m.wide()-62)
	fmt.Fprintf(&b, "\n   %s %s %s %s %s %s\n", dim.Render(fit("when", 19)), dim.Render(fit("agent", 11)), dim.Render(rjust("ran", 6)),
		dim.Render(rjust("you", 6)), dim.Render(fit("activity", 10)), dim.Render("what"))
	cur := m.cur[sessionsScr]
	lo, hi := visible(len(ss), cur, m.room(&b, 5))
	for i := lo; i < hi; i++ {
		at := ss[i]
		first, last := bounds(at)
		mark, what := "   ", fit(at.Name(), whatW)
		if i == cur {
			mark, what = paint(accent, " › "), bold.Render(what)
		}
		fmt.Fprintf(&b, "%s%s %s %s %s %s %s\n", mark, fit(clock(first, last, m.whole), 19), dim.Render(fit(at.Agent, 11)),
			rjust(h(hours(sum(at.Running)))+"h", 6), rjust(h(hours(sum(at.Thinking)))+"h", 6), paint(c, spark(busy(at), first, last, 10)), what)
	}
	if len(ss) == 0 {
		b.WriteString(dim.Render("   nothing here") + "\n")
	}
	m.hiddenLine(&b, hidden, "subagent sessions")
	m.filterLine(&b)
	return b.String() + keys("[⏎] prompts", "[tab] day/week", "[/] filter", "[o] order: "+sessionOrders[m.order[sessionsScr]], "[a] all", "[esc] back")
}

// clock is a session's time range: "14:02–15:40", with the weekday when the scope is a week or
// the session crosses midnight.
func clock(a, b time.Time, withDay bool) string {
	a, b = a.Local(), b.Local()
	f, g := "15:04", "15:04"
	if withDay {
		f = "Mon 15:04"
	}
	if !startOfDay(a).Equal(startOfDay(b)) {
		f, g = "Mon 15:04", "Mon 15:04"
	}
	return a.Format(f) + "–" + b.Format(g)
}

// minutes shows a duration for a prompt row: – for none, <1m, 12m, 1h05m.
func minutes(d time.Duration) string {
	switch {
	case d <= 0:
		return "–"
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func (m model) viewDetail() string {
	at := m.at
	w := m.wide()
	var b strings.Builder
	fmt.Fprintf(&b, "\n🌿 %s  %s\n   %s %s %s\n", logo(), bold.Render(fit(at.Name(), w-14)), dim.Render(at.Agent+" ·"),
		short(at.Project, w/2), paint(whomColor(m.rules, at.Whom), "· "+title(at.Whom)))
	if m.detail == nil || m.derr != nil {
		msg := "reading transcript…"
		if m.derr != nil {
			msg = "could not read: " + m.derr.Error()
		}
		return b.String() + "\n   " + dim.Render(msg) + "\n" + keys("[esc] back", "[q] quit")
	}
	d := *m.detail
	if len(d.Events) == 0 {
		return b.String() + "\n   " + dim.Render("this transcript is empty now") + "\n" + keys("[esc] back", "[q] quit")
	}
	rows := promptRows(d, m.idle) // sorts d.Events
	first, last := d.Events[0].T, d.Events[len(d.Events)-1].T
	run, think := spans(d, m.idle, first, last)
	fmt.Fprintf(&b, "   %s %s\n   %s\n\n", when(first, last), dim.Render(fmt.Sprintf("· %d prompts · %sh agents ran · %sh you", len(rows),
		h(hours(sum(run))), h(hours(sum(think))))), dim.Render(fitLeft(tilde(d.File), w-4)))
	sw := max(10, w-12)
	fmt.Fprintf(&b, "   %s %s\n   %s %s\n", bold.Render("agents"), paint(accent, spark(run, first, last, sw)),
		bold.Render(fit("you", 6)), paint(voiceHex, spark(think, first, last, sw)))
	textW := max(10, w-27)
	fmt.Fprintf(&b, "\n   %s %s %s %s\n", dim.Render(fit("when", 9)), dim.Render(rjust("you", 5)), dim.Render(rjust("agents", 6)), dim.Render("prompt"))
	cur := min(m.cur[detailScr], max(0, len(rows)-1))
	lo, hi := visible(len(rows), cur, m.room(&b, 10))
	for i := lo; i < hi; i++ {
		r := rows[i]
		mark, text := "   ", fit(r.Text, textW)
		if i == cur {
			mark, text = paint(accent, " › "), bold.Render(text)
		}
		fmt.Fprintf(&b, "%s%s %s %s %s\n", mark, r.T.Local().Format("Mon 15:04"), rjust(minutes(r.Think), 5), rjust(minutes(r.Run), 6), text)
	}
	if len(rows) == 0 {
		b.WriteString(dim.Render("   no prompts from you: an agent ran this on its own") + "\n")
	} else { // the selected prompt in full, up to five lines
		lines := strings.Split(lipgloss.NewStyle().Width(w-8).Render(rows[cur].Text), "\n")
		if len(lines) > 5 {
			lines = append(lines[:5], "…")
		}
		box := lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(lipgloss.Color(accent)).PaddingLeft(1)
		b.WriteString("\n" + lipgloss.NewStyle().MarginLeft(3).Render(box.Render(strings.Join(lines, "\n"))) + "\n")
	}
	return b.String() + keys("[↑/↓] prompt", "[esc] back", "[q] quit")
}
