package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

type mediaArchiveEnvelope struct {
	SchemaVersion          int             `json:"schemaVersion"`
	Snapshot               string          `json:"snapshot"`
	Key                    string          `json:"key"`
	Raw                    json.RawMessage `json:"raw"`
	RecordSHA256           string          `json:"recordSha256"`
	ProviderInterpretation string          `json:"providerInterpretation,omitempty"`
	VideoResolution        string          `json:"videoResolution,omitempty"`
}
type mediaArchiveVideo struct {
	ID           int64    `json:"id"`
	Source       string   `json:"source"`
	VideoID      string   `json:"videoId"`
	StartSeconds *float64 `json:"startSeconds"`
	EndSeconds   *float64 `json:"endSeconds"`
}
type mediaArchiveTrigger struct {
	ID      int64  `json:"id"`
	Text    string `json:"text"`
	VideoID int64  `json:"videoId"`
}

func archiveCanonical(data []byte) ([]byte, error) {
	var value any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(value); err != nil {
		return nil, err
	}
	result := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	result = bytes.ReplaceAll(result, []byte(`\u2028`), []byte("\u2028"))
	result = bytes.ReplaceAll(result, []byte(`\u2029`), []byte("\u2029"))
	return result, nil
}
func archiveEnvelope(n node, root, group int64, snapshot, kind string) (mediaArchiveEnvelope, error) {
	var p mediaArchiveEnvelope
	typ, _, err := mime.ParseMediaType(n.ContentType)
	if err != nil || typ != "application/json" || n.Content == nil || n.Owner != 4 || n.Root != root || !n.privateAccess() || n.SkipEmbedding == nil || !*n.SkipEmbedding || n.Type != "legacy-archive-"+kind || mediaJSON([]byte(*n.Content), &p) != nil || p.SchemaVersion != 1 || p.Snapshot != snapshot || !plainLinkBetween(n.Links, n.ID, group) {
		return p, errCorrupt
	}
	var raw map[string]json.RawMessage
	if mediaJSON(p.Raw, &raw) != nil || raw == nil {
		return p, errCorrupt
	}
	canonical, err := archiveCanonical(p.Raw)
	if err != nil {
		return p, errCorrupt
	}
	sum := sha256.Sum256(canonical)
	digest := hex.EncodeToString(sum[:])
	plural := kind + "s"
	if p.RecordSHA256 != digest || p.Key != plural+":"+digest+":1" || n.Name != "legacy-archive:"+snapshot+":"+p.Key {
		return p, errCorrupt
	}
	return p, nil
}
func projectMediaArchive(videos, triggers []node, root, videoGroup, triggerGroup int64, snapshot, auth, tenant string) (mediaCatalog, error) {
	c := mediaCatalog{SchemaVersion: 1, AuthConfigID: auth, TenantUserID: tenant, NextClipID: 1, NextTriggerID: 1, Clips: []mediaClip{}, Triggers: []mediaTrigger{}, Origin: mediaOrigin{ArchiveRootID: strconv.FormatInt(root, 10), Snapshot: snapshot, VideoCount: 90, TriggerCount: 122}}
	if len(videos) != 90 || len(triggers) != 122 || root == videoGroup || root == triggerGroup || videoGroup == triggerGroup {
		return c, errCorrupt
	}
	videoIDs := map[int64]string{}
	graphIDs := map[int64]int64{}
	expected := map[int64][]relation{}
	allIDs := map[int64]bool{}
	for _, n := range videos {
		p, err := archiveEnvelope(n, root, videoGroup, snapshot, "video")
		if err != nil || p.ProviderInterpretation != "Youtube" || p.VideoResolution != "" {
			return c, errCorrupt
		}
		var raw mediaArchiveVideo
		if mediaJSON(p.Raw, &raw) != nil || raw.ID <= 0 || raw.Source != "Youtube" || videoIDs[raw.ID] != "" || allIDs[n.ID] {
			return c, errCorrupt
		}
		var fields map[string]json.RawMessage
		json.Unmarshal(p.Raw, &fields)
		for _, key := range []string{"startSeconds", "endSeconds"} {
			if v, ok := fields[key]; ok && !nonNull(v) {
				return c, errCorrupt
			}
		}
		id := strconv.FormatInt(raw.ID, 10)
		v := mediaClip{ID: id, Provider: "youtube", RemoteVideoID: raw.VideoID, StartSeconds: raw.StartSeconds, EndSeconds: raw.EndSeconds, Legacy: &mediaLegacy{Snapshot: snapshot, ArchiveNodeID: strconv.FormatInt(n.ID, 10), LegacyID: id}}
		if !v.valid() {
			return c, errCorrupt
		}
		c.Clips = append(c.Clips, v)
		c.NextClipID = max(c.NextClipID, raw.ID+1)
		videoIDs[raw.ID] = id
		graphIDs[raw.ID] = n.ID
		allIDs[n.ID] = true
		expected[n.ID] = []relation{{Source: n.ID, Target: videoGroup, Type: "None"}}
	}
	resolved := 0
	seen := map[int64]bool{}
	for _, n := range triggers {
		p, err := archiveEnvelope(n, root, triggerGroup, snapshot, "trigger")
		if err != nil || p.ProviderInterpretation != "" {
			return c, errCorrupt
		}
		var raw mediaArchiveTrigger
		if mediaJSON(p.Raw, &raw) != nil || raw.ID <= 0 || raw.VideoID <= 0 || seen[raw.ID] || allIDs[n.ID] || !validMediaText(raw.Text) {
			return c, errCorrupt
		}
		seen[raw.ID] = true
		allIDs[n.ID] = true
		id := strconv.FormatInt(raw.ID, 10)
		v := mediaTrigger{ID: id, Text: raw.Text, Legacy: &mediaLegacy{Snapshot: snapshot, ArchiveNodeID: strconv.FormatInt(n.ID, 10), LegacyID: id, LegacyVideoID: strconv.FormatInt(raw.VideoID, 10), InitialResolution: "unresolved"}}
		expected[n.ID] = []relation{{Source: n.ID, Target: triggerGroup, Type: "None"}}
		if target, ok := videoIDs[raw.VideoID]; ok {
			if p.VideoResolution != "resolved-numeric-FK" {
				return c, errCorrupt
			}
			v.ClipID = &target
			v.Legacy.InitialResolution = "resolved"
			resolved++
			edge := relation{Source: n.ID, Target: graphIDs[raw.VideoID], Type: "Unidirectional", Context: "references-video"}
			expected[n.ID] = append(expected[n.ID], edge)
			expected[edge.Target] = append(expected[edge.Target], edge)
		} else if p.VideoResolution != "unresolved" {
			return c, errCorrupt
		}
		c.Triggers = append(c.Triggers, v)
		c.NextTriggerID = max(c.NextTriggerID, raw.ID+1)
	}
	if resolved != 107 || !c.valid(auth, tenant) {
		return c, errCorrupt
	}
	for _, n := range append(append([]node{}, videos...), triggers...) {
		if !mediaEdgesEqual(n.Links, expected[n.ID]) {
			return c, errCorrupt
		}
	}
	return c, nil
}
func mediaEdgesEqual(actual, expected []relation) bool {
	normalize := func(e relation) relation {
		if e.Type == "None" && e.Source > e.Target {
			e.Source, e.Target = e.Target, e.Source
		}
		return e
	}
	if len(actual) != len(expected) {
		return false
	}
	seen := map[relation]bool{}
	for _, e := range actual {
		e = normalize(e)
		if seen[e] {
			return false
		}
		seen[e] = true
	}
	for _, e := range expected {
		if !seen[normalize(e)] {
			return false
		}
	}
	return true
}
func (m *mediaStore) archiveNodes(ctx context.Context, root, group int64, kind string) ([]node, error) {
	result := []node{}
	offset, total := 0, -1
	last := int64(0)
	for {
		q := url.Values{"rootNodeId": {strconv.FormatInt(root, 10)}, "linkedto": {strconv.FormatInt(group, 10)}, "type": {"legacy-archive-" + kind}, "fields": {"id,name,type,rootNodeId,ownerId,access,skipEmbedding,content,contentType,linkDetails"}, "sort": {"id"}, "descending": {"false"}, "count": {"500"}, "continue": {strconv.Itoa(offset)}}
		data, err := m.graph.request(ctx, http.MethodGet, "/nodes", q, nil)
		if err != nil {
			return nil, err
		}
		var page graphPage
		if json.Unmarshal(data, &page) != nil || page.Result == nil || page.Total < 0 || total >= 0 && total != page.Total {
			return nil, errCorrupt
		}
		total = page.Total
		if total > 212 {
			return nil, errCorrupt
		}
		for _, n := range page.Result {
			if n.ID <= last {
				return nil, errCorrupt
			}
			last = n.ID
			result = append(result, n)
		}
		next := offset + len(page.Result)
		if next > total || (page.Continue != nil) != (next < total) || page.Continue != nil && (*page.Continue != next || next <= offset) {
			return nil, errCorrupt
		}
		if next == total {
			return result, nil
		}
		offset = next
	}
}
func (s *server) seedMedia(ctx context.Context, root, videoGroup, triggerGroup int64, snapshot string) (string, error) {
	m := s.media
	if m == nil || len(snapshot) != 64 || root <= 0 || videoGroup <= 0 || triggerGroup <= 0 || root == m.graph.root {
		return "", errCorrupt
	}
	if _, err := hex.DecodeString(snapshot); err != nil {
		return "", errCorrupt
	}
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		return "", err
	}
	tenant, ok := cfg.Users[m.tenant]
	if !ok || !tenant.Enabled || !hasHost(tenant.Permissions) {
		return "", errCorrupt
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	target, err := m.metadata(ctx)
	if err != nil {
		return "", err
	}
	if target.Content != nil && *target.Content != "" {
		c, err := m.decode(target)
		if err != nil || c.Origin.ArchiveRootID != strconv.FormatInt(root, 10) || c.Origin.Snapshot != snapshot {
			return "", errCorrupt
		}
		return "already_initialized", nil
	}
	archiveRoot, err := m.readNode(ctx, root)
	if err != nil {
		return "", err
	}
	name := "legacy-archive:" + snapshot
	if archiveRoot.Type != "project" || archiveRoot.Name != name || archiveRoot.Owner != 4 || !archiveRoot.privateAccess() || archiveRoot.Root != 0 || archiveRoot.SkipEmbedding == nil || !*archiveRoot.SkipEmbedding {
		return "", errCorrupt
	}
	for _, g := range []struct {
		id   int64
		kind string
	}{{videoGroup, "video"}, {triggerGroup, "trigger"}} {
		n, err := m.readNode(ctx, g.id)
		if err != nil {
			return "", err
		}
		if n.Type != "legacy-archive-group" || n.Name != name+":group:legacy-archive-"+g.kind || n.Root != root || n.Owner != 4 || !n.privateAccess() || n.SkipEmbedding == nil || !*n.SkipEmbedding || !plainLinkBetween(n.Links, n.ID, root) || !plainLinkBetween(archiveRoot.Links, root, n.ID) {
			return "", errCorrupt
		}
	}
	videos, err := m.archiveNodes(ctx, root, videoGroup, "video")
	if err != nil {
		return "", err
	}
	triggers, err := m.archiveNodes(ctx, root, triggerGroup, "trigger")
	if err != nil {
		return "", err
	}
	c, err := projectMediaArchive(videos, triggers, root, videoGroup, triggerGroup, snapshot, m.authID, m.tenant)
	if err != nil {
		return "", err
	}
	if err = m.save(ctx, c); err != nil {
		return "", err
	}
	return "initialized", nil
}
func seedMediaCommand(args []string) error {
	flags := flag.NewFlagSet("seed-media", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.Int64("archive-root", 0, "")
	videos := flags.Int64("videos-group", 0, "")
	triggers := flags.Int64("triggers-group", 0, "")
	snapshot := flags.String("snapshot", "", "")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return errCorrupt
	}
	s, _, err := bootstrap()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := s.seedMedia(ctx, *root, *videos, *triggers, *snapshot)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, result)
	return err
}
