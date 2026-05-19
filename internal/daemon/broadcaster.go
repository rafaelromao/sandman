package daemon

import (
	"bytes"
	"net"
	"sync"
)

type Broadcaster struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	clients map[net.Conn]struct{}
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		clients: make(map[net.Conn]struct{}),
	}
}

func (b *Broadcaster) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buffer.Write(p)
	if err != nil {
		b.mu.Unlock()
		return n, err
	}

	clients := make([]net.Conn, 0, len(b.clients))
	for c := range b.clients {
		clients = append(clients, c)
	}
	b.mu.Unlock()

	for _, conn := range clients {
		if _, err := conn.Write(p); err != nil {
			conn.Close()
			b.mu.Lock()
			delete(b.clients, conn)
			b.mu.Unlock()
		}
	}

	return n, nil
}

func (b *Broadcaster) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Bytes()
}

func (b *Broadcaster) AddClient(conn net.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.buffer.Len() > 0 {
		if _, err := conn.Write(b.buffer.Bytes()); err != nil {
			conn.Close()
			return
		}
	}

	b.clients[conn] = struct{}{}
}

func (b *Broadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for client := range b.clients {
		client.Close()
	}
	b.clients = make(map[net.Conn]struct{})
}
