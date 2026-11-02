package main

import "context"

func (g *graph) validateInboundStaging(ctx context.Context, n node, p twitchProfile, event twitchEvent, owner int64) error {
	seen := map[string]bool{}
	for _, edge := range n.Links {
		key := edge.Context
		if edge.Type == "None" && edge.Context == "" && (edge.Source == n.ID && edge.Target == g.group || edge.Target == n.ID && edge.Source == g.group) {
			key = "membership"
		} else {
			if edge.Source != n.ID || edge.Type != "Unidirectional" {
				return errCorrupt
			}
			switch edge.Context {
			case "in-channel":
				if edge.Target != mustResourceID(p.ChannelID) {
					return errCorrupt
				}
			case "authored-by":
				user, err := g.readEntity(ctx, edge.Target, "user", owner)
				if err != nil {
					return err
				}
				if _, err := identityValue(user, "user", "Twitch", event.ChatterUserID); err != nil {
					return err
				}
			case "replies-to":
				if event.Reply == nil || edge.Target >= n.ID {
					return errCorrupt
				}
				parent, err := g.readEntity(ctx, edge.Target, "message", owner)
				if err != nil {
					return err
				}
				value, err := decodeMessage(parent)
				if err != nil || parent.Content == nil || value.Deleted || value.ChannelID != p.ChannelID || parent.Name != twitchMessageName(p.BroadcasterUserID, event.Reply.ParentMessageID) {
					return errCorrupt
				}
			default:
				return errCorrupt
			}
		}
		if seen[key] {
			return errCorrupt
		}
		seen[key] = true
	}
	return nil
}
