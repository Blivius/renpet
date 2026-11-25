package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The view borrows termheat's grammar: a weekday × time grid of 2-character cells, one colour
// ramp from less to more, a fire flicker where something burned hot (here: 3+ agents at once),
// a bold stats block, one line of voice, and bracketed keys. Motion: one 8 fps clock, a
// left-to-right reveal on every load, and lit cells that breathe.
// Motion is time-based, not frame-based, so it looks the same at any frame rate. Crisp means: a
// short ease-out reveal, cells that stay near full brightness, and fire that flickers fast.
const (
	fps     = 30
	reveal  = 450 * time.Millisecond
	breathe = 3 * time.Second
	gutter  = 7 // "Wed ›  " — every row, header and lane starts after this
)

var (
	ramp     = []string{"#30363d", "#0e4429", "#006d32", "#26a641", "#39d353"} // spring green: renewal
	fireRamp = []string{"#ff5f1f", "#ff8c42", "#ffb627", "#ffd23f"}
	accent   = "#39d353"
	voiceHex = "#ffb627"
	dim      = lipgloss.NewStyle().Faint(true)
	bold     = lipgloss.NewStyle().Bold(true)
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

// level maps time inside a cell to a ramp step 0–4, by quarters of the cell.
func level(d, cell time.Duration) int {
	if d <= 0 {
		return 0
	}
	return min(4, int(math.Ceil(4*float64(d)/float64(cell))))
}

type weekData struct {
	start time.Time
	cells [7][]Cell // 24 hourly cells per day, Monday first
	days  [7]Report
	week  Report
}

func loadWeek(start time.Time, rules []Rule, idle time.Duration) weekData {
	end := start.AddDate(0, 0, 7)
	sessions := load(start, end)
	ats := attribute(sessions, rules, idle, start, end)
	w := weekData{start: start, week: build(sessions, rules, idle, start, end)}
	for i := range 7 {
		d := start.AddDate(0, 0, i)
		w.cells[i] = laneCells(ats, d, time.Hour)
		w.days[i] = build(sessions, rules, idle, d, d.AddDate(0, 0, 1))
	}
	return w
}

func startOfDay(t time.Time) time.Time {
	y, mo, d := t.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, time.Local)
}

// weekday is t's row in the grid: 0 for the configured first day of the week … 6.
func weekday(t time.Time) int { return (int(t.Weekday()) - int(weekStart) + 7) % 7 }

// weekOf returns the first day of t's week.
func weekOf(t time.Time) time.Time { return startOfDay(t).AddDate(0, 0, -weekday(t)) }

type tickMsg time.Time
type loadedMsg struct{ w weekData }

type model struct {
	rules      []Rule
	idle       time.Duration
	user       string
	start      time.Time // first day of the shown week
	sel        int       // selected row, 0 = first day of the week
	w          *weekData
	past       map[time.Time]*weekData // finished weeks can't change, so each is read once
	now, shown time.Time               // shown = when the current week appeared; the reveal replays from there
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second/fps, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) load() tea.Cmd {
	start, rules, idle := m.start, m.rules, m.idle
	return func() tea.Msg { return loadedMsg{loadWeek(start, rules, idle)} }
}

func (m model) Init() tea.Cmd { return tea.Batch(tickCmd(), m.load()) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		m.now = time.Time(msg)
		return m, tickCmd()
	case loadedMsg:
		if msg.w.start.Before(weekOf(time.Now())) {
			m.past[msg.w.start] = &msg.w
		}
		if msg.w.start.Equal(m.start) { // drop a slow load for a week we already left
			m.w, m.shown = &msg.w, m.now
		}
		return m, nil
	case tea.KeyPressMsg:
		start := m.start
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			m.sel = max(0, m.sel-1)
		case "down", "j":
			m.sel = min(6, m.sel+1)
		case "left", "h":
			start = m.start.AddDate(0, 0, -7)
		case "right", "l":
			if m.start.Before(weekOf(time.Now())) {
				start = m.start.AddDate(0, 0, 7)
			}
		case "t":
			start, m.sel = weekOf(time.Now()), weekday(time.Now())
		}
		if !start.Equal(m.start) {
			m.start, m.w, m.shown = start, m.past[start], m.now // a remembered week replays its reveal
			if m.w == nil {
				return m, m.load()
			}
		}
	}
	return m, nil
}

func (m model) View() tea.View {
	var b strings.Builder
	t := m.now.Sub(time.Time{}).Seconds()
	glow := 0.9 + 0.1*(0.5+0.5*math.Sin(2*math.Pi*t/breathe.Seconds())) // 90–100%: alive, not muddy
	fmt.Fprintf(&b, "\n🌿 %s  week of %s%s\n\n", bold.Render(paint(accent, "renpet")), m.start.Format("Jan 2"), dim.Render(" — "+m.user))
	if m.w == nil {
		fmt.Fprintf(&b, "   %s\n", paint(scaleHex(accent, glow), "reading transcripts…"))
		return tea.NewView(b.String() + footer())
	}
	p := math.Min(1, math.Max(0, m.now.Sub(m.shown).Seconds()/reveal.Seconds()))
	wipe := 1 - math.Pow(1-p, 3) // ease-out: fast start, soft landing
	b.WriteString(strings.Repeat(" ", gutter))
	for hr := 0; hr < 24; hr += 2 {
		b.WriteString(dim.Render(fmt.Sprintf("%02d  ", hr)))
	}
	b.WriteString("\n")

	today := startOfDay(time.Now())
	for i := range 7 {
		day := m.start.AddDate(0, 0, i)
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
		for j, c := range m.w.cells[i] {
			b.WriteString(m.cell(j, i*24+j, c.Active, c.Peak, wipe, glow, t))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n" + strings.Repeat(" ", gutter) + dim.Render("Less ") + paint(ramp[0], "··"))
	for _, hex := range ramp[1:] {
		b.WriteString(" " + paint(hex, "██"))
	}
	b.WriteString(dim.Render(" More   ") + paint(fireRamp[1], "██") + dim.Render(" 3+ agents at once") + "\n\n")

	// the selected day: agents and you, hour by hour, aligned under the grid
	sel := m.w.cells[m.sel]
	b.WriteString(bold.Render(fmt.Sprintf("%-*s", gutter, "agents")))
	for j, c := range sel {
		b.WriteString(m.cell(j, 1000+j, c.Run, c.Peak, wipe, glow, t))
	}
	b.WriteString("\n" + bold.Render(fmt.Sprintf("%-*s", gutter, "you")))
	for j, c := range sel {
		b.WriteString(m.cell(j, 2000+j, c.Think, 0, wipe, glow, t))
	}
	b.WriteString("\n\n")
	day := m.start.AddDate(0, 0, m.sel)
	stats(&b, day.Format("Mon Jan 2"), m.w.days[m.sel])
	stats(&b, "This week", m.w.week)
	fmt.Fprintf(&b, "\n%s\n", paint(voiceHex, voice(*m.w)))
	return tea.NewView(b.String() + footer())
}

// cell draws one 2-character cell. Hours where 3+ agents ran at once burn; each burning cell
// flickers on its own phase so a hot evening shimmers instead of blinking in unison.
func (m model) cell(col, phase int, d time.Duration, peakAgents int, wipe, glow, t float64) string {
	if float64(col)/24 >= wipe && wipe < 1 { // chronological wipe, oldest hour first
		return "  "
	}
	if peakAgents >= 3 && d > 0 {
		hex := fireRamp[min(peakAgents-3, len(fireRamp)-1)]
		return bold.Render(paint(scaleHex(hex, 0.8+0.2*math.Sin(2*math.Pi*1.4*t+float64(phase)*1.1)), "██"))
	}
	if l := level(d, time.Hour); l > 0 {
		return paint(scaleHex(ramp[l], glow), "██")
	}
	return paint(ramp[0], "··")
}

// stats writes termheat's block: plain labels, bold numbers, then who the time was for.
func stats(b *strings.Builder, title string, r Report) {
	var total Row
	var whoms []string
	for _, row := range r.Rows {
		switch {
		case row.Project != "*":
		case row.Whom == "TOTAL":
			total = row
		case row.ActiveHours >= 0.05:
			whoms = append(whoms, strings.ToUpper(row.Whom[:1])+row.Whom[1:]+" "+bold.Render(h(row.ActiveHours)+"h"))
		}
	}
	fmt.Fprintf(b, "%s  %s with agents · %s agents ran · %s you · peak %s\n",
		dim.Render(fmt.Sprintf("%-10s", title)), bold.Render(h(total.ActiveHours)+"h"), bold.Render(h(total.AgentHours)+"h"),
		bold.Render(h(total.ThinkHours)+"h"), bold.Render(strconv.Itoa(total.Peak)))
	if len(whoms) > 0 {
		fmt.Fprintf(b, "%s  %s\n", strings.Repeat(" ", 10), strings.Join(whoms, " · "))
	}
}

// voice picks the single most telling true fact about the week, like termheat's "The grid is
// cooling…". Every line comes from the numbers on screen; nothing is invented.
func voice(w weekData) string {
	var total Row
	var top Row
	var agentSum float64
	for _, row := range w.week.Rows {
		switch {
		case row.Project != "*":
		case row.Whom == "TOTAL":
			total = row
		default:
			agentSum += row.AgentHours
			if row.AgentHours > top.AgentHours {
				top = row
			}
		}
	}
	if total.ActiveHours == 0 {
		return "🌱 Nothing logged this week yet. Renpet waits for spring."
	}
	for _, row := range w.week.Rows {
		if row.Project == "*" && row.Whom == "unknown" && row.AgentHours/agentSum >= 0.5 {
			return fmt.Sprintf("❓ %.0f%% of your agent time has no owner yet. Add rules: renpet config", 100*row.AgentHours/agentSum)
		}
	}
	burst, burstDay, burstHour, nights := 0, 0, 0, 0
	for i := range 7 {
		late := false
		for j, c := range w.cells[i] {
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
		return fmt.Sprintf("⚖  %s took %.0f%% of your agent time this week.", strings.ToUpper(top.Whom[:1])+top.Whom[1:], 100*share)
	case burst >= 5:
		return fmt.Sprintf("🔥 %d agents ran at once on %s at %02d:00.", burst, w.start.AddDate(0, 0, burstDay).Format("Mon"), burstHour)
	case nights >= 2:
		return fmt.Sprintf("🌙 You or your agents were up past midnight on %d days.", nights)
	default:
		return fmt.Sprintf("🧠 You spent %.0f%% of your agent hours thinking; the agents ran the rest.", 100*total.ThinkHours/total.ActiveHours)
	}
}

func footer() string {
	return "\n" + dim.Render("[←/→] week  [↑/↓] day  [t] today  [q] quit") + "\n"
}

func runTUI(rules []Rule, idle time.Duration, day time.Time) error {
	user := os.Getenv("USER")
	start := weekOf(day)
	now := time.Now()
	_, err := tea.NewProgram(model{rules: rules, idle: idle, user: user, start: start, sel: weekday(day),
		past: map[time.Time]*weekData{}, now: now, shown: now}).Run()
	return err
}
