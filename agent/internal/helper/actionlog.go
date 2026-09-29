package helper

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// DefaultActionLogDir holds the Action log. Only root can read it.
const DefaultActionLogDir = "/var/log/hostbeacon"

const (
	actionLogFormat = 1
	// A new file each month; files are kept for a year (v1 spec §11).
	actionLogMonths = 12
	noHAUser        = "no HA user"
)

// ActionLog is the Action log (v1 spec §11): one JSON line per request and
// per result, in one file per month. Only the root helper writes it.
type ActionLog struct {
	Dir string
	Now func() time.Time
}

// logEntry is one line. A request line has the Pairing, user, and status; a
// result line has the result. Later releases may add fields.
type logEntry struct {
	Format    int                    `json:"format"`
	Time      string                 `json:"time"`
	Entry     string                 `json:"entry"` // request or result
	ActionID  string                 `json:"action_id"`
	Action    protocol.Action        `json:"action"`
	PairingID string                 `json:"pairing_id,omitempty"`
	Pairing   string                 `json:"pairing,omitempty"`
	User      *string                `json:"user,omitempty"`
	Status    string                 `json:"status,omitempty"`
	Reason    protocol.RefusalReason `json:"reason,omitempty"`
	Result    string                 `json:"result,omitempty"`
	Error     *string                `json:"error,omitempty"`
}

func (l ActionLog) file(month time.Time) string {
	return filepath.Join(l.Dir, "actions-"+month.Format("2006-01")+".log")
}

// find looks for an earlier request with actionID in this month's and last
// month's files: Home Assistant never repeats an Action ID older than 1 hour.
// first is the first request's result when it is known. A refused request's
// result is its refusal.
func (l ActionLog) find(actionID string) (found bool, first *protocol.ActionOutcome, err error) {
	now := l.Now().UTC()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, month := range []time.Time{thisMonth.AddDate(0, -1, 0), thisMonth} {
		file, err := os.Open(l.file(month))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, nil, err
		}
		lines := bufio.NewScanner(file)
		lines.Buffer(nil, 1<<20)
		for lines.Scan() {
			var entry logEntry
			if json.Unmarshal(lines.Bytes(), &entry) != nil || entry.ActionID != actionID {
				continue
			}
			switch {
			case entry.Entry == "request" && !found:
				found = true
				if entry.Status == "refused" {
					refusal := "refused: " + string(entry.Reason)
					first = &protocol.ActionOutcome{Result: "failed", Error: &refusal}
				}
			case entry.Entry == "result" && found && first == nil:
				first = &protocol.ActionOutcome{Result: entry.Result, Error: entry.Error}
			}
		}
		err = lines.Err()
		file.Close()
		if err != nil {
			return false, nil, err
		}
	}
	return found, first, nil
}

// write appends one entry and waits until it is on disk. It also removes
// files older than a year.
func (l ActionLog) write(entry logEntry) error {
	now := l.Now().UTC()
	entry.Format = actionLogFormat
	entry.Time = now.Format(time.RFC3339)
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(l.file(now), os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	l.removeOld(now)
	return nil
}

// removeOld removes the files of months more than a year before now.
func (l ActionLog) removeOld(now time.Time) {
	oldest := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -actionLogMonths, 0)
	files, _ := filepath.Glob(filepath.Join(l.Dir, "actions-*.log"))
	for _, name := range files {
		month, err := time.Parse("2006-01", strings.TrimSuffix(strings.TrimPrefix(filepath.Base(name), "actions-"), ".log"))
		if err == nil && month.Before(oldest) {
			os.Remove(name)
		}
	}
}
