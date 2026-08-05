package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// requireBearer guards the endpoints that answer questions about the cluster
// (/status and /metrics-query). The caller is Struct8, from outside in, using
// the token this agent itself announced (see announce.go).
//
// The comparison runs in constant time: the token is a shared secret, and a
// comparison that returns on the first differing byte leaks the correct prefix
// to anyone timing the response. `subtle.ConstantTimeCompare` also returns 0 for
// differing lengths, so the length needs no separate check.
//
// `/healthz` deliberately does NOT go through here: the caller is the kubelet,
// on the node, and it has no way to carry the token. Requiring authentication
// there would fail the probe and restart the Pod in a loop.
func requireBearer(token string, next http.HandlerFunc) http.HandlerFunc {
	expected := []byte(token)
	return func(w http.ResponseWriter, r *http.Request) {
		received := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(received), expected) != 1 {
			// No detail about the reason: telling "wrong token" apart from
			// "missing token" is free information for whoever is guessing.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
