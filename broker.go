package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	brokerPeerTTL   = 2 * time.Minute
	brokerSignalTTL = 10 * time.Minute
	brokerMaxPeers  = 8
	brokerMaxSignal = 24 << 10
)

// brokerCommand is deliberately a small capability protocol rather than
// general database access. A function may ask the platform to relay WebRTC
// negotiation messages, but it never receives credentials, SQL, or another
// project's rooms.
type brokerCommand struct {
	Action string          `json:"action"`
	Room   string          `json:"room"`
	Peer   string          `json:"peer"`
	Name   string          `json:"name,omitempty"`
	To     string          `json:"to,omitempty"`
	Signal json.RawMessage `json:"signal,omitempty"`
	Since  int64           `json:"since,omitempty"`
}

func hasCapability(fn functionManifest, wanted string) bool {
	for _, capability := range fn.Capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

func normalizeBrokerCommand(command *brokerCommand) error {
	if command == nil {
		return errors.New("broker command is required")
	}
	command.Action = strings.ToLower(strings.TrimSpace(command.Action))
	command.Room = strings.ToUpper(strings.TrimSpace(command.Room))
	command.Peer = strings.TrimSpace(command.Peer)
	command.To = strings.TrimSpace(command.To)
	command.Name = strings.TrimSpace(command.Name)
	if command.Action != "join" && command.Action != "poll" && command.Action != "signal" && command.Action != "leave" {
		return errors.New("broker action must be join, poll, signal, or leave")
	}
	if !brokerIdentifier(command.Room, 4, 12, true) {
		return errors.New("room must be 4 to 12 letters or digits")
	}
	if !brokerIdentifier(command.Peer, 8, 64, false) {
		return errors.New("peer must be an 8 to 64 character identifier")
	}
	if command.To != "" && !brokerIdentifier(command.To, 8, 64, false) {
		return errors.New("signal recipient is invalid")
	}
	if !utf8.ValidString(command.Name) || len([]rune(command.Name)) > 24 || strings.ContainsAny(command.Name, "\r\n\x00") {
		return errors.New("player name must be at most 24 characters")
	}
	if command.Since < 0 {
		return errors.New("signal cursor cannot be negative")
	}
	if command.Action == "signal" {
		if command.To == "" || len(command.Signal) == 0 || len(command.Signal) > brokerMaxSignal || !json.Valid(command.Signal) {
			return fmt.Errorf("signal needs a recipient and at most %d bytes of JSON", brokerMaxSignal)
		}
	}
	return nil
}

func brokerIdentifier(value string, minLen, maxLen int, room bool) bool {
	if len(value) < minLen || len(value) > maxLen {
		return false
	}
	for _, r := range value {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			!room && (r >= 'a' && r <= 'z' || r == '-' || r == '_') {
			continue
		}
		return false
	}
	return true
}

func (s *Server) executeBroker(projectID string, command *brokerCommand) (map[string]any, error) {
	if err := normalizeBrokerCommand(command); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Cleanup is demand-driven and indexed. An abandoned invite disappears
	// without a background service or a permanent record of who joined it.
	if _, err = tx.Exec(`DELETE FROM app_room_signals WHERE created_at < $1`, now.Add(-brokerSignalTTL)); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`DELETE FROM app_room_peers WHERE last_seen_at < $1`, now.Add(-brokerPeerTTL)); err != nil {
		return nil, err
	}

	switch command.Action {
	case "join":
		var exists bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM app_room_peers
			WHERE project_id=$1 AND room=$2 AND peer_id=$3)`, projectID, command.Room, command.Peer).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			var count int
			if err = tx.QueryRow(`SELECT COUNT(*) FROM app_room_peers WHERE project_id=$1 AND room=$2`,
				projectID, command.Room).Scan(&count); err != nil {
				return nil, err
			}
			if count >= brokerMaxPeers {
				return nil, fmt.Errorf("room is full (%d peers maximum)", brokerMaxPeers)
			}
		}
		if command.Name == "" {
			command.Name = "Player"
		}
		_, err = tx.Exec(`INSERT INTO app_room_peers(project_id,room,peer_id,name,joined_at,last_seen_at)
			VALUES($1,$2,$3,$4,$5,$5) ON CONFLICT(project_id,room,peer_id)
			DO UPDATE SET name=EXCLUDED.name,last_seen_at=EXCLUDED.last_seen_at`,
			projectID, command.Room, command.Peer, command.Name, now)
	case "poll":
		result, touchErr := tx.Exec(`UPDATE app_room_peers SET last_seen_at=$4
			WHERE project_id=$1 AND room=$2 AND peer_id=$3`, projectID, command.Room, command.Peer, now)
		if touchErr != nil {
			return nil, touchErr
		}
		if changed, _ := result.RowsAffected(); changed == 0 {
			return nil, errors.New("join the room before polling")
		}
	case "signal":
		var joined bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM app_room_peers
			WHERE project_id=$1 AND room=$2 AND peer_id=$3)`, projectID, command.Room, command.Peer).Scan(&joined); err != nil {
			return nil, err
		}
		if !joined {
			return nil, errors.New("join the room before signaling")
		}
		var signalID int64
		if err = tx.QueryRow(`INSERT INTO app_room_signals(project_id,room,sender,to_peer,payload,created_at)
			VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, projectID, command.Room, command.Peer,
			command.To, command.Signal, now).Scan(&signalID); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "room": command.Room, "signal_id": signalID}, nil
	case "leave":
		_, err = tx.Exec(`DELETE FROM app_room_peers WHERE project_id=$1 AND room=$2 AND peer_id=$3`,
			projectID, command.Room, command.Peer)
		if err == nil {
			_, err = tx.Exec(`DELETE FROM app_room_signals WHERE project_id=$1 AND room=$2
				AND (sender=$3 OR to_peer=$3)`, projectID, command.Room, command.Peer)
		}
		if err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "room": command.Room, "left": true}, nil
	}
	if err != nil {
		return nil, err
	}

	response, err := brokerSnapshot(tx, projectID, command, now)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return response, nil
}

func brokerSnapshot(tx *sql.Tx, projectID string, command *brokerCommand, now time.Time) (map[string]any, error) {
	peerRows, err := tx.Query(`SELECT peer_id,name FROM app_room_peers
		WHERE project_id=$1 AND room=$2 AND last_seen_at >= $3 ORDER BY joined_at`,
		projectID, command.Room, now.Add(-brokerPeerTTL))
	if err != nil {
		return nil, err
	}
	peers := make([]map[string]any, 0)
	for peerRows.Next() {
		var id, name string
		if peerRows.Scan(&id, &name) == nil {
			peers = append(peers, map[string]any{"id": id, "name": name})
		}
	}
	_ = peerRows.Close()

	rows, err := tx.Query(`SELECT id,sender,payload::text FROM app_room_signals
		WHERE project_id=$1 AND room=$2 AND id>$3 AND to_peer=$4 AND sender<>$4
		ORDER BY id LIMIT 100`, projectID, command.Room, command.Since, command.Peer)
	if err != nil {
		return nil, err
	}
	signals := make([]map[string]any, 0)
	next := command.Since
	for rows.Next() {
		var id int64
		var sender, payloadJSON string
		if rows.Scan(&id, &sender, &payloadJSON) != nil {
			continue
		}
		var payload any
		if json.Unmarshal([]byte(payloadJSON), &payload) != nil {
			continue
		}
		signals = append(signals, map[string]any{"id": id, "from": sender, "signal": payload})
		if id > next {
			next = id
		}
	}
	_ = rows.Close()
	return map[string]any{"ok": true, "room": command.Room, "peer": command.Peer,
		"peers": peers, "signals": signals, "next": next}, nil
}
