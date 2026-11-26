package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The shell around every screen: the logo and views down the left (a tab bar when the terminal
// is narrow), a key sheet on ?, a command palette on : or ctrl+k that reaches everything the CLI
// flags do, and the mouse. Keyboard first; the mouse only shortcuts what keys already do.

const (
	sideW  = 20 // sidebar width, its divider included
	navTop = 9  // the sidebar line the first view sits on
	minW   = 80
	minH   = 24
)

type navItem struct {
	name string
	scr  screen
}

var nav = []navItem{{"Overview", overviewScr}, {"Timeline", timelineScr}, {"Projects", projectsScr}, {"Sessions", sessionsScr},
	{"Records", recordsScr}, {"Gaps", gapsScr}, {"Settings", settingsScr}}

// side is true when there is room for the sidebar; narrower terminals get a tab bar.
func (m model) side() bool { return m.width >= 100 }

// wide is the width a screen may draw in: what the sidebar and the 1-column margin leave.
func (m model) wide() int {
	w := m.width
	if w == 0 {
		w = minW
	}
	if m.side() {
		w -= sideW
	}
	return w - 1
}

// head is every screen's first lines: a title, a dim subtitle, and something on the right.
func (m model) head(title, sub, right string) string {
	left := bold.Render(title)
	if sub != "" {
		left += "  " + dim.Render(sub)
	}
	gap := m.wide() - ansi.StringWidth(left) - ansi.StringWidth(right)
	if right == "" || gap < 2 {
		return "\n" + fit(left, m.wide()) + "\n\n"
	}
	return "\n" + left + strings.Repeat(" ", gap) + right + "\n\n"
}

// keys is the key line at the bottom of every screen, or a fresh note in its place.
func (m model) keys(k ...string) string {
	if m.note != "" && m.now.Sub(m.noteAt) < 6*time.Second {
		return "\n" + paint(accent, fit(m.note, m.wide())) + "\n"
	}
	return "\n" + dim.Render(fit(strings.Join(append(k, "[?] help"), "  "), m.wide())) + "\n"
}

// active is the view the sidebar lights up for the current screen.
func (m model) active() screen {
	if m.scr == detailScr {
		return sessionsScr
	}
	return m.scr
}

func (m model) View() tea.View {
	v := tea.NewView(m.frame())
	v.AltScreen = true // a full-screen app: quitting leaves your scrollback as it was
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (m model) frame() string {
	w, h := m.width, m.height
	if w == 0 {
		w, h = minW, minH
	}
	if w < minW || h < minH {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center,
			fmt.Sprintf("renpet needs %d×%d\nthis terminal is %d×%d", minW, minH, w, h))
	}
	if !m.loaded {
		return m.splash(w, h)
	}
	lines := strings.Split(strings.TrimSuffix(m.content(), "\n"), "\n")
	for i := range lines {
		lines[i] = " " + lines[i]
	}
	body := strings.Join(lines, "\n")
	s := ""
	if m.side() {
		s = lipgloss.JoinHorizontal(lipgloss.Top, m.sidebar(h), body)
	} else {
		bar, _ := m.tabs()
		s = bar + "\n" + body
	}
	switch {
	case m.pal.open:
		s = overlay(s, m.paletteBox(), w, 2)
	case m.help:
		s = overlay(s, m.helpBox(), w, 1)
	case m.gs.open:
		s = overlay(s, m.guessBox(), w, 4)
	}
	return s
}

// content is the current screen without the shell.
func (m model) content() string {
	if m.d == nil {
		return m.head(m.rangeLabel(), "", "") + paint(accent, "reading transcripts…") + "\n"
	}
	if m.gs.open && !m.gs.done { // no peeking while you guess
		return m.head(m.rangeLabel(), "", "") + dim.Render("numbers hidden while you guess") + "\n"
	}
	switch m.scr {
	case timelineScr:
		return m.viewTimeline()
	case recordsScr:
		return m.viewRecords()
	case projectsScr:
		return m.viewProjects()
	case sessionsScr:
		return m.viewSessions()
	case detailScr:
		return m.viewDetail()
	case gapsScr:
		return m.viewGaps()
	case settingsScr:
		return m.viewSettings()
	}
	return m.viewOverview()
}

// overlay draws box centred over base, top rows down.
func overlay(base, box string, w, top int) string {
	x := max(0, (w-lipgloss.Width(box))/2)
	return lipgloss.NewCompositor(lipgloss.NewLayer(base), lipgloss.NewLayer(box).X(x).Y(top).Z(1)).Render()
}

// The mark: the logo's four staggered tags, drawn from assets/renpet-mark.svg's own polygons
// (32×32 viewBox) with quadrant blocks, so it is the real logo at any size and needs no image
// support. Each cell is 2×2 pixels, which keeps the tags' slanted cuts (half blocks lost them);
// a pixel is lit when most of it falls inside a tag.
var markTags = [][][2]float64{
	{{2, 10}, {6, 6}, {20, 6}, {18, 14}, {2, 14}},
	{{23, 6}, {26, 6}, {26, 14}, {21, 14}},
	{{6, 18}, {17, 18}, {15, 26}, {6, 26}},
	{{20, 18}, {30, 18}, {30, 22}, {26, 26}, {18, 26}},
}

// markTag is which tag (0–3) holds the point, or -1.
func markTag(x, y float64) int {
	for k, poly := range markTags {
		in := false
		for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
			a, b := poly[i], poly[j]
			if (a[1] > y) != (b[1] > y) && x < (b[0]-a[0])*(y-a[1])/(b[1]-a[1])+a[0] {
				in = !in
			}
		}
		if in {
			return k
		}
	}
	return -1
}

// markColors paints the tags in the app's own legend: the top two in the agents' greens, the
// bottom two in your amber and the fire of many agents at once.
func markColors() []string { return []string{accent, ramp[3], voiceHex, fireRamp[1]} }

// markLines draws the mark cols wide and rows tall over the tags' box x 2–30, y 6–26,
// sampling each pixel 4×4. With colors (one per tag), each cell takes its tag's colour.
func markLines(cols, rows int, colors []string) []string {
	lit := func(px, py int) int { // the tag most of the pixel falls in, or -1
		var n [4]int
		for sy := range 4 {
			for sx := range 4 {
				x := 2 + 28*(float64(px)+(float64(sx)+0.5)/4)/float64(2*cols)
				y := 6 + 20*(float64(py)+(float64(sy)+0.5)/4)/float64(2*rows)
				if k := markTag(x, y); k >= 0 {
					n[k]++
				}
			}
		}
		best := 0
		for k := range n {
			if n[k] > n[best] {
				best = k
			}
		}
		if n[0]+n[1]+n[2]+n[3] < 8 {
			return -1
		}
		return best
	}
	quad := []rune(" ▘▝▀▖▌▞▛▗▚▐▜▄▙▟█") // by mask: top-left 1, top-right 2, bottom-left 4, bottom-right 8
	out := make([]string, rows)
	for r := range rows {
		var b strings.Builder
		for c := range cols {
			mask, tag := 0, -1
			for i, p := range [][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
				if k := lit(2*c+p[0], 2*r+p[1]); k >= 0 {
					mask, tag = mask|1<<i, k
				}
			}
			if ch := string(quad[mask]); tag >= 0 && colors != nil {
				b.WriteString(paint(colors[tag], ch))
			} else {
				b.WriteString(ch)
			}
		}
		out[r] = b.String()
	}
	return out
}

func (m model) splash(w, h int) string {
	t := m.now.Sub(time.Time{}).Seconds()
	glow := 0.75 + 0.25*(0.5+0.5*math.Sin(2*math.Pi*t/breathe.Seconds()))
	var colors []string
	for _, c := range markColors() {
		colors = append(colors, scaleHex(c, glow))
	}
	lines := markLines(24, 8, colors)
	lines = append(lines, "", bold.Render(paint(accent, "renpet")), dim.Render("reading transcripts…"))
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, lipgloss.JoinVertical(lipgloss.Center, lines...))
}

func (m model) sidebar(h int) string {
	lines := []string{""}
	for _, l := range markLines(16, 5, markColors()) {
		lines = append(lines, "  "+l)
	}
	lines = append(lines, "", "  "+bold.Render(paint(accent, "renpet")), "")
	for i, it := range nav {
		label := fmt.Sprintf("%d  %s", i+1, it.name)
		if it.scr == m.active() {
			lines = append(lines, " "+paint(accent, "▌")+bold.Render(label))
		} else {
			lines = append(lines, "  "+dim.Render(label))
		}
	}
	for len(lines) < h-3 {
		lines = append(lines, "")
	}
	lines = append(lines, "  "+dim.Render(periodNames[m.per]+" · [r]"), "  "+dim.Render(": commands"), "  "+dim.Render("? keys"))
	for i := range lines {
		lines[i] = fit(lines[i], sideW-1) + dim.Render("│")
	}
	return strings.Join(lines[:h], "\n")
}

// tabs is the narrow terminal's top line and where each view's tab sits on it.
func (m model) tabs() (string, [][2]int) {
	var b strings.Builder
	b.WriteString(bold.Render(paint(accent, "renpet")) + " ")
	x := 7
	var at [][2]int
	for i, it := range nav {
		label := fmt.Sprintf(" %d %s", i+1, it.name)
		if it.scr == m.active() {
			b.WriteString(lipgloss.NewStyle().Reverse(true).Foreground(lipgloss.Color(accent)).Render(label))
		} else {
			b.WriteString(dim.Render(label))
		}
		at = append(at, [2]int{x, x + len(label)})
		x += len(label)
	}
	return b.String(), at
}

func (m model) goTo(s screen) model {
	m.help = false
	switch s {
	case sessionsScr:
		m.proj = Row{} // every session; projects → ⏎ narrows it to one project
	case gapsScr:
		m.ret, m.gapIdle = overviewScr, snapIdle(m.idle)
	}
	return m.open(s)
}

// back is esc: one level up the drill-down, or home.
func (m model) back() model {
	m.filter = ""
	switch m.scr {
	case detailScr:
		m.scr = m.ret // the sessions list or the timeline, whichever opened it
	case sessionsScr:
		if m.proj.Project != "" {
			m.scr = projectsScr
		} else {
			m.scr = overviewScr
		}
	case gapsScr:
		m.scr = m.ret
	default:
		m.scr = overviewScr
	}
	return m
}

// click maps a left click to what keys already do: a view in the sidebar or tab bar, a row in a
// list (a second click on the selected row opens it), a day in the week grid.
func (m model) click(x, y int) (tea.Model, tea.Cmd) {
	if m.pal.open || m.help || m.gs.open || !m.loaded {
		return m, nil
	}
	if m.side() {
		if x < sideW {
			if i := y - navTop; i >= 0 && i < len(nav) {
				return m.goTo(nav[i].scr), nil
			}
			return m, nil
		}
	} else {
		if y == 0 {
			_, at := m.tabs()
			for i, r := range at {
				if x >= r[0] && x < r[1] {
					return m.goTo(nav[i].scr), nil
				}
			}
			return m, nil
		}
		y--
	}
	if m.d == nil {
		return m, nil
	}
	// The selected row is the one drawn with a › marker; rows above and below it are one line each.
	lines := strings.Split(ansi.Strip(m.content()), "\n")
	mark := -1
	for i, l := range lines {
		if strings.HasPrefix(l, " › ") || (m.scr == overviewScr && len(m.d.days) <= 7 && len([]rune(l)) > 5 && []rune(l)[4] == '›') {
			mark = i
			break
		}
	}
	if mark < 0 {
		return m, nil
	}
	switch m.scr {
	case overviewScr:
		if i := m.sel + y - mark; len(m.d.days) <= 7 && y > mark-m.sel-1 && i >= 0 && i < len(m.d.days) {
			m.sel = i
		}
		return m, nil
	case recordsScr:
		if y == mark {
			return m.recordsKey("enter")
		}
		m.cur[m.scr] = max(0, min(m.cur[m.scr]+y-mark, m.count()-1))
	case projectsScr, sessionsScr, detailScr, timelineScr:
		if y == mark {
			return m.listKey("enter")
		}
		m.cur[m.scr] = max(0, min(m.cur[m.scr]+y-mark, m.count()-1))
	}
	return m, nil
}

// helpBox is the key sheet: this screen's keys, then the ones that work everywhere.
func (m model) helpBox() string {
	here := map[screen][][2]string{
		timelineScr: {{"↑/↓", "pick a session"}, {"←/→", "previous / next day"}, {"⏎", "the session's prompts"}, {"/", "filter"},
			{"a", "subagents in their own lanes"}},
		recordsScr:  {{"↑/↓", "pick a record"}, {"⏎", "the timeline of that day"}},
		overviewScr: {{"↑/↓", "pick a day"}, {"←/→  [ ]", "previous / next " + periodNames[m.per]}, {"⏎", "projects"}},
		projectsScr: {{"↑/↓", "move"}, {"⏎  →", "the project's sessions"}, {"esc  ←", "back"}, {"tab", "selected day / whole range"},
			{"/", "filter"}, {"o", "order"}, {"a", "list small projects and folders one by one"}},
		sessionsScr: {{"↑/↓", "move"}, {"⏎  →", "the session's prompts"}, {"esc  ←", "back"}, {"tab", "selected day / whole range"},
			{"/", "filter"}, {"o", "order"}, {"a", "list subagents one by one"}},
		detailScr:   {{"↑/↓", "pick a prompt"}, {"esc  ←", "back"}},
		gapsScr:     {{"←/→", "move the idle line"}, {"⏎", "save it as your idle gap"}, {"esc", "back"}},
		settingsScr: {{"↑/↓", "move"}, {"←/→", "change"}, {"⏎", "assign a folder"}, {"x", "delete a rule"}, {"K/J", "move a rule up/down"}, {"g", "prompt gaps"}},
	}[m.scr]
	every := [][2]string{{"1–7", "views"}, {":  ctrl+k", "commands"}, {"r", "day · week · month"}, {"[ ]", "previous / next"},
		{"t", "today"}, {"p", "guess your hours"}, {"mouse", "click and scroll"}, {"?", "close"}, {"q", "quit"}}
	var b strings.Builder
	row := func(k, v string) { fmt.Fprintf(&b, "%s %s\n", paint(accent, fmt.Sprintf("%-10s", k)), v) }
	b.WriteString(bold.Render(title(nav[max(0, navIndex(m.active()))].name)) + "\n")
	for _, kv := range here {
		row(kv[0], kv[1])
	}
	b.WriteString("\n" + bold.Render("Everywhere") + "\n")
	for _, kv := range every {
		row(kv[0], kv[1])
	}
	b.WriteString("\n" + dim.Render("grid: brighter = busier hour · ") + paint(fireRamp[1], "██") + dim.Render(" 3+ agents at once"))
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(accent)).Padding(0, 2).
		Render(strings.TrimRight(b.String(), "\n"))
}

func navIndex(s screen) int {
	for i, it := range nav {
		if it.scr == s {
			return i
		}
	}
	return -1
}

// The command palette: everything the CLI's flags and subcommands do, by name.

type paletteState struct {
	open  bool
	query string
	cur   int
	ask   string // "custom": the query is a date range being typed, not a search
}

// command is one palette entry: its name, the key that does the same, and the CLI flag it stands for.
type command struct {
	name, key, flag string
	run             func(m model) (model, tea.Cmd)
}

func (m model) commands() []command {
	var cs []command
	for i, it := range nav {
		cs = append(cs, command{"Go to " + it.name, fmt.Sprint(i + 1), "", func(m model) (model, tea.Cmd) { return m.goTo(it.scr), nil }})
	}
	for _, p := range []period{dayP, weekP, monthP} {
		cs = append(cs, command{"Range: " + periodNames[p], "r", "", func(m model) (model, tea.Cmd) { return m.setPeriod(p) }})
	}
	cs = append(cs,
		command{"Range: custom dates…", "", "--since --until", func(m model) (model, tea.Cmd) {
			m.pal = paletteState{open: true, ask: "custom"}
			return m, nil
		}},
		command{"Today", "t", "", func(m model) (model, tea.Cmd) { return m.today() }},
		command{"Perception check: guess this " + periodNames[m.per] + "'s hours", "p", "", func(m model) (model, tea.Cmd) {
			m.gs = guessState{open: true}
			return m, nil
		}},
		command{"Previous " + periodNames[m.per], "[", "", func(m model) (model, tea.Cmd) { return m.shift(-1) }},
		command{"Next " + periodNames[m.per], "]", "", func(m model) (model, tea.Cmd) { return m.shift(1) }},
		command{map[bool]string{false: "Show hidden rows", true: "Fold hidden rows again"}[m.all], "a", "--all",
			func(m model) (model, tea.Cmd) { m.all = !m.all; return m, nil }},
		command{"Idle gap: pick from your prompt gaps", "", "--idle", func(m model) (model, tea.Cmd) { return m.goTo(gapsScr), nil }},
		command{"Export this " + periodNames[m.per] + " as JSON", "", "--json", func(m model) (model, tea.Cmd) { return m.export("json"), nil }},
		command{"Export this " + periodNames[m.per] + " as a table", "", "renpet week", func(m model) (model, tea.Cmd) { return m.export("txt"), nil }},
		command{"Theme: " + map[bool]string{false: "light", true: "dark"}[m.light], "", "", func(m model) (model, tea.Cmd) {
			m.light = !m.light
			usePalette(m.light)
			return m, nil
		}},
	)
	if _, err := os.Stat(m.cfgPath); err != nil {
		cs = append(cs, command{"Create the settings file", "", "config init", func(m model) (model, tea.Cmd) {
			if err := writeJSON(m.cfgPath, m.cfg); err != nil {
				return m.flash("not saved: " + err.Error()), nil
			}
			return m.flash("wrote " + tilde(m.cfgPath)), nil
		}})
	}
	return append(cs, command{"Quit", "q", "", func(m model) (model, tea.Cmd) { return m, tea.Quit }})
}

// fuzzy is true when every rune of q appears in s in order: "rw" finds "Range: week".
func fuzzy(q, s string) bool {
	s = strings.ToLower(s)
	for _, r := range strings.ToLower(q) {
		i := strings.IndexRune(s, r)
		if i < 0 {
			return false
		}
		s = s[i+len(string(r)):]
	}
	return true
}

func (m model) found() []command {
	var out []command
	for _, c := range m.commands() {
		if fuzzy(m.pal.query, c.name) {
			out = append(out, c)
		}
	}
	return out
}

func (m model) paletteKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k := msg.String(); k {
	case "esc":
		m.pal = paletteState{}
	case "up", "ctrl+p":
		m.pal.cur = max(0, m.pal.cur-1)
	case "down", "ctrl+n":
		m.pal.cur = min(len(m.found())-1, m.pal.cur+1)
	case "backspace":
		if r := []rune(m.pal.query); len(r) > 0 {
			m.pal.query, m.pal.cur = string(r[:len(r)-1]), 0
		}
	case "enter":
		if m.pal.ask == "custom" {
			return m.customRange(m.pal.query)
		}
		cs := m.found()
		if len(cs) == 0 {
			return m, nil
		}
		c := cs[min(m.pal.cur, len(cs)-1)]
		m.pal = paletteState{}
		return c.run(m)
	default:
		m.pal.query += msg.Text
		m.pal.cur = 0
	}
	return m, nil
}

// customRange reads "2026-11-01 2026-11-25" (both days included) and shows it.
func (m model) customRange(s string) (tea.Model, tea.Cmd) {
	f := strings.Fields(strings.ReplaceAll(s, "→", " "))
	if len(f) != 2 {
		return m.flash("want two dates: 2026-11-01 2026-11-25"), nil
	}
	from, err1 := time.ParseInLocation("2006-01-02", f[0], time.Local)
	last, err2 := time.ParseInLocation("2006-01-02", f[1], time.Local)
	if err1 != nil || err2 != nil || last.Before(from) {
		return m.flash("want two dates, first one first: 2026-11-01 2026-11-25"), nil
	}
	m.pal, m.per = paletteState{}, customP
	return m.setRange(from, last.AddDate(0, 0, 1), from)
}

func (m model) paletteBox() string {
	w := min(64, m.wide()-6)
	var b strings.Builder
	if m.pal.ask == "custom" {
		b.WriteString(bold.Render("Custom range") + dim.Render("  first and last day, e.g. 2026-11-01 2026-11-25") + "\n\n")
		b.WriteString(paint(accent, "› ") + m.pal.query + paint(accent, "▏"))
	} else {
		b.WriteString(paint(accent, "› ") + m.pal.query + paint(accent, "▏") + "\n")
		cs := m.found()
		lo, hi := visible(len(cs), m.pal.cur, 10)
		for i := lo; i < hi; i++ {
			c := cs[i]
			name := fit(c.name, w-22) // marker 2 + name + key 5 + flag 15 = w
			if i == m.pal.cur {
				name = bold.Render(paint(accent, "› ") + name)
			} else {
				name = "  " + name
			}
			key := "   "
			if c.key != "" {
				key = " " + bold.Render(paint(accent, c.key)) + " "
			}
			b.WriteString("\n" + name + " " + key + " " + dim.Render(rjust(c.flag, 15)))
		}
		if len(cs) == 0 {
			b.WriteString("\n" + dim.Render("  no command matches"))
		}
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(accent)).Padding(0, 1).Width(w + 4).
		Render(b.String())
}

// export writes the range's report next to where renpet was started, as the CLI would print it.
func (m model) export(kind string) model {
	if m.d == nil {
		return m.flash("still reading: try again in a moment")
	}
	dir, err := os.Getwd()
	if err != nil {
		return m.flash("can't export: " + err.Error())
	}
	path := filepath.Join(dir, fmt.Sprintf("renpet-%s-%s.%s", m.from.Format("2006-01-02"), m.to.AddDate(0, 0, -1).Format("2006-01-02"), kind))
	f, err := os.Create(path)
	if err == nil {
		if kind == "json" {
			enc := json.NewEncoder(f)
			enc.SetIndent("", "  ")
			err = enc.Encode(m.d.total)
		} else {
			printWeek(f, m.d.total, m.all, m.rules) // a file isn't a terminal: plain text
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		return m.flash("can't export: " + err.Error())
	}
	return m.flash("wrote " + tilde(path))
}
