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

// Animation follows termheat: one 8 fps clock, a left-to-right reveal on every data load, a
// gentle breathe on every lit cell, and a fire flicker where several agents burned at once.
const (
	fps           = 8
	revealSeconds = 2.0
	breatheSecs   = 4.0
)

var (
	agentRamp = []string{"#30363d", "#164e63", "#0e7490", "#38bdf8", "#a5b4fc"} // termheat "ocean"
	thinkRamp = []string{"#30363d", "#0e4429", "#006d32", "#26a641", "#39d353"} // termheat "github"
	fireRamp  = []string{"#ff5f1f", "#ff8c42", "#ffb627", "#ffd23f"}
	whomColor = map[string]string{"office": "#f97316", "blivius": "#a78bfa", "self": "#39d353", "automation": "#8a8a8a", "unknown": "#585858"}
	dim       = lipgloss.NewStyle().Faint(true)
	bold      = lipgloss.NewStyle().Bold(true)
)

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

// agentLevel maps "agents at once" to a ramp step: 1, 2, 3–4, 5+.
func agentLevel(peak int) int {
	switch {
	case peak >= 5:
		return 4
	case peak >= 3:
		return 3
	default:
		return peak
	}
}

type tickMsg struct{}

type loadedMsg struct {
	day          time.Time
	fine, coarse []Cell // 15-minute and 30-minute cells; narrow terminals get the coarse ones
	r            Report
}

type dayModel struct {
	rules        []Rule
	idle         time.Duration
	day          time.Time
	fine, coarse []Cell
	r            Report
	width        int
	loading      bool
	tick, start  int // start = tick when the current data arrived; the reveal replays from there
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second/fps, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m dayModel) load() tea.Cmd {
	day, rules, idle := m.day, m.rules, m.idle
	return func() tea.Msg {
		to := day.AddDate(0, 0, 1)
		sessions := load(day)
		r := build(sessions, rules, idle, day, to)
		ats := attribute(sessions, rules, idle, day, to)
		return loadedMsg{day, laneCells(ats, day, 15*time.Minute), laneCells(ats, day, 30*time.Minute), r}
	}
}

func (m dayModel) Init() tea.Cmd { return tea.Batch(tickCmd(), m.load()) }

func (m dayModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		m.tick++
		return m, tickCmd()
	case loadedMsg:
		if msg.day.Equal(m.day) { // ignore a slow load for a day we already left
			m.fine, m.coarse, m.r, m.loading, m.start = msg.fine, msg.coarse, msg.r, false, m.tick
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyPressMsg:
		today := startOfDay(time.Now())
		next := m.day
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "left", "h":
			next = m.day.AddDate(0, 0, -1)
		case "right", "l":
			if m.day.Before(today) {
				next = m.day.AddDate(0, 0, 1)
			}
		case "t":
			next = today
		}
		if !next.Equal(m.day) {
			m.day, m.loading = next, true
			return m, m.load()
		}
	}
	return m, nil
}

func startOfDay(t time.Time) time.Time {
	y, mo, d := t.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, time.Local)
}

func (m dayModel) View() tea.View {
	var b strings.Builder
	title := m.day.Format("Mon 2006-01-02")
	if m.day.Equal(startOfDay(time.Now())) {
		title += " · today"
	}
	fmt.Fprintf(&b, "\n %s  %s\n\n", bold.Render(paint("#ffb627", "renpet")), title)

	breathe := 0.72 + 0.28*(0.5+0.5*math.Sin(2*math.Pi*float64(m.tick)/(fps*breatheSecs)))
	if m.loading || m.fine == nil {
		fmt.Fprintf(&b, " %s\n", paint(scaleHex("#38bdf8", breathe), "reading transcripts…"))
		return tea.NewView(b.String() + footer())
	}
	reveal := math.Min(1, float64(m.tick-m.start)/(fps*revealSeconds))

	cells, perHour, cellSize := m.fine, 4, 15*time.Minute
	if m.width > 0 && m.width < 9+96+2 { // 96 cells + label don't fit: half the resolution
		cells, perHour, cellSize = m.coarse, 2, 30*time.Minute
	}
	var ruler strings.Builder
	for hr := range 24 {
		label := strconv.Itoa(hr)
		if perHour == 2 && hr%2 == 1 {
			label = "" // "10 11" would collide in two columns
		}
		fmt.Fprintf(&ruler, "%-*s", perHour, label)
	}
	b.WriteString("         " + dim.Render(ruler.String()) + "\n")
	lane := func(label string, cell func(i int, c Cell) string) {
		b.WriteString(dim.Render(fmt.Sprintf(" %7s ", label)))
		for i, c := range cells {
			if float64(i)/float64(len(cells)) > reveal { // chronological wipe
				b.WriteByte(' ')
				continue
			}
			b.WriteString(cell(i, c))
		}
		b.WriteByte('\n')
	}
	lane("agents", func(i int, c Cell) string {
		if c.Peak == 0 {
			return paint(agentRamp[0], "·")
		}
		if c.Peak >= 3 { // several agents at once: burn like a termheat streak
			ramp := fireRamp[min(c.Peak-3, len(fireRamp)-1)]
			return paint(scaleHex(ramp, 0.78+0.22*math.Sin(float64(m.tick)*0.9+float64(i)*1.1)), "█")
		}
		return paint(scaleHex(agentRamp[agentLevel(c.Peak)], breathe), "█")
	})
	lane("you", func(_ int, c Cell) string {
		switch {
		case c.Think >= 5*time.Minute:
			return paint(scaleHex(thinkRamp[4], breathe), "█")
		case c.Think > 0:
			return paint(scaleHex(thinkRamp[2], breathe), "▄")
		}
		return paint(thinkRamp[0], "·")
	})
	lane("for", func(_ int, c Cell) string {
		if c.Whom == "" {
			return paint(agentRamp[0], "·")
		}
		return paint(scaleHex(whomColor[c.Whom], breathe), "█")
	})
	if m.day.Equal(startOfDay(time.Now())) {
		now := time.Since(m.day) / cellSize
		flicker := 0.78 + 0.22*math.Sin(float64(m.tick)*0.9)
		fmt.Fprintf(&b, "         %s%s\n", strings.Repeat(" ", int(now)), paint(scaleHex(fireRamp[3], flicker), "▲ now"))
	}

	// legend
	b.WriteString("\n " + dim.Render("agents ") + paint(agentRamp[1], "█") + dim.Render("1 ") + paint(agentRamp[2], "█") + dim.Render("2 ") +
		paint(fireRamp[0], "█") + dim.Render("3 ") + paint(fireRamp[2], "█") + dim.Render("5+") +
		dim.Render(fmt.Sprintf("   one cell = %d min", int(cellSize.Minutes()))) + "\n " + dim.Render("you ") + paint(thinkRamp[2], "▄") + dim.Render("thinking ") + paint(thinkRamp[4], "█") + dim.Render("≥5 min   for "))
	for _, w := range []string{"office", "blivius", "self", "automation", "unknown"} {
		b.WriteString(paint(whomColor[w], "█") + dim.Render(w+" "))
	}
	b.WriteString("\n\n")

	// totals per whom, then the top projects
	var total Row
	for _, row := range m.r.Rows {
		if row.Project != "*" {
			continue
		}
		if row.Whom == "TOTAL" {
			total = row
			continue
		}
		fmt.Fprintf(&b, " %s %5sh  %s\n", paint(whomColor[row.Whom], "█"), h(row.ActiveHours), row.Whom)
	}
	fmt.Fprintf(&b, " %s\n", dim.Render(fmt.Sprintf("  %5sh  with agents · %sh agents ran · %sh you thought · peak %d at once",
		h(total.ActiveHours), h(total.AgentHours), h(total.ThinkHours), total.Peak)))
	shown := 0
	for _, row := range m.r.Rows {
		if row.Project == "*" || row.ActiveHours < 0.1 || shown == 6 {
			continue
		}
		if shown == 0 {
			b.WriteString("\n")
		}
		shown++
		fmt.Fprintf(&b, " %5sh  %s  %s\n", h(row.ActiveHours), paint(whomColor[row.Whom], fmt.Sprintf("%-8s", row.Whom)), row.Project)
	}
	return tea.NewView(b.String() + footer())
}

func footer() string {
	return "\n " + dim.Render("←/→ day · t today · q quit · renpet day --static for the full table") + "\n"
}

func runDayTUI(rules []Rule, idle time.Duration, day time.Time) error {
	_, err := tea.NewProgram(dayModel{rules: rules, idle: idle, day: day, loading: true}).Run()
	return err
}
