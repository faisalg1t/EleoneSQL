// Package wire defines EleoneSQL's client/server protocol: a small,
// text-based, line-delimited protocol (not compatible with PostgreSQL's
// wire protocol — see the README roadmap). It's deliberately simple so the
// server and a CLI client can both be implemented in a few hundred lines
// without a binary framing layer.
//
// Request:  one line of SQL, terminated by "\n". A statement must not
// itself contain a literal newline.
//
// Response: a sequence of lines:
//
//	OK <rows_affected> <message>          -- non-SELECT success
//	ROWS <ncols>
//	COLUMNS\t<col1>\t<col2>\t...
//	ROW\t<val1>\t<val2>\t...              -- repeated once per row
//	END
//	ERR <message>
//
// Values are rendered with catalog.Value.String(); NULL prints as "NULL".
package wire

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"

	"github.com/faisalg1t/EleoneSQL/internal/catalog"
	"github.com/faisalg1t/EleoneSQL/internal/engine"
)

// WriteResult serializes a *engine.Result to w following the protocol above.
func WriteResult(w *bufio.Writer, res *engine.Result) error {
	if res.Columns == nil && res.Rows == nil {
		if _, err := fmt.Fprintf(w, "OK %d %s\n", res.RowsAffected, res.Message); err != nil {
			return err
		}
		return w.Flush()
	}
	if _, err := fmt.Fprintf(w, "ROWS %d\n", len(res.Columns)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "COLUMNS\t%s\n", strings.Join(res.Columns, "\t")); err != nil {
		return err
	}
	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = v.String()
		}
		if _, err := fmt.Fprintf(w, "ROW\t%s\n", strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, "END"); err != nil {
		return err
	}
	return w.Flush()
}

// WriteError serializes an error to w.
func WriteError(w *bufio.Writer, err error) error {
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	if _, werr := fmt.Fprintf(w, "ERR %s\n", msg); werr != nil {
		return werr
	}
	return w.Flush()
}

// ClientResult mirrors engine.Result for the CLI side, which only has
// string-rendered values (it never touches the server's catalog types).
type ClientResult struct {
	OK           bool
	Message      string
	RowsAffected int
	Columns      []string
	Rows         [][]string
	Err          string
}

// ReadResult parses one server response from r.
func ReadResult(r *bufio.Reader) (*ClientResult, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\n")

	switch {
	case strings.HasPrefix(line, "OK "):
		parts := strings.SplitN(line[3:], " ", 2)
		n, _ := strconv.Atoi(parts[0])
		msg := ""
		if len(parts) > 1 {
			msg = parts[1]
		}
		return &ClientResult{OK: true, RowsAffected: n, Message: msg}, nil

	case strings.HasPrefix(line, "ERR "):
		return &ClientResult{Err: line[4:]}, nil

	case strings.HasPrefix(line, "ROWS "):
		colLine, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		colLine = strings.TrimRight(colLine, "\n")
		cols := strings.Split(strings.TrimPrefix(colLine, "COLUMNS\t"), "\t")
		if colLine == "COLUMNS\t" || colLine == "COLUMNS" {
			cols = nil
		}
		var rows [][]string
		for {
			rl, err := r.ReadString('\n')
			if err != nil {
				return nil, err
			}
			rl = strings.TrimRight(rl, "\n")
			if rl == "END" {
				break
			}
			cells := strings.Split(strings.TrimPrefix(rl, "ROW\t"), "\t")
			rows = append(rows, cells)
		}
		return &ClientResult{OK: true, Columns: cols, Rows: rows}, nil

	default:
		return nil, fmt.Errorf("wire: unexpected response line %q", line)
	}
}

// ValueStrings converts an engine row to its wire string form directly
// (used by tests / in-process helpers).
func ValueStrings(row []catalog.Value) []string {
	out := make([]string, len(row))
	for i, v := range row {
		out[i] = v.String()
	}
	return out
}
