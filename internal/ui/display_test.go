package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestStripANSI(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b[1;32mbold green\x1b[0m", "bold green"},
		{"\x1b[?25l\x1b[?25h", ""},
		{"⠋ Reading file...", "⠋ Reading file..."},
	}
	for _, tt := range tests {
		got := stripANSI(tt.input)
		if got != tt.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{5 * time.Second, "5s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m 00s"},
		{65 * time.Second, "1m 05s"},
		{123 * time.Second, "2m 03s"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestAgentPanelWrite(t *testing.T) {
	p := &AgentPanel{
		start: time.Now(),
		lines: make([]string, 0, maxOutputLines),
	}

	p.Write([]byte("Reading file src/main.go\n"))
	p.Write([]byte("Editing handler.go\n"))
	p.Write([]byte("Running tests\n"))

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(p.lines))
	}
	if p.lines[0] != "Reading file src/main.go" {
		t.Errorf("line[0] = %q", p.lines[0])
	}
}

func TestAgentPanelWriteCarriageReturn(t *testing.T) {
	p := &AgentPanel{
		start: time.Now(),
		lines: make([]string, 0, maxOutputLines),
	}

	p.Write([]byte("⠋ Reading file...\r"))
	p.Write([]byte("⠙ Reading file...\r"))
	p.Write([]byte("✓ Read file done\n"))

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.lines) != 1 {
		t.Fatalf("got %d lines, want 1 (CR should reset partial)", len(p.lines))
	}
	if p.lines[0] != "✓ Read file done" {
		t.Errorf("line = %q, want %q", p.lines[0], "✓ Read file done")
	}
}

func TestAgentPanelWriteANSIStripped(t *testing.T) {
	p := &AgentPanel{
		start: time.Now(),
		lines: make([]string, 0, maxOutputLines),
	}

	p.Write([]byte("\x1b[32m✓ Success\x1b[0m\n"))

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(p.lines))
	}
	if p.lines[0] != "✓ Success" {
		t.Errorf("line = %q, want %q", p.lines[0], "✓ Success")
	}
}

func TestAgentPanelRingBuffer(t *testing.T) {
	p := &AgentPanel{
		start: time.Now(),
		lines: make([]string, 0, maxOutputLines),
	}

	for i := 0; i < maxOutputLines+10; i++ {
		p.Write([]byte("line content here\n"))
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.lines) != maxOutputLines {
		t.Errorf("got %d lines, want %d (ring buffer cap)", len(p.lines), maxOutputLines)
	}
}

func TestAgentPanelFinish(t *testing.T) {
	p := &AgentPanel{
		start: time.Now().Add(-2 * time.Second),
		lines: make([]string, 0),
	}

	p.Finish("success", "PR #42")

	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.done {
		t.Error("panel should be done")
	}
	if p.detail != "PR #42" {
		t.Errorf("detail = %q", p.detail)
	}
	if p.elapsed < 2*time.Second {
		t.Errorf("elapsed = %v, want >= 2s", p.elapsed)
	}
}

func TestLiveViewRender(t *testing.T) {
	d := &Display{out: &bytes.Buffer{}}
	lv := &LiveView{
		d: d,
		panels: []*AgentPanel{
			{label: "Dev Agent", tree: "├", message: "Writing...", start: time.Now(), lines: []string{"line1", "line2"}},
			{label: "Test Agent", tree: "└", message: "Testing...", start: time.Now(), lines: []string{"testA"}},
		},
		frameIdx: 0,
	}

	rendered := lv.render()

	if !strings.Contains(rendered, "├") {
		t.Error("missing ├ tree connector")
	}
	if !strings.Contains(rendered, "└") {
		t.Error("missing └ tree connector")
	}
	if !strings.Contains(rendered, "Dev Agent") {
		t.Error("missing Dev Agent label")
	}
	if !strings.Contains(rendered, "Test Agent") {
		t.Error("missing Test Agent label")
	}
	if !strings.Contains(rendered, "line1") {
		t.Error("missing output line1")
	}
	if !strings.Contains(rendered, "testA") {
		t.Error("missing output testA")
	}
	// Separator between panels when first panel is not done
	if !strings.Contains(rendered, "│\n") {
		t.Error("missing │ separator between panels")
	}
}

func TestLiveViewRenderDone(t *testing.T) {
	d := &Display{out: &bytes.Buffer{}}
	lv := &LiveView{
		d: d,
		panels: []*AgentPanel{
			{label: "Dev Agent", tree: "├", start: time.Now(), done: true, mark: "✓", detail: "PR #42", elapsed: 2 * time.Minute},
			{label: "Test Agent", tree: "└", start: time.Now(), done: true, mark: "✓", detail: "PR #43", elapsed: 90 * time.Second},
		},
		frameIdx: 0,
	}

	rendered := lv.render()

	if !strings.Contains(rendered, "PR #42") {
		t.Error("missing finished detail for dev")
	}
	if !strings.Contains(rendered, "PR #43") {
		t.Error("missing finished detail for test")
	}
	// No output lines when done
	lineCount := strings.Count(rendered, "\n")
	if lineCount != 2 {
		t.Errorf("finished render should be 2 lines, got %d", lineCount)
	}
	// No separator when both done
	if strings.Contains(rendered, "│\n") {
		t.Error("should not have │ separator when both panels are done")
	}
}

func TestLiveViewRenderHeightShrink(t *testing.T) {
	var buf bytes.Buffer
	d := &Display{out: &buf}

	panels := []*AgentPanel{
		{label: "Dev Agent", tree: "├", message: "Writing...", start: time.Now(),
			lines: []string{"a", "b", "c"}},
		{label: "Test Agent", tree: "└", message: "Testing...", start: time.Now(),
			lines: []string{"x", "y"}},
	}

	lv := &LiveView{d: d, panels: panels, frameIdx: 0}

	// First render (tall: status + output lines + separator)
	lv.redraw()
	firstHeight := lv.lastHeight

	// Finish both panels (short: just 2 status lines)
	panels[0].Finish("success", "done")
	panels[1].Finish("success", "done")

	buf.Reset()
	lv.redraw()

	if lv.lastHeight >= firstHeight {
		t.Errorf("height should shrink after finish: %d >= %d", lv.lastHeight, firstHeight)
	}
	if lv.lastHeight != 2 {
		t.Errorf("finished height should be 2, got %d", lv.lastHeight)
	}
}
