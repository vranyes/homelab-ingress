package state

import (
	"io"
	"log/slog"
	"testing"
)

func TestCloneRateLimitIsDeepCopy(t *testing.T) {
	st := testState()
	svc := svcWithAddr("ns/svc", "10.44.0.50")
	svc.RateLimit = &RateLimit{Rate: 5, Burst: 10}
	st.UpdateService(svc)

	snap := st.Clone()
	got := snap.Services["ns/svc"]
	if got == nil || got.RateLimit == nil {
		t.Fatalf("clone lost the rate limit: %+v", got)
	}
	if *got.RateLimit != (RateLimit{Rate: 5, Burst: 10}) {
		t.Fatalf("clone rate limit = %+v", got.RateLimit)
	}

	got.RateLimit.Rate = 99
	orig, _ := st.GetService("ns/svc")
	if orig.RateLimit.Rate != 5 {
		t.Fatalf("mutating the clone changed the original: %+v", orig.RateLimit)
	}
}

func TestStoreRateLimitRoundTrip(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := NewStore(t.TempDir()+"/state.db", logger)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	svc := svcWithAddr("ns/svc", "10.44.0.50")
	svc.RateLimit = &RateLimit{Rate: 7, Burst: 9}
	if err := store.SaveService(svc); err != nil {
		t.Fatalf("save: %v", err)
	}

	services, err := store.LoadServices()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := services["ns/svc"]
	if got == nil || got.RateLimit == nil {
		t.Fatalf("rate limit lost in persistence: %+v", got)
	}
	if *got.RateLimit != (RateLimit{Rate: 7, Burst: 9}) {
		t.Fatalf("persisted rate limit = %+v", got.RateLimit)
	}
}
