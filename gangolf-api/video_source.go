package main

import "context"

func (s *server) videoSource(ctx context.Context, cfg *authConfig, id int64) (messageContent, bool, error) {
	rows, err := s.graph.queryResourceItem(ctx, "message", "", cfg.serviceOwner, s.graph.group, id)
	if err != nil {
		return messageContent{}, false, err
	}
	if len(rows) == 0 {
		return messageContent{}, false, nil
	}
	if len(rows) != 1 {
		return messageContent{}, false, errCorrupt
	}
	source, err := decodeMessage(rows[0])
	if err != nil || source.Deleted {
		return source, false, nil
	}
	if err = s.graph.validateRelations(ctx, rows[0], source, cfg.serviceOwner); err != nil {
		return source, false, err
	}
	return source, true, nil
}

func (s *server) publishVideo(id string, source messageContent, trigger mediaTrigger, clip mediaClip) eventEnvelope {
	return s.publishEvent("video.triggered", source.Service, source.Channel, &source.ChannelID, struct {
		MessageID string       `json:"messageId"`
		Trigger   mediaTrigger `json:"trigger"`
		Clip      mediaClip    `json:"clip"`
	}{id, trigger, clip})
}
