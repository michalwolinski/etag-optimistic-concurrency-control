// A deliberately small origin server whose only interesting property is that it
// refuses to apply a write whose precondition does not hold.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

var (
	errNotFound     = errors.New("order not found")
	errPrecondition = errors.New("precondition failed")
)

// Order mirrors the row the PHP layer persists: same id, same status, same
// monotonically increasing version.
type Order struct {
	ID      int    `json:"id"`
	Status  string `json:"status"`
	Version int    `json:"version"`
}

// Store is the entire storage engine: a map and a mutex. The mutex keeps
// concurrent access from corrupting the map. It does nothing to prevent lost
// updates - that is the version comparison's job.
type Store struct {
	mu     sync.Mutex
	orders map[int]Order
}

func NewStore() *Store {
	return &Store{orders: map[int]Order{
		1: {ID: 1, Status: "new", Version: 1},
	}}
}

func (s *Store) Get(id int) (Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	order, ok := s.orders[id]
	return order, ok
}

// Update is the compare-and-set the SQL statement performs, expressed in Go:
//
//	UPDATE orders SET status = ?, version = version + 1
//	 WHERE id = ? AND version = ?
//
// Read, compare and write happen under one lock, so two writers cannot both
// match the same version.
func (s *Store) Update(id, expectedVersion int, status string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.orders[id]
	if !ok {
		return Order{}, errNotFound
	}
	if current.Version != expectedVersion {
		return current, errPrecondition
	}

	current.Status = status
	current.Version++
	s.orders[id] = current

	return current, nil
}

// entityTag mints a STRONG validator. It is derived from the version, which is
// unique across every representation this resource has ever had - the property
// RFC 9110 section 8.8.1 demands of a strong validator. A hash of the body
// would not have it, because a body can recur.
func entityTag(order Order) string {
	return fmt.Sprintf("\"order-%d-v%d\"", order.ID, order.Version)
}

// strongCompare is the comparison function from RFC 9110 section 8.8.3.2: two
// entity tags are equivalent only if BOTH are strong and their opaque tags
// match character for character. A weak tag never matches, on either side.
func strongCompare(a, b string) bool {
	if strings.HasPrefix(a, "W/") || strings.HasPrefix(b, "W/") {
		return false
	}
	return a == b
}

// weakCompare is the comparison function If-None-Match uses: the weakness
// indicator is ignored. Cache validation can tolerate a validator that did not
// change for every byte change. A precondition cannot.
func weakCompare(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}

// splitEntityTags splits an entity-tag list into its members, ignoring the
// commas that sit inside a quoted tag. RFC 9110 allows any VCHAR except DQUOTE
// inside an opaque tag, and "," is a VCHAR: strings.Split would shred such a
// tag into two members that mean something the client never sent.
//
// Empty members are dropped, which RFC 9110 section 5.6.1.2 requires a
// recipient to tolerate.
func splitEntityTags(value string) []string {
	var (
		tags     []string
		current  strings.Builder
		inQuotes bool
	)
	flush := func() {
		if tag := strings.TrimSpace(current.String()); tag != "" {
			tags = append(tags, tag)
		}
		current.Reset()
	}

	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case c == '"':
			inQuotes = !inQuotes
			current.WriteByte(c)
		case c == ',' && !inQuotes:
			flush()
		default:
			current.WriteByte(c)
		}
	}
	flush()

	return tags
}

// evaluateIfMatch implements RFC 9110 section 13.1.1. It reports whether the
// field was present at all, so the caller can answer 428 when it insists on
// conditional writes, and whether the condition holds.
func evaluateIfMatch(header http.Header, current string) (present, satisfied bool) {
	// Repeated field lines combine into one comma-separated list, so every
	// line has to be considered - not just the first one.
	values := header.Values("If-Match")
	if len(values) == 0 {
		return false, false
	}

	for _, value := range values {
		for _, tag := range splitEntityTags(value) {
			if tag == "*" {
				return true, true // any current representation satisfies it
			}
			if strongCompare(tag, current) {
				return true, true
			}
		}
	}

	return true, false
}

func evaluatesToNotModified(header http.Header, current string) bool {
	for _, value := range header.Values("If-None-Match") {
		for _, tag := range splitEntityTags(value) {
			if tag == "*" || weakCompare(tag, current) {
				return true
			}
		}
	}

	return false
}

type Server struct {
	store *Store

	// HTTP does not require conditional writes; this server does. Admitting an
	// unconditional write is admitting last-write-wins, which is the failure
	// mode the ETag exists to prevent (RFC 6585 section 3).
	requireIfMatch bool
}

func NewServer(store *Store) *Server {
	return &Server{store: store, requireIfMatch: true}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/orders/"))
	if err != nil || id <= 0 {
		http.Error(w, "order id must be a positive integer", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGet(w, r, id)
	case http.MethodPut:
		s.handlePut(w, r, id)
	default:
		w.Header().Set("Allow", "GET, PUT")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, id int) {
	order, ok := s.store.Get(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	tag := entityTag(order)
	w.Header().Set("ETag", tag)

	// The other half of the ETag story. If-None-Match is a cache validator, so
	// RFC 9110 section 13.1.2 evaluates it with the WEAK comparison function:
	// a cache only needs to know whether this is the representation it holds.
	if evaluatesToNotModified(r.Header, tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	writeJSON(w, http.StatusOK, order)
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, id int) {
	current, ok := s.store.Get(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	present, satisfied := evaluateIfMatch(r.Header, entityTag(current))

	switch {
	case !present && s.requireIfMatch:
		// Refuse to guess rather than overwrite. The client gets the current
		// validator back so its retry can be conditional.
		w.Header().Set("ETag", entityTag(current))
		http.Error(w, "conditional request required: send If-Match", http.StatusPreconditionRequired)
		return
	case present && !satisfied:
		// RFC 9110 section 13.2.2, step 1: If-Match is evaluated first, and a
		// false condition means the method is not performed.
		w.Header().Set("ETag", entityTag(current))
		http.Error(w, "precondition failed", http.StatusPreconditionFailed)
		return
	}

	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "body must be {\"status\": \"...\"}", http.StatusBadRequest)
		return
	}
	if body.Status == "" {
		http.Error(w, "status must not be empty", http.StatusUnprocessableEntity)
		return
	}

	updated, err := s.store.Update(id, current.Version, body.Status)
	if err != nil {
		// The row moved between the precondition check and the write. This is
		// the same closing window the SQL statement closes with
		// rowCount()==0, and it produces the same answer to the client.
		w.Header().Set("ETag", entityTag(updated))
		http.Error(w, "precondition failed", http.StatusPreconditionFailed)
		return
	}

	w.Header().Set("ETag", entityTag(updated))
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("writing response: %v", err)
	}
}

func main() {
	addr := ":8080"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}

	log.Printf("listening on %s - GET %s, PUT with If-Match", addr, "/orders/1")
	log.Fatal(http.ListenAndServe(addr, NewServer(NewStore())))
}
