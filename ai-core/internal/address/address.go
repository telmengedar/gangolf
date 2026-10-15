package address

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const minNameRunes = 4

const apostrophes = "'’‘´`ʼ"

const addressEnds = ",:!?."

var greetings = wordSet("hi", "hey", "hello", "yo", "elo", "hallo", "moin", "sup", "oi", "heya", "hiya", "morning", "evening", "night", "gn", "gm")

var questionWords = wordSet("what", "where", "when", "who", "whom", "whose", "why", "how", "which")

var secondPerson = wordSet("you", "your", "youre", "yours")

var thirdPerson = wordSet("he", "she", "they", "him", "her", "his", "hers", "them", "their", "theirs", "himself", "herself", "themselves")

var roomWords = wordSet("chat")

const sentenceEnds = "?!.…？！‽⁇⁈⁉。．"

var auxiliaries = wordSet("do", "did", "are", "were", "have", "had", "can", "could", "will", "would", "should", "shall", "may", "might")

type token struct {
	word    string
	mention bool
	ends    bool
	next    rune
	breaks  bool
}

// ForSomeoneElse reports whether a line is plainly addressed to another chatter.
func ForSomeoneElse(speaker, text string, roster, self []string) bool {
	lower := strings.ToLower(text)
	for _, name := range self {
		if name != "" && strings.Contains(lower, strings.ToLower(name)) {
			return false
		}
	}
	tokens := tokenize(lower)
	greeted := false
	for len(tokens) > 0 && !tokens[0].mention && greetings[tokens[0].word] {
		tokens = tokens[1:]
		greeted = true
	}
	if len(tokens) == 0 || !isCandidate(tokens[0].word, speaker, roster, self) {
		return false
	}
	first := tokens[0]
	if first.mention {
		return true
	}
	if greeted && (first.ends || strings.ContainsRune(addressEnds, first.next)) {
		return true
	}
	return secondPersonForm(tokens[1:])
}

func secondPersonForm(rest []token) bool {
	var sentence []string
	for _, t := range rest {
		if t.breaks {
			break
		}
		sentence = append(sentence, t.word)
	}
	if len(sentence) == 0 {
		return false
	}
	for _, word := range sentence {
		if thirdPerson[word] {
			return false
		}
	}
	switch first := sentence[0]; {
	case secondPerson[first]:
		return true
	case auxiliaries[first]:
		return len(sentence) > 1 && secondPerson[sentence[1]]
	case questionWords[first]:
		for _, word := range sentence[1:] {
			if secondPerson[word] {
				return true
			}
		}
	}
	return false
}

func isCandidate(word, speaker string, roster, self []string) bool {
	if utf8.RuneCountInString(word) < minNameRunes || roomWords[word] {
		return false
	}
	for _, name := range self {
		if strings.EqualFold(name, word) {
			return false
		}
	}
	if strings.EqualFold(speaker, word) {
		return false
	}
	for _, name := range roster {
		if strings.EqualFold(name, word) {
			return true
		}
	}
	return false
}

func tokenize(text string) []token {
	runes := []rune(text)
	var tokens []token
	previousEnd := 0
	for i := 0; i < len(runes); {
		if !isWordRune(runes[i]) {
			i++
			continue
		}
		start := i
		for i < len(runes) && isWordRune(runes[i]) {
			i++
		}
		t := token{
			word:    string(runes[start:i]),
			mention: start > 0 && runes[start-1] == '@',
			ends:    strings.TrimSpace(string(runes[i:])) == "",
			breaks:  strings.ContainsAny(string(runes[previousEnd:start]), sentenceEnds),
		}
		previousEnd = i
		if i < len(runes) {
			t.next = runes[i]
		}
		tokens = append(tokens, t)
	}
	return tokens
}

func isWordRune(r rune) bool {
	if strings.ContainsRune(apostrophes, r) {
		return false
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func wordSet(words ...string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, word := range words {
		set[word] = true
	}
	return set
}
