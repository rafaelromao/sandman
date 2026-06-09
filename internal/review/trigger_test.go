package review

import "testing"

func TestParseTrigger_MatchSimple(t *testing.T) {
	focus, ok := ParseTrigger("/sandman review")
	if !ok {
		t.Fatal("expected trigger to match")
	}
	if focus != "" {
		t.Errorf("expected empty focus, got %q", focus)
	}
}

func TestParseTrigger_MatchWithFocus(t *testing.T) {
	focus, ok := ParseTrigger("/sandman review focus on tests")
	if !ok {
		t.Fatal("expected trigger to match")
	}
	if focus != "focus on tests" {
		t.Errorf("expected focus %q, got %q", "focus on tests", focus)
	}
}

func TestParseTrigger_CaseInsensitive(t *testing.T) {
	cases := []string{"/Sandman Review", "/SANDMAN review", "/sandman REVIEW"}
	for _, body := range cases {
		if _, ok := ParseTrigger(body); !ok {
			t.Errorf("expected %q to match", body)
		}
	}
}

func TestParseTrigger_MultipleSpaces(t *testing.T) {
	if _, ok := ParseTrigger("/sandman    review"); !ok {
		t.Errorf("expected multiple spaces to match")
	}
}

func TestParseTrigger_NoMatch(t *testing.T) {
	cases := []string{"", "thanks!", "/sandman reviews please", "/sandmanreview", "sandman review"}
	for _, body := range cases {
		if _, ok := ParseTrigger(body); ok {
			t.Errorf("expected %q to NOT match", body)
		}
	}
}

func TestParseTrigger_StripsBotMention(t *testing.T) {
	focus, ok := ParseTrigger("@sandman-bot /sandman review look at config")
	if !ok {
		t.Fatal("expected trigger to match")
	}
	if focus != "look at config" {
		t.Errorf("expected focus %q, got %q", "look at config", focus)
	}
}

func TestParseTrigger_NewlineSeparated(t *testing.T) {
	focus, ok := ParseTrigger("hi\n/sandman review check the tests")
	if !ok {
		t.Fatal("expected trigger to match across lines")
	}
	if focus != "check the tests" {
		t.Errorf("expected focus %q, got %q", "check the tests", focus)
	}
}
