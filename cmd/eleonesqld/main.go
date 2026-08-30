// Command eleonesqld runs the EleoneSQL server.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/faisaljs/EleoneSQL/internal/server"
	"github.com/faisaljs/EleoneSQL/internal/txn"
)

func main() {
	dataFile := flag.String("data", "eleonesql.edb", "path to the database data file")
	walFile := flag.String("wal", "eleonesql.wal", "path to the write-ahead log file")
	addr := flag.String("addr", ":5432", "address to listen on")
	flag.Parse()

	store, err := txn.Open(*dataFile, *walFile)
	if err != nil {
		log.Fatalf("eleonesqld: %v", err)
	}
	defer store.Close()

	srv := server.New(store)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("eleonesqld: shutting down...")
		srv.Close()
	}()

	if err := srv.ListenAndServe(*addr); err != nil {
		log.Fatalf("eleonesqld: %v", err)
	}
}
