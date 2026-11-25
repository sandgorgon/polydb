package engine

import "time"

// Limits bound what one statement, connection or user may use, so that
// a single heavy query or client can't starve a shared server. Zero
// fields get the defaults; a negative one means no limit. A statement's
// time and memory are the defaults of the session settings timeout and
// max_memory: a session may lower them, and only a server admin may
// raise them past these values.
type Limits struct {
	// StatementTimeout is how long one statement may run (default none).
	StatementTimeout time.Duration
	// MaxMemory is how many bytes a statement may hold: the rows its
	// steps keep (for sort, group, distinct and its result) and the large
	// values it loads (default DefaultMaxMemory).
	MaxMemory int64
	// MaxConnections is how many connections the server accepts at once
	// (default 1,000), and MaxUserConnections how many one user may have
	// (default 100).
	MaxConnections     int
	MaxUserConnections int
	// MaxTransactions is how many explicit transactions one session may
	// have open (default 16).
	MaxTransactions int
	// MaxUpload is the largest single upload (default 4 GiB, the largest
	// bytes value), and UploadQuota how many bytes of uploads one user
	// may hold before statements use them (default 16 GiB).
	MaxUpload   int64
	UploadQuota int64
}

// Default limits.
const (
	DefaultMaxMemory          = 1 << 30
	DefaultMaxConnections     = 1000
	DefaultMaxUserConnections = 100
	DefaultMaxTransactions    = 16
	DefaultMaxUpload          = 4 << 30
	DefaultUploadQuota        = 16 << 30
)

func (l *Limits) defaults() {
	def := func(v *int64, d int64) {
		if *v == 0 {
			*v = d
		}
	}
	defInt := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	def(&l.MaxMemory, DefaultMaxMemory)
	def(&l.MaxUpload, DefaultMaxUpload)
	def(&l.UploadQuota, DefaultUploadQuota)
	defInt(&l.MaxConnections, DefaultMaxConnections)
	defInt(&l.MaxUserConnections, DefaultMaxUserConnections)
	defInt(&l.MaxTransactions, DefaultMaxTransactions)
}

// Of returns a limit as a number with 0 meaning none, the form session
// settings use.
func Of(n int64) int64 { return max(n, 0) }
