// Package eventlog keeps a small in-memory ring buffer of noteworthy events
// (webhook arrivals, skips, jobs, classification errors, config reloads, mail
// lookups…) so they can be surfaced and filtered in the app's Journal view.
package eventlog

import (
	"sync"
	"time"
)

// Entry is a single journal line.
type Entry struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"` // info | warn | error
	Type  string    `json:"type"`  // webhook | job | scan | config | mail | review | system
	Msg   string    `json:"msg"`   // human-readable detail (libellé)
	TxnID string    `json:"txn_id,omitempty"`
}

const maxEntries = 1000

var (
	mu      sync.Mutex
	entries []Entry
)

// Add appends an entry, trimming the buffer to the most recent maxEntries.
func Add(level, typ, msg, txnID string) {
	mu.Lock()
	defer mu.Unlock()
	entries = append(entries, Entry{
		Time:  time.Now(),
		Level: level,
		Type:  typ,
		Msg:   msg,
		TxnID: txnID,
	})
	if len(entries) > maxEntries {
		entries = entries[len(entries)-maxEntries:]
	}
}

// Info/Warn/Error are convenience wrappers keyed by type.
func Info(typ, msg, txnID string)  { Add("info", typ, msg, txnID) }
func Warn(typ, msg, txnID string)  { Add("warn", typ, msg, txnID) }
func Error(typ, msg, txnID string) { Add("error", typ, msg, txnID) }

// Entries returns a copy of the buffer, newest first.
func Entries() []Entry {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Entry, len(entries))
	for i, e := range entries {
		out[len(entries)-1-i] = e
	}
	return out
}

// Clear empties the buffer.
func Clear() {
	mu.Lock()
	defer mu.Unlock()
	entries = nil
}
