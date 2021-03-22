// Copyright 2021 Tuzig LTD. All rights reserved.
// based on Gorilla WebSocket.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/gorilla/websocket"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

var (
	newline = []byte{'\n'}
	space   = []byte{' '}
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// PeerDoc is the info we store at redis
type PeerDoc struct {
	user        string
	fingerprint string
	name        string
	kind        string
}

// Peer is a middleman between the websocket connection and the hub.
type Peer struct {
	hub *Hub
	// The websocket connection.
	ws *websocket.Conn
	// Buffered channel of outbound messages.
	send          chan interface{}
	authenticated bool
	pd            *PeerDoc
}

// StatusMessage is used to update the peer to a change of state,
// like 200 after the peer has been authorized
type StatusMessage struct {
	status_code int
	description string
}

func newPeer(hub *Hub, q url.Values) (*Peer, error) {
	var pd PeerDoc
	fp := q.Get("fingerprint")
	if fp == "" {
		return nil, fmt.Errorf("Missing `fingerprint` query parameters")
	}
	key := fmt.Sprintf("peer:%s", fp)
	exists, err := redis.Bool(hub.redis.Do("EXISTS", key))
	if err != nil {
		return nil, err
	}
	peer := Peer{hub: hub, send: make(chan interface{}, 8), authenticated: false}
	if !exists {
		return &peer, &PeerNotFound{}
	}
	values, err := redis.Values(hub.redis.Do("HGETALL", key))
	if err = redis.ScanStruct(values, &pd); err != nil {
		return nil, fmt.Errorf("Failed to scan peer %q: %w", key, err)
	}
	peer.pd = &pd
	if pd.name != q.Get("name") ||
		pd.user != q.Get("user") ||
		pd.kind != q.Get("kind") {
		return &peer, &PeerChanged{}
	}
	peer.authenticated = true
	return &peer, nil
}

// readPump pumps messages from the websocket connection to the hub.
//
// The application runs readPump in a per-connection goroutine. The application
// ensures that there is at most one reader on a connection by executing all
// reads from this goroutine.
func (p *Peer) readPump() {
	var message map[string]string

	defer func() {
		p.hub.unregister <- p
		p.ws.Close()
	}()
	p.ws.SetReadLimit(maxMessageSize)
	p.ws.SetReadDeadline(time.Now().Add(pongWait))
	p.ws.SetPongHandler(func(string) error { p.ws.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		err := p.ws.ReadJSON(&message)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				Logger.Errorf("error: %v", err)
			}
			break
		}
		message["source_fp"] = p.pd.fingerprint
		message["source_name"] = p.pd.name
		p.hub.requests <- message
	}
}

// writePump pumps messages from the hub to the websocket connection.
//
// A goroutine running writePump is started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (p *Peer) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		p.ws.Close()
	}()
	for {
		select {
		case message, ok := <-p.send:
			p.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel.
				p.ws.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := p.ws.WriteJSON(message); err != nil {
				Logger.Warnf("failed to send message: %w", err)
				continue
			}
		case <-ticker.C:
			p.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := p.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
func (p *Peer) sendStatus(code int, err error) {
	msg := StatusMessage{code, err.Error()}
	p.send <- msg
}
func (p *Peer) sendAuthEmail() error {
	// TODO: send an email in the background, the email should havssss
	return nil
}
func (p *Peer) Upgrade(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		Logger.Errorf("Failed to upgrade socket: %w", err)
	}
	p.ws = conn
}

// serveWs handles websocket requests from the peer.
func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	peer, err := newPeer(hub, q)
	if peer == nil {
		msg := fmt.Sprintf("Failed to create a new peer: %s", err)
		Logger.Warn(msg)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		_, notFound := err.(*PeerNotFound)
		_, changed := err.(*PeerChanged)
		if notFound || changed {
			peer.sendStatus(401, err)
			err = peer.sendAuthEmail()
			if err != nil {
				Logger.Errorf("Failed to send an auth email: %w", err)
			}
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	peer.Upgrade(w, r)
	// Allow collection of memory referenced by the caller by doing all work in
	// new goroutines.
	go peer.writePump()
	go peer.readPump()
}
