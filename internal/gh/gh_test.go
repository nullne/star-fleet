package gh

import (
	"strings"
	"strconv"
	"testing"
)

func TestParsePRURL(t *testing.T) {
	tests := []struct {
		url    string
		wantN  int
		wantOK bool
	}{
		{"https://github.com/owner/repo/pull/42", 42, true},
		{"https://github.com/org/my-repo/pull/1", 1, true},
		{"https://github.com/org/repo/pull/999", 999, true},
		{"https://github.com/org/repo/pull/abc", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		parts := strings.Split(tt.url, "/")
		if len(parts) < 2 {
			if tt.wantOK {
				t.Errorf("URL %q: split too short", tt.url)
			}
			continue
		}
		num, err := strconv.Atoi(parts[len(parts)-1])
		if tt.wantOK {
			if err != nil {
				t.Errorf("URL %q: unexpected error %v", tt.url, err)
			}
			if num != tt.wantN {
				t.Errorf("URL %q: got %d, want %d", tt.url, num, tt.wantN)
			}
		} else {
			if err == nil {
				t.Errorf("URL %q: expected error, got %d", tt.url, num)
			}
		}
	}
}
