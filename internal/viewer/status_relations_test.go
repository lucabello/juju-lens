package viewer

import (
	"testing"

	"github.com/lucabello/juju-lens/internal/index"
)

func TestBuildRelations(t *testing.T) {
	rows := []index.SnapshotRow{
		{Scope: "databag:loki.certificates#ca.certificates:loki/0"},
		{Scope: "databag:loki.certificates#ca.certificates:ca/0"},
		{Scope: "databag:mimir.mimir-peers:mimir/0"}, // peer relation, no '#'
		{Scope: "not-a-databag-scope"},               // ignored
	}
	rels := buildRelations(rows)
	if len(rels) != 2 {
		t.Fatalf("want 2 relations, got %d: %+v", len(rels), rels)
	}
	// Sorted by key: "loki..." before "mimir...".
	certs := rels[0]
	if certs.Key != "loki.certificates#ca.certificates" {
		t.Fatalf("rel[0] key = %s", certs.Key)
	}
	if len(certs.Endpoints) != 2 || certs.Endpoints[0] != "loki:certificates" || certs.Endpoints[1] != "ca:certificates" {
		t.Errorf("endpoints = %v", certs.Endpoints)
	}
	if len(certs.Entities) != 2 || certs.Entities[0] != "ca/0" || certs.Entities[1] != "loki/0" {
		t.Errorf("entities = %v (want sorted [ca/0 loki/0])", certs.Entities)
	}
	peer := rels[1]
	if peer.Key != "mimir.mimir-peers" || len(peer.Endpoints) != 1 || peer.Endpoints[0] != "mimir:mimir-peers" {
		t.Errorf("peer relation parsed wrong: %+v", peer)
	}
}
