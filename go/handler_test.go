package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func getOrder(baseURL string) (etag string, body string, err error) {
	resp, err := http.Get(baseURL + "/orders/1")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	return resp.Header.Get("ETag"), strings.TrimSpace(string(raw)), nil
}

// putOrderDetailed is the whole result of a conditional write: the status, the
// validator the server handed back, and the body. Tests that assert on the
// response's ETag need all three.
func putOrderDetailed(baseURL, ifMatch, status string) (code int, etag, body string, err error) {
	payload, err := json.Marshal(map[string]string{"status": status})
	if err != nil {
		return 0, "", "", err
	}

	req, err := http.NewRequest(http.MethodPut, baseURL+"/orders/1", bytes.NewReader(payload))
	if err != nil {
		return 0, "", "", err
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, resp.Header.Get("ETag"), "", err
	}

	return resp.StatusCode, resp.Header.Get("ETag"), strings.TrimSpace(string(raw)), nil
}

// putOrder is deliberately free of *testing.T so that concurrent writers can
// call it from their own goroutines and report back through their return
// values. It also makes the conditional write a single line in every test.
func putOrder(baseURL, ifMatch, status string) (code int, body string, err error) {
	code, _, body, err = putOrderDetailed(baseURL, ifMatch, status)
	return code, body, err
}

// TestConcurrentWritersExactlyOneWins is the point of the whole article: two
// writers act on the same read, and the precondition decides which one of them
// is allowed to change the resource. Simulated concurrency is not enough here -
// these are two real goroutines issuing two real HTTP requests.
func TestConcurrentWritersExactlyOneWins(t *testing.T) {
	server := httptest.NewServer(NewServer(NewStore()))
	defer server.Close()

	// Both writers read the resource first, so both hold the same validator.
	// That is the entire premise of the lost update.
	etag, _, err := getOrder(server.URL)
	if err != nil {
		t.Fatalf("initial read: %v", err)
	}
	if etag != `"order-1-v1"` {
		t.Fatalf("want initial ETag %q, got %q", `"order-1-v1"`, etag)
	}

	const writers = 2
	statuses := [writers]string{"paid", "shipped"}
	codes := make([]int, writers)
	bodies := make([]string, writers)
	errs := make([]error, writers)

	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			<-start // release both writers as close to simultaneously as possible

			// Both requests carry the SAME validator, taken from the same read.
			codes[i], bodies[i], errs[i] = putOrder(server.URL, etag, statuses[i])
		}(i)
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	winners, losers := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusNoContent:
			winners++
		case http.StatusPreconditionFailed:
			losers++
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("want exactly one winner (204) and one loser (412), got %v (%v)", codes, bodies)
	}

	// The resource was written once, not twice, and its version advanced once.
	finalETag, finalBody, err := getOrder(server.URL)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if finalETag != `"order-1-v2"` {
		t.Fatalf("want final ETag %q, got %q", `"order-1-v2"`, finalETag)
	}
	if !strings.Contains(finalBody, `"version":2`) {
		t.Fatalf("want version 2 to be recorded once, got %s", finalBody)
	}

	t.Logf("both writers sent If-Match: %s", etag)
	t.Logf("writer 0 -> %d %s", codes[0], bodies[0])
	t.Logf("writer 1 -> %d %s", codes[1], bodies[1])
	t.Logf("codes %v: one 204 No Content, one 412 Precondition Failed", codes)
	t.Logf("resource afterwards: %s %s", finalETag, finalBody)
}

// TestWeakETagCannotSatisfyIfMatch pins the reason a server must not mint weak
// validators for resources it also guards with If-Match: strong comparison
// fails on a weak tag, so the precondition can never be satisfied.
func TestWeakETagCannotSatisfyIfMatch(t *testing.T) {
	server := httptest.NewServer(NewServer(NewStore()))
	defer server.Close()

	etag, _, err := getOrder(server.URL)
	if err != nil {
		t.Fatalf("initial read: %v", err)
	}

	code, body, err := putOrder(server.URL, "W/"+etag, "paid")
	if err != nil {
		t.Fatalf("weak conditional put: %v", err)
	}
	if code != http.StatusPreconditionFailed {
		t.Fatalf("want 412 for a weak validator in If-Match, got %d (%s)", code, body)
	}

	code, body, err = putOrder(server.URL, etag, "paid")
	if err != nil {
		t.Fatalf("strong conditional put: %v", err)
	}
	if code != http.StatusNoContent {
		t.Fatalf("want 204 for the strong validator, got %d (%s)", code, body)
	}

	t.Logf("If-Match: W/%s -> 412; If-Match: %s -> 204", etag, etag)
}

// TestUnconditionalWriteIsRefused covers the RFC 6585 policy: a server can
// decline to accept a write that carries no precondition at all.
func TestUnconditionalWriteIsRefused(t *testing.T) {
	server := httptest.NewServer(NewServer(NewStore()))
	defer server.Close()

	code, body, err := putOrder(server.URL, "", "paid")
	if err != nil {
		t.Fatalf("unconditional put: %v", err)
	}
	if code != http.StatusPreconditionRequired {
		t.Fatalf("want 428 without If-Match, got %d (%s)", code, body)
	}

	t.Logf("PUT without If-Match -> %d %s", code, body)
}

// TestSplitEntityTagsKeepsCommasInsideTags guards the parser that a naive
// strings.Split would get wrong: a comma is legal inside an opaque tag.
func TestSplitEntityTagsKeepsCommasInsideTags(t *testing.T) {
	got := splitEntityTags(`"order-1-v1", "weird,tag", W/"order-2-v1",`)
	want := []string{`"order-1-v1"`, `"weird,tag"`, `W/"order-2-v1"`}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %q, got %q", want, got)
	}

	t.Logf("input kept three members, not four: %q", got)
}

// TestIfNoneMatchUsesWeakComparison is the contrast that makes the strong/weak
// split concrete: the same weak validator that fails as a precondition is a
// perfectly good cache validator.
func TestIfNoneMatchUsesWeakComparison(t *testing.T) {
	server := httptest.NewServer(NewServer(NewStore()))
	defer server.Close()

	etag, _, err := getOrder(server.URL)
	if err != nil {
		t.Fatalf("initial read: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, server.URL+"/orders/1", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("If-None-Match", "W/"+etag)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("want 304 for a weak validator in If-None-Match, got %d", resp.StatusCode)
	}

	t.Logf("If-None-Match: W/%s -> %d", etag, resp.StatusCode)
}

// TestIfMatchStarMatchesAnyRepresentation covers the other accepted form of the
// field: "*" means "as long as the resource exists".
func TestIfMatchStarMatchesAnyRepresentation(t *testing.T) {
	server := httptest.NewServer(NewServer(NewStore()))
	defer server.Close()

	code, body, err := putOrder(server.URL, "*", "paid")
	if err != nil {
		t.Fatalf("star conditional put: %v", err)
	}
	if code != http.StatusNoContent {
		t.Fatalf("want 204 for If-Match: *, got %d (%s)", code, body)
	}

	t.Logf("If-Match: * -> %d", code)
}

// TestStaleWriterIsHandedTheCurrentValidator covers the end-to-end behaviour a
// rejected client depends on: the 412 carries the validator that now applies, so
// the retry needs no extra read. Note which branch produces it - the precondition
// check against the row read by Get(), not Update()'s error path. The contract
// of that error path is pinned separately, in
// TestUpdateReturnsCurrentRowOnFailedPrecondition.
func TestStaleWriterIsHandedTheCurrentValidator(t *testing.T) {
	server := httptest.NewServer(NewServer(NewStore()))
	defer server.Close()

	// Move the resource on, so the validator the client holds is now stale.
	if code, etag, body, err := putOrderDetailed(server.URL, `"order-1-v1"`, "paid"); err != nil || code != http.StatusNoContent {
		t.Fatalf("setup write: code=%d etag=%s body=%s err=%v", code, etag, body, err)
	}

	code, etag, body, err := putOrderDetailed(server.URL, `"order-1-v1"`, "shipped")
	if err != nil {
		t.Fatalf("stale conditional write: %v", err)
	}
	if code != http.StatusPreconditionFailed {
		t.Fatalf("want 412 for the stale validator, got %d (%s)", code, body)
	}
	if etag != `"order-1-v2"` {
		t.Fatalf("want the 412 to carry the current validator %q, got %q", `"order-1-v2"`, etag)
	}

	// The retry the 412 just made possible, with no extra read.
	retryCode, _, retryBody, err := putOrderDetailed(server.URL, etag, "shipped")
	if err != nil {
		t.Fatalf("retry with the returned validator: %v", err)
	}
	if retryCode != http.StatusNoContent {
		t.Fatalf("want the retry to succeed with 204, got %d (%s)", retryCode, retryBody)
	}

	t.Logf("stale If-Match %s -> %d carrying ETag %s", `"order-1-v1"`, code, etag)
	t.Logf("retry with the returned validator -> %d", retryCode)
}

// TestUpdateReturnsCurrentRowOnFailedPrecondition pins the store contract the
// handler's Update() error path depends on, and it has to be tested at this
// level: no HTTP request can reliably reach that path, because it requires the
// version to move between the handler's Get() and its Update(). A store that
// returned the zero value on error - the idiomatic Go answer - would make the
// handler emit ETag "order-0-v0", a validator that matches nothing, so every
// rejected client would retry and fail forever.
func TestUpdateReturnsCurrentRowOnFailedPrecondition(t *testing.T) {
	store := NewStore()

	if _, err := store.Update(1, 1, "paid"); err != nil {
		t.Fatalf("first write should apply: %v", err)
	}

	stale, err := store.Update(1, 1, "shipped")
	if !errors.Is(err, errPrecondition) {
		t.Fatalf("want errPrecondition from a stale write, got %v", err)
	}
	if stale.Version != 2 || stale.Status != "paid" {
		t.Fatalf("want the row as it now stands (v2, paid), got %+v", stale)
	}
	if tag := entityTag(stale); tag != `"order-1-v2"` {
		t.Fatalf("want the failed write to expose the current validator %q, got %q", `"order-1-v2"`, tag)
	}

	t.Logf("stale write returned %+v, entityTag %s", stale, entityTag(stale))
}
