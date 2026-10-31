package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// commNote keeps a provider-side copy of the protocol events. The broker has the
// authoritative record, but a local file lets the machine owner show what it was
// asked to do even when the broker is unreachable or the job row was pruned.
func (a *agent) commNote(event string, kv ...string) {
	p := filepath.Join(a.work, "comm.log")
	if st, err := os.Stat(p); err == nil && st.Size() > 5<<20 {
		os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s %s\n", time.Now().Format(time.RFC3339), event, strings.Join(kv, " "))
}
