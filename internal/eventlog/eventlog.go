// Package eventlog keeps a small in-memory ring buffer of noteworthy events
// (webhook arrivals, skips with reasons, jobs created, errors) so they can be
// surfaced in the app's Journal view — even for webhooks ignored right away.
package eventlog

import (
	"sync"
	"time"
)

// Entry is a single journal line.
type Entry struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"` // info | warn | error
	Event string    `json:"event"` // short machine tag, e.g. "webhook_received"
	Msg   string    `json:"msg"`   // human-readable detail
	TxnID string    `json:"txn_id,omitempty"`
}

const maxEntries = 500

var (
	mu      sync.Mutex
	entries []Entry
)

// Add appends an entry, trimming the buffer to the most recent maxEntries.
func Add(level, event, msg, txnID string) {
	mu.Lock()
	defer mu.Unlock()
	entries = append(entries, Entry{
		Time:  time.Now(),
		Level: level,
		Event: event,
		Msg:   msg,
		TxnID: txnID,
	})
	if len(entries) > maxEntries {
		entries = entries[len(entries)-maxEntries:]
	}
}

// Info/Warn/Error are convenience wrappers.
func Info(event, msg, txnID string)  { Add("info", event, msg, txnID) }
func Warn(event, msg, txnID string)  { Add("warn", event, msg, txnID) }
func Error(event, msg, txnID string) { Add("error", event, msg, txnID) }

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
