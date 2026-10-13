package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProbeTraceparent(t *testing.T) {
	for _, parent := range []string{"", "00-11111111111111111111111111111111-2222222222222222-01", "00-33333333333333333333333333333333-4444444444444444-00"} {
		t.Run(parent, func(t *testing.T) {
			t.Setenv("PROBE_TRACEPARENT", parent)
			got := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- r.Header.Get("traceparent")
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			require.NoError(t, probe(server.URL, 1))
			require.Equal(t, parent, <-got)
		})
	}
}
