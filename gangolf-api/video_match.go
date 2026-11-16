package main

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

func normalizeVideoPhrase(text string) string {
	text = strings.TrimFunc(text, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) })
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

func messageVideoText(m messageContent) string {
	var text strings.Builder
	for _, part := range m.Chunks {
		if part.Type == 0 {
			text.WriteString(part.Data)
		} else if part.Type == 1 {
			text.WriteString(part.Code)
		}
	}
	return normalizeVideoPhrase(text.String())
}

func matchVideo(c mediaCatalog, text string, choose func(int) int) (mediaTrigger, mediaClip, bool) {
	clips := make(map[string]mediaClip, len(c.Clips))
	for _, clip := range c.Clips {
		clips[clip.ID] = clip
	}
	phrase := ""
	length := 0
	lowest := int64(0)
	groups := map[string][]mediaTrigger{}
	for _, trigger := range c.Triggers {
		if trigger.ClipID == nil {
			continue
		}
		if _, ok := clips[*trigger.ClipID]; !ok {
			continue
		}
		normalized := normalizeVideoPhrase(trigger.Text)
		if normalized == "" || text != normalized && !strings.HasPrefix(text, normalized+" ") && !strings.HasSuffix(text, " "+normalized) {
			continue
		}
		size := utf8.RuneCountInString(normalized)
		id, _ := resourceID(trigger.ID)
		if size > length || size == length && normalized != phrase && id < lowest {
			phrase, length, lowest = normalized, size, id
		}
		groups[normalized] = append(groups[normalized], trigger)
		if normalized == phrase && id < lowest {
			lowest = id
		}
	}
	candidates := groups[phrase]
	if len(candidates) == 0 {
		return mediaTrigger{}, mediaClip{}, false
	}
	trigger := candidates[choose(len(candidates))]
	return trigger, clips[*trigger.ClipID], true
}
