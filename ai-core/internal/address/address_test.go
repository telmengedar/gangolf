package address_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/telmengedar/gangolf/ai-core/internal/address"
)

type verdict struct {
	speaker string
	text    string
	gated   bool
}

func check(t *testing.T, roster []string, cases []verdict) {
	t.Helper()
	for _, c := range cases {
		if got := address.ForSomeoneElse(c.speaker, c.text, roster, persona); got != c.gated {
			t.Errorf("%s: %q gated = %v, want %v", c.speaker, c.text, got, c.gated)
		}
	}
}

func TestALineOpeningWithAnotherChattersNameIsForThem(t *testing.T) {
	check(t, chatters, []verdict{
		{"gentlemandapper", "rugenforth what time do you start tomorrow?", true},
		{"gentlemandapper", "Rugenforth what time do you start tomorrow?", true},
		{"gentlemandapper", "rugenforth did you eat already?", true},
		{"gentlemandapper", "rugenforth, did you eat?", true},
		{"gentlemandapper", "rugenforth: did you eat?", true},
		{"toni", "vesper you're late", true},
		{"toni", "vesper you’re late", true},
		{"toni", "Vesper WHAT do you think", true},
		{"toni", "Hi Vesper", true},
		{"toni", "vesper your turn", true},
		{"alice", "tester you there?", true},
		{"vesper", "you have no clue what you're talking about", false},
		{"vesper", "what level is this rugenforth?", false},
		{"vesper", "rugenforth did eat already?", false},
		{"vesper", "rugenforth", false},
	})
}

func TestEverySecondPersonWordOpeningTheSentenceMakesAnAddress(t *testing.T) {
	for _, word := range []string{"you", "your", "youre", "yours"} {
		if !address.ForSomeoneElse("toni", "vesper "+word+" now", chatters, persona) {
			t.Errorf("word %q after a name was not an address", word)
		}
	}
}

func TestAQuestionWordNeedsASecondPersonWordInItsSentence(t *testing.T) {
	for _, word := range []string{"what", "where", "when", "who", "whom", "whose", "why", "how", "which"} {
		if !address.ForSomeoneElse("toni", "vesper "+word+" do you know", chatters, persona) {
			t.Errorf("question word %q with a second-person word was not an address", word)
		}
		if address.ForSomeoneElse("toni", "vesper "+word+" is that", chatters, persona) {
			t.Errorf("bare question word %q was an address", word)
		}
	}
	check(t, chatters, []verdict{
		{"gentlemandapper", "rugenforth what level is this?", false},
		{"toni", "vesper which boss was that, remind me", false},
		{"toni", "vesper what time do you start tomorrow?", true},
		{"toni", "vesper what is that, do you know", true},
		{"toni", "rugenforth what is that? do you know?", false},
		{"toni", "rugenforth what is that! you know", false},
		{"toni", "rugenforth what is that. you know", false},
		{"toni", "rugenforth where, you know", true},
	})
}

func TestAThirdPersonPronounMakesTheNameATopic(t *testing.T) {
	check(t, chatters, []verdict{
		{"vesper", "rugenforth why does he always die here? any idea?", false},
		{"vesper", "rugenforth how is he still alive lol, explain", false},
		{"toni", "vesper when she streams next, remind me", false},
		{"toni", "rugenforth what do you know about him?", false},
		{"toni", "vesper what did she say about the boss, do you remember?", false},
		{"toni", "vesper you and him should duel", false},
		{"toni", "vesper did you see her", false},
		{"toni", "vesper you should see this? he is back", true},
		{"toni", "vesper you should see this", true},
	})
	for _, word := range []string{"he", "she", "they", "him", "her", "his", "hers", "them", "their", "theirs", "himself", "herself", "themselves"} {
		if address.ForSomeoneElse("toni", "vesper do you know "+word, chatters, persona) {
			t.Errorf("pronoun %q in the sentence did not stop the gate", word)
		}
	}
}

func TestUIsNotASecondPersonWord(t *testing.T) {
	check(t, chatters, []verdict{
		{"toni", "rugenforth u vesper streiten wieder, was meinst du?", false},
		{"toni", "rugenforth u. vesper spielen heute zusammen, wer gewinnt?", false},
		{"toni", "rugenforth did u eat already?", false},
		{"toni", "rugenforth what do u think", false},
		{"toni", "rugenforth u know", false},
		{"toni", "rugenforth ur lustig heute, was meint ihr?", false},
		{"toni", "rugenforth ya genau, was sagst du dazu?", false},
		{"toni", "rugenforth what is ur plan", false},
		{"toni", "rugenforth did ya see it", false},
	})
}

func TestEverySentenceEndSeparatesTheNameFromTheNextSentence(t *testing.T) {
	for _, end := range []string{"?", "!", ".", "…", "？", "！", "‽", "⁇", "⁈", "⁉", "。", "．"} {
		for _, text := range []string{
			"rugenforth what is that" + end + " do you know?",
			"rugenforth what is that" + end + "do you know?",
		} {
			if address.ForSomeoneElse("toni", text, chatters, persona) {
				t.Errorf("%q was gated across the sentence end %q", text, end)
			}
		}
	}
	check(t, chatters, []verdict{
		{"toni", "rugenforth which level was that… do you know, bot?", false},
		{"toni", "vesper what a run… can you summarize it?", false},
		{"toni", "rugenforth what is that？ do you know?", false},
		{"toni", "rugenforth what was that‽ do you know?", false},
		{"toni", "rugenforth which level was that, do you know, bot?", true},
		{"toni", "vesper what a run, can you summarize it?", true},
	})
}

func TestChatIsNeverAChattersName(t *testing.T) {
	roster := append([]string{"chat"}, chatters...)
	check(t, roster, []verdict{
		{"vesper", "hi chat!", false},
		{"vesper", "chat what do you think about this build?", false},
		{"vesper", "@chat what do you think", false},
		{"vesper", "Chat you there", false},
		{"toni", "hi vesper", true},
	})
}

func TestTheConfiguredBotNameIsNeverACandidateEvenWhenCaseFoldingHidesIt(t *testing.T) {
	roster := []string{"sasha", "vesper"}
	self := []string{"Sasha", "Gangolf"}
	if address.ForSomeoneElse("toni", "ſasha what do you think", roster, self) {
		t.Error("a case-folded spelling of the bot's own name was a candidate")
	}
	if !address.ForSomeoneElse("toni", "vesper what do you think", roster, self) {
		t.Error("another chatter beside the bot name stopped being a candidate")
	}
}

func TestEveryAuxiliaryNeedsASecondPersonWordAfterIt(t *testing.T) {
	for _, word := range []string{"do", "did", "are", "were", "have", "had", "can", "could", "will", "would", "should", "shall", "may", "might"} {
		if !address.ForSomeoneElse("toni", "vesper "+word+" you know", chatters, persona) {
			t.Errorf("auxiliary %q plus you was not an address", word)
		}
		if address.ForSomeoneElse("toni", "vesper "+word+" know", chatters, persona) {
			t.Errorf("auxiliary %q without a second-person word was an address", word)
		}
	}
	if address.ForSomeoneElse("toni", "vesper do", chatters, persona) {
		t.Error("a trailing auxiliary was an address")
	}
}

func TestAnAtMentionOfAnotherChatterIsForThem(t *testing.T) {
	check(t, chatters, []verdict{
		{"vesper", "@rugenforth you're putty again", true},
		{"gentlemandapper", "@Vesper you missed the boss fight lol", true},
		{"gentlemandapper", "@vesper is the best", true},
		{"gentlemandapper", "@vesper", true},
		{"vesper", "@somebody you missed it", false},
		{"vesper", "@rugen you missed it", false},
		{"vesper", "@vesper2 you missed it", false},
		{"vesper", "you missed it @rugenforth", false},
		{"toni", "@hey vesper you there", false},
		{"vesper", "@vesper you missed it", false},
		{"vesper", "@lol you are funny", false},
	})
}

func TestALineNamingGangolfAnywhereIsNeverForSomeoneElse(t *testing.T) {
	check(t, chatters, []verdict{
		{"vesper", "rugenforth, gangolf: who won?", false},
		{"gentlemandapper", "@Vesper how was your long talk with gangolf yesterday?", false},
		{"gentlemandapper", "rugenforth, ask GANGOLF about it", false},
		{"gentlemandapper", "@vesper GangolfBot is broken", false},
		{"vesper", "hi gangolfs", false},
	})
}

func TestALeadingNameFollowedByAnythingElseIsNotAnAddress(t *testing.T) {
	check(t, chatters, []verdict{
		{"vesper", "rugenforth wants you to sing a song", false},
		{"vesper", "rugenforth told me you know everything about dragons", false},
		{"vesper", "rugenforth asked you something", false},
		{"vesper", "rugenforth needs a hint, any ideas?", false},
		{"vesper", "rugenforth is so bad at this lol", false},
		{"rugenforth", "vesper said you were dodgy", false},
		{"toni", "vesper is right, you are wrong", false},
		{"toni", "hey, vesper said you're dodgy", false},
		{"vesper", "rugenforth that jump was insane", false},
		{"vesper", "rugenforth", false},
		{"vesper", "rugenforth, back me up here", false},
	})
}

func TestOnlyAnExactLoginCountsAsAName(t *testing.T) {
	check(t, []string{"rugenforth", "quicksilver", "gangolf", "vesper"}, []verdict{
		{"vesper", "hey rugen, how was your weekend?", false},
		{"vesper", "rugen, you there?", false},
		{"vesper", "hey rugen you there", false},
		{"vesper", "hey gango, you there?", false},
		{"vesper", "hey quick question, what is a wizard?", false},
		{"vesper", "rugenfort did you see it", false},
		{"vesper", "rugenforth2 did you see it", false},
		{"vesper", "rugenforths did you see it", false},
		{"toni", "rugenforth did you see it", true},
	})
}

func TestTheSpeakersOwnNameIsNotAnAddress(t *testing.T) {
	check(t, chatters, []verdict{
		{"vesper", "vesper what are you doing", false},
		{"Vesper", "vesper what are you doing", false},
		{"VESPER", "@vesper what are you doing", false},
		{"rugenforth", "hi rugenforth", false},
		{"rugenforth", "hi vesper", true},
	})
}

func TestSelfNamesAndShortNamesAreNeverCandidates(t *testing.T) {
	roster := []string{"vesper", "gangolf", "gangolfbot", "lol", "bob", "abc"}
	check(t, roster, []verdict{
		{"vesper", "gangolf what do you think", false},
		{"vesper", "gangolfbot what do you think", false},
		{"vesper", "@gangolf what do you think", false},
		{"vesper", "hi gangolf", false},
		{"toni", "lol you are so funny", false},
		{"toni", "hi bob", false},
		{"toni", "@lol what do you think", false},
		{"toni", "abc what do you think", false},
		{"toni", "vesper what do you think", true},
	})
	selfOnly := []string{"Helper", "vesper"}
	if address.ForSomeoneElse("toni", "helper what do you think", selfOnly, []string{"Helper"}) {
		t.Error("a self name in the roster was a candidate")
	}
	if !address.ForSomeoneElse("toni", "vesper what do you think", selfOnly, []string{"Helper"}) {
		t.Error("another name beside a self name stopped being a candidate")
	}
}

func TestANameOfExactlyFourCharactersIsACandidate(t *testing.T) {
	check(t, []string{"toni", "tom"}, []verdict{
		{"vesper", "toni what do you think", true},
		{"vesper", "tom what do you think", false},
	})
}

func TestAGreetedNameMustEndTheAddress(t *testing.T) {
	check(t, chatters, []verdict{
		{"gentlemandapper", "hi vesper", true},
		{"gentlemandapper", "hey rugenforth!", true},
		{"gentlemandapper", "hello, vesper", true},
		{"gentlemandapper", "hi vesper.", true},
		{"gentlemandapper", "hi vesper, nice stream", true},
		{"gentlemandapper", "hi vesper: nice stream", true},
		{"gentlemandapper", "hi vesper?", true},
		{"gentlemandapper", "hi vesper   ", true},
		{"gentlemandapper", "hey yo vesper", true},
		{"toni", "hey, vesper said you're dodgy", false},
		{"toni", "hi vesper is late", false},
		{"toni", "hi vesper lol", false},
		{"gentlemandapper", "hi vesper !", false},
		{"vesper", "hi rugenforth ;)", false},
		{"toni", "hi vesper ;)", false},
		{"toni", "hi vesper’s birthday", false},
		{"toni", "hi vesper and tester", false},
		{"toni", "vesper", false},
		{"toni", "vesper!", false},
		{"toni", "hi", false},
		{"toni", "hey hi", false},
	})
}

func TestEveryGreetingIsSkippedBeforeTheName(t *testing.T) {
	for _, greeting := range []string{"hi", "hey", "hello", "yo", "elo", "hallo", "moin", "sup", "oi", "heya", "hiya", "morning", "evening", "night", "gn", "gm"} {
		if !address.ForSomeoneElse("toni", greeting+" vesper", chatters, persona) {
			t.Errorf("greeting %q before a name was not skipped", greeting)
		}
	}
}

func TestEveryApostropheGivesTheSameVerdict(t *testing.T) {
	for _, a := range apostropheCodePoints {
		for _, text := range []string{
			fmt.Sprintf("vesper%ss birthday is tomorrow, remember that", a),
			fmt.Sprintf("vesper isn%st here, do you know where she is?", a),
		} {
			if address.ForSomeoneElse("toni", text, chatters, persona) {
				t.Errorf("%q was gated", text)
			}
		}
		if text := fmt.Sprintf("vesper you%sre late", a); !address.ForSomeoneElse("toni", text, chatters, persona) {
			t.Errorf("%q was not gated", text)
		}
	}
}

func TestAnUnderscoreBelongsToAName(t *testing.T) {
	check(t, []string{"toni_g"}, []verdict{
		{"vesper", "toni_g did you see it", true},
		{"vesper", "toni did you see it", false},
	})
}

func TestUnicodeNamesAndCasingAreMatchedWithoutRegardToCase(t *testing.T) {
	check(t, []string{"Jürgen", "ÉLODIE"}, []verdict{
		{"toni", "jürgen did you see it", true},
		{"toni", "@ÉLODIE hi", true},
		{"toni", "hi élodie", true},
	})
	check(t, []string{"jür"}, []verdict{
		{"toni", "jür what do you think", false},
	})
}

func TestALineWithoutAnyWordIsNeverForSomeoneElse(t *testing.T) {
	check(t, chatters, []verdict{
		{"vesper", "", false},
		{"vesper", "   ", false},
		{"vesper", "?!", false},
		{"vesper", "hey", false},
	})
}

func TestAnEmptySelfNameDoesNotDisableTheGate(t *testing.T) {
	if !address.ForSomeoneElse("gentlemandapper", "rugenforth did you eat?", chatters, []string{"", "Gangolf"}) {
		t.Error("an empty self name stopped the gate from firing")
	}
}

func TestNoLineAddressedToGangolfInTheRecordedCorpusIsGated(t *testing.T) {
	for _, r := range corpus {
		speaker, text, ok := strings.Cut(r.line, ": ")
		if !ok {
			t.Fatalf("corpus line %q has no speaker", r.line)
		}
		got := address.ForSomeoneElse(speaker, text, chatters, persona)
		if r.label == toGangolf && got {
			t.Errorf("addressed line gated: %q", r.line)
		}
		if got != r.gated {
			t.Errorf("%q gated = %v, want %v", r.line, got, r.gated)
		}
	}
	for _, p := range probes {
		speaker, text, ok := strings.Cut(p.line, ": ")
		if !ok {
			t.Fatalf("probe line %q has no speaker", p.line)
		}
		got := address.ForSomeoneElse(speaker, text, append(append([]string{}, chatters...), p.extra...), persona)
		if p.label == toGangolf && got {
			t.Errorf("addressed probe gated: %q", p.line)
		}
		if got != p.gated {
			t.Errorf("%q gated = %v, want %v", p.line, got, p.gated)
		}
	}
}

func TestTheRecordedCorpusKeepsItsLabelledShape(t *testing.T) {
	var counts [3]int
	var gated int
	for _, r := range corpus {
		counts[r.label]++
		if r.gated {
			gated++
		}
	}
	var probeCounts [4]int
	var probesGated int
	for _, p := range probes {
		probeCounts[p.label]++
		if p.gated {
			probesGated++
		}
	}
	if counts != [3]int{44, 21, 29} || gated != 8 {
		t.Fatalf("corpus holds addressed/other/room = %v and %d gated, want [44 21 29] and 8", counts, gated)
	}
	if probeCounts != [4]int{49, 12, 3, 7} || probesGated != 13 {
		t.Fatalf("probes hold addressed/other/room/accepted-residual = %v and %d gated, want [49 12 3 7] and 13", probeCounts, probesGated)
	}
}
