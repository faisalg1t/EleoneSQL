// Package server hosts EleoneSQL over TCP using the line-based protocol in
// internal/server/wire.
package server

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"

	"github.com/faisaljs/EleoneSQL/internal/engine"
	"github.com/faisaljs/EleoneSQL/internal/server/wire"
	"github.com/faisaljs/EleoneSQL/internal/txn"
)

// Server accepts client connections and dispatches each to its own
// engine.Session. Table-level concurrency is handled by the Store's single
// global write lock (see internal/txn); the server itself just multiplexes
// connections onto that.
type Server struct {
	Store    *txn.Store
	listener net.Listener
	wg       sync.WaitGroup
}

func New(store *txn.Store) *Server {
	return &Server{Store: store}
}

// ListenAndServe binds addr (e.g. ":5432") and serves until the listener
// is closed.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", addr, err)
	}
	s.listener = ln
	log.Printf("EleoneSQL listening on %s", addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				s.wg.Wait()
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
}

// Close stops accepting new connections.
func (s *Server) Close() error {
	if s.listener == nil {
		return nil
	}
	return s.listener.Close()
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	sess := engine.NewSession(s.Store)
	defer sess.Close()

	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return // client disconnected
		}
		sql := strings.TrimSpace(line)
		if sql == "" {
			continue
		}
		if strings.EqualFold(sql, "quit") || strings.EqualFold(sql, "exit") {
			return
		}
		res, err := sess.Execute(sql)
		if err != nil {
			if werr := wire.WriteError(w, err); werr != nil {
				return
			}
			continue
		}
		if werr := wire.WriteResult(w, res); werr != nil {
			return
		}
	}
}
