package main

import "testing"

func TestNormalizeVideoPhrase_NonAlphanumericInput_RetainsLettersDigitsAndSpaces(t *testing.T) {
	for _, input := range []struct {
		text string
		want string
	}{
		{" W.A-T?  42! ", "wat 42"},
		{"GRÜßE世界١٢", "grüße世界١٢"},
		{"wat\t42\nnow\r\u00a0please", "wat42nowplease"},
		{"wat💥+_=42", "wat42"},
		{"?!💥\t\n", ""},
	} {
		t.Run(input.text, func(t *testing.T) {
			if got := normalizeVideoPhrase(input.text); got != input.want {
				t.Fatalf("normalized=%q, want %q", got, input.want)
			}
		})
	}
}

func TestMatchVideo_PunctuationIgnored_MatchesOnlyWholeEdgePhrase(t *testing.T) {
	c := mediaCatalog{Clips: []mediaClip{{ID: "1"}}, Triggers: []mediaTrigger{{ID: "1", Text: "W.A-T? 42!", ClipID: new("1")}}}
	for _, input := range []struct {
		text string
		want bool
	}{
		{"wat 42", true}, {"WAT? 42! extra", true}, {"extra w.a.t 42?", true},
		{"extra wat 42 extra", false}, {"water 42", false}, {"wat 420", false},
		{"wat\t42", false}, {"???", false},
	} {
		t.Run(input.text, func(t *testing.T) {
			trigger, clip, ok := matchVideo(c, normalizeVideoPhrase(input.text), func(int) int { return 0 })
			if ok != input.want || ok && (trigger.ID != "1" || clip.ID != "1") {
				t.Fatalf("match=%t trigger=%q clip=%q", ok, trigger.ID, clip.ID)
			}
		})
	}
	if got := messageVideoText(messageContent{Chunks: []chunk{{Type: 0, Data: "W."}, {Type: 1, Code: "A-T?"}, {Type: 0, Data: " 42!"}}}); got != "wat 42" {
		t.Fatalf("chunk text=%q", got)
	}
}

func TestVideoPhraseMatching(t *testing.T) {
	c := mediaCatalog{Clips: []mediaClip{{ID: "1", RemoteVideoID: "abcdefghijk"}}, Triggers: []mediaTrigger{{ID: "1", Text: "hello", ClipID: new("1")}}}
	for _, input := range []struct {
		text string
		want bool
	}{
		{"HELLO!", true}, {"hello there", true}, {"say hello", true}, {"say hello friend", false}, {"hellothere", false}, {"", false}, {"!!!", false}, {"hello\tthere", false}, {"hello\nthere", false}, {"“HELLO”", true},
	} {
		t.Run(input.text, func(t *testing.T) {
			_, _, ok := matchVideo(c, normalizeVideoPhrase(input.text), func(int) int { return 0 })
			if ok != input.want {
				t.Fatalf("match=%t", ok)
			}
		})
	}
	c.Triggers[0].Text = "!hello world!"
	for _, input := range []struct {
		text string
		want bool
	}{{"hello   WORLD!", true}, {"say hello world", true}, {"hello, world", true}, {"hello world friend", true}, {"other hello world friend", false}} {
		_, _, ok := matchVideo(c, normalizeVideoPhrase(input.text), func(int) int { return 0 })
		if ok != input.want {
			t.Fatalf("phrase %q match=%t", input.text, ok)
		}
	}
	c.Triggers[0].Text = "!!!"
	if _, _, ok := matchVideo(c, "hello", func(int) int { return 0 }); ok {
		t.Fatal("empty normalized phrase matched")
	}
	c.Triggers[0].Text = "grüße"
	if _, _, ok := matchVideo(c, normalizeVideoPhrase("GRÜßE!"), func(int) int { return 0 }); !ok {
		t.Fatal("Unicode lower did not match")
	}
	if got := messageVideoText(messageContent{Chunks: []chunk{{Type: 0, Data: "he"}, {Type: 1, Code: "llo"}, {Type: 0, Data: " world!"}}}); got != "hello world" {
		t.Fatalf("chunk text=%q", got)
	}
}

func TestVideoPhraseRankingAndRandomRows(t *testing.T) {
	c := mediaCatalog{Clips: []mediaClip{{ID: "1"}, {ID: "2"}}, Triggers: []mediaTrigger{
		{ID: "20", Text: "hello world", ClipID: new("1")},
		{ID: "7", Text: "other words", ClipID: new("2")},
		{ID: "2", Text: "HELLO WORLD!", ClipID: new("2")},
		{ID: "3", Text: "hello world", ClipID: new("2")},
		{ID: "1", Text: "hello", ClipID: new("1")},
		{ID: "4", Text: "hello world other words", ClipID: nil},
		{ID: "5", Text: "hello world other words", ClipID: new("99")},
	}}
	for index, want := range []string{"20", "2", "3"} {
		trigger, clip, ok := matchVideo(c, "hello world other words", func(count int) int {
			if count != 3 {
				t.Fatalf("variant count=%d", count)
			}
			return index
		})
		if !ok || trigger.ID != want || clip.ID != *trigger.ClipID {
			t.Fatalf("selection=%s", trigger.ID)
		}
	}
	c.Triggers = append(c.Triggers, mediaTrigger{ID: "30", Text: "hello world other", ClipID: new("1")})
	trigger, _, ok := matchVideo(c, "hello world other words", func(int) int { return 0 })
	if !ok || trigger.ID != "30" {
		t.Fatal("longest resolved phrase lost")
	}
	c.Triggers = nil
	if _, _, ok := matchVideo(c, "hello", func(int) int { t.Fatal("selector called without candidates"); return 0 }); ok {
		t.Fatal("empty catalog matched")
	}
}
