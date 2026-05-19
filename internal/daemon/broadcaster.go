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
	defer b.mu.Unlock()

	n, err := b.buffer.Write(p)
	if err != nil {
		return n, err
	}

	for client := range b.clients {
		if _, err := client.Write(p); err != nil {
			client.Close()
			delete(b.clients, client)
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
		conn.Write(b.buffer.Bytes())
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
