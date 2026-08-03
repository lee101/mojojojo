package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRoomBrokerRelaysSignalsWithinOneProject(t *testing.T) {
	s := authTestServer(t)
	u, err := s.auth.createUser("rooms@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, id := range []string{"snake", "other-app"} {
		if _, err := s.db.Exec(`INSERT INTO projects(id,user_id,name,slug,runtime,created_at,updated_at)
			VALUES($1,$2,$1,$1,'app',$3,$3)`, id, u.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	host := "host_peer_123"
	guest := "guest_peer_456"
	for _, peer := range []struct{ id, name string }{{host, "Ada"}, {guest, "Grace"}} {
		result, err := s.executeBroker("snake", &brokerCommand{Action: "join", Room: "PLAY42", Peer: peer.id, Name: peer.name})
		if err != nil || len(result["peers"].([]map[string]any)) == 0 {
			t.Fatalf("join %s: %#v %v", peer.id, result, err)
		}
	}
	payload := json.RawMessage(`{"description":{"type":"offer","sdp":"small"}}`)
	sent, err := s.executeBroker("snake", &brokerCommand{Action: "signal", Room: "PLAY42", Peer: host, To: guest, Signal: payload})
	if err != nil || sent["signal_id"].(int64) < 1 {
		t.Fatalf("signal: %#v %v", sent, err)
	}
	polled, err := s.executeBroker("snake", &brokerCommand{Action: "poll", Room: "PLAY42", Peer: guest})
	if err != nil {
		t.Fatal(err)
	}
	signals := polled["signals"].([]map[string]any)
	if len(signals) != 1 || signals[0]["from"] != host {
		t.Fatalf("wrong signals: %#v", signals)
	}
	if peers := polled["peers"].([]map[string]any); len(peers) != 2 {
		t.Fatalf("wrong peer list: %#v", peers)
	}
	// The same room code in a different app is a different namespace.
	isolated, err := s.executeBroker("other-app", &brokerCommand{Action: "join", Room: "PLAY42", Peer: "other_peer_789", Name: "Lin"})
	if err != nil || len(isolated["peers"].([]map[string]any)) != 1 || len(isolated["signals"].([]map[string]any)) != 0 {
		t.Fatalf("project boundary failed: %#v %v", isolated, err)
	}
}

func TestRoomBrokerValidationAndMembership(t *testing.T) {
	for name, command := range map[string]*brokerCommand{
		"room":   {Action: "join", Room: "../bad", Peer: "valid_peer_123"},
		"peer":   {Action: "join", Room: "GOOD42", Peer: "short"},
		"action": {Action: "delete", Room: "GOOD42", Peer: "valid_peer_123"},
		"signal": {Action: "signal", Room: "GOOD42", Peer: "valid_peer_123", To: "other_peer_123"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := normalizeBrokerCommand(command); err == nil {
				t.Fatal("invalid broker command was accepted")
			}
		})
	}
}

func TestManifestRequiresKnownFunctionCapabilities(t *testing.T) {
	for name, capability := range map[string]string{"allowed": "room-broker", "unknown": "database"} {
		t.Run(name, func(t *testing.T) {
			_, _, err := readBundle(zipFixture(t, map[string]string{
				"index.html":          "ok",
				"functions/broker.py": "def handler(request): return {}",
				"mojojojo.json":       `{"runtime":"app","functions":{"broker":{"language":"python","entrypoint":"functions/broker.py","capabilities":["` + capability + `"]}}}`,
			}))
			if capability == "room-broker" && err != nil {
				t.Fatal(err)
			}
			if capability != "room-broker" && err == nil {
				t.Fatal("unknown capability was accepted")
			}
		})
	}
}
