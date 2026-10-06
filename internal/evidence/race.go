package evidence

// Grant is one holder's belief that it held a rig, from the moment the
// database granted it to the moment it released or its lease expired,
// whichever came first, both on the database clock in microseconds.
type Grant struct {
	Rig     string `json:"rig"`
	Holder  string `json:"holder"`
	Fence   int64  `json:"fence"`
	StartUS int64  `json:"start_us"`
	EndUS   int64  `json:"end_us"`
	Ended   string `json:"ended"`
}

// Op is one acquisition attempt as the client saw it, on the client's
// monotonic clock, which is what peak concurrency is computed from.
type Op struct {
	Worker  int   `json:"worker"`
	StartNS int64 `json:"start_ns"`
	EndNS   int64 `json:"end_ns"`
	Granted bool  `json:"granted"`
}

type RaceSummary struct {
	Mode           string `json:"mode"`
	Rigs           int    `json:"rigs"`
	Workers        int    `json:"workers"`
	Attempts       int    `json:"attempts"`
	Grants         int    `json:"grants"`
	EndedByExpiry  int    `json:"ended_by_expiry"`
	DoubleBookings int    `json:"double_bookings"`
	PeakInFlight   int    `json:"peak_in_flight"`
	Errors         int    `json:"errors"`
}

// SummarizeRace is the one definition of how a race is tallied. The harness
// writes its result and the validator recomputes it from the raw files.
func SummarizeRace(mode string, grants []Grant, ops []Op) RaceSummary {
	s := RaceSummary{Mode: mode, Attempts: len(ops), Grants: len(grants)}
	spans := make([]Span, len(grants))
	for i, g := range grants {
		spans[i] = Span{Resource: g.Rig, Owner: g.Holder, Start: g.StartUS, End: g.EndUS}
		if g.Ended == "expired" {
			s.EndedByExpiry++
		}
	}
	opSpans := make([]Span, len(ops))
	for i, o := range ops {
		opSpans[i] = Span{Start: o.StartNS, End: o.EndNS}
	}
	s.DoubleBookings = OverlapPairs(spans)
	s.PeakInFlight = PeakConcurrency(opSpans)
	return s
}
