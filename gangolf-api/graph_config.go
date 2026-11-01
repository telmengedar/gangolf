package main

import (
	"context"
	"mime"
	"strings"
)

const authConfigType = "gangolf-auth-config"

func authProfileName(name string) bool {
	profile, ok := strings.CutPrefix(name, "Gangolf API auth — ")
	if !ok || len(profile) == 0 || len(profile) > 128 || profile[0] == '-' || profile[len(profile)-1] == '-' {
		return false
	}
	for _, c := range profile {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func (g *graph) loadConfig(ctx context.Context) (*authConfig, error) {
	owner, err := g.serviceIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if err := g.privateDomainGroups(ctx, owner); err != nil {
		return nil, err
	}
	g.groupMu.RLock()
	group := g.groups["configuration"]
	for _, id := range g.groups {
		if id == g.config {
			g.groupMu.RUnlock()
			return nil, errCorrupt
		}
	}
	g.groupMu.RUnlock()
	nodes, err := g.queryResourceItem(ctx, authConfigType, "", owner, group, g.config)
	if err != nil {
		return nil, err
	}
	if len(nodes) != 1 || !authProfileName(nodes[0].Name) || nodes[0].SkipEmbedding == nil || !*nodes[0].SkipEmbedding {
		return nil, errCorrupt
	}
	n := nodes[0]
	named, err := g.queryResourceNodes(ctx, authConfigType, n.Name, owner, group)
	if err != nil {
		return nil, err
	}
	if len(named) != 1 || !sameIdentityProjection(n, named[0]) {
		return nil, errCorrupt
	}
	integrity, err := g.queryResourceNodes(ctx, authConfigType, n.Name, owner, 0)
	if err != nil {
		return nil, err
	}
	if len(integrity) != 1 || !sameIdentityProjection(n, integrity[0]) {
		return nil, errCorrupt
	}
	mediaType, _, err := mime.ParseMediaType(n.ContentType)
	var cfg authConfig
	if err != nil || mediaType != "application/json" || n.Content == nil || strictJSON([]byte(*n.Content), &cfg) != nil || !cfg.valid() {
		return nil, errCorrupt
	}
	cfg.serviceOwner = owner
	return &cfg, nil
}
