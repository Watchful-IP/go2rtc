package exacq

import (
	"strings"
	"testing"
)

func TestSource(t *testing.T) {
	s, err := ParseURL("exacq://user:p%40ss@[::1]/459264?start=1700000000000&end=1700000010000&speed=2&gaps=preserve")
	if err != nil {
		t.Fatal(err)
	}
	if s.URL.Host != "[::1]:22609" || s.Camera != 459264 || !s.Playback || s.Speed != 2 || s.End.Sub(s.Start).Seconds() != 10 {
		t.Fatalf("bad source: %+v", s)
	}
	if strings.Contains(s.Redacted(), "p%40ss") || strings.Contains(s.Redacted(), "p@ss") {
		t.Fatal("password exposed")
	}
	if strings.Contains(s.Redacted(), "user:") {
		t.Fatal("username exposed")
	}
	if s.options(0).Password != "p@ss" {
		t.Fatal("password not decoded")
	}
}

func TestInvalidSource(t *testing.T) {
	base := "exacq://user:secret@recorder/459264"
	for _, raw := range []string{
		"exacq://recorder/459264", "exacq://user:secret@/459264", "exacq://user:secret@host:0/459264",
		"exacq://user:secret@host/0", "exacq://user:secret@host/459264/1",
		base + "?start=1700000000000", base + "?end=1700000000000", base + "?speed=2", base + "?stream=-1",
		base + "?start=1700000000000&end=1700000010000&speed=NaN", base + "?start=1700000000000&end=1700000010000&speed=Inf",
		base + "?start=1700000000000&end=1700000010000&speed=0", base + "?start=1700000000000&end=1700000000000",
		base + "?start=1700000000000&end=1700007200000", base + "?start=-1&end=1000", base + "?gaps=skip", base + "?stream=1&stream=2",
		base + "?start=2147483648000&end=2147483650000",
	} {
		if _, err := ParseURL(raw); err == nil {
			t.Fatalf("accepted %s", strings.ReplaceAll(raw, "secret", "xxx"))
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatal("error exposed credentials")
		}
	}
}

func TestLiveStreamKnownBeforeInventory(t *testing.T) {
	for raw, want := range map[string]uint32{
		"exacq://u:p@r/459264":                           459264,
		"exacq://u:p@r/4784640?stream=541655552":         541655552,
		"exacq://u:p@r/4784640?select=lowest":            0,
		"exacq://u:p@r/459264?start=1000000&end=1060000": 0,
	} {
		s, err := ParseURL(raw)
		if err != nil {
			t.Fatal(raw, err)
		}
		if got := s.liveStream(); got != want {
			t.Fatalf("%s: prestart %d, want %d", raw, got, want)
		}
	}
}
