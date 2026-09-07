package platform

import "time"

// Rate is observed provider quota data. Storage and admission belong to callers.
type Rate struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

// RateObserver receives wire observations without coupling clients to an
// application's counters, persistence or admission policy.
type RateObserver interface {
	RecordRequest()
	UpdateFromRate(Rate)
}
