package main

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode"
)

type mediaStore struct {
	graph      *graph
	catalogID  int64
	tenant     string
	authID     string
	mu         sync.Mutex
	generation uint64
}

func mediaCredential(path string) (string, error) {
	expected, err := os.Lstat(path)
	if err != nil || !privateCredentialInfo(expected, false) || expected.Mode().Perm() != 0600 {
		return "", errUnavailable
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", errUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !privateCredentialInfo(info, false) || !os.SameFile(expected, info) {
		return "", errUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(data) > 16384 {
		return "", errUnavailable
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		k, v, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "=")
		if !ok || fields[k] != "" || v == "" || (k != "User" && k != "UserId" && k != "Key") {
			return "", errUnavailable
		}
		fields[k] = v
	}
	if len(fields) != 3 || fields["UserId"] != "4" || strings.ContainsFunc(fields["Key"], unicode.IsSpace) {
		return "", errUnavailable
	}
	return fields["Key"], nil
}

func configureMedia(primary *graph) (*mediaStore, error) {
	names := []string{"GANGOLF_MEDIA_DIVOID_TOKEN_FILE", "GANGOLF_MEDIA_ROOT_ID", "GANGOLF_MEDIA_CATALOG_ID", "GANGOLF_MEDIA_TENANT_USER_ID"}
	values := make([]string, 4)
	set := 0
	for i, n := range names {
		values[i] = os.Getenv(n)
		if values[i] != "" {
			set++
		}
	}
	if set == 0 {
		return nil, nil
	}
	if set != 4 {
		return nil, errUnavailable
	}
	root, ok := resourceID(values[1])
	if !ok {
		return nil, errUnavailable
	}
	catalog, ok := resourceID(values[2])
	if !ok || root == catalog || root == primary.root || catalog == primary.config || catalog == primary.group {
		return nil, errUnavailable
	}
	if _, ok = resourceID(values[3]); !ok || !filepath.IsAbs(values[0]) {
		return nil, errUnavailable
	}
	token, err := mediaCredential(values[0])
	if err != nil {
		return nil, err
	}
	return &mediaStore{graph: &graph{base: primary.base, http: primary.http, token: token, root: root}, catalogID: catalog, tenant: values[3], authID: strconv.FormatInt(primary.config, 10)}, nil
}

func (m *mediaStore) readNode(ctx context.Context, id int64) (node, error) {
	q := url.Values{"id": {strconv.FormatInt(id, 10)}, "fields": {"id,name,type,rootNodeId,ownerId,access,content,contentType,linkDetails,skipEmbedding"}, "count": {"2"}}
	data, err := m.graph.request(ctx, http.MethodGet, "/nodes", q, nil)
	if err != nil {
		return node{}, err
	}
	var page graphPage
	var fields map[string]json.RawMessage
	if mediaJSON(data, &fields) != nil || json.Unmarshal(data, &page) != nil || page.Total != 1 || len(page.Result) != 1 || page.Continue != nil || page.Result[0].ID != id {
		return node{}, errCorrupt
	}
	return page.Result[0], nil
}

func (m *mediaStore) metadata(ctx context.Context) (node, error) {
	owner, err := m.graph.serviceIdentity(ctx)
	if err != nil {
		return node{}, err
	}
	root, err := m.readNode(ctx, m.graph.root)
	if err != nil {
		return node{}, err
	}
	n, err := m.readNode(ctx, m.catalogID)
	if err != nil {
		return node{}, err
	}
	if owner != 4 || root.Owner != owner || !root.privateAccess() || root.Type != "project" || root.Name != "Gangolf Media" || root.Root != 0 || len(root.Links) != 1 || !plainLinkBetween(root.Links, root.ID, n.ID) || !m.graph.privateNode(n, owner) || n.Type != "gangolf-media-catalog" || n.Name != "catalog" || n.SkipEmbedding == nil || !*n.SkipEmbedding || len(n.Links) != 1 || !plainLinkBetween(n.Links, n.ID, root.ID) {
		return node{}, errCorrupt
	}
	return n, nil
}
func (m *mediaStore) load(ctx context.Context) (mediaCatalog, error) {
	n, err := m.metadata(ctx)
	if err != nil {
		return mediaCatalog{}, err
	}
	return m.decode(n)
}
func (m *mediaStore) decode(n node) (mediaCatalog, error) {
	var c mediaCatalog
	typ, _, err := mime.ParseMediaType(n.ContentType)
	if err != nil || typ != "application/json" || n.Content == nil || len(*n.Content) > mediaCapacity || mediaJSON([]byte(*n.Content), &c) != nil || !c.valid(m.authID, m.tenant) {
		return c, errCorrupt
	}
	return c, nil
}
func (m *mediaStore) save(ctx context.Context, c mediaCatalog) error {
	m.generation++
	data, err := json.Marshal(c)
	if err != nil || len(data) > mediaCapacity {
		return errMediaCapacity
	}
	if !c.valid(m.authID, m.tenant) {
		return errCorrupt
	}
	_, writeErr := m.graph.request(ctx, http.MethodPost, nodePath(m.catalogID)+"/content", nil, data)
	actual, err := m.load(ctx)
	if err == nil && reflect.DeepEqual(actual, c) {
		return nil
	}
	if writeErr != nil || err != nil || !reflect.DeepEqual(actual, c) {
		return errOutcomeUnknown
	}
	return nil
}
