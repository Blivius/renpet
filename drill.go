package main

import (
	"fmt"
	"hash/fnv"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The drill-down: overview → projects → sessions → one session's prompts. Enter (or →) goes one
// level deeper, esc (or ←) one level back, so every number on screen leads to its evidence.

type screen int

const (
	overviewScr screen = iota
	projectsScr
	sessionsScr
	detailScr
	gapsScr
	settingsScr
	timelineScr
	recordsScr
)

var (
	projectOrders = []string{"hours", "name", "whom"}
	sessionOrders = []string{"time", "hours"}
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

// projLabel is a project as its repo's name, then where it lives, dimmed: names are what you scan
// for, and the path only tells two of the same name apart. Exactly w cells.
func projLabel(p string, w int, sel bool) string {
	name := filepath.Base(p)
	if p == "" || strings.HasPrefix(p, "(") || name == p {
		return fit(p, w)
	}
	if sel {
		name = bold.Render(name)
	}
	rest := w - ansi.StringWidth(name) - 1
	if rest < 6 {
		return fit(name, w)
	}
	return name + " " + dim.Render(fitLeft(filepath.Dir(p)+"/", rest))
}

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

// room is how many list rows fit below what b already holds, leaving below lines free.
func (m model) room(b *strings.Builder, below int) int {
	h := m.height
	if h == 0 {
		h = minH
	}
	if !m.side() {
		h-- // the tab bar
	}
	return h - strings.Count(b.String(), "\n") - below
}

func (m model) scope() (from, to time.Time) {
	if m.whole {
		return m.from, m.to
	}
	d := m.selDay()
	return d, d.AddDate(0, 0, 1)
}

func (m model) scopeLabel() string {
	if m.whole || m.days() == 1 {
		return m.rangeLabel()
	}
	return m.selDay().Format("Mon Jan 2")
}

func (m model) report() Report {
	if m.whole {
		return m.d.total
	}
	return m.d.days[m.sel]
}

func (m model) open(s screen) model {
	m.scr, m.filter, m.typing = s, "", false
	m.cur[s] = 0
	return m
}

// projects are the scope's projects. Ones under 6 minutes are hidden, and automation folders are
// one row, until you ask ([a]) or filter: they are still in every total.
func (m model) projects() (out []Row, hidden int) {
	r := m.report()
	folders := 0
	for _, row := range r.Rows {
		if row.Project != "*" && row.Whom == "automation" {
			folders++
		}
	}
	fold := !m.all && m.filter == "" && folders > 1
	for _, row := range r.Rows {
		switch {
		case row.Project == "*" || !matches(m.filter, row.Project, row.Whom):
		case fold && row.Whom == "automation":
		case !m.all && m.filter == "" && row.ActiveHours < 0.1:
			hidden++
		default:
			out = append(out, row) // already by hours, most first
		}
	}
	if fold {
		auto := whomRow(r, "automation")
		auto.Sessions = make([]SessionLine, folders) // only its length is read: the folder count
		i := sort.Search(len(out), func(i int) bool { return out[i].ActiveHours < auto.ActiveHours })
		out = slices.Insert(out, i, auto)
	}
	switch projectOrders[m.order[projectsScr]] {
	case "name":
		sort.SliceStable(out, func(i, j int) bool { return filepath.Base(out[i].Project) < filepath.Base(out[j].Project) })
	case "whom":
		sort.SliceStable(out, func(i, j int) bool { return out[i].Whom < out[j].Whom })
	}
	return out, hidden
}

// projRow is the selected project's numbers in the current scope (they change with tab).
func (m model) projRow() Row {
	r := m.report()
	switch m.proj.Project {
	case "":
		return total(r)
	case "*":
		return whomRow(r, m.proj.Whom)
	}
	for _, row := range r.Rows {
		if row.Project == m.proj.Project && row.Whom == m.proj.Whom {
			return row
		}
	}
	return Row{Project: m.proj.Project, Whom: m.proj.Whom}
}

// inProj is whether a session belongs to the project being looked at.
func (m model) inProj(at Attributed) bool {
	switch m.proj.Project {
	case "":
		return true
	case "*":
		return at.Whom == m.proj.Whom
	}
	return at.Project == m.proj.Project && at.Whom == m.proj.Whom
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

// sessRow is a session as listed: its subagents are folded into it.
type sessRow struct {
	Attributed
	subs    int
	subRun  time.Duration
	subSpan []Span // the folded subagents' running spans: the timeline draws them in this lane
}

// fold lists sessions with each subagent folded into the session that started it (its parent
// is the session you remember), or every one on its own when all is set.
func fold(ats []Attributed, all bool) (out []sessRow, folded int) {
	var subs []Attributed
	for _, at := range ats {
		if at.Agent == "claude-sub" && !all {
			subs = append(subs, at)
		} else {
			out = append(out, sessRow{Attributed: at})
		}
	}
	parent := map[string]int{}
	for i, r := range out {
		if _, ok := parent[r.File]; !ok {
			parent[r.File] = i
		}
	}
	for _, s := range subs { // <parent>/subagents/agent-x.jsonl belongs to <parent>.jsonl
		if i, ok := parent[filepath.Dir(filepath.Dir(s.File))+".jsonl"]; ok {
			out[i].subs++
			out[i].subRun += sum(s.Running)
			out[i].subSpan = append(out[i].subSpan, s.Running...)
			folded++
		} else {
			out = append(out, sessRow{Attributed: s})
		}
	}
	return out, folded
}

// sessions are the project's sessions, cut to the current scope, subagents folded until [a].
func (m model) sessions() (out []sessRow, folded int) {
	from, to := m.scope()
	var in []Attributed
	for _, at := range clipAts(m.d.ats, from, to) {
		if m.inProj(at) && matches(m.filter, at.Name(), at.Agent, at.Project) {
			in = append(in, at)
		}
	}
	out, folded = fold(in, m.all)
	sort.SliceStable(out, func(i, j int) bool {
		if sessionOrders[m.order[sessionsScr]] == "hours" {
			return unionLen(busy(out[i].Attributed))+out[i].subRun > unionLen(busy(out[j].Attributed))+out[j].subRun
		}
		a, _ := bounds(out[i].Attributed)
		b, _ := bounds(out[j].Attributed)
		return a.Before(b)
	})
	return out, folded
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
	case timelineScr:
		ls, _ := m.lanes()
		return len(ls)
	case recordsScr:
		if m.rec != nil {
			return len(m.rec.list)
		}
	}
	return 0
}

func (m model) listKey(k string) (tea.Model, tea.Cmd) {
	cur := &m.cur[m.scr]
	switch k {
	case "esc", "left", "h":
		if m.filter != "" && k == "esc" {
			m.filter, *cur = "", 0
			return m, nil
		}
		return m.back(), nil
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
		if m.scr == projectsScr || m.scr == sessionsScr {
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
		switch m.scr {
		case projectsScr:
			if rows, _ := m.projects(); len(rows) > 0 {
				m.proj = rows[min(*cur, len(rows)-1)]
				return m.open(sessionsScr), nil
			}
		case sessionsScr, timelineScr:
			ss, _ := m.sessions()
			if m.scr == timelineScr {
				ss, _ = m.lanes()
				if len(ss) > 0 && ss[min(*cur, len(ss)-1)].Agent == "*" { // the automation lane: its sessions that day
					m.proj, m.whole = Row{Project: "*", Whom: "automation"}, false
					return m.open(sessionsScr), nil
				}
			}
			if len(ss) > 0 {
				m.at, m.detail, m.derr, m.ret = ss[min(*cur, len(ss)-1)].Attributed, nil, nil, m.scr
				return m.open(detailScr), readSession(m.at.Session)
			}
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
func (m model) hiddenLine(b *strings.Builder, s string) {
	if s != "" {
		fmt.Fprintf(b, "%s\n", dim.Render("   "+s+" · [a] shows them"))
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
	b.WriteString(m.head("Projects", m.scopeLabel()+fmt.Sprintf(" · %d", len(rows)), ""))
	m.summary(&b, total(m.report()), m.report())
	nameW := max(16, m.wide()-56)
	fmt.Fprintf(&b, "\n   %s %s %s %s %s %s %s\n", dim.Render(fit("for whom", 10)), dim.Render(fit("project", nameW)), fit("", 10),
		dim.Render(rjust("with", 7)), dim.Render(rjust("agents", 7)), dim.Render(rjust("you", 7)), dim.Render(rjust("∥", 3)))
	most := 0.0
	for _, r := range rows {
		most = math.Max(most, r.ActiveHours)
	}
	cur := m.cur[projectsScr]
	lo, hi := visible(len(rows), cur, m.room(&b, 5))
	for i := lo; i < hi; i++ {
		r := rows[i]
		c := whomColor(m.rules, r.Whom)
		mark, name := "   ", projLabel(r.Project, nameW, i == cur)
		if r.Project == "*" {
			name = fit(fmt.Sprintf("%d folders", len(r.Sessions)), nameW)
		}
		if i == cur {
			mark = paint(accent, " › ")
		}
		fmt.Fprintf(&b, "%s%s %s %s %s %s %s %s\n", mark, paint(c, fit(title(r.Whom), 10)), name, paint(c, fit(bar(r.ActiveHours/most, 10), 10)),
			rjust(hdur(r.ActiveHours), 7), rjust(hdur(r.AgentHours), 7), rjust(hdur(r.YouHours), 7), rjust(strconv.Itoa(r.Peak), 3))
	}
	if len(rows) == 0 {
		b.WriteString(dim.Render("   nothing here") + "\n")
	}
	if hidden > 0 {
		m.hiddenLine(&b, fmt.Sprintf("+ %d under 6 min, in the totals", hidden))
	}
	m.filterLine(&b)
	return b.String() + m.keys("[⏎] sessions", "[tab] day/"+periodNames[m.per], "[/] filter", "[o] "+projectOrders[m.order[projectsScr]], "[a] all")
}

func (m model) viewSessions() string {
	ss, folded := m.sessions()
	pr := m.projRow()
	var b strings.Builder
	switch m.proj.Project {
	case "":
		b.WriteString(m.head("Sessions", m.scopeLabel()+fmt.Sprintf(" · %d", len(ss)), ""))
	case "*":
		b.WriteString(m.head(title(pr.Whom)+" folders", m.scopeLabel()+fmt.Sprintf(" · %d sessions", len(ss)), ""))
	default:
		b.WriteString(m.head(filepath.Base(pr.Project), short(filepath.Dir(pr.Project), m.wide()/2)+" · "+m.scopeLabel()+fmt.Sprintf(" · %d sessions", len(ss)),
			paint(whomColor(m.rules, pr.Whom), title(pr.Whom))))
	}
	m.summary(&b, pr, m.report())
	whatW := max(10, m.wide()-61)
	fmt.Fprintf(&b, "\n   %s %s %s %s %s %s\n", dim.Render(fit("when", 19)), dim.Render(fit("agent", 8)), dim.Render(rjust("agents", 7)),
		dim.Render(rjust("you", 7)), dim.Render(fit("activity", 10)), dim.Render("what"))
	cur := m.cur[sessionsScr]
	lo, hi := visible(len(ss), cur, m.room(&b, 5))
	for i := lo; i < hi; i++ {
		s := ss[i]
		first, last := bounds(s.Attributed)
		what := s.Name()
		if m.proj.Project == "" || m.proj.Project == "*" {
			what += dim.Render(" · " + filepath.Base(s.Project))
		}
		if s.subs > 0 {
			what += dim.Render(fmt.Sprintf(" +%d subagents", s.subs))
		}
		mark, what := "   ", fit(what, whatW)
		if i == cur {
			mark, what = paint(accent, " › "), bold.Render(what)
		}
		fmt.Fprintf(&b, "%s%s %s %s %s %s %s\n", mark, fit(clock(first, last, m.whole), 19), dim.Render(fit(s.Agent, 8)),
			rjust(dur(sum(s.Running)+s.subRun), 7), rjust(dur(sum(s.Thinking)), 7), paint(whomColor(m.rules, s.Whom), spark(busy(s.Attributed), first, last, 10)), what)
	}
	if len(ss) == 0 {
		b.WriteString(dim.Render("   nothing here") + "\n")
	}
	if folded > 0 {
		m.hiddenLine(&b, fmt.Sprintf("%d subagent sessions folded into the sessions that started them", folded))
	}
	m.filterLine(&b)
	return b.String() + m.keys("[⏎] prompts", "[tab] day/"+periodNames[m.per], "[/] filter", "[o] "+sessionOrders[m.order[sessionsScr]], "[a] all")
}

// clock is a session's time range: "14:02–15:40", with the weekday when the scope is a range or
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

func (m model) viewDetail() string {
	at := m.at
	w := m.wide()
	var b strings.Builder
	b.WriteString(m.head(fit(at.Name(), w-16), "", paint(whomColor(m.rules, at.Whom), title(at.Whom))))
	if m.detail == nil || m.derr != nil {
		msg := "reading transcript…"
		if m.derr != nil {
			msg = "could not read: " + m.derr.Error()
		}
		return b.String() + dim.Render(at.Agent+" · "+at.Project) + "\n\n   " + dim.Render(msg) + "\n" + m.keys("[esc] back")
	}
	d := *m.detail
	if len(d.Events) == 0 {
		return b.String() + "   " + dim.Render("this transcript is empty now") + "\n" + m.keys("[esc] back")
	}
	rows := promptRows(d, m.idle) // sorts d.Events
	first, last := d.Events[0].T, d.Events[len(d.Events)-1].T
	run, think := spans(d, m.idle, first, last)
	fmt.Fprintf(&b, "%s %s\n%s\n\n", when(first, last), dim.Render(fmt.Sprintf("· %s · %d prompts · agents %s · you %s", at.Agent, len(rows),
		dur(sum(run)), dur(sum(think)))), dim.Render(fitLeft(tilde(d.File), w-1)))
	sw := max(10, w-8)
	fmt.Fprintf(&b, "%s %s\n%s %s\n", bold.Render("agents"), paint(accent, spark(run, first, last, sw)),
		bold.Render(fit("you", 6)), paint(voiceHex, spark(think, first, last, sw)))
	textW := max(10, w-30)
	fmt.Fprintf(&b, "\n   %s %s %s %s\n", dim.Render(fit("when", 9)), dim.Render(rjust("you", 7)), dim.Render(rjust("agents", 7)), dim.Render("prompt"))
	cur := min(m.cur[detailScr], max(0, len(rows)-1))
	lo, hi := visible(len(rows), cur, m.room(&b, 10))
	for i := lo; i < hi; i++ {
		r := rows[i]
		mark, text := "   ", fit(r.Text, textW)
		if i == cur {
			mark, text = paint(accent, " › "), bold.Render(text)
		}
		fmt.Fprintf(&b, "%s%s %s %s %s\n", mark, r.T.Local().Format("Mon 15:04"), rjust(dur(r.Think), 7), rjust(dur(r.Run), 7), text)
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
	return b.String() + m.keys("[↑/↓] prompt", "[esc] back")
}
