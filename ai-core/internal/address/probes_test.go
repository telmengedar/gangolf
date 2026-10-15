package address_test

import "fmt"

type probe struct {
	label label
	line  string
	gated bool
	extra []string
}

var apostropheCodePoints = []string{"'", "’", "´", "`", "‘", "ʼ"}

var probes = buildProbes()

func buildProbes() []probe {
	var out []probe
	for _, a := range apostropheCodePoints {
		out = append(out,
			probe{label: toGangolf, line: fmt.Sprintf("toni: vesper%ss birthday is tomorrow, remember that", a)},
			probe{label: toGangolf, line: fmt.Sprintf("toni: vesper isn%st here, do you know where she is?", a)},
		)
	}
	return append(out,
		probe{label: toGangolf, line: "vesper: hey gango, you there?", extra: []string{"gangolf"}},
		probe{label: toGangolf, line: "vesper: rugenforth wants you to sing a song"},
		probe{label: toGangolf, line: "vesper: rugenforth told me you know everything about dragons"},
		probe{label: toGangolf, line: "vesper: rugenforth asked you something"},
		probe{label: toGangolf, line: "vesper: rugenforth needs a hint, any ideas?"},
		probe{label: toGangolf, line: "vesper: lol you are so funny", extra: []string{"lol"}},
		probe{label: toGangolf, line: "vesper: hey quick question, what is a wizard?", extra: []string{"quicksilver"}},
		probe{label: toGangolf, line: "vesper: yeah, what do you think about that?", extra: []string{"yeahbuddy"}},
		probe{label: toSomeoneElse, line: "toni: vesper you’re late again", gated: true},
		probe{label: toGangolf, line: "toni: hey, vesper said you’re dodgy"},
		probe{label: toGangolf, line: "vesper: rugenforth why does he always die here? any idea?"},
		probe{label: toGangolf, line: "vesper: rugenforth how is he still alive lol, explain"},
		probe{label: toGangolf, line: "toni: vesper when she streams next, remind me"},
		probe{label: toGangolf, line: "toni: rugenforth u vesper streiten wieder, was meinst du?"},
		probe{label: toGangolf, line: "toni: rugenforth u. vesper spielen heute zusammen, wer gewinnt?"},
		probe{label: toTheRoom, line: "vesper: hi chat!", extra: []string{"chat"}},
		probe{label: toGangolf, line: "vesper: chat what do you think about this build?", extra: []string{"chat"}},
		probe{label: toTheRoom, line: "vesper: rugenforth vesper who is right?"},
		probe{label: toSomeoneElse, line: "vesper: Kappa @rugenforth what do you think"},
		probe{label: toSomeoneElse, line: "vesper: hi rugenforth ;)"},
		probe{label: toSomeoneElse, line: "gentlemandapper: hi vesper !", extra: []string{"toni"}},
		probe{label: toGangolf, line: "vesper: rugenforth, ｇａｎｇｏｌｆ who won?"},
		probe{label: toSomeoneElse, line: "vesper: @rugenforth can your bot sing?", gated: true},
		probe{label: toSomeoneElse, line: "toni: vesper you and the bot should duel", gated: true},
		probe{label: toGangolf, line: "toni: rugenforth what do you know about him?"},
		probe{label: toGangolf, line: "toni: vesper what did she say about the boss, do you remember?"},
		probe{label: toSomeoneElse, line: "toni: rugenforth did u eat already?"},
		probe{label: toGangolf, line: "toni: vesper which boss was that, remind me"},
		probe{label: toTheRoom, line: "toni: rugenforth how many deaths now? anyone counting?"},
	)
}
