package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Records are your bests across every transcript still on disk, and the perception check is
// you guessing a range's hours before you look. Both read only what the other screens read.

type record struct {
	name, value, note string
	day               time.Time // where ⏎ goes: the timeline of the day it happened
}

type recordsData struct {
	list        []record
	oldest      time.Time
	hours       float64 // with agents, all of it
	n           int     // sessions
	cleanupDays int     // cached from ~/.claude/settings.json when records load
}

type recordsMsg struct {
	r   recordsData
	gen int
}

// loadRecords reads every transcript from the last year in the background.
func (m model) loadRecords() tea.Cmd {
	rules, idle, gen := m.rules, m.idle, m.gen
	return func() tea.Msg {
		now := time.Now()
		from := now.AddDate(-1, 0, 0)
		return recordsMsg{records(attribute(load(from, now), rules, idle, from, now), now), gen}
	}
}

// eachDay cuts a span at local midnights.
func eachDay(s Span, fn func(day time.Time, s Span)) {
	for d := startOfDay(s.Start.Local()); d.Before(s.End); d = d.AddDate(0, 0, 1) {
		if c := (Span{maxT(s.Start, d), minT(s.End, d.AddDate(0, 0, 1))}); c.Len() > 0 {
			fn(d, c)
		}
	}
}

// records finds the bests in ats. Every one names the day it happened, so it leads to evidence.
func records(ats []Attributed, now time.Time) (r recordsData) {
	type dayAcc struct{ run, think []Span }
	days := map[time.Time]*dayAcc{}
	var all, busyAll []Span
	var longest Span
	var longestAt Attributed
	var late time.Time
	r.oldest = now
	for _, at := range ats {
		all = append(all, at.Running...)
		busyAll = append(busyAll, busy(at)...)
		// a run without you: agent spans that touch, unbroken by a prompt or an idle gap
		var cur Span
		for _, s := range at.Running {
			if s.Start.Equal(cur.End) {
				cur.End = s.End
			} else {
				cur = s
			}
			if cur.Len() > longest.Len() {
				longest, longestAt = cur, at
			}
		}
		for _, s := range busy(at) {
			r.oldest = minT(r.oldest, s.Start)
			if t := s.End.Local(); t.Hour() < 5 && (late.IsZero() || clockOf(t) > clockOf(late)) {
				late = t
			}
		}
		for k, sp := range [][]Span{at.Running, at.Thinking} {
			for _, s := range sp {
				eachDay(s, func(d time.Time, c Span) {
					if days[d] == nil {
						days[d] = &dayAcc{}
					}
					if k == 0 {
						days[d].run = append(days[d].run, c)
					} else {
						days[d].think = append(days[d].think, c)
					}
				})
			}
		}
	}
	r.n = len(ats)
	r.hours = hours(unionLen(busyAll))
	r.cleanupDays = claudeCleanupDays()
	if len(days) == 0 {
		return r
	}
	if n, at := peakAt(all); n > 0 {
		r.list = append(r.list, record{"Most agents at once", fmt.Sprintf("%d", n), at.Local().Format("Mon Jan 2 15:04"), at})
	}
	if longest.Len() > 0 {
		r.list = append(r.list, record{"Longest run without you", dur(longest.Len()),
			longest.Start.Local().Format("Mon Jan 2 15:04") + " · " + longestAt.Name(), longest.Start})
	}
	if !late.IsZero() {
		r.list = append(r.list, record{"Latest night", late.Format("15:04"), "the night into " + late.Format("Mon Jan 2"), late})
	}
	var ds []time.Time
	for d := range days {
		ds = append(ds, d)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].Before(ds[j]) })
	var busiest, multDay time.Time
	var most time.Duration
	mult := 0.0
	for _, d := range ds {
		a := days[d]
		if act := unionLen(append(append([]Span(nil), a.run...), a.think...)); act > most {
			busiest, most = d, act
		}
		if you := unionLen(a.think); you >= 30*time.Minute {
			if x := float64(sum(a.run)) / float64(you); x > mult {
				mult, multDay = x, d
			}
		}
	}
	r.list = append(r.list, record{"Busiest day", dur(most), busiest.Format("Mon Jan 2") + " · with agents", busiest})
	if mult > 0 {
		a := days[multDay]
		r.list = append(r.list, record{"Biggest multiple", fmt.Sprintf("%.1f×", mult),
			fmt.Sprintf("%s · agents %s, you %s", multDay.Format("Mon Jan 2"), dur(sum(a.run)), dur(unionLen(a.think))), multDay})
	}
	// streaks: days in a row with at least ten minutes with agents
	var best, run int
	var bestEnd, runStart time.Time
	for i, d := range ds {
		a := days[d]
		if unionLen(append(append([]Span(nil), a.run...), a.think...)) < 10*time.Minute {
			run = 0
			continue
		}
		if run > 0 && i > 0 && ds[i-1].AddDate(0, 0, 1).Equal(d) {
			run++
		} else {
			run, runStart = 1, d
		}
		if run > best {
			best, bestEnd = run, d
		}
	}
	current := run > 0 && !ds[len(ds)-1].Before(startOfDay(now).AddDate(0, 0, -1)) // still going if today or yesterday counts
	if best > 1 && !(current && run == best) {
		r.list = append(r.list, record{"Longest streak", plural(best, "day"),
			bestEnd.AddDate(0, 0, 1-best).Format("Jan 2") + " – " + bestEnd.Format("Jan 2"), bestEnd})
	}
	if current {
		note := "since " + runStart.Format("Mon Jan 2")
		if run == best && run > 1 {
			note += " · your longest"
		}
		r.list = append(r.list, record{"Current streak", plural(run, "day"), note, ds[len(ds)-1]})
	}
	return r
}

const defaultClaudeCleanupDays = 30

// claudeCleanupDays reads cleanupPeriodDays from ~/.claude/settings.json,
// returning defaultClaudeCleanupDays when the file is missing, unreadable,
// unparseable, or omits a positive integer setting.
func claudeCleanupDays() int {
	home, err := os.UserHomeDir()
	if err != nil {
		return defaultClaudeCleanupDays
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return defaultClaudeCleanupDays
	}
	var cfg struct {
		CleanupPeriodDays int `json:"cleanupPeriodDays"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.CleanupPeriodDays <= 0 {
		return defaultClaudeCleanupDays
	}
	return cfg.CleanupPeriodDays
}

// clockOf is minutes past midnight.
func clockOf(t time.Time) int { return t.Hour()*60 + t.Minute() }

func plural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return strconv.Itoa(n) + " " + w + "s"
}

func (m model) viewRecords() string {
	var b strings.Builder
	if m.rec == nil {
		b.WriteString(m.head("Records", "every transcript on this machine", ""))
		b.WriteString(paint(accent, "   reading a year of transcripts…") + "\n")
		return b.String() + m.keys("[p] guess your hours")
	}
	r := m.rec
	b.WriteString(m.head("Records", "since "+r.oldest.Local().Format("Jan 2, 2006"), ""))
	fmt.Fprintf(&b, "%s with agents across %s\n", bold.Render(hdur(r.hours)), bold.Render(plural(r.n, "session")))
	if r.cleanupDays > 0 && r.cleanupDays < 90 && time.Since(r.oldest) < 35*24*time.Hour {
		msg := fmt.Sprintf("Claude Code keeps %d days of transcripts (cleanupPeriodDays); raise it to keep your history", r.cleanupDays)
		b.WriteString(dim.Render(fit(msg, m.wide())) + "\n")
	}
	b.WriteString("\n")
	cur := min(m.cur[recordsScr], max(0, len(r.list)-1))
	for i, rec := range r.list {
		mark, name := "   ", fit(rec.name, 24)
		if i == cur {
			mark, name = paint(accent, " › "), bold.Render(name)
		}
		fmt.Fprintf(&b, "%s%s %s  %s\n", mark, name, bold.Render(paint(accent, rjust(rec.value, 8))), dim.Render(fit(rec.note, m.wide()-39)))
	}
	if len(r.list) == 0 {
		b.WriteString(dim.Render("   nothing logged yet") + "\n")
	}
	gs := readGuesses(guessPath(m.cfgPath))
	b.WriteString("\n" + bold.Render("Perception check") + dim.Render("  [p] guess this "+periodNames[m.per]+"'s hours before you look") + "\n")
	if len(gs) == 0 {
		b.WriteString(dim.Render(fit("   no guesses yet. Developers misjudge their time with AI; how far off are you?", m.wide())) + "\n")
	}
	for i := max(0, len(gs)-min(4, m.room(&b, 4))); i < len(gs); i++ {
		g := gs[i]
		fmt.Fprintf(&b, "   %s guessed %s  measured %s  %s\n", fit(g.label(), 18), bold.Render(rjust(g.hours(), 7)),
			bold.Render(rjust(hdur(g.Measured), 7)), dim.Render(fit(guessVerdict(g.Guess, g.Measured), m.wide()-50)))
	}
	return b.String() + m.keys("[↑/↓] record", "[⏎] that day's timeline", "[p] guess")
}

func (m model) recordsKey(k string) (tea.Model, tea.Cmd) {
	if m.rec == nil {
		return m, nil
	}
	cur := &m.cur[recordsScr]
	switch k {
	case "up", "k":
		*cur = max(0, *cur-1)
	case "down", "j":
		*cur = min(len(m.rec.list)-1, *cur+1)
	case "esc":
		return m.back(), nil
	case "enter", "right", "l":
		if len(m.rec.list) == 0 {
			return m, nil
		}
		m, cmd := m.pickDay(m.rec.list[*cur].day)
		return m.goTo(timelineScr), cmd
	}
	return m, nil
}

// The perception check.

type guess struct {
	From, To        string // dates; To is the last day, included
	Period          string
	Guess, Measured float64 // hours with agents
	At              time.Time
}

// hours is the guess as you typed it: "12h", "12.5h".
func (g guess) hours() string { return strconv.FormatFloat(g.Guess, 'f', -1, 64) + "h" }

func (g guess) label() string {
	f, _ := time.ParseInLocation("2006-01-02", g.From, time.Local)
	switch g.Period {
	case "day":
		return f.Format("Mon Jan 2")
	case "week":
		return "week of " + f.Format("Jan 2")
	case "month":
		return f.Format("January 2006")
	}
	return g.From + " – " + g.To
}

type guessState struct {
	open, done bool
	input      string
	g          guess
}

func guessPath(cfgPath string) string { return filepath.Join(filepath.Dir(cfgPath), "guesses.json") }

func readGuesses(path string) (gs []guess) {
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &gs) // a broken file reads as no guesses; the next save rewrites it
	}
	return gs
}

// guessVerdict says how far off a guess was.
func guessVerdict(guessed, measured float64) string {
	if measured < 0.05 {
		return "nothing measured"
	}
	off := 100 * (guessed - measured) / measured
	switch {
	case math.Abs(off) < 10:
		return fmt.Sprintf("within %.0f%%: you know your time", math.Max(1, math.Abs(off)))
	case off < 0:
		return fmt.Sprintf("under by %.0f%%: it took more of you than it felt", -off)
	default:
		return fmt.Sprintf("over by %.0f%%: it felt longer than it was", off)
	}
}

func (m model) guessKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.gs.done {
		m.gs = guessState{}
		return m, nil
	}
	switch k := msg.String(); k {
	case "esc":
		m.gs = guessState{}
	case "backspace":
		if r := []rune(m.gs.input); len(r) > 0 {
			m.gs.input = string(r[:len(r)-1])
		}
	case "enter":
		h, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(m.gs.input), "h"), 64)
		if err != nil || h < 0 {
			m.gs.input = ""
			return m.flash("a number of hours, like 12 or 12.5"), nil
		}
		g := guess{From: m.from.Format("2006-01-02"), To: m.to.AddDate(0, 0, -1).Format("2006-01-02"), Period: periodNames[m.per],
			Guess: h, Measured: total(m.d.total).ActiveHours, At: time.Now()}
		path := guessPath(m.cfgPath)
		gs := readGuesses(path)
		for i := 0; i < len(gs); i++ { // a new guess for the same range replaces the old one
			if gs[i].From == g.From && gs[i].To == g.To {
				gs = append(gs[:i], gs[i+1:]...)
				i--
			}
		}
		if err := writeJSON(path, append(gs, g)); err != nil {
			m.gs = guessState{}
			return m.flash("not saved: " + err.Error()), nil
		}
		m.gs.done, m.gs.g = true, g
	default:
		if t := msg.Text; t != "" && strings.Trim(t, "0123456789.h") == "" {
			m.gs.input += t
		}
	}
	return m, nil
}

func (m model) guessBox() string {
	var b strings.Builder
	b.WriteString(bold.Render("Perception check") + "\n\n")
	if !m.gs.done {
		what := "this " + periodNames[m.per]
		if m.per == customP {
			what = m.rangeLabel()
		}
		if time.Now().Before(m.to) {
			what += " so far"
		}
		fmt.Fprintf(&b, "How many hours were you working with\nyour agents %s (%s)?\n\n", what, m.rangeLabel())
		b.WriteString(paint(accent, "› ") + m.gs.input + paint(accent, "▏") + dim.Render(" hours") + "\n\n")
		b.WriteString(dim.Render("[⏎] check  [esc] cancel"))
	} else {
		g := m.gs.g
		fmt.Fprintf(&b, "You guessed %s. Measured: %s.\n%s\n", bold.Render(g.hours()), bold.Render(paint(accent, hdur(g.Measured))),
			paint(voiceHex, guessVerdict(g.Guess, g.Measured)))
		if gs := readGuesses(guessPath(m.cfgPath)); len(gs) > 1 {
			var off float64
			for _, g := range gs {
				off += math.Abs(g.Guess-g.Measured) / math.Max(0.05, g.Measured)
			}
			fmt.Fprintf(&b, "\n%s", dim.Render(fmt.Sprintf("Over %d checks you were off by %.0f%% on average.", len(gs), 100*off/float64(len(gs)))))
		}
		b.WriteString("\n\n" + dim.Render("saved as guesses.json next to your settings · any key closes"))
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(accent)).Padding(1, 3).Render(b.String())
}
