package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The timeline: one day, one lane per session, so parallel agents read as parallel lines. A lane
// is ━ while its agent works, ─ while it waits on you, and ticked (┿ │) where you sent a prompt. The ∥ row
// on top counts the agents working at once, subagents included.

// lanes are the selected day's sessions by when they started. Until [a] or a filter, subagents
// ride in their parent's lane, automation shares one lane, and sessions under two minutes are
// hidden: they still count in every number.
func (m model) lanes() (out []sessRow, hidden int) {
	day := m.selDay()
	var in []Attributed
	auto := Attributed{Session: Session{Agent: "*"}, Project: "automation", Whom: "automation"}
	n := 0
	for _, at := range clipAts(m.d.ats, day, day.AddDate(0, 0, 1)) {
		switch {
		case !matches(m.filter, at.Name(), at.Agent, at.Project):
		case m.all || m.filter != "":
			in = append(in, at)
		case at.Whom == "automation":
			auto.Running, auto.Thinking = append(auto.Running, at.Running...), append(auto.Thinking, at.Thinking...)
			n++
		default:
			in = append(in, at)
		}
	}
	all, _ := fold(in, m.all)
	for _, l := range all {
		if m.all || m.filter != "" || unionLen(busy(l.Attributed))+l.subRun >= 2*time.Minute {
			out = append(out, l)
		} else {
			hidden++
		}
	}
	if n > 0 {
		auto.Prompt = plural(n, "automation session")
		out = append(out, sessRow{Attributed: auto})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := bounds(out[i].Attributed)
		b, _ := bounds(out[j].Attributed)
		return a.Before(b)
	})
	return out, hidden
}

// window is the whole hours the day's lanes cover, at least three.
func window(day time.Time, ls []sessRow) (t0, t1 time.Time) {
	t0, t1 = day.AddDate(0, 0, 1), day
	for _, l := range ls {
		for _, s := range append(busy(l.Attributed), l.subSpan...) {
			t0, t1 = minT(t0, s.Start), maxT(t1, s.End)
		}
	}
	if !t0.Before(t1) {
		return day.Add(9 * time.Hour), day.Add(18 * time.Hour)
	}
	t0 = day.Add(time.Duration(int(t0.Sub(day).Hours())) * time.Hour)
	t1 = day.Add(time.Duration(min(24, int(t1.Sub(day).Hours()+0.999))) * time.Hour)
	if t1.Sub(t0) < 3*time.Hour {
		t1 = minT(day.AddDate(0, 0, 1), t0.Add(3*time.Hour))
		t0 = t1.Add(-3 * time.Hour)
	}
	return t0, t1
}

func overlaps(sp []Span, a, b time.Time) bool {
	for _, s := range sp {
		if s.Start.Before(b) && s.End.After(a) {
			return true
		}
	}
	return false
}

// lanePrompts are your prompts inside the lane's own time: a session that moved repos is one
// lane per repo, and each shows only the prompts it holds.
func lanePrompts(l sessRow) (out []time.Time) {
	first, last := bounds(l.Attributed)
	for _, e := range l.Events {
		if e.Human && !e.T.Before(first) && !e.T.After(last) {
			out = append(out, e.T)
		}
	}
	return out
}

// lane draws one session across n columns from t0, step apart.
func (m model) lane(l sessRow, t0 time.Time, step time.Duration, n int) string {
	run := append(append([]Span(nil), l.Running...), l.subSpan...)
	prompts := lanePrompts(l)
	c := whomColor(m.rules, l.Whom)
	var b strings.Builder
	for i := range n {
		a := t0.Add(time.Duration(i) * step)
		z := a.Add(step)
		prompt := false
		for _, p := range prompts {
			if !p.Before(a) && p.Before(z) {
				prompt = true
				break
			}
		}
		running := overlaps(run, a, z)
		switch {
		case prompt && running:
			b.WriteString(paint(voiceHex, "┿"))
		case prompt:
			b.WriteString(paint(voiceHex, "│"))
		case running:
			b.WriteString(paint(c, "━"))
		case overlaps(l.Thinking, a, z):
			b.WriteString(paint(scaleHex(voiceHex, 0.6), "─"))
		default:
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func (m model) viewTimeline() string {
	day := m.selDay()
	ls, hidden := m.lanes()
	var b strings.Builder
	b.WriteString(m.head("Timeline", day.Format("Mon Jan 2")+fmt.Sprintf(" · %d sessions", len(ls)), ""))
	keys := m.keys("[↑/↓] session", "[←/→] day", "[⏎] prompts", "[/] filter", "[a] all")
	if len(ls) == 0 {
		b.WriteString(dim.Render("   no sessions this day: [←/→] another day") + "\n")
		m.filterLine(&b)
		return b.String() + keys
	}
	labelW := min(30, max(18, m.wide()/4))
	n := m.wide() - 3 - labelW - 1
	t0, t1 := window(day, ls)
	step := t1.Sub(t0) / time.Duration(n)

	// the hour ruler: every hour that has room for its label
	ruler := []rune(strings.Repeat(" ", n))
	every := 1
	for float64(every)*float64(time.Hour)/float64(step) < 3 {
		every++
	}
	for h := t0; !h.After(t1); h = h.Add(time.Hour) {
		col := int(h.Sub(t0) / step)
		if h.Hour()%every == 0 && col+2 <= n {
			copy(ruler[col:], []rune(fmt.Sprintf("%02d", h.Hour())))
		}
	}
	fmt.Fprintf(&b, "   %s %s\n", strings.Repeat(" ", labelW), dim.Render(string(ruler)))

	// ∥: how many agents worked at once in each column
	var all []Span
	for _, l := range ls {
		all = append(append(all, l.Running...), l.subSpan...)
	}
	var par strings.Builder
	for i := range n {
		a := t0.Add(time.Duration(i) * step)
		k := peak(clip(all, a, a.Add(step)))
		switch {
		case k == 0:
			par.WriteByte(' ')
		case k >= 3:
			par.WriteString(bold.Render(paint(fireRamp[min(k-3, len(fireRamp)-1)], strconv.Itoa(min(k, 9)))))
		default:
			par.WriteString(paint(ramp[2+k], strconv.Itoa(k)))
		}
	}
	fmt.Fprintf(&b, "   %s %s\n", dim.Render(rjust("∥ at once", labelW)), par.String())

	cur := min(m.cur[timelineScr], len(ls)-1)
	lo, hi := visible(len(ls), cur, m.room(&b, 6+min(1, hidden)))
	for i := lo; i < hi; i++ {
		l := ls[i]
		proj := filepath.Base(l.Project)
		label := fit(proj, 10) + " " + fit(l.Name(), labelW-11)
		mark := "   "
		if i == cur {
			mark, label = paint(accent, " › "), bold.Render(label)
		} else {
			label = dim.Render(fit(proj, 10)) + " " + fit(l.Name(), labelW-11)
		}
		b.WriteString(mark + label + " " + m.lane(l, t0, step, n) + "\n")
	}
	s := ls[cur]
	prompts := len(lanePrompts(s))
	first, last := bounds(s.Attributed)
	info := fmt.Sprintf("%s · %s · agents %s · you %s · %s", clock(first, last, false), s.Agent, dur(sum(s.Running)+s.subRun), dur(sum(s.Thinking)), plural(prompts, "prompt"))
	if s.Agent == "*" {
		info = fmt.Sprintf("%s · %s · agents %s · ⏎ lists them", clock(first, last, false), s.Name(), dur(sum(s.Running)))
	}
	if s.subs > 0 {
		info += fmt.Sprintf(" · +%d subagents", s.subs)
	}
	if hidden > 0 {
		m.hiddenLine(&b, fmt.Sprintf("+ %d under 2 min, in the totals", hidden))
	}
	fmt.Fprintf(&b, "\n   %s %s\n", paint(whomColor(m.rules, s.Whom), title(s.Whom)), dim.Render(fit(info, m.wide()-5-len(title(s.Whom)))))
	b.WriteString("   " + paint(accent, "━") + dim.Render(" agent working  ") + paint(scaleHex(voiceHex, 0.6), "─") +
		dim.Render(" waiting on you  ") + paint(voiceHex, "┿") + dim.Render(" your prompt") + "\n")
	m.filterLine(&b)
	return b.String() + keys
}

// pickDay selects day, moving the range to the period holding it when it is outside.
func (m model) pickDay(day time.Time) (model, tea.Cmd) {
	day = startOfDay(day)
	if day.After(time.Now()) {
		return m, nil
	}
	m.cur[timelineScr] = 0
	if !day.Before(m.from) && day.Before(m.to) {
		m.sel = nDays(m.from, day)
		return m, nil
	}
	if m.per == customP {
		m.per = weekP
	}
	f, t := periodSpan(m.per, day)
	return m.setRange(f, t, day)
}

func (m model) timelineKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "left", "h":
		return m.pickDay(m.selDay().AddDate(0, 0, -1))
	case "right", "l":
		return m.pickDay(m.selDay().AddDate(0, 0, 1))
	}
	return m.listKey(k)
}
