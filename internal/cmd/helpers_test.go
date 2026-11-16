package cmd

import (
	"strings"
	"testing"
	"time"
)

func TestParseDotenv(t *testing.T) {
	in := `# comment
export A=1
B = "two words"
C='single # not comment'
D=plain # trailing comment
E="line\nbreak"
EMPTY=
`
	got, err := parseDotenv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"A", "1"}, {"B", "two words"}, {"C", "single # not comment"}, {"D", "plain"}, {"E", "line\nbreak"}, {"EMPTY", ""}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %q, want %q", i, got[i], want[i])
		}
	}
	if _, err := parseDotenv(strings.NewReader("NOEQUALS\n")); err == nil {
		t.Error("expected an error for a line without =")
	}
}

func TestQuoteEnvRoundTrip(t *testing.T) {
	for _, v := range []string{"", "plain", "two words", `quote"d`, "multi\nline", "a#b", "$HOME"} {
		parsed, err := parseDotenv(strings.NewReader("K=" + quoteEnv(v) + "\n"))
		if err != nil || len(parsed) != 1 || parsed[0][1] != v {
			t.Errorf("round trip of %q gave %v, %v", v, parsed, err)
		}
	}
}

func TestParseAssignments(t *testing.T) {
	got, err := parseAssignments([]string{"A=1", "B=x=y", "C=-"}, "", strings.NewReader("from stdin\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got[1][1] != "x=y" || got[2][1] != "from stdin" {
		t.Fatalf("got %v", got)
	}
	if _, err := parseAssignments([]string{"1BAD=x"}, "", nil); err == nil {
		t.Error("expected invalid key error")
	}
	if _, err := parseAssignments([]string{"NOVALUE"}, "", nil); err == nil {
		t.Error("expected missing = error")
	}
}

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"30m": now.Add(-30 * time.Minute),
		"2h":  now.Add(-2 * time.Hour),
		"3d":  now.Add(-72 * time.Hour),
	}
	for in, want := range cases {
		got, err := parseTime(in, now)
		if err != nil || got != want.UnixMilli() {
			t.Errorf("%s: got %d, %v", in, got, err)
		}
	}
	if got, err := parseTime("2026-11-16T09:00:00Z", now); err != nil || got != time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("RFC 3339: %d, %v", got, err)
	}
	if _, err := parseTime("yesterday", now); err == nil {
		t.Error("expected an error")
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{"My API": "my-api", "  Shop--Front ": "shop-front", "Ünïcode App": "n-code-app", "api_v2": "api-v2"} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSeenSet(t *testing.T) {
	s := newSeenSet(2)
	if !s.add("a") || s.add("a") || !s.add("b") || !s.add("c") || !s.add("a") {
		t.Fatal("unexpected de-duplication")
	}
}

func TestGithubRepo(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:acme/api.git":     "acme/api",
		"https://github.com/acme/api":     "acme/api",
		"https://github.com/acme/api.git": "acme/api",
		"https://gitlab.com/acme/api.git": "",
	} {
		m := githubRemote.FindStringSubmatch(url)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", url, got, want)
		}
	}
}
