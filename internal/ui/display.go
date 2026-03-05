package ui

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true)
	successMark = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("✓")
	warnMark    = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render("⚠")
	failMark    = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render("✗")
	bulletMark  = lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Render("●")
	dimStyle    = lipgloss.NewStyle().Faint(true)
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type Display struct {
	out io.Writer
	mu  sync.Mutex
}

func New() *Display {
	return &Display{out: os.Stderr}
}

func (d *Display) Title(owner, repo string, number int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.out, "\n%s  %s  %s/%s#%d\n\n",
		bulletMark,
		titleStyle.Render("Start Fleet"),
		owner, repo, number)
}

func (d *Display) Step(label, detail string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.out, "  %-22s %s  %s\n", label, successMark, detail)
}

func (d *Display) StepFail(label, detail string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.out, "  %-22s %s  %s\n", label, failMark, detail)
}

func (d *Display) StepWarn(label, detail string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.out, "  %-22s %s  %s\n", label, warnMark, detail)
}

type Spinner struct {
	d       *Display
	prefix  string
	tree    string
	stopCh  chan struct{}
	doneCh  chan struct{}
}

func (d *Display) TreeBranch(label, message string) *Spinner {
	return d.startSpinner("├", label, message)
}

func (d *Display) TreeLeaf(label, message string) *Spinner {
	return d.startSpinner("└", label, message)
}

func (d *Display) startSpinner(tree, label, message string) *Spinner {
	s := &Spinner{
		d:      d,
		prefix: label,
		tree:   tree,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go func() {
		defer close(s.doneCh)
		i := 0
		for {
			select {
			case <-s.stopCh:
				return
			default:
				frame := spinnerFrames[i%len(spinnerFrames)]
				s.d.mu.Lock()
				fmt.Fprintf(s.d.out, "\r  %s %s%s  %s",
					s.tree,
					lipgloss.NewStyle().Width(20).Render(s.prefix),
					lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render(frame),
					dimStyle.Render(message))
				s.d.mu.Unlock()
				i++
				time.Sleep(80 * time.Millisecond)
			}
		}
	}()
	return s
}

func (s *Spinner) Stop(status, detail string) {
	close(s.stopCh)
	<-s.doneCh

	var mark string
	switch status {
	case "success":
		mark = successMark
	case "warn":
		mark = warnMark
	case "fail":
		mark = failMark
	default:
		mark = successMark
	}

	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	fmt.Fprintf(s.d.out, "\r  %s %s%s  %s\n",
		s.tree,
		lipgloss.NewStyle().Width(20).Render(s.prefix),
		mark,
		detail)
}

func (d *Display) Blank() {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintln(d.out)
}

func (d *Display) Info(msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.out, "  %s\n", msg)
}

func (d *Display) Success(msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.out, "  %s  %s\n", successMark, msg)
}

func (d *Display) Warn(msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.out, "  %s  %s\n", warnMark, msg)
}

func (d *Display) Fail(msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.out, "  %s  %s\n", failMark, msg)
}

func (d *Display) Result(url string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.out, "\n  %s  PR ready for review\n     → %s\n\n",
		successMark, url)
}

func (d *Display) FailResult(msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.out, "\n  %s  %s\n\n", failMark, msg)
}

// --- Live agent view (multi-agent display with scrolling output) ---

const (
	maxOutputLines     = 50
	visibleOutputLines = 3
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m > 0 {
		return fmt.Sprintf("%dm %02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// AgentConfig configures a panel in the live view.
type AgentConfig struct {
	Label   string
	Tree    string // "├" or "└"
	Message string
}

// AgentPanel tracks one agent's status and captures its subprocess output.
// It implements io.Writer so it can be passed directly as the output sink.
type AgentPanel struct {
	label   string
	tree    string
	message string
	start   time.Time

	mu      sync.Mutex
	lines   []string
	partial strings.Builder

	done    bool
	mark    string
	detail  string
	elapsed time.Duration
}

// Write implements io.Writer. It strips ANSI escapes and buffers lines,
// handling \r (carriage return) by resetting the current partial line.
func (p *AgentPanel) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	text := stripANSI(string(data))
	for _, ch := range text {
		switch ch {
		case '\n':
			line := strings.TrimSpace(p.partial.String())
			p.partial.Reset()
			if len(line) >= 3 {
				p.lines = append(p.lines, line)
				if len(p.lines) > maxOutputLines {
					p.lines = p.lines[len(p.lines)-maxOutputLines:]
				}
			}
		case '\r':
			p.partial.Reset()
		default:
			p.partial.WriteRune(ch)
		}
	}
	return len(data), nil
}

// Finish marks the panel as completed with a status icon and detail message.
func (p *AgentPanel) Finish(status, detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.done = true
	p.elapsed = time.Since(p.start)
	p.detail = detail
	switch status {
	case "success":
		p.mark = successMark
	case "fail":
		p.mark = failMark
	case "warn":
		p.mark = warnMark
	default:
		p.mark = successMark
	}
}

// UpdateMessage changes the spinner status text while the panel is active.
func (p *AgentPanel) UpdateMessage(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.message = msg
}

// IsDone returns whether the panel has been finished.
func (p *AgentPanel) IsDone() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// LiveView manages a multi-line live display that renders all agent panels
// as a single block, using ANSI cursor control to update in place.
type LiveView struct {
	d          *Display
	panels     []*AgentPanel
	stopCh     chan struct{}
	doneCh     chan struct{}
	lastHeight int
	frameIdx   int
}

// StartLiveView creates panels for each agent and starts the refresh loop.
func (d *Display) StartLiveView(configs []AgentConfig) *LiveView {
	panels := make([]*AgentPanel, len(configs))
	for i, c := range configs {
		panels[i] = &AgentPanel{
			label:   c.Label,
			tree:    c.Tree,
			message: c.Message,
			start:   time.Now(),
			lines:   make([]string, 0, maxOutputLines),
		}
	}

	lv := &LiveView{
		d:      d,
		panels: panels,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go lv.loop()
	return lv
}

// Panel returns the AgentPanel at index i (also usable as an io.Writer).
func (lv *LiveView) Panel(i int) *AgentPanel {
	return lv.panels[i]
}

func (lv *LiveView) loop() {
	defer close(lv.doneCh)
	for {
		select {
		case <-lv.stopCh:
			return
		default:
			lv.redraw()
			lv.frameIdx++
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func (lv *LiveView) redraw() {
	rendered := lv.render()
	lines := strings.Split(rendered, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	newHeight := len(lines)

	lv.d.mu.Lock()
	defer lv.d.mu.Unlock()

	if lv.lastHeight > 0 {
		fmt.Fprintf(lv.d.out, "\033[%dA\r", lv.lastHeight)
	}
	for _, line := range lines {
		fmt.Fprintf(lv.d.out, "\033[2K%s\n", line)
	}
	for i := newHeight; i < lv.lastHeight; i++ {
		fmt.Fprintf(lv.d.out, "\033[2K\n")
	}
	if extra := lv.lastHeight - newHeight; extra > 0 {
		fmt.Fprintf(lv.d.out, "\033[%dA", extra)
	}
	lv.lastHeight = newHeight
}

func (lv *LiveView) render() string {
	var b strings.Builder

	for i, p := range lv.panels {
		p.mu.Lock()
		isDone := p.done

		var statusIcon, message string
		var elapsed time.Duration

		if isDone {
			statusIcon = p.mark
			message = p.detail
			elapsed = p.elapsed
		} else {
			frame := spinnerFrames[lv.frameIdx%len(spinnerFrames)]
			statusIcon = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render(frame)
			message = p.message
			elapsed = time.Since(p.start)
		}

		var lines []string
		if !isDone {
			start := len(p.lines) - visibleOutputLines
			if start < 0 {
				start = 0
			}
			lines = make([]string, len(p.lines[start:]))
			copy(lines, p.lines[start:])
		}
		p.mu.Unlock()

		elapsedStr := formatDuration(elapsed)

		fmt.Fprintf(&b, "  %s %s%s  %s  %s\n",
			p.tree,
			lipgloss.NewStyle().Width(20).Render(p.label),
			statusIcon,
			message,
			dimStyle.Render(elapsedStr))

		if len(lines) > 0 {
			connector := "│"
			if p.tree == "└" {
				connector = " "
			}
			for _, line := range lines {
				if len(line) > 74 {
					line = line[:71] + "..."
				}
				fmt.Fprintf(&b, "  %s   %s\n", connector, dimStyle.Render(line))
			}
		}

		if i < len(lv.panels)-1 && !isDone {
			fmt.Fprintf(&b, "  │\n")
		}
	}

	return b.String()
}

// Stop terminates the refresh loop and does a final render.
func (lv *LiveView) Stop() {
	close(lv.stopCh)
	<-lv.doneCh
	lv.redraw()
}
