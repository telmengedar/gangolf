package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

const mediaCapacity = 8 << 20

var remoteVideoPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

type mediaLegacy struct {
	Snapshot          string `json:"snapshot"`
	ArchiveNodeID     string `json:"archiveNodeId"`
	LegacyID          string `json:"legacyId"`
	LegacyVideoID     string `json:"legacyVideoId,omitempty"`
	InitialResolution string `json:"initialResolution,omitempty"`
}

type mediaClip struct {
	ID            string       `json:"id"`
	Provider      string       `json:"provider"`
	RemoteVideoID string       `json:"remoteVideoId"`
	StartSeconds  *float64     `json:"startSeconds"`
	EndSeconds    *float64     `json:"endSeconds"`
	Title         *string      `json:"title"`
	Legacy        *mediaLegacy `json:"legacy,omitempty"`
}

type mediaTrigger struct {
	ID     string       `json:"id"`
	Text   string       `json:"text"`
	ClipID *string      `json:"clipId"`
	Legacy *mediaLegacy `json:"legacy,omitempty"`
}

type mediaOrigin struct {
	ArchiveRootID string `json:"archiveRootId"`
	Snapshot      string `json:"snapshot"`
	VideoCount    int    `json:"videoCount"`
	TriggerCount  int    `json:"triggerCount"`
}

type mediaCatalog struct {
	SchemaVersion int            `json:"schemaVersion"`
	AuthConfigID  string         `json:"authConfigId"`
	TenantUserID  string         `json:"tenantUserId"`
	NextClipID    int64          `json:"nextClipId"`
	NextTriggerID int64          `json:"nextTriggerId"`
	Clips         []mediaClip    `json:"clips"`
	Triggers      []mediaTrigger `json:"triggers"`
	Origin        mediaOrigin    `json:"origin"`
}

func mediaJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errCorrupt
	}
	d := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				k, ok := key.(string)
				if !ok || seen[k] {
					return errCorrupt
				}
				seen[k] = true
				if err = walk(); err != nil {
					return err
				}
			}
			tok, err = d.Token()
			if err != nil || tok != json.Delim('}') {
				return errCorrupt
			}
		case json.Delim('['):
			for d.More() {
				if err = walk(); err != nil {
					return err
				}
			}
			tok, err = d.Token()
			if err != nil || tok != json.Delim(']') {
				return errCorrupt
			}
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errCorrupt
	}
	return strictJSON(data, target)
}

func validMediaText(s string) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= 256
}
func (c mediaClip) valid() bool {
	if c.Provider != "youtube" || !remoteVideoPattern.MatchString(c.RemoteVideoID) {
		return false
	}
	for _, v := range []*float64{c.StartSeconds, c.EndSeconds} {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0) {
			return false
		}
	}
	start := 0.0
	if c.StartSeconds != nil {
		start = *c.StartSeconds
	}
	if c.EndSeconds != nil && *c.EndSeconds <= start {
		return false
	}
	return c.Title == nil || utf8.ValidString(*c.Title) && utf8.RuneCountInString(*c.Title) <= 256
}
func validMediaLegacy(l *mediaLegacy, origin mediaOrigin, trigger bool) bool {
	if l == nil {
		return true
	}
	if l.Snapshot != origin.Snapshot {
		return false
	}
	if _, ok := resourceID(l.ArchiveNodeID); !ok {
		return false
	}
	if _, ok := resourceID(l.LegacyID); !ok {
		return false
	}
	if trigger {
		_, ok := resourceID(l.LegacyVideoID)
		return ok && (l.InitialResolution == "resolved" || l.InitialResolution == "unresolved")
	}
	return l.LegacyVideoID == "" && l.InitialResolution == ""
}
func (c mediaCatalog) valid(auth, tenant string) bool {
	if c.SchemaVersion != 1 || c.AuthConfigID != auth || c.TenantUserID != tenant || c.Clips == nil || c.Triggers == nil || c.NextClipID <= 0 || c.NextTriggerID <= 0 || c.Origin.VideoCount != 90 || c.Origin.TriggerCount != 122 || len(c.Origin.Snapshot) != 64 {
		return false
	}
	if _, err := hex.DecodeString(c.Origin.Snapshot); err != nil {
		return false
	}
	if _, ok := resourceID(c.Origin.ArchiveRootID); !ok {
		return false
	}
	clips := map[string]bool{}
	triggers := map[string]bool{}
	for _, v := range c.Clips {
		id, ok := resourceID(v.ID)
		if !ok || id >= c.NextClipID || clips[v.ID] || !v.valid() || !validMediaLegacy(v.Legacy, c.Origin, false) {
			return false
		}
		clips[v.ID] = true
	}
	for _, v := range c.Triggers {
		id, ok := resourceID(v.ID)
		if !ok || id >= c.NextTriggerID || triggers[v.ID] || !validMediaText(v.Text) || !validMediaLegacy(v.Legacy, c.Origin, true) {
			return false
		}
		triggers[v.ID] = true
		if v.ClipID == nil {
			if v.Legacy == nil || v.Legacy.InitialResolution != "unresolved" {
				return false
			}
		} else if !clips[*v.ClipID] {
			return false
		}
	}
	return true
}
