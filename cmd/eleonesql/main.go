// Command eleonesql is EleoneSQL's interactive CLI client. By default it
// connects to a running eleonesqld server; with -embed it opens a data file
// directly in-process instead (handy for quick local scripting without
// starting a server).
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/faisalg1t/EleoneSQL/internal/engine"
	"github.com/faisalg1t/EleoneSQL/internal/server/wire"
	"github.com/faisalg1t/EleoneSQL/internal/txn"
)

func main() {
	addr := flag.String("addr", "localhost:5432", "eleonesqld address to connect to")
	embedData := flag.String("embed", "", "skip the network and open this data file in-process instead")
	embedWAL := flag.String("embed-wal", "", "WAL path for -embed (defaults to <data>.wal)")
	execFlag := flag.String("c", "", "execute a single statement and exit")
	flag.Parse()

	if *embedData != "" {
		runEmbedded(*embedData, *embedWAL, *execFlag)
		return
	}
	runNetworked(*addr, *execFlag)
}

func runEmbedded(dataPath, walPath, execOnly string) {
	if walPath == "" {
		walPath = dataPath + ".wal"
	}
	store, err := txn.Open(dataPath, walPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eleonesql:", err)
		os.Exit(1)
	}
	defer store.Close()
	sess := engine.NewSession(store)

	if execOnly != "" {
		res, err := sess.Execute(execOnly)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		printLocalResult(res)
		return
	}
	fmt.Printf("EleoneSQL embedded session on %s (type 'exit;' to quit)\n", dataPath)
	repl(func(sql string) {
		res, err := sess.Execute(sql)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return
		}
		printLocalResult(res)
	})
}

func printLocalResult(res *engine.Result) {
	if res.Columns == nil {
		fmt.Printf("OK (%d rows affected) %s\n", res.RowsAffected, res.Message)
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(res.Columns, "\t"))
	for _, row := range res.Rows {
		fmt.Fprintln(tw, strings.Join(wire.ValueStrings(row), "\t"))
	}
	tw.Flush()
	fmt.Printf("(%d rows)\n", len(res.Rows))
}

func runNetworked(addr, execOnly string) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eleonesql: could not connect to", addr, "-", err)
		os.Exit(1)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	send := func(sql string) (*wire.ClientResult, error) {
		if _, err := fmt.Fprintln(w, sql); err != nil {
			return nil, err
		}
		if err := w.Flush(); err != nil {
			return nil, err
		}
		return wire.ReadResult(r)
	}

	if execOnly != "" {
		res, err := send(execOnly)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		printRemoteResult(res)
		if res.Err != "" {
			os.Exit(1)
		}
		return
	}

	fmt.Printf("Connected to EleoneSQL at %s (type 'exit;' to quit)\n", addr)
	repl(func(sql string) {
		res, err := send(sql)
		if err != nil {
			fmt.Fprintln(os.Stderr, "connection error:", err)
			os.Exit(1)
		}
		printRemoteResult(res)
	})
}

func printRemoteResult(res *wire.ClientResult) {
	if res.Err != "" {
		fmt.Fprintln(os.Stderr, "error:", res.Err)
		return
	}
	if res.Columns == nil && res.Rows == nil {
		fmt.Printf("OK (%d rows affected) %s\n", res.RowsAffected, res.Message)
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(res.Columns, "\t"))
	for _, row := range res.Rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	tw.Flush()
	fmt.Printf("(%d rows)\n", len(res.Rows))
}

// repl reads statements terminated by ';' from stdin and hands each to run.
// A line consisting only of "exit" or "quit" (with or without a trailing
// semicolon) ends the session.
func repl(run func(sql string)) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var buf strings.Builder
	fmt.Print("eleonesql> ")
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if buf.Len() == 0 {
			lower := strings.ToLower(strings.TrimSuffix(trimmed, ";"))
			if lower == "exit" || lower == "quit" {
				return
			}
		}
		buf.WriteString(line)
		buf.WriteString(" ")
		if strings.HasSuffix(trimmed, ";") {
			sql := strings.TrimSpace(buf.String())
			buf.Reset()
			if sql != "" {
				run(sql)
			}
			fmt.Print("eleonesql> ")
			continue
		}
		fmt.Print("        -> ")
	}
}
